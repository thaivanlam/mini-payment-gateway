package app

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/thaivanlam/mini-payment-gateway/internal/repository"
	"github.com/thaivanlam/mini-payment-gateway/migrations"
)

// Migrate applies the embedded migrations.
//
// goose runs over database/sql, so the pgx pool is adapted rather than opening
// a second connection path with its own configuration.
func Migrate(ctx context.Context, db *repository.DB, command string) error {
	goose.SetBaseFS(migrations.FS)
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("set goose dialect: %w", err)
	}

	sqlDB := stdlib.OpenDBFromPool(db.Pool)
	defer func() { _ = sqlDB.Close() }()

	return runGoose(ctx, sqlDB, command)
}

func runGoose(ctx context.Context, sqlDB *sql.DB, command string) error {
	switch command {
	case "up", "":
		return goose.UpContext(ctx, sqlDB, ".")
	case "down":
		return goose.DownContext(ctx, sqlDB, ".")
	case "reset":
		return goose.ResetContext(ctx, sqlDB, ".")
	case "status":
		return goose.StatusContext(ctx, sqlDB, ".")
	case "version":
		return goose.VersionContext(ctx, sqlDB, ".")
	default:
		return fmt.Errorf("unknown migration command %q (use up, down, reset, status, version)", command)
	}
}
