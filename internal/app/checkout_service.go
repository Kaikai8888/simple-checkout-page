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

// Submit validates the format, persists a draft, reserves stock in Valkey and publishes to Kafka.
// Order creation itself happens asynchronously in ProcessDraft.
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

	draftID, created, err := s.drafts.Create(ctx, uuid.NewString(), idempotencyKey, req)
	if err != nil {
		return adapter.SubmitCheckoutOutput{}, err
	}
	if !created {
		return adapter.SubmitCheckoutOutput{DraftID: draftID, Status: def.DraftStatusPending}, nil
	}

	// Draft first, reserve second: if we crash in between, the draft is still processed and the DB
	// conditional update protects stock (we may under-reserve, but never oversell or leak units).
	if err := s.products.ReserveStock(ctx, draftID, req.Items); err != nil {
		appErr, ok := def.AsAppError(err)
		if !ok {
			appErr = def.NewError(def.ErrInternal, "庫存預留失敗，請稍後再試")
		}
		if markErr := s.drafts.MarkFailed(ctx, draftID, appErr); markErr != nil {
			slog.ErrorContext(ctx, "mark draft failed", "draft_id", draftID, "err", markErr)
		}
		return adapter.SubmitCheckoutOutput{}, appErr
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
		s.products.CommitStock(ctx, draft.ID, draft.Request.ProductIDs())
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
	// Release first so that once the shopper sees "failed", the units are already available again.
	// If MarkFailed then fails, the draft is reclaimed and re-processed; the DB guard still prevents overselling.
	s.products.ReleaseStock(ctx, draft.ID)
	if err := s.drafts.MarkFailed(ctx, draft.ID, appErr); err != nil {
		return fmt.Errorf("mark draft failed: %w", err)
	}
	return nil
}

func newOrderNo(now time.Time) string {
	n, _ := rand.Int(rand.Reader, big.NewInt(1_000_000))
	return fmt.Sprintf("CO%s%06d", now.Format("20060102"), n.Int64())
}
