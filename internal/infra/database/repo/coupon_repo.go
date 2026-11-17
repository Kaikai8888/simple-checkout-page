package repo

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"checkout/internal/domain/model"
)

const couponColumns = "code, description, discount_type, value, min_subtotal, usage_limit, used_count, active, expires_at"

type CouponRepo struct {
	pool *pgxpool.Pool
}

func NewCouponRepo(pool *pgxpool.Pool) *CouponRepo {
	return &CouponRepo{pool: pool}
}

// Get returns nil when the coupon does not exist.
func (r *CouponRepo) Get(ctx context.Context, code string) (*model.Coupon, error) {
	return getCoupon(ctx, r.pool, "SELECT "+couponColumns+" FROM coupons WHERE code = $1", code)
}

type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func getCoupon(ctx context.Context, q querier, sql, code string) (*model.Coupon, error) {
	var c model.Coupon
	err := q.QueryRow(ctx, sql, code).Scan(&c.Code, &c.Description, &c.DiscountType, &c.Value,
		&c.MinSubtotal, &c.UsageLimit, &c.UsedCount, &c.Active, &c.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}
