package repo

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"checkout/internal/domain/def"
	"checkout/internal/domain/model"
)

const productColumns = "id, name, description, highlights, image_url, price, original_price, stock, is_addon, version, updated_at"

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

func (r *ProductRepo) GetByIDs(ctx context.Context, ids []int64) ([]model.Product, error) {
	rows, err := r.pool.Query(ctx, "SELECT "+productColumns+" FROM products WHERE id = ANY($1) ORDER BY id", ids)
	if err != nil {
		return nil, fmt.Errorf("get products: %w", err)
	}
	return collectProducts(rows)
}

type UpdateProductParams struct {
	Price *int64
	Stock *int
}

// Update writes the product to DB and calls syncCache before commit.
// If syncCache fails the DB transaction is rolled back, so "DB + cache both succeed" or nothing changes.
// If the commit itself fails after the cache was written, onCommitFailed lets the caller invalidate the cache.
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
		       stock = COALESCE($3, stock),
		       version = version + 1,
		       updated_at = now()
		 WHERE id = $1
		RETURNING `+productColumns, id, p.Price, p.Stock)
	product, err := scanProduct(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Product{}, def.NewError(def.ErrProductNotFound, "product not found")
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
		&p.OriginalPrice, &p.Stock, &p.IsAddon, &p.Version, &p.UpdatedAt)
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
