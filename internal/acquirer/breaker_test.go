package acquirer

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeClock lets the breaker's cooldown be tested without sleeping.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newTestBreaker(threshold int, cooldown time.Duration) (*Breaker, *fakeClock) {
	clock := &fakeClock{now: time.Date(2026, 8, 27, 0, 0, 0, 0, time.UTC)}
	b := NewBreaker(threshold, cooldown)
	b.now = clock.Now
	return b, clock
}

func TestBreakerStartsClosed(t *testing.T) {
	b, _ := newTestBreaker(5, 30*time.Second)
	assert.Equal(t, StateClosed, b.State())
	assert.NoError(t, b.Allow())
}

func TestBreakerOpensAfterThresholdFailures(t *testing.T) {
	b, _ := newTestBreaker(5, 30*time.Second)

	for i := 0; i < 4; i++ {
		require.NoError(t, b.Allow())
		b.Failure()
		assert.Equal(t, StateClosed, b.State(), "still closed after %d failures", i+1)
	}

	require.NoError(t, b.Allow())
	b.Failure()

	assert.Equal(t, StateOpen, b.State())
	assert.ErrorIs(t, b.Allow(), ErrCircuitOpen, "an open breaker fails fast")
}

func TestBreakerSuccessResetsTheCount(t *testing.T) {
	b, _ := newTestBreaker(3, 30*time.Second)

	b.Failure()
	b.Failure()
	b.Success()
	b.Failure()
	b.Failure()

	assert.Equal(t, StateClosed, b.State(), "a success in between must reset the streak")
}

func TestBreakerHalfOpensAfterCooldown(t *testing.T) {
	b, clock := newTestBreaker(2, 30*time.Second)

	b.Failure()
	b.Failure()
	require.Equal(t, StateOpen, b.State())

	clock.Advance(29 * time.Second)
	assert.Equal(t, StateOpen, b.State(), "still open before the cooldown elapses")

	clock.Advance(2 * time.Second)
	assert.Equal(t, StateHalfOpen, b.State())
}

func TestBreakerHalfOpenAllowsExactlyOneProbe(t *testing.T) {
	b, clock := newTestBreaker(2, 30*time.Second)
	b.Failure()
	b.Failure()
	clock.Advance(31 * time.Second)
	require.Equal(t, StateHalfOpen, b.State())

	assert.NoError(t, b.Allow(), "the first probe passes")
	assert.ErrorIs(t, b.Allow(), ErrCircuitOpen, "a second concurrent probe is refused")
}

func TestBreakerHalfOpenProbeSuccessCloses(t *testing.T) {
	b, clock := newTestBreaker(2, 30*time.Second)
	b.Failure()
	b.Failure()
	clock.Advance(31 * time.Second)

	require.NoError(t, b.Allow())
	b.Success()

	assert.Equal(t, StateClosed, b.State())
	assert.NoError(t, b.Allow())
}

func TestBreakerHalfOpenProbeFailureReopens(t *testing.T) {
	b, clock := newTestBreaker(5, 30*time.Second)
	for i := 0; i < 5; i++ {
		b.Failure()
	}
	clock.Advance(31 * time.Second)
	require.Equal(t, StateHalfOpen, b.State())

	require.NoError(t, b.Allow())
	b.Failure() // a single failure in half-open is enough to reopen

	assert.Equal(t, StateOpen, b.State())
	assert.ErrorIs(t, b.Allow(), ErrCircuitOpen)
}

func TestBreakerIsRaceFree(t *testing.T) {
	b, _ := newTestBreaker(5, time.Millisecond)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = b.Allow()
			if i%2 == 0 {
				b.Success()
			} else {
				b.Failure()
			}
			_ = b.State()
		}(i)
	}
	wg.Wait()
}
