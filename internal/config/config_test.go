package config

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testKeyHex = "0000000000000000000000000000000000000000000000000000000000000000"

// setMinimalEnv sets the variables without a safe default, so Load can succeed.
func setMinimalEnv(t *testing.T) {
	t.Helper()
	t.Setenv("DATABASE_URL", "postgres://u:p@localhost:5432/db?sslmode=disable")
	t.Setenv("ADMIN_TOKEN", "token")
	t.Setenv("SECRET_ENC_KEY", testKeyHex)
}

func TestLoadDefaults(t *testing.T) {
	setMinimalEnv(t)

	cfg, err := Load()
	require.NoError(t, err)

	assert.Equal(t, "development", cfg.AppEnv)
	assert.Equal(t, 8080, cfg.HTTPPort)
	assert.Equal(t, ":8080", cfg.Addr())
	assert.False(t, cfg.IsProduction())
	assert.Equal(t, 200, cfg.PlatformFeeBPS)
	assert.Len(t, cfg.SecretEncKey, 32)

	assert.Equal(t, 3*time.Second, cfg.Acquirer.Timeout)
	assert.InDelta(t, 0.10, cfg.Acquirer.DeclineRate, 0.0001)
	assert.Equal(t, 5, cfg.Acquirer.BreakerThreshold)

	assert.Equal(t, 5, cfg.Webhook.Workers)
	assert.Equal(t, 6, cfg.Webhook.MaxAttempts)

	assert.Equal(t, 24*time.Hour, cfg.Limits.IdempotencyTTL)
	assert.Equal(t, 5*time.Minute, cfg.Limits.AuthMaxClockSkew)
}

func TestLoadOverrides(t *testing.T) {
	setMinimalEnv(t)
	t.Setenv("APP_ENV", "production")
	t.Setenv("HTTP_PORT", "9090")
	t.Setenv("PLATFORM_FEE_BPS", "150")
	t.Setenv("ACQUIRER_TIMEOUT", "1500ms")
	t.Setenv("WEBHOOK_WORKERS", "12")
	t.Setenv("IDEMPOTENCY_TTL", "1h")

	cfg, err := Load()
	require.NoError(t, err)

	assert.True(t, cfg.IsProduction())
	assert.Equal(t, ":9090", cfg.Addr())
	assert.Equal(t, 150, cfg.PlatformFeeBPS)
	assert.Equal(t, 1500*time.Millisecond, cfg.Acquirer.Timeout)
	assert.Equal(t, 12, cfg.Webhook.Workers)
	assert.Equal(t, time.Hour, cfg.Limits.IdempotencyTTL)
}

// Missing configuration must stop the process at startup, not surface as a nil
// dereference on the first request.
func TestLoadRequiresCriticalValues(t *testing.T) {
	t.Run("database url", func(t *testing.T) {
		t.Setenv("DATABASE_URL", "")
		t.Setenv("ADMIN_TOKEN", "token")
		t.Setenv("SECRET_ENC_KEY", testKeyHex)

		_, err := Load()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "DATABASE_URL")
	})

	t.Run("encryption key", func(t *testing.T) {
		t.Setenv("DATABASE_URL", "postgres://localhost/db")
		t.Setenv("ADMIN_TOKEN", "token")
		t.Setenv("SECRET_ENC_KEY", "")

		_, err := Load()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "SECRET_ENC_KEY")
	})
}

func TestLoadReportsEveryProblemAtOnce(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	t.Setenv("ADMIN_TOKEN", "")
	t.Setenv("SECRET_ENC_KEY", "")

	_, err := Load()
	require.Error(t, err)

	// One startup, one complete list: fixing them one restart at a time is
	// exactly the experience this avoids.
	for _, want := range []string{"DATABASE_URL", "SECRET_ENC_KEY"} {
		assert.Containsf(t, err.Error(), want, "error should mention %s", want)
	}
}

// ADMIN_TOKEN is only needed by the process that serves the admin endpoint, so
// the worker and the migrator start without it.
func TestRequireAdminToken(t *testing.T) {
	setMinimalEnv(t)
	t.Setenv("ADMIN_TOKEN", "")

	cfg, err := Load()
	require.NoError(t, err, "the worker must be able to start without an admin token")

	err = cfg.RequireAdminToken()
	require.Error(t, err, "the API must refuse to start without one")
	assert.Contains(t, err.Error(), "ADMIN_TOKEN")

	t.Setenv("ADMIN_TOKEN", "token")
	cfg, err = Load()
	require.NoError(t, err)
	assert.NoError(t, cfg.RequireAdminToken())
}

func TestLoadRejectsMalformedValues(t *testing.T) {
	tests := []struct {
		name  string
		key   string
		value string
	}{
		{name: "port", key: "HTTP_PORT", value: "eighty-eighty"},
		{name: "decline rate", key: "ACQUIRER_DECLINE_RATE", value: "quite often"},
		{name: "duration", key: "ACQUIRER_TIMEOUT", value: "3 seconds"},
		{name: "fee out of range", key: "PLATFORM_FEE_BPS", value: "20000"},
		{name: "no workers", key: "WEBHOOK_WORKERS", value: "0"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			setMinimalEnv(t)
			t.Setenv(tc.key, tc.value)

			_, err := Load()
			require.Error(t, err)
		})
	}
}

func TestLoadRejectsInvertedLatencyRange(t *testing.T) {
	setMinimalEnv(t)
	t.Setenv("ACQUIRER_MIN_LATENCY", "900ms")
	t.Setenv("ACQUIRER_MAX_LATENCY", "100ms")

	_, err := Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ACQUIRER_MIN_LATENCY")
}

// The encryption key may be written as hex or base64, so a developer can paste
// whichever form openssl gave them.
func TestSecretKeyAcceptsHexAndBase64(t *testing.T) {
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = byte(i + 1)
	}

	t.Run("hex", func(t *testing.T) {
		setMinimalEnv(t)
		cfg, err := Load()
		require.NoError(t, err)
		assert.Len(t, cfg.SecretEncKey, 32)
	})

	t.Run("base64", func(t *testing.T) {
		setMinimalEnv(t)
		t.Setenv("SECRET_ENC_KEY", base64.StdEncoding.EncodeToString(raw))

		cfg, err := Load()
		require.NoError(t, err)
		assert.Equal(t, raw, cfg.SecretEncKey)
	})

	t.Run("wrong length", func(t *testing.T) {
		setMinimalEnv(t)
		t.Setenv("SECRET_ENC_KEY", strings.Repeat("ab", 8)) // 8 bytes

		_, err := Load()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "32 bytes")
	})

	t.Run("not hex or base64", func(t *testing.T) {
		setMinimalEnv(t)
		t.Setenv("SECRET_ENC_KEY", "!!! not a key !!!")

		_, err := Load()
		require.Error(t, err)
	})
}

func TestDecodeHexOrBase64(t *testing.T) {
	b, err := decodeHexOrBase64("00ff")
	require.NoError(t, err)
	assert.Equal(t, []byte{0x00, 0xff}, b)

	b, err = decodeHexOrBase64(base64.StdEncoding.EncodeToString([]byte("hello!")))
	require.NoError(t, err)
	assert.Equal(t, []byte("hello!"), b)

	_, err = decodeHexOrBase64("!!!")
	assert.Error(t, err)
}
