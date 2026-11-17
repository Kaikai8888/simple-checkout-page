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

type ProductService struct {
	repo   *repo.ProductRepo
	drafts *repo.DraftRepo
	cache  *cache.ProductCache
	stock  *cache.StockStore
}

func NewProductService(r *repo.ProductRepo, drafts *repo.DraftRepo, c *cache.ProductCache, s *cache.StockStore) *ProductService {
	return &ProductService{repo: r, drafts: drafts, cache: c, stock: s}
}

func (s *ProductService) List(ctx context.Context) ([]model.Product, error) {
	products, err := s.repo.List(ctx)
	if err != nil {
		return nil, err
	}
	return s.withAvailableStock(ctx, products)
}

// GetProducts reads through the cache (cache-aside) and overlays the live "available" stock counter.
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

	list := make([]model.Product, 0, len(found))
	for _, p := range found {
		list = append(list, p)
	}
	list, err = s.withAvailableStock(ctx, list)
	if err != nil {
		return nil, err
	}
	result := make(map[int64]model.Product, len(list))
	for _, p := range list {
		result[p.ID] = p
	}
	return result, nil
}

// UpdateProduct is the admin write path: DB and cache must both succeed, otherwise the DB change is rolled back.
func (s *ProductService) UpdateProduct(ctx context.Context, id int64, in adapter.UpdateProductInput) (model.Product, error) {
	if (in.Price != nil && *in.Price < 0) || (in.Stock != nil && *in.Stock < 0) {
		return model.Product{}, def.NewError(def.ErrInvalidRequest, "price and stock must be >= 0")
	}
	syncCache := func(p model.Product) error {
		if err := s.cache.SetMany(ctx, []model.Product{p}); err != nil {
			return err
		}
		if in.Stock == nil {
			return nil
		}
		outstanding, err := s.drafts.OutstandingReserved(ctx, []int64{id}, "")
		if err != nil {
			return err
		}
		return s.stock.Set(ctx, id, max(p.Stock-outstanding[id], 0))
	}
	onCommitFailed := func(id int64) {
		if err := s.cache.Delete(ctx, id); err != nil {
			slog.ErrorContext(ctx, "invalidate product cache after failed commit", "product_id", id, "err", err)
		}
	}
	p, err := s.repo.Update(ctx, id, repo.UpdateProductParams{Price: in.Price, Stock: in.Stock}, syncCache, onCommitFailed)
	if err != nil {
		return model.Product{}, err
	}
	list, err := s.withAvailableStock(ctx, []model.Product{p})
	if err != nil {
		return p, nil
	}
	return list[0], nil
}

// ReserveStock atomically reserves units in Valkey for a draft. This decides who gets the last unit.
func (s *ProductService) ReserveStock(ctx context.Context, draftID string, items []model.CheckoutItem) error {
	reserveItems := make([]cache.ReserveItem, len(items))
	for i, it := range items {
		reserveItems[i] = cache.ReserveItem{ProductID: it.ProductID, Quantity: it.Quantity}
	}

	for attempt := 0; attempt < 2; attempt++ {
		res, err := s.stock.Reserve(ctx, draftID, reserveItems)
		if err != nil {
			return err
		}
		switch {
		case res.OK:
			return nil
		case res.ShortProductID != 0:
			return s.stockShortError(ctx, items, res.ShortProductID, res.AvailableForShort)
		case res.MissingProductID != 0 && attempt == 0:
			if err := s.initStockCounters(ctx, []int64{res.MissingProductID}, draftID); err != nil {
				return err
			}
		}
	}
	return def.NewError(def.ErrItemsChanged, "商品不存在").WithDetails(map[string]any{
		"items": []model.ItemIssue{{ProductID: items[0].ProductID, Reason: model.ItemIssueNotFound}},
	})
}

func (s *ProductService) ReleaseStock(ctx context.Context, draftID string) {
	if _, err := s.stock.Release(ctx, draftID); err != nil {
		slog.ErrorContext(ctx, "release stock reservation failed", "draft_id", draftID, "err", err)
	}
}

// CommitStock is called after the order is created: the reservation becomes a real deduction.
func (s *ProductService) CommitStock(ctx context.Context, draftID string, productIDs []int64) {
	if err := s.stock.Commit(ctx, draftID); err != nil {
		slog.ErrorContext(ctx, "commit stock reservation failed", "draft_id", draftID, "err", err)
	}
	s.invalidate(ctx, productIDs)
}

// Restock gives units back after an unpaid order expired (DB stock was already restored).
func (s *ProductService) Restock(ctx context.Context, lines []repo.RestockedLine) {
	var ids []int64
	for _, l := range lines {
		if err := s.stock.Add(ctx, l.ProductID, l.Quantity); err != nil {
			slog.ErrorContext(ctx, "restock valkey counter failed", "product_id", l.ProductID, "err", err)
		}
		ids = append(ids, l.ProductID)
	}
	s.invalidate(ctx, ids)
}

// WarmUp loads every product into the cache and initialises the stock counters.
func (s *ProductService) WarmUp(ctx context.Context) error {
	products, err := s.repo.List(ctx)
	if err != nil {
		return err
	}
	if err := s.cache.SetMany(ctx, products); err != nil {
		return err
	}
	ids := make([]int64, len(products))
	for i, p := range products {
		ids[i] = p.ID
	}
	return s.initStockCounters(ctx, ids, "")
}

func (s *ProductService) withAvailableStock(ctx context.Context, products []model.Product) ([]model.Product, error) {
	ids := make([]int64, len(products))
	for i, p := range products {
		ids[i] = p.ID
	}
	available, missing, err := s.stock.GetMany(ctx, ids)
	if err != nil {
		return nil, err
	}
	if len(missing) > 0 {
		if err := s.initStockCounters(ctx, missing, ""); err != nil {
			return nil, err
		}
		if available, _, err = s.stock.GetMany(ctx, ids); err != nil {
			return nil, err
		}
	}
	for i := range products {
		products[i].Stock = available[products[i].ID]
	}
	return products, nil
}

// initStockCounters rebuilds missing counters: available = DB stock - units held by in-flight drafts.
func (s *ProductService) initStockCounters(ctx context.Context, ids []int64, excludeDraftID string) error {
	products, err := s.repo.GetByIDs(ctx, ids)
	if err != nil {
		return err
	}
	outstanding, err := s.drafts.OutstandingReserved(ctx, ids, excludeDraftID)
	if err != nil {
		return err
	}
	for _, p := range products {
		if err := s.stock.InitIfAbsent(ctx, p.ID, max(p.Stock-outstanding[p.ID], 0)); err != nil {
			return err
		}
	}
	return nil
}

func (s *ProductService) stockShortError(ctx context.Context, items []model.CheckoutItem, productID int64, available int) error {
	issue := model.ItemIssue{ProductID: productID, Reason: model.ItemIssueInsufficientStock, AvailableStock: available}
	if available <= 0 {
		issue.Reason = model.ItemIssueSoldOut
	}
	for _, it := range items {
		if it.ProductID == productID {
			issue.RequestedQty, issue.ExpectedPrice = it.Quantity, it.UnitPrice
		}
	}
	if products, err := s.GetProducts(ctx, []int64{productID}); err == nil {
		issue.Name, issue.CurrentPrice = products[productID].Name, products[productID].Price
	}
	return def.NewError(def.ErrItemsChanged, "商品庫存不足，請確認後重新送出").WithDetails(map[string]any{"items": []model.ItemIssue{issue}})
}

func (s *ProductService) invalidate(ctx context.Context, ids []int64) {
	if err := s.cache.Delete(ctx, ids...); err != nil {
		slog.ErrorContext(ctx, "invalidate product cache failed", "product_ids", ids, "err", err)
	}
}
