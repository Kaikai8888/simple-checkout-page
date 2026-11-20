// Command gatesync realigns the Valkey flash-sale gates with PostgreSQL once and exits.
// Schedule it with a cron runner (k8s CronJob, or the gate-sync service in docker-compose).
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"checkout/config"
	"checkout/internal/app"
	"checkout/internal/infra/cache"
	"checkout/internal/infra/database/repo"
	"checkout/internal/interfaces/job"
)

const runTimeout = 20 * time.Second

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, nil)))
	if err := run(); err != nil {
		slog.Error("gate sync failed", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg := config.Load()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, runTimeout)
	defer cancel()

	pool, err := pgxpool.New(ctx, cfg.DatabaseUrl)
	if err != nil {
		return fmt.Errorf("create pg pool: %w", err)
	}
	defer pool.Close()

	valkey := redis.NewClient(&redis.Options{Addr: cfg.ValkeyAddr})
	defer valkey.Close()

	// Only the product repo and the gate are used; the product cache is wired because ProductService needs it.
	productSvc := app.NewProductService(repo.NewProductRepo(pool), cache.NewProductCache(valkey), cache.NewFlashSaleGate(valkey))
	return job.NewGateSyncJob(productSvc).Run(ctx)
}
