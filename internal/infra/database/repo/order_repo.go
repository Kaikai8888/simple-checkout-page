package repo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"checkout/internal/domain/def"
	"checkout/internal/domain/model"
)

type OrderRepo struct {
	pool *pgxpool.Pool
}

func NewOrderRepo(pool *pgxpool.Pool) *OrderRepo {
	return &OrderRepo{pool: pool}
}

type PlaceOrderParams struct {
	OrderID   string
	OrderNo   string
	Draft     *model.CheckoutDraft
	ExpiresAt time.Time
	// Quote runs domain validation + pricing against the *locked* rows.
	Quote func(products map[int64]model.Product, coupon *model.Coupon) (model.PriceBreakdown, error)
}

// PlaceOrder creates the order in a single transaction:
// lock products -> validate & price -> deduct stock -> commit reservation -> consume coupon -> upsert customer
// -> insert order -> complete draft.
func (r *OrderRepo) PlaceOrder(ctx context.Context, p PlaceOrderParams) (model.Order, error) {
	req := p.Draft.Request
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return model.Order{}, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	// ORDER BY id gives a stable lock order across concurrent transactions -> no deadlock.
	rows, err := tx.Query(ctx, "SELECT "+productColumns+" FROM products WHERE id = ANY($1) ORDER BY id FOR UPDATE", req.ProductIDs())
	if err != nil {
		return model.Order{}, fmt.Errorf("lock products: %w", err)
	}
	locked, err := collectProducts(rows)
	if err != nil {
		return model.Order{}, err
	}
	products := make(map[int64]model.Product, len(locked))
	for _, prod := range locked {
		products[prod.ID] = prod
	}

	var coupon *model.Coupon
	if req.CouponCode != "" {
		coupon, err = getCoupon(ctx, tx, "SELECT "+couponColumns+" FROM coupons WHERE code = $1 FOR UPDATE", req.CouponCode)
		if err != nil {
			return model.Order{}, fmt.Errorf("lock coupon: %w", err)
		}
	}

	breakdown, err := p.Quote(products, coupon)
	if err != nil {
		return model.Order{}, err
	}

	for _, line := range breakdown.Lines {
		// stock >= qty in WHERE + CHECK (stock >= 0) => can never oversell
		tag, err := tx.Exec(ctx, `
			UPDATE products SET stock = stock - $2, version = version + 1, updated_at = now()
			 WHERE id = $1 AND stock >= $2`, line.ProductID, line.Quantity)
		if err != nil {
			return model.Order{}, fmt.Errorf("deduct stock: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return model.Order{}, def.NewError(def.ErrItemsChanged, "商品庫存不足")
		}
	}
	// available_stock was already deducted when the draft reserved; the reservation now belongs to the order.
	if _, err := tx.Exec(ctx, `
		UPDATE inventory_reservations SET status = $2, updated_at = now() WHERE draft_id = $1 AND status = $3`,
		p.Draft.ID, reservationCommitted, reservationHeld); err != nil {
		return model.Order{}, fmt.Errorf("commit reservation: %w", err)
	}
	if coupon != nil {
		if _, err := tx.Exec(ctx, "UPDATE coupons SET used_count = used_count + 1 WHERE code = $1", coupon.Code); err != nil {
			return model.Order{}, fmt.Errorf("consume coupon: %w", err)
		}
	}

	var customerID int64
	err = tx.QueryRow(ctx, `
		INSERT INTO customers (email, name, phone) VALUES ($1, $2, $3)
		ON CONFLICT (email) DO UPDATE SET name = EXCLUDED.name, phone = EXCLUDED.phone, updated_at = now()
		RETURNING id`, req.Customer.Email, req.Customer.Name, req.Customer.Phone).Scan(&customerID)
	if err != nil {
		return model.Order{}, fmt.Errorf("upsert customer: %w", err)
	}

	shipping, _ := json.Marshal(req.Shipping)
	invoice, _ := json.Marshal(req.Invoice)
	var createdAt time.Time
	err = tx.QueryRow(ctx, `
		INSERT INTO orders (id, order_no, draft_id, customer_id, status, subtotal, discount, shipping_fee, donation, total,
		                    coupon_code, payment_method, shipping, invoice, note, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, NULLIF($11, ''), $12, $13, $14, $15, $16)
		RETURNING created_at`,
		p.OrderID, p.OrderNo, p.Draft.ID, customerID, def.OrderStatusPendingPayment,
		breakdown.Subtotal, breakdown.Discount, breakdown.ShippingFee, breakdown.Donation, breakdown.Total,
		req.CouponCode, req.PaymentMethod, shipping, invoice, req.Note, p.ExpiresAt).Scan(&createdAt)
	if err != nil {
		return model.Order{}, fmt.Errorf("insert order: %w", err)
	}
	for _, line := range breakdown.Lines {
		_, err := tx.Exec(ctx, `
			INSERT INTO order_items (order_id, product_id, name, unit_price, quantity, amount)
			VALUES ($1, $2, $3, $4, $5, $6)`, p.OrderID, line.ProductID, line.Name, line.UnitPrice, line.Quantity, line.Amount)
		if err != nil {
			return model.Order{}, fmt.Errorf("insert order item: %w", err)
		}
	}

	if _, err := tx.Exec(ctx, `
		UPDATE checkout_drafts SET status = $2, order_id = $3, updated_at = now() WHERE id = $1`,
		p.Draft.ID, def.DraftStatusCompleted, p.OrderID); err != nil {
		return model.Order{}, fmt.Errorf("complete draft: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return model.Order{}, fmt.Errorf("commit order: %w", err)
	}

	return model.Order{
		ID: p.OrderID, OrderNo: p.OrderNo, DraftID: p.Draft.ID, CustomerID: customerID,
		Status: def.OrderStatusPendingPayment, Lines: breakdown.Lines,
		Subtotal: breakdown.Subtotal, Discount: breakdown.Discount, ShippingFee: breakdown.ShippingFee,
		Donation: breakdown.Donation, Total: breakdown.Total, CouponCode: req.CouponCode,
		PaymentMethod: req.PaymentMethod, Shipping: req.Shipping, Invoice: req.Invoice,
		Customer: req.Customer, Note: req.Note, ExpiresAt: p.ExpiresAt, CreatedAt: createdAt,
	}, nil
}

// Get returns nil when the order does not exist.
func (r *OrderRepo) Get(ctx context.Context, id string) (*model.Order, error) {
	var o model.Order
	var shipping, invoice []byte
	err := r.pool.QueryRow(ctx, `
		SELECT o.id, o.order_no, o.draft_id, o.customer_id, o.status, o.subtotal, o.discount, o.shipping_fee, o.donation,
		       o.total, COALESCE(o.coupon_code, ''), o.payment_method, o.shipping, o.invoice, o.note, o.expires_at,
		       o.paid_at, o.created_at, c.name, c.email, c.phone
		  FROM orders o JOIN customers c ON c.id = o.customer_id
		 WHERE o.id = $1`, id).Scan(
		&o.ID, &o.OrderNo, &o.DraftID, &o.CustomerID, &o.Status, &o.Subtotal, &o.Discount, &o.ShippingFee, &o.Donation,
		&o.Total, &o.CouponCode, &o.PaymentMethod, &shipping, &invoice, &o.Note, &o.ExpiresAt,
		&o.PaidAt, &o.CreatedAt, &o.Customer.Name, &o.Customer.Email, &o.Customer.Phone)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get order: %w", err)
	}
	_ = json.Unmarshal(shipping, &o.Shipping)
	_ = json.Unmarshal(invoice, &o.Invoice)

	rows, err := r.pool.Query(ctx, `
		SELECT product_id, name, unit_price, quantity, amount FROM order_items WHERE order_id = $1 ORDER BY id`, id)
	if err != nil {
		return nil, fmt.Errorf("get order items: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var l model.OrderLine
		if err := rows.Scan(&l.ProductID, &l.Name, &l.UnitPrice, &l.Quantity, &l.Amount); err != nil {
			return nil, err
		}
		o.Lines = append(o.Lines, l)
	}
	return &o, rows.Err()
}

// ExpireOverdue expires unpaid orders past their deadline and gives their units back to stock and
// available_stock (sellable again), plus coupon usage. Returns the restocked lines so the caller can
// also refill the Valkey flash-sale gate.
func (r *OrderRepo) ExpireOverdue(ctx context.Context, limit int) ([]string, []StockLine, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	type overdue struct{ id, coupon string }
	rows, err := tx.Query(ctx, `
		SELECT id, COALESCE(coupon_code, '') FROM orders
		 WHERE status = $1 AND expires_at < now()
		 ORDER BY expires_at LIMIT $2
		 FOR UPDATE SKIP LOCKED`, def.OrderStatusPendingPayment, limit)
	if err != nil {
		return nil, nil, fmt.Errorf("select overdue orders: %w", err)
	}
	var orders []overdue
	for rows.Next() {
		var o overdue
		if err := rows.Scan(&o.id, &o.coupon); err != nil {
			rows.Close()
			return nil, nil, err
		}
		orders = append(orders, o)
	}
	rows.Close()
	if len(orders) == 0 {
		return nil, nil, nil
	}

	var ids []string
	var restocked []StockLine
	for _, o := range orders {
		if _, err := tx.Exec(ctx, "UPDATE orders SET status = $2, updated_at = now() WHERE id = $1", o.id, def.OrderStatusExpired); err != nil {
			return nil, nil, fmt.Errorf("expire order: %w", err)
		}
		if _, err := tx.Exec(ctx, "UPDATE payments SET status = $2, updated_at = now() WHERE order_id = $1 AND status = $3",
			o.id, def.PaymentStatusExpired, def.PaymentStatusPending); err != nil {
			return nil, nil, fmt.Errorf("expire payments: %w", err)
		}
		lineRows, err := tx.Query(ctx, `
			UPDATE products p
			   SET stock = p.stock + oi.quantity, available_stock = p.available_stock + oi.quantity, updated_at = now()
			  FROM order_items oi
			 WHERE oi.order_id = $1 AND p.id = oi.product_id
			RETURNING p.id, oi.quantity, p.flash_sale`, o.id)
		if err != nil {
			return nil, nil, fmt.Errorf("restore stock: %w", err)
		}
		for lineRows.Next() {
			var l StockLine
			if err := lineRows.Scan(&l.ProductID, &l.Quantity, &l.FlashSale); err != nil {
				lineRows.Close()
				return nil, nil, err
			}
			restocked = append(restocked, l)
		}
		lineRows.Close()
		if o.coupon != "" {
			if _, err := tx.Exec(ctx, "UPDATE coupons SET used_count = GREATEST(used_count - 1, 0) WHERE code = $1", o.coupon); err != nil {
				return nil, nil, fmt.Errorf("restore coupon: %w", err)
			}
		}
		ids = append(ids, o.id)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, nil, fmt.Errorf("commit expire: %w", err)
	}
	return ids, restocked, nil
}
