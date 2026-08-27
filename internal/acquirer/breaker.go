package acquirer

import (
	"errors"
	"sync"
	"time"
)

// ErrCircuitOpen is returned while the breaker refuses calls.
var ErrCircuitOpen = errors.New("circuit breaker is open")

// State is the breaker position.
type State string

const (
	StateClosed   State = "closed"
	StateOpen     State = "open"
	StateHalfOpen State = "half_open"
)

// Breaker is a small three-state circuit breaker.
//
// In "closed" state calls pass, and `threshold` consecutive failures open it.
// In "open" state calls fail fast, and after `cooldown` it becomes half-open.
// In "half_open" state exactly one probe call is allowed through: success
// closes the breaker, failure opens it again for another cooldown.
//
// It is deliberately hand-written and tiny: a dependency here would hide the
// one behaviour worth understanding, which is that a downstream outage must
// not turn into a queue of hanging goroutines holding database connections.
type Breaker struct {
	threshold int
	cooldown  time.Duration
	now       func() time.Time

	mu          sync.Mutex
	state       State
	failures    int
	openedAt    time.Time
	probeInFlgt bool
}

// NewBreaker builds a closed breaker.
func NewBreaker(threshold int, cooldown time.Duration) *Breaker {
	if threshold < 1 {
		threshold = 1
	}
	return &Breaker{
		threshold: threshold,
		cooldown:  cooldown,
		now:       time.Now,
		state:     StateClosed,
	}
}

// State reports the current position, transitioning open -> half_open when the
// cooldown has elapsed.
func (b *Breaker) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refreshLocked()
	return b.state
}

// Allow reports whether a call may proceed, and reserves the half-open probe.
func (b *Breaker) Allow() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refreshLocked()

	switch b.state {
	case StateOpen:
		return ErrCircuitOpen
	case StateHalfOpen:
		if b.probeInFlgt {
			return ErrCircuitOpen
		}
		b.probeInFlgt = true
		return nil
	default:
		return nil
	}
}

// Success records a healthy call.
func (b *Breaker) Success() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures = 0
	b.probeInFlgt = false
	b.state = StateClosed
}

// Failure records a call that failed for infrastructure reasons. A card
// decline is a valid answer from a healthy processor and must not be reported
// here, or a run of declines would take the gateway down.
func (b *Breaker) Failure() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.probeInFlgt = false
	b.failures++
	if b.state == StateHalfOpen || b.failures >= b.threshold {
		b.state = StateOpen
		b.openedAt = b.now()
	}
}

func (b *Breaker) refreshLocked() {
	if b.state == StateOpen && b.now().Sub(b.openedAt) >= b.cooldown {
		b.state = StateHalfOpen
		b.probeInFlgt = false
	}
}
