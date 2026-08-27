// Command migrate applies the embedded goose migrations.
//
//	migrate up | down | reset | status | version
package main

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/thaivanlam/mini-payment-gateway/internal/app"
	"github.com/thaivanlam/mini-payment-gateway/internal/config"
)

func main() {
	command := "up"
	if len(os.Args) > 1 {
		command = os.Args[1]
	}

	cfg, err := config.Load()
	if err != nil {
		slog.Error("load config", "error", err)
		os.Exit(1)
	}
	log := app.NewLogger(cfg.LogLevel, cfg.AppEnv)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	application, err := app.New(ctx, cfg, log)
	if err != nil {
		log.Error("connect", "error", err)
		os.Exit(1)
	}
	defer application.Close()

	if err := app.Migrate(ctx, application.DB, command); err != nil {
		log.Error("migrate", "command", command, "error", err)
		os.Exit(1)
	}
	log.Info("migrations done", "command", command)
}
