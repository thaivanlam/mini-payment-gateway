//go:build integration

// Package integration exercises the gateway against a real Postgres and a real
// Redis, started by docker compose. Nothing here is mocked except the acquirer:
// a fake database would not tell us whether SELECT FOR UPDATE, the unique
// constraints or the append-only journal actually behave as designed.
package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/thaivanlam/mini-payment-gateway/internal/acquirer"
	"github.com/thaivanlam/mini-payment-gateway/internal/app"
	"github.com/thaivanlam/mini-payment-gateway/internal/config"
	"github.com/thaivanlam/mini-payment-gateway/internal/domain"
	"github.com/thaivanlam/mini-payment-gateway/internal/service"
	transport "github.com/thaivanlam/mini-payment-gateway/internal/transport/http"
)

// Test cards. All Luhn-valid; the last two carry deterministic behaviour.
const (
	CardNormal  = "4242424242424242"
	CardDecline = "4242424242420000"
	CardTimeout = "4242424242400002"
)

const adminToken = "test-admin-token"

var shared *app.App

func TestMain(m *testing.M) {
	setDefault("DATABASE_URL", "postgres://gateway:gateway@localhost:5432/gateway?sslmode=disable")
	setDefault("REDIS_URL", "redis://localhost:6379/1")
	setDefault("ADMIN_TOKEN", adminToken)
	setDefault("SECRET_ENC_KEY", "0000000000000000000000000000000000000000000000000000000000000000")
	setDefault("PLATFORM_FEE_BPS", "200")
	setDefault("LOG_LEVEL", "warn")
	// Deterministic acquirer: the integration tests are about our own
	// correctness, not about random declines.
	setDefault("ACQUIRER_DECLINE_RATE", "0")
	setDefault("ACQUIRER_TIMEOUT_RATE", "0")
	setDefault("ACQUIRER_MIN_LATENCY", "0s")
	setDefault("ACQUIRER_MAX_LATENCY", "0s")
	setDefault("ACQUIRER_TIMEOUT", "2s")
	setDefault("REPORT_DIR", os.TempDir())

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "integration: load config: %v\n", err)
		os.Exit(1)
	}
	log := app.NewLogger(cfg.LogLevel, "development")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	shared, err = app.New(ctx, cfg, log)
	if err != nil {
		fmt.Fprintf(os.Stderr, "integration: cannot reach postgres/redis (run `make up` first): %v\n", err)
		os.Exit(1)
	}
	if err := app.Migrate(ctx, shared.DB, "up"); err != nil {
		fmt.Fprintf(os.Stderr, "integration: migrate: %v\n", err)
		os.Exit(1)
	}

	code := m.Run()
	shared.Close()
	os.Exit(code)
}

func setDefault(key, value string) {
	if os.Getenv(key) == "" {
		_ = os.Setenv(key, value)
	}
}

// env is one isolated test fixture: a clean database, a clean Redis namespace,
// a running HTTP server and a merchant with credentials.
type env struct {
	t        *testing.T
	app      *app.App
	server   *httptest.Server
	log      *slog.Logger
	Merchant *domain.Merchant
	APIKey   string
	Secret   string
	Received *webhookSink
}

// newEnv truncates the datastores and builds a fresh fixture.
func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()

	truncate(t, ctx)
	flushRedis(t, ctx, shared.Redis)

	sink := newWebhookSink()
	t.Cleanup(sink.Close)

	e := &env{t: t, app: shared, log: shared.Log, Received: sink}

	router := transport.NewRouter(transport.Deps{
		Merchants: transport.NewMerchantHandler(shared.Merchants),
		Payments:  transport.NewPaymentHandler(shared.Payments),
		Ledger:    transport.NewLedgerHandler(shared.Ledger, shared.Recon),
		System: transport.NewSystemHandler(
			shared.DB, shared.Redis, shared.Merchants.WebhookSecret),
		Auth:       shared.Merchants,
		Idem:       shared.Idempotency,
		Limiter:    shared.Limiter,
		AdminToken: adminToken,
		Log:        shared.Log,
	})
	e.server = httptest.NewServer(router)
	t.Cleanup(e.server.Close)

	created, err := shared.Merchants.Create(ctx, service.CreateMerchantInput{
		Name:       "Integration Merchant",
		Email:      fmt.Sprintf("merchant-%s@example.com", uuid.NewString()[:8]),
		WebhookURL: sink.URL(),
	})
	require.NoError(t, err)

	e.Merchant = created.Merchant
	e.APIKey = created.Merchant.APIKey
	e.Secret = created.APISecret
	sink.SetSecret(created.WebhookSecret)
	return e
}

func truncate(t *testing.T, ctx context.Context) {
	t.Helper()
	// TRUNCATE does not fire the row-level trigger that blocks DELETE on
	// ledger_entries, which is exactly why the journal can still be reset here.
	_, err := shared.DB.Pool.Exec(ctx,
		`TRUNCATE ledger_entries, webhook_deliveries, transactions, merchants RESTART IDENTITY CASCADE`)
	require.NoError(t, err)
}

func flushRedis(t *testing.T, ctx context.Context, rdb *redis.Client) {
	t.Helper()
	require.NoError(t, rdb.FlushDB(ctx).Err())
}

// ---- signed HTTP client ----

type apiResponse struct {
	Status  int
	Header  http.Header
	Body    []byte
	Decoded map[string]any
}

// JSON decodes the body into v.
func (r apiResponse) JSON(t *testing.T, v any) {
	t.Helper()
	require.NoError(t, json.Unmarshal(r.Body, v), "body: %s", r.Body)
}

// ErrorCode returns error.code from a failure envelope.
func (r apiResponse) ErrorCode() string {
	var body transport.ErrorResponse
	if err := json.Unmarshal(r.Body, &body); err != nil {
		return ""
	}
	return body.Error.Code
}

// requestOpts tweaks a single call.
type requestOpts struct {
	IdempotencyKey string
	Timestamp      string // override for clock-skew tests
	Signature      string // override for tampering tests
	SkipAuth       bool
}

// do sends an HMAC-signed request, exactly as a merchant SDK would.
func (e *env) do(method, path string, body any, opts ...requestOpts) apiResponse {
	e.t.Helper()

	var opt requestOpts
	if len(opts) > 0 {
		opt = opts[0]
	}

	var payload []byte
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		require.NoError(e.t, err)
	}

	req, err := http.NewRequest(method, e.server.URL+path, bytes.NewReader(payload))
	require.NoError(e.t, err)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	if !opt.SkipAuth {
		ts := opt.Timestamp
		if ts == "" {
			ts = strconv.FormatInt(time.Now().Unix(), 10)
		}
		signature := opt.Signature
		if signature == "" {
			// Only the path is signed, never the query string: that is what
			// the server verifies against r.URL.Path.
			signPath := strings.SplitN(path, "?", 2)[0]
			signature = service.ComputeRequestSignature(e.Secret, ts, method, signPath, payload)
		}
		req.Header.Set(transport.HeaderAPIKey, e.APIKey)
		req.Header.Set(transport.HeaderTimestamp, ts)
		req.Header.Set(transport.HeaderSignature, signature)
	}
	if opt.IdempotencyKey != "" {
		req.Header.Set(transport.IdempotencyKeyHeader, opt.IdempotencyKey)
	}

	resp, err := e.server.Client().Do(req)
	require.NoError(e.t, err)
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	require.NoError(e.t, err)

	out := apiResponse{Status: resp.StatusCode, Header: resp.Header, Body: raw}
	_ = json.Unmarshal(raw, &out.Decoded)
	return out
}

// createPayment authorizes a payment through the API.
func (e *env) createPayment(reference string, amount int64, card string, capture bool, idemKey string) apiResponse {
	e.t.Helper()
	if idemKey == "" {
		idemKey = uuid.NewString()
	}
	return e.do(http.MethodPost, "/api/v1/payments", map[string]any{
		"reference": reference,
		"amount":    amount,
		"currency":  "VND",
		"card": map[string]any{
			"number":    card,
			"exp_month": 12,
			"exp_year":  2030,
			"cvv":       "123",
		},
		"capture":  capture,
		"metadata": map[string]string{"suite": "integration"},
	}, requestOpts{IdempotencyKey: idemKey})
}

// authorize is createPayment plus the assertion that it worked.
func (e *env) authorize(reference string, amount int64) transport.PaymentResponse {
	e.t.Helper()
	resp := e.createPayment(reference, amount, CardNormal, false, "")
	require.Equalf(e.t, http.StatusCreated, resp.Status, "authorize failed: %s", resp.Body)

	var payment transport.PaymentResponse
	resp.JSON(e.t, &payment)
	require.Equal(e.t, string(domain.StatusAuthorized), payment.Status)
	return payment
}

// ---- assertions shared by every scenario ----

// assertLedgerBalanced is the invariant check the spec requires after every
// integration scenario: across the whole journal, every entry group balances.
func (e *env) assertLedgerBalanced() {
	e.t.Helper()
	unbalanced, err := shared.Ledger.CheckInvariant(context.Background())
	require.NoError(e.t, err)
	require.Emptyf(e.t, unbalanced, "unbalanced ledger entry groups: %+v", unbalanced)
}

// entriesFor returns the journal lines of one transaction.
func (e *env) entriesFor(txID uuid.UUID) []domain.LedgerEntry {
	e.t.Helper()
	entries, err := shared.LedgerRepo.ListByTransaction(context.Background(), shared.DB.Pool, txID)
	require.NoError(e.t, err)
	return entries
}

// transaction reloads a transaction straight from the database.
func (e *env) transaction(txID uuid.UUID) *domain.Transaction {
	e.t.Helper()
	txn, err := shared.TransactionRepo.GetByID(context.Background(), shared.DB.Pool, txID)
	require.NoError(e.t, err)
	return txn
}

// deliveries returns the outbox rows of one transaction.
func (e *env) deliveries(txID uuid.UUID) []*domain.WebhookDelivery {
	e.t.Helper()
	rows, err := shared.WebhookRepo.ListByTransaction(context.Background(), shared.DB.Pool, txID)
	require.NoError(e.t, err)
	return rows
}

// paymentsWithFastAcquirer builds a payment service backed by a specific
// acquirer, for tests that need a decline or a timeout on demand.
func paymentsWithAcquirer(acq acquirer.Acquirer) *service.PaymentService {
	return service.NewPaymentService(
		shared.DB, shared.TransactionRepo, shared.LedgerRepo, shared.WebhookRepo,
		acq, shared.Cfg.PlatformFeeBPS, shared.Log)
}

// mustJSON marshals v the same way the test client does.
func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	require.NoError(t, err)
	return raw
}

// bytesReader wraps a payload for an unsigned (admin) request.
func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }

// decodeBody reads a JSON response body into v.
func decodeBody(r io.Reader, v any) error { return json.NewDecoder(r).Decode(v) }
