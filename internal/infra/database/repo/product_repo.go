package repo

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"checkout/internal/domain/def"
	"checkout/internal/domain/model"
)

const productColumns = "id, name, description, highlights, image_url, price, original_price, stock, available_stock, flash_sale, is_addon, version, updated_at"

const pgCheckViolation = "23514"

type ProductRepo struct {
	pool *pgxpool.Pool
}

func NewProductRepo(pool *pgxpool.Pool) *ProductRepo {
	return &ProductRepo{pool: pool}
}

func (r *ProductRepo) List(ctx context.Context) ([]model.Product, error) {
	rows, err := r.pool.Query(ctx, "SELECT "+productColumns+" FROM products ORDER BY is_addon, id")
	if err != nil {
		return nil, fmt.Errorf("list products: %w", err)
	}
	return collectProducts(rows)
}

func (r *ProductRepo) ListFlashSale(ctx context.Context) ([]model.Product, error) {
	rows, err := r.pool.Query(ctx, "SELECT "+productColumns+" FROM products WHERE flash_sale ORDER BY id")
	if err != nil {
		return nil, fmt.Errorf("list flash sale products: %w", err)
	}
	return collectProducts(rows)
}

func (r *ProductRepo) GetByIDs(ctx context.Context, ids []int64) ([]model.Product, error) {
	rows, err := r.pool.Query(ctx, "SELECT "+productColumns+" FROM products WHERE id = ANY($1) ORDER BY id", ids)
	if err != nil {
		return nil, fmt.Errorf("get products: %w", err)
	}
	return collectProducts(rows)
}

type StockLevel struct {
	Stock     int
	Available int
}

// GetStockLevels reads the live stock numbers. Plain MVCC reads never wait on row locks,
// so this stays cheap even while checkouts are updating the same rows.
func (r *ProductRepo) GetStockLevels(ctx context.Context, ids []int64) (map[int64]StockLevel, error) {
	rows, err := r.pool.Query(ctx, "SELECT id, stock, available_stock FROM products WHERE id = ANY($1)", ids)
	if err != nil {
		return nil, fmt.Errorf("get stock levels: %w", err)
	}
	defer rows.Close()
	levels := make(map[int64]StockLevel, len(ids))
	for rows.Next() {
		var id int64
		var l StockLevel
		if err := rows.Scan(&id, &l.Stock, &l.Available); err != nil {
			return nil, err
		}
		levels[id] = l
	}
	return levels, rows.Err()
}

type UpdateProductParams struct {
	Price     *int64
	Stock     *int
	FlashSale *bool
}

// Update writes the product to DB and calls syncCache before commit.
// If syncCache fails the DB transaction is rolled back, so "DB + cache both succeed" or nothing changes.
// If the commit itself fails after the cache was written, onCommitFailed lets the caller invalidate the cache.
//
// Setting stock moves available_stock by the same delta, so units held by in-flight checkouts stay held.
func (r *ProductRepo) Update(ctx context.Context, id int64, p UpdateProductParams,
	syncCache func(model.Product) error, onCommitFailed func(id int64)) (model.Product, error) {

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return model.Product{}, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	row := tx.QueryRow(ctx, `
		UPDATE products
		   SET price = COALESCE($2, price),
		       available_stock = available_stock + (COALESCE($3, stock) - stock),
		       stock = COALESCE($3, stock),
		       flash_sale = COALESCE($4, flash_sale),
		       version = version + 1,
		       updated_at = now()
		 WHERE id = $1
		RETURNING `+productColumns, id, p.Price, p.Stock, p.FlashSale)
	product, err := scanProduct(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Product{}, def.NewError(def.ErrProductNotFound, "product not found")
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgCheckViolation {
		return model.Product{}, def.NewError(def.ErrInvalidRequest, "庫存不可小於目前結帳中已預留的數量")
	}
	if err != nil {
		return model.Product{}, fmt.Errorf("update product: %w", err)
	}

	if err := syncCache(product); err != nil {
		return model.Product{}, def.NewError(def.ErrCacheWrite, "failed to update product cache, change rolled back")
	}
	if err := tx.Commit(ctx); err != nil {
		onCommitFailed(id)
		return model.Product{}, fmt.Errorf("commit product update: %w", err)
	}
	return product, nil
}

func scanProduct(row pgx.Row) (model.Product, error) {
	var p model.Product
	err := row.Scan(&p.ID, &p.Name, &p.Description, &p.Highlights, &p.ImageUrl, &p.Price,
		&p.OriginalPrice, &p.Stock, &p.AvailableStock, &p.FlashSale, &p.IsAddon, &p.Version, &p.UpdatedAt)
	return p, err
}

func collectProducts(rows pgx.Rows) ([]model.Product, error) {
	defer rows.Close()
	var products []model.Product
	for rows.Next() {
		p, err := scanProduct(rows)
		if err != nil {
			return nil, fmt.Errorf("scan product: %w", err)
		}
		products = append(products, p)
	}
	return products, rows.Err()
}
