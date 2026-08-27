//go:build integration

package integration

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"time"

	"github.com/thaivanlam/mini-payment-gateway/internal/webhook"
)

// receivedWebhook is one callback as the merchant saw it.
type receivedWebhook struct {
	EventType       string
	DeliveryID      string
	Body            []byte
	SignatureHeader string
	SignatureValid  bool
	Payload         struct {
		ID   string `json:"id"`
		Type string `json:"type"`
		Data struct {
			ID             string `json:"id"`
			MerchantID     string `json:"merchant_id"`
			Status         string `json:"status"`
			Amount         int64  `json:"amount"`
			CapturedAmount int64  `json:"captured_amount"`
		} `json:"data"`
	}
}

// webhookSink plays the merchant's endpoint: it records what arrives and
// verifies the signature the way a merchant is supposed to.
type webhookSink struct {
	server *httptest.Server

	mu        sync.Mutex
	secret    string
	received  []receivedWebhook
	failNext  int  // return 500 for the next N calls
	rejectAll bool // return 500 for everything
}

func newWebhookSink() *webhookSink {
	s := &webhookSink{}
	s.server = httptest.NewServer(http.HandlerFunc(s.handle))
	return s
}

// URL is the address to configure as the merchant's webhook_url.
func (s *webhookSink) URL() string { return s.server.URL }

// Close shuts the sink down.
func (s *webhookSink) Close() { s.server.Close() }

// SetSecret tells the sink which secret to verify signatures against.
func (s *webhookSink) SetSecret(secret string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.secret = secret
}

// FailNext makes the next n deliveries fail, to exercise the retry path.
func (s *webhookSink) FailNext(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failNext = n
}

// RejectAll makes every delivery fail.
func (s *webhookSink) RejectAll(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rejectAll = v
}

// All returns everything received so far.
func (s *webhookSink) All() []receivedWebhook {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]receivedWebhook, len(s.received))
	copy(out, s.received)
	return out
}

// Count returns how many callbacks arrived.
func (s *webhookSink) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.received)
}

// WaitFor blocks until a callback of the given type arrives, or the deadline
// passes.
func (s *webhookSink) WaitFor(eventType string, timeout time.Duration) (receivedWebhook, bool) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, r := range s.All() {
			if r.Payload.Type == eventType {
				return r, true
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return receivedWebhook{}, false
}

func (s *webhookSink) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)

	rec := receivedWebhook{
		EventType:       r.Header.Get("X-Event-Type"),
		DeliveryID:      r.Header.Get("X-Webhook-Id"),
		Body:            body,
		SignatureHeader: r.Header.Get(webhook.SignatureHeader),
	}
	_ = json.Unmarshal(body, &rec.Payload)

	s.mu.Lock()
	if s.secret != "" {
		rec.SignatureValid = webhook.Verify(
			s.secret, rec.SignatureHeader, body, time.Now(), 5*time.Minute) == nil
	}
	s.received = append(s.received, rec)

	fail := s.rejectAll
	if s.failNext > 0 {
		s.failNext--
		fail = true
	}
	s.mu.Unlock()

	if fail {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}
