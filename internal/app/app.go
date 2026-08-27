// Package app wires the object graph once, so every entrypoint (api, worker,
// seed) builds the same components the same way.
package app

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/thaivanlam/mini-payment-gateway/internal/acquirer"
	"github.com/thaivanlam/mini-payment-gateway/internal/cache"
	"github.com/thaivanlam/mini-payment-gateway/internal/config"
	"github.com/thaivanlam/mini-payment-gateway/internal/idempotency"
	"github.com/thaivanlam/mini-payment-gateway/internal/ratelimit"
	"github.com/thaivanlam/mini-payment-gateway/internal/repository"
	"github.com/thaivanlam/mini-payment-gateway/internal/secrets"
	"github.com/thaivanlam/mini-payment-gateway/internal/service"
)

// App holds the fully constructed dependency graph.
type App struct {
	Cfg *config.Config
	Log *slog.Logger

	DB    *repository.DB
	Redis *redis.Client

	Cipher *secrets.Cipher

	MerchantRepo    *repository.MerchantRepo
	TransactionRepo *repository.TransactionRepo
	LedgerRepo      *repository.LedgerRepo
	WebhookRepo     *repository.WebhookRepo

	Acquirer *acquirer.Guarded

	Merchants *service.MerchantService
	Payments  *service.PaymentService
	Ledger    *service.LedgerService
	Recon     *service.ReconciliationService

	Idempotency *idempotency.Store
	Limiter     *ratelimit.Limiter
}

// New builds the graph. Every constructor takes its dependencies explicitly;
// nothing reaches for a package-level variable.
func New(ctx context.Context, cfg *config.Config, log *slog.Logger) (*App, error) {
	db, err := repository.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("connect postgres: %w", err)
	}

	rdb, err := cache.Connect(ctx, cfg.RedisURL)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("connect redis: %w", err)
	}

	cipher, err := secrets.NewCipher(cfg.SecretEncKey)
	if err != nil {
		db.Close()
		_ = rdb.Close()
		return nil, err
	}

	merchantRepo := repository.NewMerchantRepo()
	transactionRepo := repository.NewTransactionRepo()
	ledgerRepo := repository.NewLedgerRepo()
	webhookRepo := repository.NewWebhookRepo()

	mock := acquirer.NewMock(cfg.Acquirer, 0)
	breaker := acquirer.NewBreaker(cfg.Acquirer.BreakerThreshold, cfg.Acquirer.BreakerCooldown)
	guarded := acquirer.NewGuarded(mock, breaker, cfg.Acquirer.Timeout, log)

	app := &App{
		Cfg:             cfg,
		Log:             log,
		DB:              db,
		Redis:           rdb,
		Cipher:          cipher,
		MerchantRepo:    merchantRepo,
		TransactionRepo: transactionRepo,
		LedgerRepo:      ledgerRepo,
		WebhookRepo:     webhookRepo,
		Acquirer:        guarded,
		Merchants:       service.NewMerchantService(db, merchantRepo, cipher, cfg.Limits.AuthMaxClockSkew, log),
		Payments: service.NewPaymentService(
			db, transactionRepo, ledgerRepo, webhookRepo, guarded, cfg.PlatformFeeBPS, log),
		Ledger: service.NewLedgerService(db, ledgerRepo),
		Recon: service.NewReconciliationService(
			db, transactionRepo, ledgerRepo, webhookRepo, cfg.PlatformFeeBPS, cfg.ReportDir, log),
		Idempotency: idempotency.NewStore(rdb, cfg.Limits.IdempotencyTTL),
		Limiter:     ratelimit.New(rdb, cfg.Limits.RateLimitRPM, time.Minute),
	}
	return app, nil
}

// Close releases the datastore connections.
func (a *App) Close() {
	if a.DB != nil {
		a.DB.Close()
	}
	if a.Redis != nil {
		_ = a.Redis.Close()
	}
}
