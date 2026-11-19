package repo

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"checkout/internal/domain/def"
	"checkout/internal/domain/model"
)

const draftColumns = "id, idempotency_key, payload, status, attempts, COALESCE(error_code, ''), COALESCE(error_message, ''), error_details, COALESCE(order_id, ''), created_at, updated_at"

type DraftRepo struct {
	pool *pgxpool.Pool
}

func NewDraftRepo(pool *pgxpool.Pool) *DraftRepo {
	return &DraftRepo{pool: pool}
}

// StockLine is a quantity of one product moved in or out of a reservation.
type StockLine struct {
	ProductID int64
	Quantity  int
	FlashSale bool
}

const (
	reservationHeld      = "held"
	reservationCommitted = "committed"
	reservationReleased  = "released"
)

// CreateWithReservation inserts a pending draft and reserves available_stock for every item in ONE transaction:
// either the draft exists and all its units are held, or nothing happened.
// If the idempotency key was already used, the existing draft id is returned with created = false.
func (r *DraftRepo) CreateWithReservation(ctx context.Context, id, idempotencyKey string, req model.CheckoutRequest) (draftID string, created bool, err error) {
	payload, err := json.Marshal(req)
	if err != nil {
		return "", false, fmt.Errorf("marshal draft payload: %w", err)
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return "", false, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	err = tx.QueryRow(ctx, `
		INSERT INTO checkout_drafts (id, idempotency_key, payload, status)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (idempotency_key) DO NOTHING
		RETURNING id`, id, idempotencyKey, payload, def.DraftStatusPending).Scan(&draftID)
	if errors.Is(err, pgx.ErrNoRows) {
		// concurrent request with the same key won the race
		if err := r.pool.QueryRow(ctx, "SELECT id FROM checkout_drafts WHERE idempotency_key = $1", idempotencyKey).Scan(&draftID); err != nil {
			return "", false, fmt.Errorf("get draft by idempotency key: %w", err)
		}
		return draftID, false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("insert draft: %w", err)
	}

	// Lock rows in product-id order so concurrent checkouts with overlapping carts cannot deadlock.
	items := slices.Clone(req.Items)
	slices.SortFunc(items, func(a, b model.CheckoutItem) int { return cmp.Compare(a.ProductID, b.ProductID) })
	for _, it := range items {
		tag, err := tx.Exec(ctx, `
			UPDATE products SET available_stock = available_stock - $2, updated_at = now()
			 WHERE id = $1 AND available_stock >= $2`, it.ProductID, it.Quantity)
		if err != nil {
			return "", false, fmt.Errorf("reserve stock: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return "", false, r.shortageError(ctx, it)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO inventory_reservations (draft_id, product_id, quantity, status) VALUES ($1, $2, $3, $4)`,
			id, it.ProductID, it.Quantity, reservationHeld); err != nil {
			return "", false, fmt.Errorf("insert reservation: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return "", false, fmt.Errorf("commit draft: %w", err)
	}
	return draftID, true, nil
}

func (r *DraftRepo) shortageError(ctx context.Context, it model.CheckoutItem) error {
	issue := model.ItemIssue{ProductID: it.ProductID, RequestedQty: it.Quantity, ExpectedPrice: it.UnitPrice}
	err := r.pool.QueryRow(ctx, "SELECT name, price, available_stock FROM products WHERE id = $1", it.ProductID).
		Scan(&issue.Name, &issue.CurrentPrice, &issue.AvailableStock)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		issue.Reason = model.ItemIssueNotFound
	case err != nil:
		return fmt.Errorf("read product for shortage: %w", err)
	case issue.AvailableStock <= 0:
		issue.Reason = model.ItemIssueSoldOut
	default:
		issue.Reason = model.ItemIssueInsufficientStock
	}
	return def.NewError(def.ErrItemsChanged, "商品庫存不足，請確認後重新送出").WithDetails(map[string]any{"items": []model.ItemIssue{issue}})
}

// GetByIdempotencyKey returns nil when the key has not been used.
func (r *DraftRepo) GetByIdempotencyKey(ctx context.Context, key string) (*model.CheckoutDraft, error) {
	d, err := scanDraft(r.pool.QueryRow(ctx, "SELECT "+draftColumns+" FROM checkout_drafts WHERE idempotency_key = $1", key))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return d, err
}

func (r *DraftRepo) Get(ctx context.Context, id string) (*model.CheckoutDraft, error) {
	d, err := scanDraft(r.pool.QueryRow(ctx, "SELECT "+draftColumns+" FROM checkout_drafts WHERE id = $1", id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return d, err
}

// Claim atomically moves a pending draft to processing. Returns nil if another worker already took it
// or it is no longer pending - this makes duplicated queue messages harmless.
func (r *DraftRepo) Claim(ctx context.Context, id string) (*model.CheckoutDraft, error) {
	d, err := scanDraft(r.pool.QueryRow(ctx, `
		UPDATE checkout_drafts
		   SET status = $2, attempts = attempts + 1, updated_at = now()
		 WHERE id = $1 AND status = $3
		RETURNING `+draftColumns, id, def.DraftStatusProcessing, def.DraftStatusPending))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return d, err
}

// ReleaseAndFail gives the held units back to available_stock and marks the draft failed, in one transaction.
// Idempotent: a second call finds no held reservation and changes nothing. Returns the released lines.
func (r *DraftRepo) ReleaseAndFail(ctx context.Context, id string, appErr *def.AppError) ([]StockLine, error) {
	details, err := json.Marshal(appErr.Details)
	if err != nil {
		return nil, fmt.Errorf("marshal error details: %w", err)
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	rows, err := tx.Query(ctx, `
		UPDATE inventory_reservations r SET status = $2, updated_at = now()
		  FROM products p
		 WHERE r.draft_id = $1 AND r.status = $3 AND p.id = r.product_id
		RETURNING r.product_id, r.quantity, p.flash_sale`, id, reservationReleased, reservationHeld)
	if err != nil {
		return nil, fmt.Errorf("release reservations: %w", err)
	}
	var lines []StockLine
	for rows.Next() {
		var l StockLine
		if err := rows.Scan(&l.ProductID, &l.Quantity, &l.FlashSale); err != nil {
			rows.Close()
			return nil, err
		}
		lines = append(lines, l)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	slices.SortFunc(lines, func(a, b StockLine) int { return cmp.Compare(a.ProductID, b.ProductID) })
	for _, l := range lines {
		if _, err := tx.Exec(ctx, `
			UPDATE products SET available_stock = available_stock + $2, updated_at = now() WHERE id = $1`,
			l.ProductID, l.Quantity); err != nil {
			return nil, fmt.Errorf("restore available stock: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, `
		UPDATE checkout_drafts
		   SET status = $2, error_code = $3, error_message = $4, error_details = $5, updated_at = now()
		 WHERE id = $1 AND status IN ($6, $7)`,
		id, def.DraftStatusFailed, string(appErr.Code), appErr.Message, details, def.DraftStatusPending, def.DraftStatusProcessing); err != nil {
		return nil, fmt.Errorf("mark draft failed: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit release: %w", err)
	}
	return lines, nil
}

// Release puts a draft back to pending so it can be retried after a transient error.
func (r *DraftRepo) Release(ctx context.Context, id string) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE checkout_drafts SET status = $2, updated_at = now()
		 WHERE id = $1 AND status = $3`, id, def.DraftStatusPending, def.DraftStatusProcessing)
	return err
}

// ReclaimStale recovers drafts whose queue message may have been lost (pending too long)
// or whose worker crashed (processing too long). Returned ids should be re-enqueued.
func (r *DraftRepo) ReclaimStale(ctx context.Context, pendingAfter, processingAfter time.Duration, limit int) ([]string, error) {
	_, err := r.pool.Exec(ctx, `
		UPDATE checkout_drafts SET status = $1, updated_at = now() - make_interval(secs => $3)
		 WHERE status = $2 AND updated_at < now() - make_interval(secs => $4)`,
		def.DraftStatusPending, def.DraftStatusProcessing, pendingAfter.Seconds(), processingAfter.Seconds())
	if err != nil {
		return nil, fmt.Errorf("reset stuck drafts: %w", err)
	}

	rows, err := r.pool.Query(ctx, `
		UPDATE checkout_drafts SET updated_at = now()
		 WHERE id IN (
			SELECT id FROM checkout_drafts
			 WHERE status = $1 AND updated_at < now() - make_interval(secs => $2)
			 ORDER BY updated_at LIMIT $3
			 FOR UPDATE SKIP LOCKED)
		RETURNING id`, def.DraftStatusPending, pendingAfter.Seconds(), limit)
	if err != nil {
		return nil, fmt.Errorf("reclaim pending drafts: %w", err)
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

func scanDraft(row pgx.Row) (*model.CheckoutDraft, error) {
	var d model.CheckoutDraft
	var payload, details []byte
	err := row.Scan(&d.ID, &d.IdempotencyKey, &payload, &d.Status, &d.Attempts, &d.ErrorCode,
		&d.ErrorMessage, &details, &d.OrderID, &d.CreatedAt, &d.UpdatedAt)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(payload, &d.Request); err != nil {
		return nil, fmt.Errorf("unmarshal draft payload: %w", err)
	}
	d.ErrorDetails = details
	return &d, nil
}
