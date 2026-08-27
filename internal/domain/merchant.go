package domain

import (
	"time"

	"github.com/google/uuid"
)

// MerchantStatus is the lifecycle state of a merchant account.
type MerchantStatus string

const (
	MerchantActive    MerchantStatus = "active"
	MerchantSuspended MerchantStatus = "suspended"
)

// Merchant is an API consumer of the gateway.
//
// APISecretEnc and WebhookSecretEnc hold ciphertext, never plaintext: HMAC
// request signing and webhook signing are symmetric, so the server has to be
// able to recover the secret. The plaintext api_secret is shown exactly once,
// in the response of POST /merchants, and is not retrievable afterwards.
type Merchant struct {
	ID               uuid.UUID
	Name             string
	Email            string
	APIKey           string
	APISecretEnc     string
	WebhookURL       string
	WebhookSecretEnc string
	Status           MerchantStatus
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// IsActive reports whether the merchant may transact.
func (m *Merchant) IsActive() bool { return m.Status == MerchantActive }

// ValidateMerchantInput checks the fields a caller supplies when creating a
// merchant. Secrets and identifiers are generated server-side.
func ValidateMerchantInput(name, email, webhookURL string) error {
	if name == "" {
		return Invalid("name", "must not be empty")
	}
	if email == "" {
		return Invalid("email", "must not be empty")
	}
	if len(email) < 3 || !containsRune(email, '@') {
		return Invalid("email", "must be a valid email address")
	}
	if webhookURL != "" && !hasHTTPScheme(webhookURL) {
		return Invalid("webhook_url", "must be an http(s) URL")
	}
	return nil
}

func containsRune(s string, r rune) bool {
	for _, c := range s {
		if c == r {
			return true
		}
	}
	return false
}

func hasHTTPScheme(s string) bool {
	return len(s) > 7 && (s[:7] == "http://" || (len(s) > 8 && s[:8] == "https://"))
}
