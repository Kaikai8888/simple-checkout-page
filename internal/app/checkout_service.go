package app

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/big"
	"strings"
	"time"

	"github.com/google/uuid"

	"checkout/internal/app/adapter"
	"checkout/internal/domain/def"
	"checkout/internal/domain/model"
	"checkout/internal/infra/database/repo"
	"checkout/internal/infra/producer"
)

const (
	maxDraftAttempts = 3
	publishGrace     = 5 * time.Second  // Submit's own publish; after this the reclaim job publishes unpublished drafts
	retryBaseDelay   = 2 * time.Second  // backoff after a transient error: 2s, 4s, ...
	processTimeout   = 30 * time.Second // one ProcessDraft run
	cleanupTimeout   = 5 * time.Second  // fail / release after processTimeout
	processingLease  = 60 * time.Second // processing longer than this = worker died; must exceed processTimeout + cleanupTimeout
	reclaimBatchSize = 100
)

type CheckoutService struct {
	drafts     *repo.DraftRepo
	orders     *repo.OrderRepo
	coupons    *repo.CouponRepo
	products   *ProductService
	producer   *producer.CheckoutProducer
	paymentTTL time.Duration
	now        func() time.Time
}

func NewCheckoutService(drafts *repo.DraftRepo, orders *repo.OrderRepo, coupons *repo.CouponRepo,
	products *ProductService, p *producer.CheckoutProducer, paymentTTL time.Duration) *CheckoutService {
	return &CheckoutService{drafts: drafts, orders: orders, coupons: coupons, products: products,
		producer: p, paymentTTL: paymentTTL, now: time.Now}
}

// Submit validates the format, reserves stock and persists a draft, then publishes to Kafka.
// Order creation itself happens asynchronously in ProcessDraft.
//
// Stock reservation is hybrid:
//  1. flash-sale items first pass the Valkey gate (sheds excess traffic before it reaches the hot DB row)
//  2. every item is reserved from products.available_stock in the same DB transaction that inserts the draft
func (s *CheckoutService) Submit(ctx context.Context, user model.User, idempotencyKey string, in adapter.SubmitCheckoutInput) (adapter.SubmitCheckoutOutput, error) {
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if idempotencyKey == "" || len(idempotencyKey) > 128 {
		return adapter.SubmitCheckoutOutput{}, def.NewError(def.ErrInvalidRequest, "Idempotency-Key header is required")
	}
	req := in.ToModel()
	// The buyer is the logged-in account, never whatever email the client put in the payload.
	req.Customer = model.CustomerInfo{Name: user.Name, Email: user.Email, Phone: req.Shipping.RecipientPhone}
	if err := req.Validate(); err != nil {
		return adapter.SubmitCheckoutOutput{}, err
	}

	// Replay of an already-submitted request (double click / network retry): don't reserve again.
	if existing, err := s.drafts.GetByIdempotencyKey(ctx, user.ID, idempotencyKey); err != nil {
		return adapter.SubmitCheckoutOutput{}, err
	} else if existing != nil {
		return adapter.SubmitCheckoutOutput{DraftID: existing.ID, Status: existing.Status}, nil
	}

	products, err := s.products.GetProducts(ctx, req.ProductIDs())
	if err != nil {
		return adapter.SubmitCheckoutOutput{}, err
	}
	taken, err := s.products.PassFlashGate(ctx, req.Items, products)
	if err != nil {
		return adapter.SubmitCheckoutOutput{}, err // sold out at the gate: the DB was never touched
	}

	draftID, created, err := s.drafts.CreateWithReservation(ctx, uuid.NewString(), user.ID, idempotencyKey, req, publishGrace)
	if err != nil || !created {
		s.products.ReturnToGate(ctx, taken) // DB said no (or duplicate request): give the gate units back
	}
	if err != nil {
		return adapter.SubmitCheckoutOutput{}, err
	}
	if !created {
		return adapter.SubmitCheckoutOutput{DraftID: draftID, Status: def.DraftStatusPending}, nil
	}

	if err := s.producer.PublishCheckoutRequested(ctx, draftID); err != nil {
		// Not fatal: the draft is persisted and unpublished, the reclaim job publishes it after publishGrace.
		slog.ErrorContext(ctx, "publish checkout requested failed, will be reclaimed", "draft_id", draftID, "err", err)
	} else if err := s.drafts.MarkPublished(ctx, draftID); err != nil {
		// Worst case the reclaim job publishes a duplicate, which Claim ignores.
		slog.WarnContext(ctx, "mark draft published failed", "draft_id", draftID, "err", err)
	}
	return adapter.SubmitCheckoutOutput{DraftID: draftID, Status: def.DraftStatusPending}, nil
}

// GetStatus only returns the caller's own drafts; someone else's draft looks exactly like a missing one.
func (s *CheckoutService) GetStatus(ctx context.Context, user model.User, draftID string) (adapter.DraftStatusOutput, error) {
	d, err := s.drafts.Get(ctx, draftID)
	if err != nil {
		return adapter.DraftStatusOutput{}, err
	}
	if d == nil || d.CustomerID != user.ID {
		return adapter.DraftStatusOutput{}, def.NewError(def.ErrDraftNotFound, "checkout not found")
	}
	out := adapter.DraftStatusOutput{DraftID: d.ID, Status: d.Status, OrderID: d.OrderID}
	if d.Status == def.DraftStatusFailed {
		out.Error = &def.AppError{Code: def.ErrorCode(d.ErrorCode), Message: d.ErrorMessage}
		if len(d.ErrorDetails) > 0 {
			var details any
			if json.Unmarshal(d.ErrorDetails, &details) == nil {
				out.Error.Details = details
			}
		}
	}
	return out, nil
}

// ProcessDraft is called by the Kafka consumer. Safe to call multiple times for the same draft.
//
// A run is capped by processTimeout so a slow worker never outlives processingLease; otherwise the reclaim job
// would hand the same draft to a second worker while the first one is still working on it.
func (s *CheckoutService) ProcessDraft(parent context.Context, draftID string) error {
	ctx, cancel := context.WithTimeout(parent, processTimeout)
	defer cancel()
	// fail / release must still run when the processing context has timed out.
	cleanupCtx, cancelCleanup := context.WithTimeout(context.WithoutCancel(parent), processTimeout+cleanupTimeout)
	defer cancelCleanup()

	draft, err := s.drafts.Claim(ctx, draftID)
	if err != nil {
		return fmt.Errorf("claim draft: %w", err)
	}
	if draft == nil {
		return nil // already handled by someone else (duplicated message)
	}
	// Claim counts attempts, so this catches drafts that keep killing the worker (processing lease expired again and again).
	if draft.Attempts > maxDraftAttempts {
		return s.fail(cleanupCtx, draft, def.NewError(def.ErrInternal, "訂單建立失敗，請稍後再試"))
	}

	now := s.now()
	order, err := s.orders.PlaceOrder(ctx, repo.PlaceOrderParams{
		OrderID:   uuid.NewString(),
		OrderNo:   newOrderNo(now),
		Draft:     draft,
		ExpiresAt: now.Add(s.paymentTTL),
		Quote: func(products map[int64]model.Product, coupon *model.Coupon) (model.PriceBreakdown, error) {
			b, appErr := model.Quote(draft.Request, products, coupon, now)
			if appErr != nil {
				return b, appErr
			}
			return b, nil
		},
	})
	if err == nil {
		slog.InfoContext(ctx, "order created", "draft_id", draft.ID, "order_id", order.ID, "total", order.Total)
		return nil
	}

	if appErr, ok := def.AsAppError(err); ok {
		return s.fail(cleanupCtx, draft, appErr)
	}
	if draft.Attempts >= maxDraftAttempts {
		_ = s.fail(cleanupCtx, draft, def.NewError(def.ErrInternal, "訂單建立失敗，請稍後再試"))
		return err
	}
	// Transient error: back to pending with a backoff; the reclaim job publishes a new message when it is due.
	// If Release itself fails the draft stays processing and is recovered after processingLease.
	if relErr := s.drafts.Release(cleanupCtx, draft.ID, retryDelay(draft.Attempts)); relErr != nil {
		slog.ErrorContext(ctx, "release draft failed", "draft_id", draft.ID, "err", relErr)
	}
	return err
}

// retryDelay is the backoff before the next attempt: retryBaseDelay * 2^(attempts-1).
func retryDelay(attempts int) time.Duration {
	return retryBaseDelay << max(attempts-1, 0)
}

// ReclaimStale publishes drafts that have no message in Kafka: Submit's publish failed, a retry's backoff is
// over, or their worker died. Drafts that are published and only waiting in the queue are never re-published.
func (s *CheckoutService) ReclaimStale(ctx context.Context) error {
	n, err := s.drafts.ReclaimStale(ctx, processingLease, reclaimBatchSize, func(ids []string) error {
		if err := s.producer.PublishCheckoutRequested(ctx, ids...); err != nil {
			return fmt.Errorf("publish drafts: %w", err)
		}
		return nil
	})
	if n > 0 {
		slog.InfoContext(ctx, "published unpublished drafts", "count", n)
	}
	return err
}

func (s *CheckoutService) Options() adapter.CheckoutOptionsOutput {
	return adapter.CheckoutOptionsOutput{
		ShippingRegions:   model.ShippingRegions,
		PaymentMethods:    model.PaymentMethods,
		PaymentTTLSeconds: int(s.paymentTTL.Seconds()),
	}
}

// PreviewCoupon lets the checkout page display the discount. The real check happens again at order time.
func (s *CheckoutService) PreviewCoupon(ctx context.Context, in adapter.CouponPreviewInput) (adapter.CouponPreviewOutput, error) {
	code := strings.TrimSpace(strings.ToUpper(in.Code))
	c, err := s.coupons.Get(ctx, code)
	if err != nil {
		return adapter.CouponPreviewOutput{}, err
	}
	if c == nil {
		return adapter.CouponPreviewOutput{}, def.NewError(def.ErrCouponInvalid, "折扣碼不存在")
	}
	discount, appErr := c.Discount(in.Subtotal, s.now())
	if appErr != nil {
		return adapter.CouponPreviewOutput{}, appErr
	}
	return adapter.CouponPreviewOutput{Code: c.Code, Description: c.Description, Discount: discount}, nil
}

func (s *CheckoutService) fail(ctx context.Context, draft *model.CheckoutDraft, appErr *def.AppError) error {
	slog.InfoContext(ctx, "checkout rejected", "draft_id", draft.ID, "code", appErr.Code)
	// Releasing the reservation and failing the draft happen in one DB transaction.
	lines, err := s.drafts.ReleaseAndFail(ctx, draft.ID, appErr)
	if err != nil {
		return fmt.Errorf("release and fail draft: %w", err)
	}
	s.products.ReturnLinesToGate(ctx, lines)
	return nil
}

func newOrderNo(now time.Time) string {
	n, _ := rand.Int(rand.Reader, big.NewInt(1_000_000))
	return fmt.Sprintf("CO%s%06d", now.Format("20060102"), n.Int64())
}
