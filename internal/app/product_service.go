package app

import (
	"context"
	"log/slog"

	"checkout/internal/app/adapter"
	"checkout/internal/domain/def"
	"checkout/internal/domain/model"
	"checkout/internal/infra/cache"
	"checkout/internal/infra/database/repo"
)

// ProductService owns product reads/writes and the stock strategy:
//   - every product: available_stock in Postgres is the source of truth for reservations
//   - flash-sale products: a Valkey gate in front of the DB sheds excess traffic before it reaches the hot row
type ProductService struct {
	repo  *repo.ProductRepo
	cache *cache.ProductCache
	gate  *cache.FlashSaleGate
}

func NewProductService(r *repo.ProductRepo, c *cache.ProductCache, g *cache.FlashSaleGate) *ProductService {
	return &ProductService{repo: r, cache: c, gate: g}
}

func (s *ProductService) List(ctx context.Context) ([]model.Product, error) {
	return s.repo.List(ctx)
}

// GetProducts reads product info through the cache (cache-aside) and overlays live stock numbers from the DB.
// Stock is not cached because it changes on every checkout; a plain read never waits on row locks.
func (s *ProductService) GetProducts(ctx context.Context, ids []int64) (map[int64]model.Product, error) {
	found, missing, err := s.cache.GetMany(ctx, ids)
	if err != nil {
		slog.WarnContext(ctx, "product cache unavailable, falling back to DB", "err", err)
		found, missing = map[int64]model.Product{}, ids
	}
	if len(missing) > 0 {
		loaded, err := s.repo.GetByIDs(ctx, missing)
		if err != nil {
			return nil, err
		}
		if err := s.cache.SetMany(ctx, loaded); err != nil {
			slog.WarnContext(ctx, "fill product cache failed", "err", err)
		}
		for _, p := range loaded {
			found[p.ID] = p
		}
	}

	levels, err := s.repo.GetStockLevels(ctx, ids)
	if err != nil {
		return nil, err
	}
	for id, p := range found {
		if l, ok := levels[id]; ok {
			p.Stock, p.AvailableStock = l.Stock, l.Available
			found[id] = p
		}
	}
	return found, nil
}

// UpdateProduct is the admin write path: DB and cache (incl. the flash-sale gate) must both succeed,
// otherwise the DB change is rolled back.
func (s *ProductService) UpdateProduct(ctx context.Context, id int64, in adapter.UpdateProductInput) (model.Product, error) {
	if (in.Price != nil && *in.Price < 0) || (in.Stock != nil && *in.Stock < 0) {
		return model.Product{}, def.NewError(def.ErrInvalidRequest, "price and stock must be >= 0")
	}
	syncCache := func(p model.Product) error {
		if err := s.cache.SetMany(ctx, []model.Product{p}); err != nil {
			return err
		}
		if p.FlashSale {
			return s.gate.Set(ctx, p.ID, p.AvailableStock)
		}
		return s.gate.Delete(ctx, p.ID)
	}
	onCommitFailed := func(id int64) {
		if err := s.cache.Delete(ctx, id); err != nil {
			slog.ErrorContext(ctx, "invalidate product cache after failed commit", "product_id", id, "err", err)
		}
	}
	return s.repo.Update(ctx, id, repo.UpdateProductParams{Price: in.Price, Stock: in.Stock, FlashSale: in.FlashSale},
		syncCache, onCommitFailed)
}

// PassFlashGate takes units from the Valkey gate for the flash-sale items of a checkout (all-or-nothing).
// Non flash-sale items skip the gate and go straight to the DB reservation.
// Returns what was taken so the caller can give it back if the DB reservation does not happen.
func (s *ProductService) PassFlashGate(ctx context.Context, items []model.CheckoutItem, products map[int64]model.Product) ([]cache.GateItem, error) {
	var gateItems []cache.GateItem
	for _, it := range items {
		if p, ok := products[it.ProductID]; ok && p.FlashSale {
			gateItems = append(gateItems, cache.GateItem{ProductID: it.ProductID, Quantity: it.Quantity})
		}
	}
	if len(gateItems) == 0 {
		return nil, nil
	}

	for attempt := 0; attempt < 2; attempt++ {
		res, err := s.gate.Take(ctx, gateItems)
		if err != nil {
			// Valkey down: fail open and let the DB decide (slower under load, still correct).
			slog.WarnContext(ctx, "flash gate unavailable, falling back to DB only", "err", err)
			return nil, nil
		}
		switch {
		case res.OK:
			return gateItems, nil
		case res.ShortProductID != 0:
			return nil, shortageError(items, products[res.ShortProductID], res.ShortProductID, res.Available)
		case res.MissingProductID != 0 && attempt == 0:
			if err := s.initGate(ctx, res.MissingProductID); err != nil {
				return nil, err
			}
		}
	}
	return nil, nil // gate still missing: let the DB decide
}

// ReturnToGate gives back units taken from the gate when the checkout did not reserve in the DB.
func (s *ProductService) ReturnToGate(ctx context.Context, items []cache.GateItem) {
	if err := s.gate.Give(ctx, items); err != nil {
		slog.ErrorContext(ctx, "return units to flash gate failed (sync job will realign)", "err", err)
	}
}

// ReturnLinesToGate refills the gate after DB units became available again (checkout failed / order expired).
func (s *ProductService) ReturnLinesToGate(ctx context.Context, lines []repo.StockLine) {
	var items []cache.GateItem
	for _, l := range lines {
		if l.FlashSale {
			items = append(items, cache.GateItem{ProductID: l.ProductID, Quantity: l.Quantity})
		}
	}
	s.ReturnToGate(ctx, items)
}

// SyncFlashGates realigns every gate with the DB (source of truth). Run periodically and at startup.
func (s *ProductService) SyncFlashGates(ctx context.Context) error {
	products, err := s.repo.ListFlashSale(ctx)
	if err != nil {
		return err
	}
	for _, p := range products {
		if err := s.gate.Set(ctx, p.ID, p.AvailableStock); err != nil {
			return err
		}
	}
	return nil
}

// WarmUp loads every product into the cache and initialises the flash-sale gates.
func (s *ProductService) WarmUp(ctx context.Context) error {
	products, err := s.repo.List(ctx)
	if err != nil {
		return err
	}
	if err := s.cache.SetMany(ctx, products); err != nil {
		return err
	}
	return s.SyncFlashGates(ctx)
}

func (s *ProductService) initGate(ctx context.Context, productID int64) error {
	levels, err := s.repo.GetStockLevels(ctx, []int64{productID})
	if err != nil {
		return err
	}
	if l, ok := levels[productID]; ok {
		return s.gate.InitIfAbsent(ctx, productID, l.Available)
	}
	return nil
}

func shortageError(items []model.CheckoutItem, p model.Product, productID int64, available int) error {
	issue := model.ItemIssue{ProductID: productID, Name: p.Name, CurrentPrice: p.Price,
		Reason: model.ItemIssueInsufficientStock, AvailableStock: max(available, 0)}
	if available <= 0 {
		issue.Reason = model.ItemIssueSoldOut
	}
	for _, it := range items {
		if it.ProductID == productID {
			issue.RequestedQty, issue.ExpectedPrice = it.Quantity, it.UnitPrice
		}
	}
	return def.NewError(def.ErrItemsChanged, "商品庫存不足，請確認後重新送出").WithDetails(map[string]any{"items": []model.ItemIssue{issue}})
}
