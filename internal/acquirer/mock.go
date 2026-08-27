package acquirer

import (
	"context"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/thaivanlam/mini-payment-gateway/internal/config"
	"github.com/thaivanlam/mini-payment-gateway/internal/domain"
	"github.com/thaivanlam/mini-payment-gateway/internal/money"
)

// Test cards, so demos and tests are deterministic without special-casing the
// production path.
const (
	// TestCardDecline: any PAN ending in 0000 is always declined.
	TestCardDecline = "0000"
	// TestCardTimeout: any PAN ending in 0002 never answers.
	TestCardTimeout = "0002"
)

var declineCodes = []domain.DeclineCode{
	domain.DeclineInsufficientFunds,
	domain.DeclineCardExpired,
	domain.DeclineDoNotHonor,
	domain.DeclineFraudSuspected,
}

// Mock simulates a card processor: variable latency, a decline rate, a timeout
// rate, and the test cards above.
type Mock struct {
	cfg config.AcquirerConfig

	mu  sync.Mutex
	rnd *rand.Rand
}

// NewMock builds a Mock. Seed 0 means "seed from the clock".
func NewMock(cfg config.AcquirerConfig, seed int64) *Mock {
	if seed == 0 {
		seed = time.Now().UnixNano()
	}
	return &Mock{cfg: cfg, rnd: rand.New(rand.NewSource(seed))}
}

var _ Acquirer = (*Mock)(nil)

// Authorize places a hold on the card.
func (m *Mock) Authorize(ctx context.Context, req AuthorizeRequest) (AuthorizeResponse, error) {
	if err := validateCard(req.Card); err != nil {
		return AuthorizeResponse{}, err
	}
	if !req.Amount.IsPositive() {
		return AuthorizeResponse{}, fmt.Errorf("%w: amount must be positive", domain.ErrInvalidAmount)
	}
	if err := m.simulate(ctx, last4(req.Card.Number)); err != nil {
		return AuthorizeResponse{}, err
	}
	return AuthorizeResponse{
		Ref:       newRef("auth"),
		CardLast4: last4(req.Card.Number),
		CardBrand: brandOf(req.Card.Number),
	}, nil
}

// Capture settles a previously authorized hold.
func (m *Mock) Capture(ctx context.Context, ref string, amount money.Amount) (CaptureResponse, error) {
	if ref == "" {
		return CaptureResponse{}, fmt.Errorf("%w: missing acquirer reference", domain.ErrValidation)
	}
	if !amount.IsPositive() {
		return CaptureResponse{}, fmt.Errorf("%w: amount must be positive", domain.ErrInvalidAmount)
	}
	// A capture against an accepted authorization is not re-scored by the
	// issuer, so only latency and transport failures are simulated here.
	if err := m.delay(ctx); err != nil {
		return CaptureResponse{}, err
	}
	return CaptureResponse{Ref: newRef("cap")}, nil
}

// Refund returns money to the cardholder.
func (m *Mock) Refund(ctx context.Context, ref string, amount money.Amount) (RefundResponse, error) {
	if ref == "" {
		return RefundResponse{}, fmt.Errorf("%w: missing acquirer reference", domain.ErrValidation)
	}
	if !amount.IsPositive() {
		return RefundResponse{}, fmt.Errorf("%w: amount must be positive", domain.ErrInvalidAmount)
	}
	if err := m.delay(ctx); err != nil {
		return RefundResponse{}, err
	}
	return RefundResponse{Ref: newRef("ref")}, nil
}

// Void releases a hold that will never be captured.
func (m *Mock) Void(ctx context.Context, ref string) (VoidResponse, error) {
	if ref == "" {
		return VoidResponse{}, fmt.Errorf("%w: missing acquirer reference", domain.ErrValidation)
	}
	if err := m.delay(ctx); err != nil {
		return VoidResponse{}, err
	}
	return VoidResponse{Ref: newRef("void")}, nil
}

// simulate applies the test cards first, then the configured random rates.
func (m *Mock) simulate(ctx context.Context, cardLast4 string) error {
	switch cardLast4 {
	case TestCardDecline:
		if err := m.delay(ctx); err != nil {
			return err
		}
		return domain.NewDeclinedError(domain.DeclineDoNotHonor)
	case TestCardTimeout:
		return m.hang(ctx)
	}

	roll := m.float64()
	switch {
	case roll < m.cfg.TimeoutRate:
		return m.hang(ctx)
	case roll < m.cfg.TimeoutRate+m.cfg.DeclineRate:
		if err := m.delay(ctx); err != nil {
			return err
		}
		return domain.NewDeclinedError(m.randomDeclineCode())
	}
	return m.delay(ctx)
}

// delay sleeps for a realistic processing time, respecting cancellation.
func (m *Mock) delay(ctx context.Context) error {
	span := m.cfg.MaxLatency - m.cfg.MinLatency
	wait := m.cfg.MinLatency
	if span > 0 {
		wait += time.Duration(m.int63n(int64(span)))
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("%w: %v", domain.ErrAcquirerUnavailable, ctx.Err())
	}
}

// hang never answers: it models the worst failure mode of a card network,
// where the caller has no idea whether the money moved.
func (m *Mock) hang(ctx context.Context) error {
	<-ctx.Done()
	return fmt.Errorf("%w: acquirer did not respond: %v", domain.ErrAcquirerUnavailable, ctx.Err())
}

func (m *Mock) randomDeclineCode() domain.DeclineCode {
	return declineCodes[m.int63n(int64(len(declineCodes)))]
}

func (m *Mock) float64() float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.rnd.Float64()
}

func (m *Mock) int63n(n int64) int64 {
	if n <= 0 {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.rnd.Int63n(n)
}

func newRef(kind string) string {
	return kind + "_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:20]
}

func last4(pan string) string {
	if len(pan) < 4 {
		return pan
	}
	return pan[len(pan)-4:]
}

// brandOf infers the network from the issuer identification number, the same
// way a terminal does.
func brandOf(pan string) string {
	switch {
	case strings.HasPrefix(pan, "4"):
		return "visa"
	case strings.HasPrefix(pan, "5"), strings.HasPrefix(pan, "2"):
		return "mastercard"
	case strings.HasPrefix(pan, "34"), strings.HasPrefix(pan, "37"):
		return "amex"
	default:
		return "unknown"
	}
}

func validateCard(c Card) error {
	digits := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, c.Number)
	if len(digits) < 12 || len(digits) > 19 || digits != c.Number {
		return domain.Invalid("card.number", "must be 12 to 19 digits")
	}
	if !luhn(digits) {
		return domain.Invalid("card.number", "failed checksum")
	}
	if c.ExpMonth < 1 || c.ExpMonth > 12 {
		return domain.Invalid("card.exp_month", "must be between 1 and 12")
	}
	if c.ExpYear < 2000 || c.ExpYear > 2100 {
		return domain.Invalid("card.exp_year", "must be a four digit year")
	}
	if len(c.CVV) < 3 || len(c.CVV) > 4 {
		return domain.Invalid("card.cvv", "must be 3 or 4 digits")
	}
	return nil
}

// luhn is the mod-10 checksum every card number satisfies. Catching a typo
// here saves a pointless round trip to the processor.
func luhn(digits string) bool {
	sum, alt := 0, false
	for i := len(digits) - 1; i >= 0; i-- {
		n := int(digits[i] - '0')
		if alt {
			n *= 2
			if n > 9 {
				n -= 9
			}
		}
		sum += n
		alt = !alt
	}
	return sum%10 == 0
}
