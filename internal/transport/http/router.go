package http

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// Deps is everything the router needs, passed in by the entrypoint. There are
// no globals and no init(): every dependency arrives through this struct.
type Deps struct {
	Merchants  *MerchantHandler
	Payments   *PaymentHandler
	Ledger     *LedgerHandler
	System     *SystemHandler
	Auth       Authenticator
	Idem       IdempotencyStore
	Limiter    RateLimiter
	AdminToken string
	Log        *slog.Logger
}

// NewRouter wires the middleware chain and the routes.
//
// Middleware order is not cosmetic:
//
//	RequestID  -> every later log line and error body can name the request
//	Logger     -> one line per request, with the id already in context
//	Recoverer  -> catches panics from everything below it
//	Auth       -> buffers the body, verifies the signature, loads the merchant
//	RateLimit  -> needs the merchant identity, so it sits after Auth
//	Idempotency-> needs both the merchant and the buffered body
func NewRouter(d Deps) http.Handler {
	r := chi.NewRouter()

	r.Use(RequestID)
	r.Use(Logger(d.Log))
	r.Use(Recoverer)
	r.Use(middleware.RealIP)
	r.Use(middleware.Timeout(30 * time.Second))

	// Unauthenticated probes.
	r.Get("/healthz", d.System.Healthz)
	r.Get("/readyz", d.System.Readyz)

	// Demo receiver: plays the merchant's side of a webhook.
	r.Post("/internal/webhook-receiver", d.System.WebhookReceiver)

	r.Route("/api/v1", func(r chi.Router) {
		// Admin: provisioning is guarded by a static token, not by HMAC,
		// because a new merchant has no signing key yet.
		r.Group(func(r chi.Router) {
			r.Use(AdminAuth(d.AdminToken))
			r.Post("/merchants", d.Merchants.Create)
		})

		// Merchant-facing: everything below is HMAC-signed and rate limited.
		r.Group(func(r chi.Router) {
			r.Use(Auth(d.Auth))
			r.Use(RateLimit(d.Limiter))

			r.Get("/merchants/me", d.Merchants.Me)

			r.Get("/payments", d.Payments.List)
			r.Get("/payments/{id}", d.Payments.Get)
			r.Post("/payments/{id}/void", d.Payments.Void)

			r.Get("/ledger/entries", d.Ledger.Entries)
			r.Get("/ledger/balance", d.Ledger.Balance)
			r.Get("/reports/settlement", d.Ledger.Settlement)

			// Money-moving endpoints: Idempotency-Key required.
			r.Group(func(r chi.Router) {
				r.Use(Idempotency(d.Idem))

				r.Post("/payments", d.Payments.Create)
				r.Post("/payments/{id}/capture", d.Payments.Capture)
				r.Post("/payments/{id}/refund", d.Payments.Refund)
			})
		})
	})

	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, ErrorResponse{Error: ErrorBody{
			Code:      "resource_not_found",
			Message:   "No route matches this path.",
			Type:      TypeValidation,
			RequestID: RequestIDFrom(r.Context()),
		}})
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusMethodNotAllowed, ErrorResponse{Error: ErrorBody{
			Code:      "method_not_allowed",
			Message:   "This method is not allowed on this path.",
			Type:      TypeValidation,
			RequestID: RequestIDFrom(r.Context()),
		}})
	})

	return r
}
