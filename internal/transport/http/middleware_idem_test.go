package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thaivanlam/mini-payment-gateway/internal/domain"
	"github.com/thaivanlam/mini-payment-gateway/internal/idempotency"
)

// memStore is an in-memory stand-in for the Redis store, implementing the same
// semantics: SET NX to claim, fingerprint comparison, replay, release.
type memStore struct {
	mu      sync.Mutex
	records map[string]*idempotency.Record
}

func newMemStore() *memStore {
	return &memStore{records: map[string]*idempotency.Record{}}
}

func (s *memStore) Begin(_ context.Context, key, fingerprint string) (*idempotency.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	existing, ok := s.records[key]
	if !ok {
		s.records[key] = &idempotency.Record{
			State:       idempotency.StateInProgress,
			Fingerprint: fingerprint,
		}
		return nil, nil
	}
	if existing.Fingerprint != fingerprint {
		return nil, idempotency.ErrKeyReuse
	}
	if existing.State == idempotency.StateInProgress {
		return nil, idempotency.ErrInFlight
	}
	return existing, nil
}

func (s *memStore) Complete(_ context.Context, key, fingerprint string, status int, contentType string, body []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records[key] = &idempotency.Record{
		State:       idempotency.StateCompleted,
		Fingerprint: fingerprint,
		StatusCode:  status,
		ContentType: contentType,
		Body:        json.RawMessage(body),
	}
	return nil
}

func (s *memStore) Release(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.records, key)
	return nil
}

func (s *memStore) has(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.records[key]
	return ok
}

// withMerchant injects an authenticated merchant, standing in for Auth.
func withMerchant(merchantID uuid.UUID, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), ctxKeyMerchant, &domain.Merchant{
			ID:     merchantID,
			Status: domain.MerchantActive,
		})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

type countingHandler struct {
	mu     sync.Mutex
	calls  int
	status int
	body   string
	panic  bool
}

func (h *countingHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	h.calls++
	h.mu.Unlock()

	if h.panic {
		panic("boom")
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(h.status)
	_, _ = w.Write([]byte(h.body))
}

func (h *countingHandler) callCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls
}

func newIdemServer(store IdempotencyStore, handler http.Handler) (http.Handler, uuid.UUID) {
	merchantID := uuid.New()
	return RequestID(withMerchant(merchantID, Idempotency(store)(handler))), merchantID
}

func postPayment(t *testing.T, h http.Handler, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/payments", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set(IdempotencyKeyHeader, key)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// Case 0: the header is mandatory.
func TestIdempotencyRequiresTheHeader(t *testing.T) {
	handler := &countingHandler{status: http.StatusCreated, body: `{"id":"txn_1"}`}
	server, _ := newIdemServer(newMemStore(), handler)

	rec := postPayment(t, server, "", `{"amount":1000}`)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, 0, handler.callCount(), "the handler must not run")

	var body ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "invalid_request", body.Error.Code)
	assert.Equal(t, IdempotencyKeyHeader, body.Error.Field)
}

// Case 1: a retry with the same key and the same body replays the stored
// response and does not run the handler again.
func TestIdempotencyReplaysStoredResponse(t *testing.T) {
	handler := &countingHandler{status: http.StatusCreated, body: `{"id":"txn_1","status":"authorized"}`}
	server, _ := newIdemServer(newMemStore(), handler)
	const key = "key-1"
	const body = `{"amount":150000,"currency":"VND"}`

	first := postPayment(t, server, key, body)
	require.Equal(t, http.StatusCreated, first.Code)
	assert.Empty(t, first.Header().Get(ReplayHeader))

	second := postPayment(t, server, key, body)

	assert.Equal(t, http.StatusCreated, second.Code, "the replay carries the original status")
	assert.JSONEq(t, first.Body.String(), second.Body.String())
	assert.Equal(t, "true", second.Header().Get(ReplayHeader))
	assert.Equal(t, 1, handler.callCount(), "the handler runs exactly once")
}

// The replay must survive cosmetic differences in the body: reordered keys and
// added whitespace are the same request.
func TestIdempotencyReplayIgnoresBodyFormatting(t *testing.T) {
	handler := &countingHandler{status: http.StatusCreated, body: `{"id":"txn_1"}`}
	server, _ := newIdemServer(newMemStore(), handler)
	const key = "key-1"

	require.Equal(t, http.StatusCreated, postPayment(t, server, key, `{"amount":150000,"currency":"VND"}`).Code)
	second := postPayment(t, server, key, "{\n  \"currency\": \"VND\",\n  \"amount\": 150000\n}")

	assert.Equal(t, http.StatusCreated, second.Code)
	assert.Equal(t, "true", second.Header().Get(ReplayHeader))
	assert.Equal(t, 1, handler.callCount())
}

// Case 2: the same key with a genuinely different body is a client bug.
func TestIdempotencyRejectsKeyReuse(t *testing.T) {
	handler := &countingHandler{status: http.StatusCreated, body: `{"id":"txn_1"}`}
	server, _ := newIdemServer(newMemStore(), handler)
	const key = "key-1"

	require.Equal(t, http.StatusCreated, postPayment(t, server, key, `{"amount":150000}`).Code)

	second := postPayment(t, server, key, `{"amount":999999}`)

	assert.Equal(t, http.StatusUnprocessableEntity, second.Code)
	assert.Equal(t, 1, handler.callCount(), "the second body must never reach the handler")

	var body ErrorResponse
	require.NoError(t, json.Unmarshal(second.Body.Bytes(), &body))
	assert.Equal(t, "idempotency_key_reuse", body.Error.Code)
	assert.Equal(t, TypeIdempotency, body.Error.Type)
}

// Case 3: a request that arrives while the first is still running.
func TestIdempotencyRejectsInFlightRequest(t *testing.T) {
	store := newMemStore()
	handler := &countingHandler{status: http.StatusCreated, body: `{"id":"txn_1"}`}
	server, merchantID := newIdemServer(store, handler)
	const key = "key-1"

	// Simulate the first request having claimed the key and not finished.
	_, err := store.Begin(context.Background(), idempotency.Key(merchantID.String(), key),
		idempotency.Fingerprint([]byte(`{"amount":150000}`)))
	require.NoError(t, err)

	rec := postPayment(t, server, key, `{"amount":150000}`)

	assert.Equal(t, http.StatusConflict, rec.Code)
	assert.Equal(t, 0, handler.callCount())

	var body ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "request_in_progress", body.Error.Code)
}

// Case 4: a server error releases the key, so the client can retry and get a
// real answer instead of being stuck with a cached 500 for 24 hours.
func TestIdempotencyReleasesKeyOnServerError(t *testing.T) {
	store := newMemStore()
	handler := &countingHandler{status: http.StatusInternalServerError, body: `{"error":"boom"}`}
	server, merchantID := newIdemServer(store, handler)
	const key = "key-1"

	first := postPayment(t, server, key, `{"amount":150000}`)
	require.Equal(t, http.StatusInternalServerError, first.Code)
	assert.False(t, store.has(idempotency.Key(merchantID.String(), key)), "the key must be released")

	// The retry runs the handler again, and now it succeeds.
	handler.status = http.StatusCreated
	handler.body = `{"id":"txn_1"}`
	second := postPayment(t, server, key, `{"amount":150000}`)

	assert.Equal(t, http.StatusCreated, second.Code)
	assert.Equal(t, 2, handler.callCount())
}

// A panic must also release the key on its way out to the recoverer.
func TestIdempotencyReleasesKeyOnPanic(t *testing.T) {
	store := newMemStore()
	handler := &countingHandler{panic: true}
	merchantID := uuid.New()
	server := RequestID(Recoverer(withMerchant(merchantID, Idempotency(store)(handler))))

	rec := postPayment(t, server, "key-1", `{"amount":150000}`)

	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.False(t, store.has(idempotency.Key(merchantID.String(), "key-1")))
}

// A 4xx is a definite answer about the request and stays cached: retrying it
// verbatim must keep returning the same refusal.
func TestIdempotencyCachesClientErrors(t *testing.T) {
	handler := &countingHandler{status: http.StatusPaymentRequired, body: `{"error":{"code":"do_not_honor"}}`}
	server, _ := newIdemServer(newMemStore(), handler)

	first := postPayment(t, server, "key-1", `{"amount":150000}`)
	second := postPayment(t, server, "key-1", `{"amount":150000}`)

	assert.Equal(t, http.StatusPaymentRequired, first.Code)
	assert.Equal(t, http.StatusPaymentRequired, second.Code)
	assert.Equal(t, "true", second.Header().Get(ReplayHeader))
	assert.Equal(t, 1, handler.callCount())
}

// Two merchants using the same Idempotency-Key must not collide.
func TestIdempotencyIsScopedPerMerchant(t *testing.T) {
	store := newMemStore()
	handler := &countingHandler{status: http.StatusCreated, body: `{"id":"txn"}`}

	serverA := RequestID(withMerchant(uuid.New(), Idempotency(store)(handler)))
	serverB := RequestID(withMerchant(uuid.New(), Idempotency(store)(handler)))

	require.Equal(t, http.StatusCreated, postPayment(t, serverA, "shared-key", `{"amount":1}`).Code)
	recB := postPayment(t, serverB, "shared-key", `{"amount":1}`)

	assert.Equal(t, http.StatusCreated, recB.Code)
	assert.Empty(t, recB.Header().Get(ReplayHeader), "merchant B gets its own execution")
	assert.Equal(t, 2, handler.callCount())
}
