package app

import (
	"context"
	"log/slog"

	"checkout/internal/app/adapter"
	"checkout/internal/domain/def"
	"checkout/internal/domain/model"
	"checkout/internal/infra/database/repo"
)

const expireBatchSize = 100

type OrderService struct {
	orders   *repo.OrderRepo
	payments *repo.PaymentRepo
	products *ProductService
}

func NewOrderService(orders *repo.OrderRepo, payments *repo.PaymentRepo, products *ProductService) *OrderService {
	return &OrderService{orders: orders, payments: payments, products: products}
}

// GetOrder only returns the caller's own orders; someone else's order looks exactly like a missing one (no IDOR).
func (s *OrderService) GetOrder(ctx context.Context, user model.User, id string) (adapter.OrderOutput, error) {
	o, err := s.orders.Get(ctx, id)
	if err != nil {
		return adapter.OrderOutput{}, err
	}
	if o == nil || o.CustomerID != user.ID {
		return adapter.OrderOutput{}, def.NewError(def.ErrOrderNotFound, "order not found")
	}
	out := adapter.OrderOutput{Order: *o}
	p, err := s.payments.LatestByOrder(ctx, id)
	if err != nil {
		return adapter.OrderOutput{}, err
	}
	if p != nil {
		out.LatestPayment = &adapter.PaymentView{Status: p.Status, FailureReason: p.FailureReason}
	}
	return out, nil
}

// ExpireOverdue cancels unpaid orders past their payment deadline and gives the stock back.
func (s *OrderService) ExpireOverdue(ctx context.Context) error {
	ids, restocked, err := s.orders.ExpireOverdue(ctx, expireBatchSize)
	if err != nil {
		return err
	}
	if len(ids) > 0 {
		slog.InfoContext(ctx, "expired unpaid orders", "order_ids", ids)
		s.products.ReturnLinesToGate(ctx, restocked)
	}
	return nil
}
