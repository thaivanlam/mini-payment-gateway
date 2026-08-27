// Command worker runs the webhook delivery loop, or a one-shot job.
//
//	worker                                   -- webhook dispatcher (long running)
//	worker -job=reconcile -date=2026-08-27   -- reconcile and settle one day
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/thaivanlam/mini-payment-gateway/internal/app"
	"github.com/thaivanlam/mini-payment-gateway/internal/config"
	"github.com/thaivanlam/mini-payment-gateway/internal/webhook"
)

func main() {
	job := flag.String("job", "webhook", "job to run: webhook | reconcile")
	date := flag.String("date", "", "date for the reconcile job (YYYY-MM-DD, default: today UTC)")
	flag.Parse()

	code, err := run(*job, *date)
	if err != nil {
		slog.Error("worker failed", "job", *job, "error", err)
		os.Exit(1)
	}
	os.Exit(code)
}

func run(job, date string) (int, error) {
	cfg, err := config.Load()
	if err != nil {
		return 1, err
	}
	log := app.NewLogger(cfg.LogLevel, cfg.AppEnv).With("process", "worker")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	application, err := app.New(ctx, cfg, log)
	if err != nil {
		return 1, err
	}
	defer application.Close()

	switch job {
	case "webhook", "":
		dispatcher := webhook.NewDispatcher(
			application.DB, application.WebhookRepo, application.MerchantRepo,
			application.Cipher, cfg.Webhook, log)
		log.Info("webhook dispatcher starting", "workers", cfg.Webhook.Workers)
		if err := dispatcher.Run(ctx); err != nil {
			return 1, err
		}
		return 0, nil

	case "reconcile":
		day := time.Now().UTC()
		if date != "" {
			day, err = time.Parse("2006-01-02", date)
			if err != nil {
				return 1, err
			}
		}
		report, err := application.Recon.Run(ctx, day)
		if err != nil {
			return 1, err
		}
		if !report.OK() {
			// A non-zero exit is what makes this usable from cron: the day did
			// not reconcile, and somebody has to look at it.
			log.Error("reconciliation found discrepancies",
				"date", report.Date,
				"discrepancies", len(report.Discrepancies),
				"unbalanced_groups", len(report.UnbalancedGroups))
			return 2, nil
		}
		log.Info("reconciliation clean",
			"date", report.Date,
			"transactions", report.TransactionCount,
			"settled", report.SettledCount)
		return 0, nil

	default:
		return 1, errUnknownJob(job)
	}
}

type errUnknownJob string

func (e errUnknownJob) Error() string { return "unknown job: " + string(e) }
