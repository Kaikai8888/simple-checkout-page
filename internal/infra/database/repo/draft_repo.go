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

const draftColumns = "id, idempotency_key, payload, status, attempts, COALESCE(error_code, ''), COALESCE(error_message, ''), error_details, COALESCE(order_id, ''), created_at, updated_at"

type DraftRepo struct {
	pool *pgxpool.Pool
}

func NewDraftRepo(pool *pgxpool.Pool) *DraftRepo {
	return &DraftRepo{pool: pool}
}

// Create inserts a pending draft. If the idempotency key was already used, the existing draft id is returned.
func (r *DraftRepo) Create(ctx context.Context, id, idempotencyKey string, req model.CheckoutRequest) (draftID string, created bool, err error) {
	payload, err := json.Marshal(req)
	if err != nil {
		return "", false, fmt.Errorf("marshal draft payload: %w", err)
	}
	err = r.pool.QueryRow(ctx, `
		INSERT INTO checkout_drafts (id, idempotency_key, payload, status)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (idempotency_key) DO NOTHING
		RETURNING id`, id, idempotencyKey, payload, def.DraftStatusPending).Scan(&draftID)
	if err == nil {
		return draftID, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", false, fmt.Errorf("insert draft: %w", err)
	}
	err = r.pool.QueryRow(ctx, "SELECT id FROM checkout_drafts WHERE idempotency_key = $1", idempotencyKey).Scan(&draftID)
	if err != nil {
		return "", false, fmt.Errorf("get draft by idempotency key: %w", err)
	}
	return draftID, false, nil
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

func (r *DraftRepo) MarkFailed(ctx context.Context, id string, appErr *def.AppError) error {
	details, err := json.Marshal(appErr.Details)
	if err != nil {
		return fmt.Errorf("marshal error details: %w", err)
	}
	_, err = r.pool.Exec(ctx, `
		UPDATE checkout_drafts
		   SET status = $2, error_code = $3, error_message = $4, error_details = $5, updated_at = now()
		 WHERE id = $1 AND status IN ($6, $7)`,
		id, def.DraftStatusFailed, string(appErr.Code), appErr.Message, details, def.DraftStatusPending, def.DraftStatusProcessing)
	return err
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

// OutstandingReserved sums quantities of drafts that still hold a Valkey stock reservation
// (pending / processing). Used to rebuild the Valkey stock counter: available = DB stock - outstanding.
// excludeDraftID skips a draft that has been inserted but has not reserved yet.
func (r *DraftRepo) OutstandingReserved(ctx context.Context, productIDs []int64, excludeDraftID string) (map[int64]int, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT (item->>'product_id')::bigint AS product_id, SUM((item->>'quantity')::int)::int
		  FROM checkout_drafts d, jsonb_array_elements(d.payload->'items') item
		 WHERE d.status IN ($1, $2) AND (item->>'product_id')::bigint = ANY($3) AND d.id <> $4
		 GROUP BY 1`, def.DraftStatusPending, def.DraftStatusProcessing, productIDs, excludeDraftID)
	if err != nil {
		return nil, fmt.Errorf("sum outstanding reservations: %w", err)
	}
	defer rows.Close()
	result := map[int64]int{}
	for rows.Next() {
		var id int64
		var qty int
		if err := rows.Scan(&id, &qty); err != nil {
			return nil, err
		}
		result[id] = qty
	}
	return result, rows.Err()
}
