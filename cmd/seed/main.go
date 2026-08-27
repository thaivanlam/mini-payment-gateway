// Command seed creates a demo merchant and a batch of sample transactions, so
// `make up && make migrate && make seed` gives a system with data in it.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"time"

	"github.com/thaivanlam/mini-payment-gateway/internal/acquirer"
	"github.com/thaivanlam/mini-payment-gateway/internal/app"
	"github.com/thaivanlam/mini-payment-gateway/internal/config"
	"github.com/thaivanlam/mini-payment-gateway/internal/domain"
	"github.com/thaivanlam/mini-payment-gateway/internal/money"
	"github.com/thaivanlam/mini-payment-gateway/internal/service"
)

// demoCard is Luhn-valid and takes the normal path through the mock acquirer.
const demoCard = "4242424242424242"

func main() {
	if err := run(); err != nil {
		slog.Error("seed failed", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log := app.NewLogger(cfg.LogLevel, cfg.AppEnv).With("process", "seed")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	application, err := app.New(ctx, cfg, log)
	if err != nil {
		return err
	}
	defer application.Close()

	// The webhook URL is resolved by the *worker*, not by whoever runs the
	// seed. Under `make up` the worker is a container on the compose network,
	// where the API answers to "api" and localhost is the worker itself.
	// Running the worker on the host instead? Set SEED_WEBHOOK_URL.
	webhookURL := envOr("SEED_WEBHOOK_URL", "http://api:8080/internal/webhook-receiver")
	count, err := strconv.Atoi(envOr("SEED_TRANSACTIONS", "50"))
	if err != nil || count < 0 {
		return fmt.Errorf("SEED_TRANSACTIONS must be a non-negative integer")
	}

	created, err := application.Merchants.Create(ctx, service.CreateMerchantInput{
		Name:       "Demo Store",
		Email:      fmt.Sprintf("demo+%d@example.com", time.Now().Unix()),
		WebhookURL: webhookURL,
	})
	if err != nil {
		return fmt.Errorf("create demo merchant: %w", err)
	}
	merchant := created.Merchant

	// Seed data should be deterministic, so the sample payments use an
	// acquirer with no latency, no declines and no timeouts. The random one is
	// what the API itself uses.
	payments := service.NewPaymentService(
		application.DB,
		application.TransactionRepo,
		application.LedgerRepo,
		application.WebhookRepo,
		fastAcquirer(cfg, log),
		cfg.PlatformFeeBPS,
		log,
	)

	stats := struct{ authorized, captured, refunded, voided int }{}
	start := time.Now()

	for i := 1; i <= count; i++ {
		reference := fmt.Sprintf("ORDER-SEED-%05d", i)
		amount := money.Amount(50_000 + int64(i%20)*10_000)

		txn, err := payments.Authorize(ctx, service.AuthorizeInput{
			MerchantID: merchant.ID,
			Reference:  reference,
			Amount:     amount,
			Currency:   money.VND,
			Card:       acquirer.Card{Number: demoCard, ExpMonth: 12, ExpYear: 2030, CVV: "123"},
			Metadata:   map[string]string{"seed": "true", "batch": "demo"},
		})
		if err != nil {
			log.Warn("seed authorize failed", "reference", reference, "error", err)
			continue
		}
		stats.authorized++

		switch i % 10 {
		case 0:
			// One in ten is voided: an authorization the merchant abandoned.
			if _, err := payments.Void(ctx, merchant.ID, txn.ID); err != nil {
				log.Warn("seed void failed", "reference", reference, "error", err)
				continue
			}
			stats.voided++

		case 7:
			// One in ten is captured and then partially refunded.
			if _, err := payments.Capture(ctx, merchant.ID, txn.ID, 0); err != nil {
				log.Warn("seed capture failed", "reference", reference, "error", err)
				continue
			}
			stats.captured++
			if _, err := payments.Refund(ctx, merchant.ID, txn.ID, amount/2); err != nil {
				log.Warn("seed refund failed", "reference", reference, "error", err)
				continue
			}
			stats.refunded++

		default:
			if _, err := payments.Capture(ctx, merchant.ID, txn.ID, 0); err != nil {
				log.Warn("seed capture failed", "reference", reference, "error", err)
				continue
			}
			stats.captured++
		}
	}

	balance, err := application.Ledger.Balance(ctx, merchant.ID, money.VND)
	if err != nil {
		return err
	}
	unbalanced, err := application.Ledger.CheckInvariant(ctx)
	if err != nil {
		return err
	}
	if len(unbalanced) > 0 {
		return errors.New("seed produced unbalanced ledger entry groups")
	}

	printSummary(created, merchant, stats.authorized, stats.captured, stats.refunded, stats.voided, balance, time.Since(start))
	return nil
}

func fastAcquirer(cfg *config.Config, log *slog.Logger) acquirer.Acquirer {
	acqCfg := cfg.Acquirer
	acqCfg.DeclineRate = 0
	acqCfg.TimeoutRate = 0
	acqCfg.MinLatency = 0
	acqCfg.MaxLatency = 0
	return acquirer.NewGuarded(
		acquirer.NewMock(acqCfg, 1),
		acquirer.NewBreaker(cfg.Acquirer.BreakerThreshold, cfg.Acquirer.BreakerCooldown),
		cfg.Acquirer.Timeout,
		log,
	)
}

func printSummary(
	created *service.CreatedMerchant,
	merchant *domain.Merchant,
	authorized, captured, refunded, voided int,
	balance domain.Balance,
	elapsed time.Duration,
) {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	path := "/api/v1/merchants/me"
	signature := service.ComputeRequestSignature(created.APISecret, ts, "GET", path, nil)

	fmt.Printf(`
================= seed complete (%s) =================

  merchant_id     %s
  api_key         %s
  api_secret      %s
  webhook_secret  %s
  webhook_url     %s

  authorized %d   captured %d   refunded %d   voided %d
  merchant_payable balance: %s

  The secrets above are shown once. Export them:

    export API_KEY=%s
    export API_SECRET=%s

  Try a signed request (the signature below expires in 5 minutes):

    curl -s http://localhost:8080%s \
      -H "X-Api-Key: %s" \
      -H "X-Timestamp: %s" \
      -H "X-Signature: %s"

  Or use the helper, which signs for you:

    ./scripts/sign.sh GET %s

======================================================

`,
		elapsed.Round(time.Millisecond),
		merchant.ID, merchant.APIKey, created.APISecret, created.WebhookSecret, merchant.WebhookURL,
		authorized, captured, refunded, voided,
		money.Format(balance.Available(), balance.Currency),
		merchant.APIKey, created.APISecret,
		path, merchant.APIKey, ts, signature,
		path,
	)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
