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
}

func NewScheduler(checkout *app.CheckoutService, orders *app.OrderService) *Scheduler {
	return &Scheduler{checkout: checkout, orders: orders}
}

// Run blocks until ctx is cancelled.
// In a multi-instance deployment these jobs are still safe (FOR UPDATE SKIP LOCKED), but would usually run as a k8s CronJob.
func (s *Scheduler) Run(ctx context.Context) {
	reclaim := time.NewTicker(5 * time.Second)
	expire := time.NewTicker(15 * time.Second)
	defer reclaim.Stop()
	defer expire.Stop()
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
		}
	}
}
