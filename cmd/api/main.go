// Command api serves the HTTP gateway.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/thaivanlam/mini-payment-gateway/internal/app"
	"github.com/thaivanlam/mini-payment-gateway/internal/config"
	transport "github.com/thaivanlam/mini-payment-gateway/internal/transport/http"
)

func main() {
	if err := run(); err != nil {
		slog.Error("api failed to start", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := cfg.RequireAdminToken(); err != nil {
		return err
	}
	log := app.NewLogger(cfg.LogLevel, cfg.AppEnv)

	// SIGINT/SIGTERM cancel this context, which unwinds everything below.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	application, err := app.New(ctx, cfg, log)
	if err != nil {
		return err
	}
	defer application.Close()

	if cfg.RunMigrations {
		if err := app.Migrate(ctx, application.DB, "up"); err != nil {
			return err
		}
		log.Info("migrations applied")
	}

	router := transport.NewRouter(transport.Deps{
		Merchants: transport.NewMerchantHandler(application.Merchants),
		Payments:  transport.NewPaymentHandler(application.Payments),
		Ledger:    transport.NewLedgerHandler(application.Ledger, application.Recon),
		System: transport.NewSystemHandler(
			application.DB, application.Redis, application.Merchants.WebhookSecret),
		Auth:       application.Merchants,
		Idem:       application.Idempotency,
		Limiter:    application.Limiter,
		AdminToken: cfg.AdminToken,
		Log:        log,
	})

	server := &http.Server{
		Addr:              cfg.Addr(),
		Handler:           router,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	serverErr := make(chan error, 1)
	go func() {
		log.Info("api listening",
			"addr", cfg.Addr(), "env", cfg.AppEnv, "fee_bps", cfg.PlatformFeeBPS)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	select {
	case err := <-serverErr:
		return err
	case <-ctx.Done():
	}

	// Graceful shutdown: stop accepting, let in-flight requests finish. A
	// payment cut off mid-flight is exactly the situation the whole
	// idempotency machinery exists to clean up after -- better not to create
	// it on every deploy.
	log.Info("api shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		return err
	}
	log.Info("api stopped")
	return nil
}
