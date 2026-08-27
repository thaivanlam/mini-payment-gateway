package secrets

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// Key prefixes, chosen to be recognisable in logs and support tickets the way
// Stripe keys are.
const (
	APIKeyPrefix    = "pk_test_"
	APISecretPrefix = "sk_test_"
	WebhookPrefix   = "whsec_"
)

// NewAPIKey returns a public, non-sensitive merchant identifier.
func NewAPIKey() (string, error) { return randomString(APIKeyPrefix, 16) }

// NewAPISecret returns the signing secret shown to the merchant exactly once.
func NewAPISecret() (string, error) { return randomString(APISecretPrefix, 32) }

// NewWebhookSecret returns the secret used to sign webhook payloads.
func NewWebhookSecret() (string, error) { return randomString(WebhookPrefix, 32) }

func randomString(prefix string, nbytes int) (string, error) {
	buf := make([]byte, nbytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("secrets: generate: %w", err)
	}
	return prefix + hex.EncodeToString(buf), nil
}
