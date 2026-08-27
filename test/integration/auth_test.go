//go:build integration

package integration

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thaivanlam/mini-payment-gateway/internal/service"
	transport "github.com/thaivanlam/mini-payment-gateway/internal/transport/http"
)

func TestValidSignatureIsAccepted(t *testing.T) {
	e := newEnv(t)

	resp := e.do(http.MethodGet, "/api/v1/merchants/me", nil)

	require.Equalf(t, http.StatusOK, resp.Status, "body: %s", resp.Body)

	var merchant transport.MerchantResponse
	resp.JSON(t, &merchant)
	assert.Equal(t, e.Merchant.ID.String(), merchant.ID)
	assert.Equal(t, e.APIKey, merchant.APIKey)

	// The response must never carry a secret.
	assert.NotContains(t, string(resp.Body), e.Secret)
	assert.NotContains(t, string(resp.Body), "api_secret")
	assert.NotContains(t, string(resp.Body), "webhook_secret")
}

func TestMissingCredentialsAreRejected(t *testing.T) {
	e := newEnv(t)

	resp := e.do(http.MethodGet, "/api/v1/merchants/me", nil, requestOpts{SkipAuth: true})

	assert.Equal(t, http.StatusUnauthorized, resp.Status)
	assert.Equal(t, "authentication_failed", resp.ErrorCode())
}

func TestWrongSignatureIsRejected(t *testing.T) {
	e := newEnv(t)

	resp := e.do(http.MethodGet, "/api/v1/merchants/me", nil, requestOpts{
		Signature: "deadbeef",
	})

	assert.Equal(t, http.StatusUnauthorized, resp.Status)
}

// A signature is bound to one method and one path: replaying it elsewhere fails.
func TestSignatureIsBoundToMethodAndPath(t *testing.T) {
	e := newEnv(t)

	ts := strconv.FormatInt(time.Now().Unix(), 10)
	// Sign GET /api/v1/merchants/me ...
	signature := service.ComputeRequestSignature(e.Secret, ts, http.MethodGet, "/api/v1/merchants/me", nil)

	// ... and try to use it on a different path.
	resp := e.do(http.MethodGet, "/api/v1/payments", nil, requestOpts{
		Timestamp: ts,
		Signature: signature,
	})

	assert.Equal(t, http.StatusUnauthorized, resp.Status,
		"a signature for one endpoint must not authenticate another")
}

// A tampered body invalidates the signature, which is the point of signing it.
func TestTamperedBodyIsRejected(t *testing.T) {
	e := newEnv(t)

	ts := strconv.FormatInt(time.Now().Unix(), 10)
	original := mustJSON(t, map[string]any{"amount": 1000})
	signature := service.ComputeRequestSignature(
		e.Secret, ts, http.MethodPost, "/api/v1/payments", original)

	resp := e.do(http.MethodPost, "/api/v1/payments", map[string]any{"amount": 999999}, requestOpts{
		Timestamp:      ts,
		Signature:      signature,
		IdempotencyKey: uuid.NewString(),
	})

	assert.Equal(t, http.StatusUnauthorized, resp.Status)
}

// Old and future timestamps are refused, which bounds how long a captured
// request stays replayable.
func TestExpiredTimestampIsRejected(t *testing.T) {
	e := newEnv(t)

	for _, drift := range []time.Duration{-10 * time.Minute, 10 * time.Minute} {
		ts := strconv.FormatInt(time.Now().Add(drift).Unix(), 10)
		signature := service.ComputeRequestSignature(
			e.Secret, ts, http.MethodGet, "/api/v1/merchants/me", nil)

		resp := e.do(http.MethodGet, "/api/v1/merchants/me", nil, requestOpts{
			Timestamp: ts,
			Signature: signature,
		})

		assert.Equalf(t, http.StatusUnauthorized, resp.Status,
			"a signature drifted by %s must be refused even though it is valid", drift)
	}
}

// A timestamp inside the window is accepted.
func TestTimestampInsideSkewWindowIsAccepted(t *testing.T) {
	e := newEnv(t)

	ts := strconv.FormatInt(time.Now().Add(-2*time.Minute).Unix(), 10)
	signature := service.ComputeRequestSignature(
		e.Secret, ts, http.MethodGet, "/api/v1/merchants/me", nil)

	resp := e.do(http.MethodGet, "/api/v1/merchants/me", nil, requestOpts{
		Timestamp: ts,
		Signature: signature,
	})

	assert.Equal(t, http.StatusOK, resp.Status)
}

func TestUnknownAPIKeyIsRejected(t *testing.T) {
	e := newEnv(t)
	e.APIKey = "pk_test_does_not_exist"

	resp := e.do(http.MethodGet, "/api/v1/merchants/me", nil)

	assert.Equal(t, http.StatusUnauthorized, resp.Status)
}

func TestSuspendedMerchantIsRefused(t *testing.T) {
	e := newEnv(t)

	_, err := shared.DB.Pool.Exec(context.Background(),
		`UPDATE merchants SET status = 'suspended' WHERE id = $1`, e.Merchant.ID)
	require.NoError(t, err)

	resp := e.do(http.MethodGet, "/api/v1/merchants/me", nil)

	assert.Equal(t, http.StatusForbidden, resp.Status)
	assert.Equal(t, "merchant_suspended", resp.ErrorCode())
}

func TestAdminEndpointRequiresToken(t *testing.T) {
	e := newEnv(t)

	req := map[string]any{"name": "New Merchant", "email": uuid.NewString() + "@example.com"}

	// No token.
	resp := e.do(http.MethodPost, "/api/v1/merchants", req, requestOpts{SkipAuth: true})
	assert.Equal(t, http.StatusUnauthorized, resp.Status)
}

// The api_secret is shown exactly once, at creation.
func TestCreateMerchantReturnsSecretsOnce(t *testing.T) {
	e := newEnv(t)

	body := mustJSON(t, map[string]any{
		"name":  "Created Merchant",
		"email": uuid.NewString() + "@example.com",
	})
	req, err := http.NewRequest(http.MethodPost, e.server.URL+"/api/v1/merchants", bytesReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+adminToken)

	resp, err := e.server.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	var created transport.CreateMerchantResponse
	require.NoError(t, decodeBody(resp.Body, &created))

	assert.NotEmpty(t, created.APISecret)
	assert.NotEmpty(t, created.WebhookSecret)
	assert.NotEmpty(t, created.Notice)

	// The database holds ciphertext, not the secret.
	var stored string
	require.NoError(t, shared.DB.Pool.QueryRow(context.Background(),
		`SELECT api_secret_enc FROM merchants WHERE id = $1`, uuid.MustParse(created.ID)).Scan(&stored))
	assert.NotEqual(t, created.APISecret, stored)
	assert.NotContains(t, stored, created.APISecret)

	// And the secret can be recovered only with the encryption key.
	plaintext, err := shared.Cipher.Decrypt(stored)
	require.NoError(t, err)
	assert.Equal(t, created.APISecret, plaintext)
}

func TestHealthAndReadyProbes(t *testing.T) {
	e := newEnv(t)

	health := e.do(http.MethodGet, "/healthz", nil, requestOpts{SkipAuth: true})
	assert.Equal(t, http.StatusOK, health.Status)

	ready := e.do(http.MethodGet, "/readyz", nil, requestOpts{SkipAuth: true})
	assert.Equal(t, http.StatusOK, ready.Status)
	assert.Contains(t, string(ready.Body), `"postgres":"ok"`)
	assert.Contains(t, string(ready.Body), `"redis":"ok"`)
}

func TestEveryResponseCarriesARequestID(t *testing.T) {
	e := newEnv(t)

	ok := e.do(http.MethodGet, "/api/v1/merchants/me", nil)
	assert.NotEmpty(t, ok.Header.Get(transport.RequestIDHeader))

	failed := e.do(http.MethodGet, "/api/v1/payments/"+uuid.NewString(), nil)
	assert.NotEmpty(t, failed.Header.Get(transport.RequestIDHeader))

	var body transport.ErrorResponse
	failed.JSON(t, &body)
	assert.Equal(t, failed.Header.Get(transport.RequestIDHeader), body.Error.RequestID,
		"the request id in the body and the header must match")
}
