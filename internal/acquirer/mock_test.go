package acquirer

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thaivanlam/mini-payment-gateway/internal/config"
	"github.com/thaivanlam/mini-payment-gateway/internal/domain"
)

// Test cards. All three are Luhn-valid, so they exercise the simulated
// behaviour rather than the input validation.
const (
	cardNormal  = "4242424242424242"
	cardDecline = "4242424242420000"
	cardTimeout = "4242424242400002"
)

func testConfig() config.AcquirerConfig {
	return config.AcquirerConfig{
		DeclineRate:      0,
		TimeoutRate:      0,
		Timeout:          200 * time.Millisecond,
		MinLatency:       0,
		MaxLatency:       0,
		BreakerThreshold: 5,
		BreakerCooldown:  30 * time.Second,
	}
}

func authorizeRequest(number string) AuthorizeRequest {
	return AuthorizeRequest{
		TransactionID: "txn_1",
		Amount:        150_000,
		Currency:      "VND",
		Card:          Card{Number: number, ExpMonth: 12, ExpYear: 2030, CVV: "123"},
	}
}

func TestMockAuthorizeSucceeds(t *testing.T) {
	m := NewMock(testConfig(), 1)

	res, err := m.Authorize(context.Background(), authorizeRequest(cardNormal))

	require.NoError(t, err)
	assert.Equal(t, "4242", res.CardLast4)
	assert.Equal(t, "visa", res.CardBrand)
	assert.NotEmpty(t, res.Ref)
}

func TestMockTestCardAlwaysDeclines(t *testing.T) {
	m := NewMock(testConfig(), 1)

	for i := 0; i < 20; i++ {
		_, err := m.Authorize(context.Background(), authorizeRequest(cardDecline))

		var declined *domain.DeclinedError
		require.Truef(t, errors.As(err, &declined), "attempt %d: want a decline, got %v", i, err)
		assert.Equal(t, domain.DeclineDoNotHonor, declined.Code)
	}
}

func TestMockTestCardAlwaysTimesOut(t *testing.T) {
	m := NewMock(testConfig(), 1)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := m.Authorize(ctx, authorizeRequest(cardTimeout))

	assert.ErrorIs(t, err, domain.ErrAcquirerUnavailable)
	assert.GreaterOrEqual(t, time.Since(start), 50*time.Millisecond,
		"the timeout card must hang until the context expires")
}

func TestMockDeclineRateIsRespected(t *testing.T) {
	cfg := testConfig()
	cfg.DeclineRate = 1.0 // decline everything
	m := NewMock(cfg, 1)

	_, err := m.Authorize(context.Background(), authorizeRequest(cardNormal))

	var declined *domain.DeclinedError
	require.True(t, errors.As(err, &declined))
	assert.Contains(t, []domain.DeclineCode{
		domain.DeclineInsufficientFunds,
		domain.DeclineCardExpired,
		domain.DeclineDoNotHonor,
		domain.DeclineFraudSuspected,
	}, declined.Code)
}

func TestMockValidatesCard(t *testing.T) {
	m := NewMock(testConfig(), 1)

	tests := []struct {
		name string
		card Card
	}{
		{name: "too short", card: Card{Number: "424242", ExpMonth: 12, ExpYear: 2030, CVV: "123"}},
		{name: "fails luhn", card: Card{Number: "4242424242424243", ExpMonth: 12, ExpYear: 2030, CVV: "123"}},
		{name: "bad month", card: Card{Number: cardNormal, ExpMonth: 13, ExpYear: 2030, CVV: "123"}},
		{name: "two digit year", card: Card{Number: cardNormal, ExpMonth: 12, ExpYear: 30, CVV: "123"}},
		{name: "short cvv", card: Card{Number: cardNormal, ExpMonth: 12, ExpYear: 2030, CVV: "12"}},
		{name: "non digits", card: Card{Number: "4242-4242-4242-4242", ExpMonth: 12, ExpYear: 2030, CVV: "123"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := m.Authorize(context.Background(), AuthorizeRequest{
				Amount: 1000, Currency: "VND", Card: tc.card,
			})
			assert.ErrorIs(t, err, domain.ErrValidation)
		})
	}
}

func TestMockCaptureRefundVoid(t *testing.T) {
	m := NewMock(testConfig(), 1)
	ctx := context.Background()

	capRes, err := m.Capture(ctx, "auth_1", 1000)
	require.NoError(t, err)
	assert.NotEmpty(t, capRes.Ref)

	ref, err := m.Refund(ctx, "auth_1", 1000)
	require.NoError(t, err)
	assert.NotEmpty(t, ref.Ref)

	void, err := m.Void(ctx, "auth_1")
	require.NoError(t, err)
	assert.NotEmpty(t, void.Ref)

	_, err = m.Capture(ctx, "", 1000)
	assert.ErrorIs(t, err, domain.ErrValidation)
	_, err = m.Capture(ctx, "auth_1", 0)
	assert.ErrorIs(t, err, domain.ErrInvalidAmount)
	_, err = m.Refund(ctx, "", 1000)
	assert.ErrorIs(t, err, domain.ErrValidation)
	_, err = m.Void(ctx, "")
	assert.ErrorIs(t, err, domain.ErrValidation)
}

func TestBrandDetection(t *testing.T) {
	assert.Equal(t, "visa", brandOf("4242424242424242"))
	assert.Equal(t, "mastercard", brandOf("5555555555554444"))
	assert.Equal(t, "mastercard", brandOf("2223003122003222"))
	assert.Equal(t, "amex", brandOf("378282246310005"))
	assert.Equal(t, "unknown", brandOf("9999999999999999"))
}

func TestLuhn(t *testing.T) {
	assert.True(t, luhn("4242424242424242"))
	assert.True(t, luhn(cardDecline), "the decline test card must be a valid PAN")
	assert.True(t, luhn(cardTimeout), "the timeout test card must be a valid PAN")
	assert.False(t, luhn("4242424242424243"))
}

// TestGuardedOpensOnRepeatedTimeouts checks the decorator's central rule: a
// run of infrastructure failures opens the circuit, while declines do not.
func TestGuardedOpensOnRepeatedTimeouts(t *testing.T) {
	cfg := testConfig()
	cfg.Timeout = 20 * time.Millisecond
	breaker := NewBreaker(3, time.Minute)
	g := NewGuarded(NewMock(cfg, 1), breaker, cfg.Timeout, nil)

	for i := 0; i < 3; i++ {
		_, err := g.Authorize(context.Background(), authorizeRequest(cardTimeout))
		require.ErrorIs(t, err, domain.ErrAcquirerUnavailable)
	}

	assert.Equal(t, StateOpen, g.State())

	// Now calls fail immediately instead of waiting for the timeout.
	start := time.Now()
	_, err := g.Authorize(context.Background(), authorizeRequest(cardNormal))
	assert.ErrorIs(t, err, domain.ErrAcquirerUnavailable)
	assert.Less(t, time.Since(start), 10*time.Millisecond, "an open circuit must fail fast")
}

func TestGuardedDoesNotOpenOnDeclines(t *testing.T) {
	breaker := NewBreaker(3, time.Minute)
	g := NewGuarded(NewMock(testConfig(), 1), breaker, time.Second, nil)

	for i := 0; i < 10; i++ {
		_, err := g.Authorize(context.Background(), authorizeRequest(cardDecline))
		var declined *domain.DeclinedError
		require.True(t, errors.As(err, &declined))
	}

	assert.Equal(t, StateClosed, g.State(),
		"declines are a healthy processor answering; they must not trip the breaker")
}

func TestGuardedDoesNotOpenOnValidationErrors(t *testing.T) {
	breaker := NewBreaker(2, time.Minute)
	g := NewGuarded(NewMock(testConfig(), 1), breaker, time.Second, nil)

	for i := 0; i < 5; i++ {
		_, err := g.Authorize(context.Background(), AuthorizeRequest{
			Amount: 1000, Currency: "VND",
			Card: Card{Number: "1234", ExpMonth: 12, ExpYear: 2030, CVV: "123"},
		})
		require.ErrorIs(t, err, domain.ErrValidation)
	}

	assert.Equal(t, StateClosed, g.State(), "our own bad input is not the processor's fault")
}
