package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"checkout/config"
	"checkout/internal/app"
	"checkout/internal/infra/cache"
	"checkout/internal/infra/database/repo"
	"checkout/internal/infra/paymentgateway"
	"checkout/internal/infra/producer"
	"checkout/internal/interfaces/consumer"
	"checkout/internal/interfaces/job"
	"checkout/internal/interfaces/web"
	"checkout/internal/mockgateway"
	staticfs "checkout/web"
)

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, nil)))
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg := config.Load()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// ---- infra ----
	pool, err := connectPostgres(ctx, cfg.DatabaseUrl)
	if err != nil {
		return err
	}
	defer pool.Close()

	valkey := redis.NewClient(&redis.Options{Addr: cfg.ValkeyAddr})
	defer valkey.Close()
	if err := retry(ctx, "valkey", func() error { return valkey.Ping(ctx).Err() }); err != nil {
		return err
	}

	if err := retry(ctx, "kafka", func() error {
		return producer.EnsureTopic(ctx, cfg.KafkaBrokers[0], cfg.KafkaTopic, cfg.KafkaPartitions)
	}); err != nil {
		return err
	}
	checkoutProducer := producer.NewCheckoutProducer(cfg.KafkaBrokers, cfg.KafkaTopic)
	defer checkoutProducer.Close()

	productRepo := repo.NewProductRepo(pool)
	draftRepo := repo.NewDraftRepo(pool)
	orderRepo := repo.NewOrderRepo(pool)
	couponRepo := repo.NewCouponRepo(pool)
	paymentRepo := repo.NewPaymentRepo(pool)
	customerRepo := repo.NewCustomerRepo(pool)
	gatewayClient := paymentgateway.NewClient(cfg.GatewayApiUrl, cfg.GatewayApiKey, cfg.GatewaySecret)

	// ---- app ----
	authSvc := app.NewAuthService(customerRepo, cache.NewSessionStore(valkey, cfg.SessionTTL),
		cache.NewLoginLimiter(valkey, 10, 15*time.Minute))
	productSvc := app.NewProductService(productRepo, cache.NewProductCache(valkey), cache.NewFlashSaleGate(valkey))
	checkoutSvc := app.NewCheckoutService(draftRepo, orderRepo, couponRepo, productSvc, checkoutProducer, cfg.PaymentTTL)
	orderSvc := app.NewOrderService(orderRepo, paymentRepo, productSvc)
	paymentSvc := app.NewPaymentService(orderRepo, paymentRepo, gatewayClient, cfg.AppPublicUrl, cfg.AppInternalUrl)

	if err := authSvc.EnsureAdmin(ctx, cfg.AdminEmail, cfg.AdminPassword); err != nil {
		return fmt.Errorf("ensure admin account: %w", err)
	}
	if err := productSvc.WarmUp(ctx); err != nil {
		return fmt.Errorf("warm up product cache: %w", err)
	}

	// ---- interfaces ----
	var wg sync.WaitGroup
	start := func(name string, fn func()) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			fn()
			slog.Info("stopped", "component", name)
		}()
	}

	apiServer := &http.Server{Addr: cfg.HTTPAddr, Handler: web.NewServer(authSvc, productSvc, checkoutSvc, orderSvc, paymentSvc, staticfs.Static(), cfg.CookieSecure).Handler(),
		ReadHeaderTimeout: 5 * time.Second}
	start("api", func() { serve(apiServer, "api") })

	var gatewayServer *http.Server
	if cfg.GatewayAddr != "" {
		gatewayServer = &http.Server{Addr: cfg.GatewayAddr, Handler: mockgateway.NewServer(cfg.GatewayPublicUrl, cfg.GatewayApiKey, cfg.GatewaySecret).Handler(),
			ReadHeaderTimeout: 5 * time.Second}
		start("mock-gateway", func() { serve(gatewayServer, "mock-gateway") })
	}

	start("consumer", func() {
		consumer.NewCheckoutConsumer(checkoutSvc, cfg.KafkaBrokers, cfg.KafkaTopic, cfg.KafkaGroupID, cfg.ConsumerCount).Run(ctx)
	})
	start("scheduler", func() { job.NewScheduler(checkoutSvc, orderSvc, productSvc).Run(ctx) })

	slog.Info("checkout system is up", "shop", cfg.AppPublicUrl, "gateway", cfg.GatewayPublicUrl)
	<-ctx.Done()
	slog.Info("shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = apiServer.Shutdown(shutdownCtx)
	if gatewayServer != nil {
		_ = gatewayServer.Shutdown(shutdownCtx)
	}
	wg.Wait()
	return nil
}

func serve(srv *http.Server, name string) {
	slog.Info("listening", "component", name, "addr", srv.Addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("server error", "component", name, "err", err)
		os.Exit(1)
	}
}

func connectPostgres(ctx context.Context, url string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("create pg pool: %w", err)
	}
	if err := retry(ctx, "postgres", func() error { return pool.Ping(ctx) }); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}

// retry waits for dependencies that start slower than the app (docker compose).
func retry(ctx context.Context, name string, fn func() error) error {
	var err error
	for i := 0; i < 30; i++ {
		if err = fn(); err == nil {
			return nil
		}
		slog.Info("waiting for dependency", "name", name, "err", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return fmt.Errorf("%s not ready: %w", name, err)
}
