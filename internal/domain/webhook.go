package domain

import (
	"math"
	"math/rand"
	"time"

	"github.com/google/uuid"
)

// WebhookEvent is the name merchants subscribe to.
type WebhookEvent string

const (
	EventPaymentAuthorized WebhookEvent = "payment.authorized"
	EventPaymentCaptured   WebhookEvent = "payment.captured"
	EventPaymentFailed     WebhookEvent = "payment.failed"
	EventPaymentRefunded   WebhookEvent = "payment.refunded"
	EventPaymentVoided     WebhookEvent = "payment.voided"
	EventPaymentSettled    WebhookEvent = "payment.settled"
)

// DeliveryStatus is the lifecycle of one webhook delivery attempt chain.
type DeliveryStatus string

const (
	DeliveryPending    DeliveryStatus = "pending"
	DeliveryDelivering DeliveryStatus = "delivering"
	DeliveryDelivered  DeliveryStatus = "delivered"
	DeliveryFailed     DeliveryStatus = "failed"
	DeliveryDead       DeliveryStatus = "dead"
)

// WebhookDelivery is an outbox row. It is written in the same database
// transaction as the money movement it describes, so a webhook can never
// announce something that was rolled back, and can never be lost because the
// process died before the HTTP call.
type WebhookDelivery struct {
	ID            uuid.UUID
	MerchantID    uuid.UUID
	TransactionID uuid.UUID
	EventType     WebhookEvent
	Payload       []byte
	Status        DeliveryStatus
	AttemptCount  int
	NextAttemptAt time.Time
	LastError     string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// NewWebhookDelivery creates a delivery due immediately.
func NewWebhookDelivery(merchantID, transactionID uuid.UUID, event WebhookEvent, payload []byte, now time.Time) *WebhookDelivery {
	return &WebhookDelivery{
		ID:            uuid.New(),
		MerchantID:    merchantID,
		TransactionID: transactionID,
		EventType:     event,
		Payload:       payload,
		Status:        DeliveryPending,
		NextAttemptAt: now,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
}

// BackoffDelay is the wait before attempt number n (1-based): 2^n seconds with
// +/-20% jitter, so a fleet of failed deliveries does not retry in lockstep.
func BackoffDelay(attempt int, rnd *rand.Rand) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 16 { // guard against overflow on absurd inputs
		attempt = 16
	}
	base := math.Pow(2, float64(attempt))
	jitter := 1.0
	if rnd != nil {
		jitter = 0.8 + rnd.Float64()*0.4
	}
	return time.Duration(base * jitter * float64(time.Second))
}

// RecordFailure advances the retry state after a failed attempt. Once
// maxAttempts is reached the delivery is parked as dead for a human to inspect.
func (d *WebhookDelivery) RecordFailure(errMsg string, maxAttempts int, now time.Time, rnd *rand.Rand) {
	d.AttemptCount++
	d.LastError = errMsg
	d.UpdatedAt = now
	if d.AttemptCount >= maxAttempts {
		d.Status = DeliveryDead
		return
	}
	d.Status = DeliveryFailed
	d.NextAttemptAt = now.Add(BackoffDelay(d.AttemptCount, rnd))
}

// RecordSuccess marks the delivery as done.
func (d *WebhookDelivery) RecordSuccess(now time.Time) {
	d.AttemptCount++
	d.Status = DeliveryDelivered
	d.LastError = ""
	d.UpdatedAt = now
}
