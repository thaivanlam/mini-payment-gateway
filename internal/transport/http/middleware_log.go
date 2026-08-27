package http

import (
	"context"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/google/uuid"
)

type ctxKey int

const (
	ctxKeyRequestID ctxKey = iota
	ctxKeyMerchant
	ctxKeyRawBody
)

// RequestIDHeader is echoed on every response.
const RequestIDHeader = "X-Request-Id"

// RequestID assigns an id to each request and puts it in the context, the
// response header, and every log line the request produces. Without it, a
// concurrent server's logs are unreadable: this is the thread that ties a
// merchant's support ticket to the lines that explain it.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(RequestIDHeader)
		if id == "" {
			id = "req_" + uuid.NewString()
		}
		ctx := context.WithValue(r.Context(), ctxKeyRequestID, id)
		w.Header().Set(RequestIDHeader, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// RequestIDFrom reads the request id out of a context.
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(ctxKeyRequestID).(string)
	return id
}

// responseRecorder captures the status code (and optionally the body) so the
// logger and the idempotency middleware can see what was sent.
type responseRecorder struct {
	http.ResponseWriter
	status  int
	written int64
	body    []byte
	capture bool
}

func (rec *responseRecorder) WriteHeader(status int) {
	if rec.status == 0 {
		rec.status = status
		rec.ResponseWriter.WriteHeader(status)
	}
}

func (rec *responseRecorder) Write(b []byte) (int, error) {
	if rec.status == 0 {
		rec.WriteHeader(http.StatusOK)
	}
	if rec.capture {
		rec.body = append(rec.body, b...)
	}
	n, err := rec.ResponseWriter.Write(b)
	rec.written += int64(n)
	return n, err
}

func (rec *responseRecorder) statusCode() int {
	if rec.status == 0 {
		return http.StatusOK
	}
	return rec.status
}

// Logger emits one structured line per request.
//
// slog with a request-scoped logger in the context: no fmt.Println anywhere,
// and nothing sensitive in the fields. Card numbers, CVVs, API secrets and
// signatures are never logged, here or anywhere else.
func Logger(base *slog.Logger) func(http.Handler) http.Handler {
	if base == nil {
		base = slog.Default()
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &responseRecorder{ResponseWriter: w}

			log := base.With(
				"request_id", RequestIDFrom(r.Context()),
				"method", r.Method,
				"path", r.URL.Path,
			)
			ctx := withLogger(r.Context(), log)

			next.ServeHTTP(rec, r.WithContext(ctx))

			log.InfoContext(ctx, "http request",
				"status", rec.statusCode(),
				"bytes", rec.written,
				"duration_ms", time.Since(start).Milliseconds(),
			)
		})
	}
}

type loggerKey struct{}

func withLogger(ctx context.Context, log *slog.Logger) context.Context {
	return context.WithValue(ctx, loggerKey{}, log)
}

// LoggerFrom returns the request-scoped logger, or the default one.
func LoggerFrom(ctx context.Context) *slog.Logger {
	if log, ok := ctx.Value(loggerKey{}).(*slog.Logger); ok {
		return log
	}
	return slog.Default()
}

// Recoverer turns a panic into a 500 instead of a dropped connection.
//
// This is the only recover in the codebase: business code returns errors. A
// panic reaching here is a bug, and it is logged with its stack as one.
func Recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				if rec == http.ErrAbortHandler {
					panic(rec) // the http package's own signal; do not swallow
				}
				LoggerFrom(r.Context()).ErrorContext(r.Context(), "panic recovered",
					"panic", rec, "stack", string(debug.Stack()))
				writeJSON(w, http.StatusInternalServerError, ErrorResponse{Error: ErrorBody{
					Code:      "internal_error",
					Message:   "An unexpected error occurred.",
					Type:      TypeAPI,
					RequestID: RequestIDFrom(r.Context()),
				}})
			}
		}()
		next.ServeHTTP(w, r)
	})
}
