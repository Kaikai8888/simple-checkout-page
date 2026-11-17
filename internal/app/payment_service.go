package app

import (
	"context"
	"log/slog"
	"net/url"
	"time"

	"github.com/google/uuid"

	"checkout/internal/app/adapter"
	"checkout/internal/domain/def"
	"checkout/internal/domain/model"
	"checkout/internal/infra/database/repo"
	"checkout/internal/infra/paymentgateway"
)

type PaymentService struct {
	orders         *repo.OrderRepo
	payments       *repo.PaymentRepo
	gateway        *paymentgateway.Client
	appPublicUrl   string
	appInternalUrl string
	now            func() time.Time
}

func NewPaymentService(orders *repo.OrderRepo, payments *repo.PaymentRepo, gateway *paymentgateway.Client,
	appPublicUrl, appInternalUrl string) *PaymentService {
	return &PaymentService{orders: orders, payments: payments, gateway: gateway,
		appPublicUrl: appPublicUrl, appInternalUrl: appInternalUrl, now: time.Now}
}

// StartPayment creates (or reuses) a payment session at the gateway and returns the URL to redirect the shopper to.
func (s *PaymentService) StartPayment(ctx context.Context, orderID string) (adapter.StartPaymentOutput, error) {
	order, err := s.orders.Get(ctx, orderID)
	if err != nil {
		return adapter.StartPaymentOutput{}, err
	}
	if order == nil {
		return adapter.StartPaymentOutput{}, def.NewError(def.ErrOrderNotFound, "order not found")
	}
	if order.Status != def.OrderStatusPendingPayment || s.now().After(order.ExpiresAt) {
		return adapter.StartPaymentOutput{}, def.NewError(def.ErrOrderNotPayable, "訂單目前無法付款").
			WithDetails(map[string]string{"status": order.Status})
	}

	latest, err := s.payments.LatestByOrder(ctx, orderID)
	if err != nil {
		return adapter.StartPaymentOutput{}, err
	}
	if latest != nil && latest.Status == def.PaymentStatusPending && latest.PaymentUrl != "" {
		return adapter.StartPaymentOutput{PaymentUrl: latest.PaymentUrl}, nil
	}

	payment := model.Payment{ID: uuid.NewString(), OrderID: order.ID, Method: order.PaymentMethod,
		Amount: order.Total, Status: def.PaymentStatusPending}
	if err := s.payments.Create(ctx, payment); err != nil {
		return adapter.StartPaymentOutput{}, err
	}

	session, err := s.gateway.CreatePayment(ctx, paymentgateway.CreatePaymentRequest{
		MerchantTradeNo: payment.ID,
		Amount:          payment.Amount,
		Method:          payment.Method,
		Description:     "訂單 " + order.OrderNo,
		ReturnUrl:       s.appPublicUrl + "/result.html?order_id=" + url.QueryEscape(order.ID),
		NotifyUrl:       s.appInternalUrl + "/api/payments/callback",
	})
	if err != nil {
		slog.ErrorContext(ctx, "create gateway payment failed", "order_id", order.ID, "err", err)
		_ = s.payments.MarkFailed(ctx, payment.ID, "gateway unavailable")
		return adapter.StartPaymentOutput{}, def.NewError(def.ErrPaymentGateway, "金流服務暫時無法使用，請稍後再試")
	}
	if err := s.payments.SetSession(ctx, payment.ID, session.SessionID, session.PaymentUrl); err != nil {
		return adapter.StartPaymentOutput{}, err
	}
	return adapter.StartPaymentOutput{PaymentUrl: session.PaymentUrl}, nil
}

// HandleCallback processes the gateway's server-to-server notification. Idempotent.
func (s *PaymentService) HandleCallback(ctx context.Context, body []byte, sig string) error {
	n, ok := s.gateway.ParseNotification(body, sig)
	if !ok {
		return def.NewError(def.ErrInvalidSignature, "invalid signature")
	}
	orderStatus, err := s.payments.ApplyResult(ctx, repo.PaymentResult{
		PaymentID:     n.MerchantTradeNo,
		Succeeded:     n.Status == paymentgateway.NotificationSuccess,
		ProviderTxnID: n.TransactionID,
		Amount:        n.Amount,
		FailureReason: n.Message,
	})
	if err != nil {
		return err
	}
	slog.InfoContext(ctx, "payment callback applied", "payment_id", n.MerchantTradeNo, "gateway_status", n.Status, "order_status", orderStatus)
	return nil
}
