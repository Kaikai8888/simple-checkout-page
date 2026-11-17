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

const paymentColumns = "id, order_id, method, amount, status, provider_session_id, provider_txn_id, payment_url, failure_reason, created_at, updated_at"

type PaymentRepo struct {
	pool *pgxpool.Pool
}

func NewPaymentRepo(pool *pgxpool.Pool) *PaymentRepo {
	return &PaymentRepo{pool: pool}
}

func (r *PaymentRepo) Create(ctx context.Context, p model.Payment) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO payments (id, order_id, method, amount, status) VALUES ($1, $2, $3, $4, $5)`,
		p.ID, p.OrderID, p.Method, p.Amount, p.Status)
	return err
}

func (r *PaymentRepo) SetSession(ctx context.Context, id, sessionID, paymentUrl string) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE payments SET provider_session_id = $2, payment_url = $3, updated_at = now() WHERE id = $1`,
		id, sessionID, paymentUrl)
	return err
}

func (r *PaymentRepo) MarkFailed(ctx context.Context, id, reason string) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE payments SET status = $2, failure_reason = $3, updated_at = now() WHERE id = $1 AND status = $4`,
		id, def.PaymentStatusFailed, reason, def.PaymentStatusPending)
	return err
}

// LatestByOrder returns nil when the order has no payment yet.
func (r *PaymentRepo) LatestByOrder(ctx context.Context, orderID string) (*model.Payment, error) {
	p, err := scanPayment(r.pool.QueryRow(ctx,
		"SELECT "+paymentColumns+" FROM payments WHERE order_id = $1 ORDER BY created_at DESC LIMIT 1", orderID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return p, err
}

type PaymentResult struct {
	PaymentID     string
	Succeeded     bool
	ProviderTxnID string
	Amount        int64
	FailureReason string
}

// ApplyResult applies a gateway callback idempotently.
// Returns the resulting order status ("" when the callback was a duplicate and nothing changed).
func (r *PaymentRepo) ApplyResult(ctx context.Context, res PaymentResult) (string, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	p, err := scanPayment(tx.QueryRow(ctx, "SELECT "+paymentColumns+" FROM payments WHERE id = $1 FOR UPDATE", res.PaymentID))
	if errors.Is(err, pgx.ErrNoRows) {
		return "", def.NewError(def.ErrPaymentNotFound, "payment not found")
	}
	if err != nil {
		return "", fmt.Errorf("lock payment: %w", err)
	}
	// succeeded / failed are final: duplicated callbacks are acknowledged without side effects.
	// expired is NOT final: the shopper may still have paid at the gateway right before we expired it.
	if p.Status == def.PaymentStatusSucceeded || p.Status == def.PaymentStatusFailed {
		return "", tx.Commit(ctx)
	}

	status, reason := def.PaymentStatusFailed, res.FailureReason
	if res.Succeeded {
		if res.Amount != p.Amount {
			status, reason = def.PaymentStatusFailed, fmt.Sprintf("amount mismatch: expected %d got %d", p.Amount, res.Amount)
		} else {
			status = def.PaymentStatusSucceeded
		}
	}
	if _, err := tx.Exec(ctx, `
		UPDATE payments SET status = $2, provider_txn_id = $3, failure_reason = $4, updated_at = now() WHERE id = $1`,
		p.ID, status, res.ProviderTxnID, reason); err != nil {
		return "", fmt.Errorf("update payment: %w", err)
	}

	var orderStatus string
	if err := tx.QueryRow(ctx, "SELECT status FROM orders WHERE id = $1 FOR UPDATE", p.OrderID).Scan(&orderStatus); err != nil {
		return "", fmt.Errorf("lock order: %w", err)
	}
	if status == def.PaymentStatusSucceeded {
		next := def.OrderStatusPaid
		if orderStatus != def.OrderStatusPendingPayment {
			// paid after the order expired (stock already released) or paid twice -> needs refund handling
			next = def.OrderStatusRefundRequired
		}
		if _, err := tx.Exec(ctx, `
			UPDATE orders SET status = $2, paid_at = CASE WHEN $2 = 'paid' THEN now() ELSE paid_at END, updated_at = now()
			 WHERE id = $1`, p.OrderID, next); err != nil {
			return "", fmt.Errorf("update order: %w", err)
		}
		orderStatus = next
	}
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("commit payment result: %w", err)
	}
	return orderStatus, nil
}

func scanPayment(row pgx.Row) (*model.Payment, error) {
	var p model.Payment
	err := row.Scan(&p.ID, &p.OrderID, &p.Method, &p.Amount, &p.Status, &p.ProviderSessionID,
		&p.ProviderTxnID, &p.PaymentUrl, &p.FailureReason, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &p, nil
}
