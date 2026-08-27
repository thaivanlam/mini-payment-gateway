// Package config loads runtime configuration from the environment.
//
// No third-party loader and no globals: Load returns a value that the
// entrypoints hand to constructors. Anything missing that has no safe default
// is an error at startup rather than a nil dereference at request time.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the fully resolved configuration of a process.
type Config struct {
	AppEnv   string
	HTTPPort int
	LogLevel string

	DatabaseURL   string
	RedisURL      string
	RunMigrations bool

	AdminToken   string
	SecretEncKey []byte

	PlatformFeeBPS int

	Acquirer  AcquirerConfig
	Webhook   WebhookConfig
	Limits    LimitsConfig
	ReportDir string
}

// AcquirerConfig drives the simulated card processor and its circuit breaker.
type AcquirerConfig struct {
	DeclineRate      float64
	TimeoutRate      float64
	Timeout          time.Duration
	MinLatency       time.Duration
	MaxLatency       time.Duration
	BreakerThreshold int
	BreakerCooldown  time.Duration
}

// WebhookConfig drives the delivery worker.
type WebhookConfig struct {
	Workers         int
	MaxAttempts     int
	PollInterval    time.Duration
	RequestTimeout  time.Duration
	ShutdownTimeout time.Duration
}

// LimitsConfig holds idempotency and rate limiting knobs.
type LimitsConfig struct {
	IdempotencyTTL   time.Duration
	AuthMaxClockSkew time.Duration
	RateLimitRPM     int
}

// Load reads the environment and validates it.
func Load() (*Config, error) {
	var errs []error
	collect := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}

	cfg := &Config{
		AppEnv:        env("APP_ENV", "development"),
		LogLevel:      env("LOG_LEVEL", "info"),
		DatabaseURL:   env("DATABASE_URL", ""),
		RedisURL:      env("REDIS_URL", "redis://localhost:6379/0"),
		AdminToken:    env("ADMIN_TOKEN", ""),
		ReportDir:     env("REPORT_DIR", "reports"),
		RunMigrations: envBool("RUN_MIGRATIONS", false),
	}

	port, err := envInt("HTTP_PORT", 8080)
	collect(err)
	cfg.HTTPPort = port

	feeBPS, err := envInt("PLATFORM_FEE_BPS", 200)
	collect(err)
	cfg.PlatformFeeBPS = feeBPS

	key, err := decodeKey(env("SECRET_ENC_KEY", ""))
	collect(err)
	cfg.SecretEncKey = key

	declineRate, err := envFloat("ACQUIRER_DECLINE_RATE", 0.10)
	collect(err)
	timeoutRate, err := envFloat("ACQUIRER_TIMEOUT_RATE", 0.05)
	collect(err)
	acqTimeout, err := envDuration("ACQUIRER_TIMEOUT", 3*time.Second)
	collect(err)
	minLat, err := envDuration("ACQUIRER_MIN_LATENCY", 50*time.Millisecond)
	collect(err)
	maxLat, err := envDuration("ACQUIRER_MAX_LATENCY", 800*time.Millisecond)
	collect(err)
	breakerThreshold, err := envInt("ACQUIRER_BREAKER_THRESHOLD", 5)
	collect(err)
	breakerCooldown, err := envDuration("ACQUIRER_BREAKER_COOLDOWN", 30*time.Second)
	collect(err)
	cfg.Acquirer = AcquirerConfig{
		DeclineRate:      declineRate,
		TimeoutRate:      timeoutRate,
		Timeout:          acqTimeout,
		MinLatency:       minLat,
		MaxLatency:       maxLat,
		BreakerThreshold: breakerThreshold,
		BreakerCooldown:  breakerCooldown,
	}

	workers, err := envInt("WEBHOOK_WORKERS", 5)
	collect(err)
	maxAttempts, err := envInt("WEBHOOK_MAX_ATTEMPTS", 6)
	collect(err)
	pollInterval, err := envDuration("WEBHOOK_POLL_INTERVAL", 2*time.Second)
	collect(err)
	whTimeout, err := envDuration("WEBHOOK_TIMEOUT", 10*time.Second)
	collect(err)
	shutdownTimeout, err := envDuration("WEBHOOK_SHUTDOWN_TIMEOUT", 30*time.Second)
	collect(err)
	cfg.Webhook = WebhookConfig{
		Workers:         workers,
		MaxAttempts:     maxAttempts,
		PollInterval:    pollInterval,
		RequestTimeout:  whTimeout,
		ShutdownTimeout: shutdownTimeout,
	}

	idemTTL, err := envDuration("IDEMPOTENCY_TTL", 24*time.Hour)
	collect(err)
	skew, err := envDuration("AUTH_MAX_CLOCK_SKEW", 5*time.Minute)
	collect(err)
	rpm, err := envInt("RATE_LIMIT_RPM", 120)
	collect(err)
	cfg.Limits = LimitsConfig{
		IdempotencyTTL:   idemTTL,
		AuthMaxClockSkew: skew,
		RateLimitRPM:     rpm,
	}

	if cfg.DatabaseURL == "" {
		collect(errors.New("DATABASE_URL is required"))
	}
	if cfg.PlatformFeeBPS < 0 || cfg.PlatformFeeBPS > 10_000 {
		collect(errors.New("PLATFORM_FEE_BPS must be between 0 and 10000"))
	}
	if cfg.Acquirer.MinLatency > cfg.Acquirer.MaxLatency {
		collect(errors.New("ACQUIRER_MIN_LATENCY must not exceed ACQUIRER_MAX_LATENCY"))
	}
	if cfg.Webhook.Workers < 1 {
		collect(errors.New("WEBHOOK_WORKERS must be at least 1"))
	}

	if len(errs) > 0 {
		return nil, fmt.Errorf("invalid configuration: %w", errors.Join(errs...))
	}
	return cfg, nil
}

// IsProduction reports whether the process runs with production semantics.
func (c *Config) IsProduction() bool { return c.AppEnv == "production" }

// RequireAdminToken is called by the API, which is the only process that serves
// the admin endpoint.
//
// It is not part of Load because the worker, the migrator and the seeder never
// touch that endpoint, and a process should not refuse to start over a value it
// will never read.
func (c *Config) RequireAdminToken() error {
	if c.AdminToken == "" {
		return errors.New("invalid configuration: ADMIN_TOKEN is required to serve the API")
	}
	return nil
}

// Addr is the listen address of the HTTP server.
func (c *Config) Addr() string { return fmt.Sprintf(":%d", c.HTTPPort) }

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) (int, error) {
	raw, ok := os.LookupEnv(key)
	if !ok || raw == "" {
		return def, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return def, fmt.Errorf("%s: %w", key, err)
	}
	return v, nil
}

func envFloat(key string, def float64) (float64, error) {
	raw, ok := os.LookupEnv(key)
	if !ok || raw == "" {
		return def, nil
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return def, fmt.Errorf("%s: %w", key, err)
	}
	return v, nil
}

func envBool(key string, def bool) bool {
	raw, ok := os.LookupEnv(key)
	if !ok || raw == "" {
		return def
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		return def
	}
	return v
}

func envDuration(key string, def time.Duration) (time.Duration, error) {
	raw, ok := os.LookupEnv(key)
	if !ok || raw == "" {
		return def, nil
	}
	v, err := time.ParseDuration(raw)
	if err != nil {
		return def, fmt.Errorf("%s: %w", key, err)
	}
	return v, nil
}

// decodeKey accepts the 32-byte secret encryption key as hex or base64.
func decodeKey(raw string) ([]byte, error) {
	if raw == "" {
		return nil, errors.New("SECRET_ENC_KEY is required (32 bytes, hex or base64)")
	}
	key, err := decodeHexOrBase64(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("SECRET_ENC_KEY: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("SECRET_ENC_KEY: must decode to 32 bytes, got %d", len(key))
	}
	return key, nil
}
