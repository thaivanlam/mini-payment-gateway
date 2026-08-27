package app

import (
	"log/slog"
	"os"
	"strings"
)

// NewLogger builds the process logger.
//
// JSON in production so a log pipeline can index the fields; text locally so a
// human can read it. Either way the fields are structured -- there is no
// fmt.Println anywhere in this codebase, and no log line ever carries a card
// number, CVV, API secret or signature.
func NewLogger(level, env string) *slog.Logger {
	opts := &slog.HandlerOptions{Level: parseLevel(level)}

	var handler slog.Handler
	if env == "production" {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		handler = slog.NewTextHandler(os.Stdout, opts)
	}
	logger := slog.New(handler)
	slog.SetDefault(logger)
	return logger
}

func parseLevel(level string) slog.Level {
	switch strings.ToLower(level) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
