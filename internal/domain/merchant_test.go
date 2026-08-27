package domain

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateMerchantInput(t *testing.T) {
	tests := []struct {
		name       string
		merchant   string
		email      string
		webhookURL string
		wantField  string
	}{
		{name: "valid without webhook", merchant: "Demo", email: "a@b.com"},
		{name: "valid with http", merchant: "Demo", email: "a@b.com", webhookURL: "http://localhost:8080/hook"},
		{name: "valid with https", merchant: "Demo", email: "a@b.com", webhookURL: "https://example.com/hook"},
		{name: "empty name", merchant: "", email: "a@b.com", wantField: "name"},
		{name: "empty email", merchant: "Demo", email: "", wantField: "email"},
		{name: "email without at sign", merchant: "Demo", email: "not-an-email", wantField: "email"},
		{name: "webhook without scheme", merchant: "Demo", email: "a@b.com", webhookURL: "example.com/hook", wantField: "webhook_url"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateMerchantInput(tc.merchant, tc.email, tc.webhookURL)
			if tc.wantField == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			var invalid *ValidationError
			require.True(t, errors.As(err, &invalid))
			assert.Equal(t, tc.wantField, invalid.Field)
			assert.ErrorIs(t, err, ErrValidation)
		})
	}
}

func TestMerchantIsActive(t *testing.T) {
	assert.True(t, (&Merchant{Status: MerchantActive}).IsActive())
	assert.False(t, (&Merchant{Status: MerchantSuspended}).IsActive())
}

func TestDeclinedError(t *testing.T) {
	err := NewDeclinedError(DeclineInsufficientFunds)
	assert.Equal(t, DeclineInsufficientFunds, err.Code)
	assert.Equal(t, "The card has insufficient funds.", err.Message)
	assert.Contains(t, err.Error(), "insufficient_funds")

	var declined *DeclinedError
	assert.True(t, errors.As(error(err), &declined))

	unknown := NewDeclinedError("something_new")
	assert.Equal(t, "The card was declined.", unknown.Message)
}

func TestValidationErrorUnwraps(t *testing.T) {
	err := Invalid("amount", "must be greater than zero")
	assert.ErrorIs(t, err, ErrValidation)
	assert.Equal(t, "amount: must be greater than zero", err.Error())
}
