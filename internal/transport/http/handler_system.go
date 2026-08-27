package http

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/thaivanlam/mini-payment-gateway/internal/repository"
	"github.com/thaivanlam/mini-payment-gateway/internal/webhook"
)

// WebhookSecretResolver returns the signing secret of a merchant. The demo
// receiver needs it for the same reason a real merchant does: to verify that a
// callback really came from this gateway.
type WebhookSecretResolver func(ctx context.Context, merchantID uuid.UUID) (string, error)

// SystemHandler serves health probes and the demo webhook receiver.
type SystemHandler struct {
	db            *repository.DB
	rdb           *redis.Client
	webhookSecret WebhookSecretResolver
}

// NewSystemHandler builds a SystemHandler.
func NewSystemHandler(db *repository.DB, rdb *redis.Client, webhookSecret WebhookSecretResolver) *SystemHandler {
	return &SystemHandler{db: db, rdb: rdb, webhookSecret: webhookSecret}
}

// Healthz is a liveness probe: it answers as long as the process is running.
// It deliberately does not touch the database -- a liveness probe that fails on
// a database blip gets the process killed instead of letting it recover.
func (h *SystemHandler) Healthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// Readyz is a readiness probe: it reports whether this instance can actually
// serve traffic, which means Postgres and Redis both answer.
func (h *SystemHandler) Readyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	checks := map[string]string{}
	ready := true

	if err := h.db.Ping(ctx); err != nil {
		checks["postgres"] = "error: " + err.Error()
		ready = false
	} else {
		checks["postgres"] = "ok"
	}

	if err := h.rdb.Ping(ctx).Err(); err != nil {
		checks["redis"] = "error: " + err.Error()
		ready = false
	} else {
		checks["redis"] = "ok"
	}

	status := http.StatusOK
	if !ready {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, map[string]any{
		"status": map[bool]string{true: "ready", false: "not_ready"}[ready],
		"checks": checks,
	})
}

// WebhookReceiver is a demo endpoint that plays the merchant's part: it
// verifies the signature exactly as a merchant should, and logs the event. The
// end-to-end test points a seeded merchant's webhook_url at it.
func (h *SystemHandler) WebhookReceiver(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	log := LoggerFrom(ctx)

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unreadable body"})
		return
	}

	// A merchant knows its own secret; this stand-in reads the merchant id out
	// of the payload and looks the secret up.
	var envelope struct {
		Data struct {
			MerchantID string `json:"merchant_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "payload is not JSON"})
		return
	}
	merchantID, err := uuid.Parse(envelope.Data.MerchantID)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "payload has no merchant_id"})
		return
	}

	secret, err := h.webhookSecret(ctx, merchantID)
	if err != nil {
		log.ErrorContext(ctx, "webhook receiver cannot resolve secret", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "no secret"})
		return
	}

	if err := webhook.Verify(secret, r.Header.Get(webhook.SignatureHeader), body, time.Now(), 5*time.Minute); err != nil {
		log.WarnContext(ctx, "webhook signature rejected", "error", err)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid signature"})
		return
	}

	log.InfoContext(ctx, "webhook received and verified",
		"event", r.Header.Get("X-Event-Type"),
		"delivery_id", r.Header.Get("X-Webhook-Id"),
		"bytes", len(body))

	writeJSON(w, http.StatusOK, map[string]string{"status": "received"})
}
