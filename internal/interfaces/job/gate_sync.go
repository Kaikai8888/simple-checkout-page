package job

import (
	"context"
	"log/slog"
	"time"

	"checkout/internal/app"
)

// GateSyncJob realigns every Valkey flash-sale gate with products.available_stock (the source of truth).
// It runs once per invocation, so it can be scheduled by any cron runner (k8s CronJob, the gate-sync
// compose service, ...) and the API process never waits on it at startup.
type GateSyncJob struct {
	products *app.ProductService
}

func NewGateSyncJob(products *app.ProductService) *GateSyncJob {
	return &GateSyncJob{products: products}
}

func (j *GateSyncJob) Run(ctx context.Context) error {
	start := time.Now()
	if err := j.products.SyncFlashGates(ctx); err != nil {
		return err
	}
	slog.InfoContext(ctx, "flash sale gates synced", "took", time.Since(start))
	return nil
}
