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
	maxDraftAttempts     = 3
	stalePendingAfter    = 10 * time.Second
	staleProcessingAfter = 60 * time.Second
	reclaimBatchSize     = 100
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
func (s *CheckoutService) Submit(ctx context.Context, idempotencyKey string, in adapter.SubmitCheckoutInput) (adapter.SubmitCheckoutOutput, error) {
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if idempotencyKey == "" || len(idempotencyKey) > 128 {
		return adapter.SubmitCheckoutOutput{}, def.NewError(def.ErrInvalidRequest, "Idempotency-Key header is required")
	}
	req := in.ToModel()
	if err := req.Validate(); err != nil {
		return adapter.SubmitCheckoutOutput{}, err
	}

	// Replay of an already-submitted request (double click / network retry): don't reserve again.
	if existing, err := s.drafts.GetByIdempotencyKey(ctx, idempotencyKey); err != nil {
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

	draftID, created, err := s.drafts.CreateWithReservation(ctx, uuid.NewString(), idempotencyKey, req)
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
		// Not fatal: the draft is persisted, the reclaim job will publish it again.
		slog.ErrorContext(ctx, "publish checkout requested failed, will be reclaimed", "draft_id", draftID, "err", err)
	}
	return adapter.SubmitCheckoutOutput{DraftID: draftID, Status: def.DraftStatusPending}, nil
}

func (s *CheckoutService) GetStatus(ctx context.Context, draftID string) (adapter.DraftStatusOutput, error) {
	d, err := s.drafts.Get(ctx, draftID)
	if err != nil {
		return adapter.DraftStatusOutput{}, err
	}
	if d == nil {
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
func (s *CheckoutService) ProcessDraft(ctx context.Context, draftID string) error {
	draft, err := s.drafts.Claim(ctx, draftID)
	if err != nil {
		return fmt.Errorf("claim draft: %w", err)
	}
	if draft == nil {
		return nil // already handled by someone else (duplicated message)
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
		return s.fail(ctx, draft, appErr)
	}
	if draft.Attempts >= maxDraftAttempts {
		_ = s.fail(ctx, draft, def.NewError(def.ErrInternal, "訂單建立失敗，請稍後再試"))
		return err
	}
	// transient error: back to pending, the reclaim job will publish it again
	if relErr := s.drafts.Release(ctx, draft.ID); relErr != nil {
		slog.ErrorContext(ctx, "release draft failed", "draft_id", draft.ID, "err", relErr)
	}
	return err
}

// ReclaimStale re-publishes drafts whose message was lost or whose worker died.
func (s *CheckoutService) ReclaimStale(ctx context.Context) error {
	ids, err := s.drafts.ReclaimStale(ctx, stalePendingAfter, staleProcessingAfter, reclaimBatchSize)
	if err != nil || len(ids) == 0 {
		return err
	}
	slog.InfoContext(ctx, "re-publishing stale drafts", "count", len(ids))
	return s.producer.PublishCheckoutRequested(ctx, ids...)
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
