package acquirer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/thaivanlam/mini-payment-gateway/internal/domain"
	"github.com/thaivanlam/mini-payment-gateway/internal/money"
)

// Guarded decorates an Acquirer with a per-call timeout and a circuit breaker.
//
// The two belong together: the timeout bounds a single slow call, the breaker
// bounds a sustained outage. Without the breaker, every request would still
// pay the full timeout while the processor is down.
type Guarded struct {
	inner   Acquirer
	breaker *Breaker
	timeout time.Duration
	log     *slog.Logger
}

// NewGuarded wraps inner.
func NewGuarded(inner Acquirer, breaker *Breaker, timeout time.Duration, log *slog.Logger) *Guarded {
	if log == nil {
		log = slog.Default()
	}
	return &Guarded{inner: inner, breaker: breaker, timeout: timeout, log: log}
}

var _ Acquirer = (*Guarded)(nil)

// Authorize runs the wrapped call under the breaker.
func (g *Guarded) Authorize(ctx context.Context, req AuthorizeRequest) (AuthorizeResponse, error) {
	var out AuthorizeResponse
	err := g.do(ctx, "authorize", func(ctx context.Context) error {
		var err error
		out, err = g.inner.Authorize(ctx, req)
		return err
	})
	return out, err
}

// Capture runs the wrapped call under the breaker.
func (g *Guarded) Capture(ctx context.Context, ref string, amount money.Amount) (CaptureResponse, error) {
	var out CaptureResponse
	err := g.do(ctx, "capture", func(ctx context.Context) error {
		var err error
		out, err = g.inner.Capture(ctx, ref, amount)
		return err
	})
	return out, err
}

// Refund runs the wrapped call under the breaker.
func (g *Guarded) Refund(ctx context.Context, ref string, amount money.Amount) (RefundResponse, error) {
	var out RefundResponse
	err := g.do(ctx, "refund", func(ctx context.Context) error {
		var err error
		out, err = g.inner.Refund(ctx, ref, amount)
		return err
	})
	return out, err
}

// Void runs the wrapped call under the breaker.
func (g *Guarded) Void(ctx context.Context, ref string) (VoidResponse, error) {
	var out VoidResponse
	err := g.do(ctx, "void", func(ctx context.Context) error {
		var err error
		out, err = g.inner.Void(ctx, ref)
		return err
	})
	return out, err
}

// State exposes the breaker position for /readyz and tests.
func (g *Guarded) State() State { return g.breaker.State() }

func (g *Guarded) do(ctx context.Context, op string, fn func(context.Context) error) error {
	if err := g.breaker.Allow(); err != nil {
		g.log.WarnContext(ctx, "acquirer call rejected by circuit breaker", "op", op)
		return fmt.Errorf("%w: %v", domain.ErrAcquirerUnavailable, err)
	}

	ctx, cancel := context.WithTimeout(ctx, g.timeout)
	defer cancel()

	start := time.Now()
	err := fn(ctx)
	elapsed := time.Since(start)

	var declined *domain.DeclinedError
	switch {
	case err == nil:
		g.breaker.Success()
	case errors.As(err, &declined):
		// A decline is a healthy processor giving a definite answer.
		g.breaker.Success()
	case errors.Is(err, domain.ErrValidation), errors.Is(err, domain.ErrInvalidAmount):
		// Our own bad input; the processor is not at fault.
		g.breaker.Success()
	default:
		g.breaker.Failure()
	}

	g.log.DebugContext(ctx, "acquirer call",
		"op", op,
		"duration_ms", elapsed.Milliseconds(),
		"breaker", string(g.breaker.State()),
		"ok", err == nil,
	)
	return err
}
