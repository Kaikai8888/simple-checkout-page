package job

import (
	"context"
	"log/slog"
	"time"

	"checkout/internal/app"
)

type Scheduler struct {
	checkout *app.CheckoutService
	orders   *app.OrderService
	products *app.ProductService
}

func NewScheduler(checkout *app.CheckoutService, orders *app.OrderService, products *app.ProductService) *Scheduler {
	return &Scheduler{checkout: checkout, orders: orders, products: products}
}

// Run blocks until ctx is cancelled.
// In a multi-instance deployment these jobs are still safe (FOR UPDATE SKIP LOCKED), but would usually run as a k8s CronJob.
func (s *Scheduler) Run(ctx context.Context) {
	reclaim := time.NewTicker(5 * time.Second)
	expire := time.NewTicker(15 * time.Second)
	gateSync := time.NewTicker(30 * time.Second)
	defer reclaim.Stop()
	defer expire.Stop()
	defer gateSync.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-reclaim.C:
			if err := s.checkout.ReclaimStale(ctx); err != nil {
				slog.Error("reclaim stale drafts failed", "err", err)
			}
		case <-expire.C:
			if err := s.orders.ExpireOverdue(ctx); err != nil {
				slog.Error("expire overdue orders failed", "err", err)
			}
		case <-gateSync.C:
			// realign Valkey flash-sale gates with available_stock (the source of truth)
			if err := s.products.SyncFlashGates(ctx); err != nil {
				slog.Error("sync flash sale gates failed", "err", err)
			}
		}
	}
}
