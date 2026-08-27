package webhook

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"sync"
	"time"

	"github.com/thaivanlam/mini-payment-gateway/internal/config"
	"github.com/thaivanlam/mini-payment-gateway/internal/domain"
	"github.com/thaivanlam/mini-payment-gateway/internal/repository"
	"github.com/thaivanlam/mini-payment-gateway/internal/secrets"
)

// staleClaimTimeout is how long a delivery may sit in 'delivering' before the
// poller assumes the worker that claimed it died.
const staleClaimTimeout = 5 * time.Minute

// Dispatcher polls the outbox and delivers webhooks.
//
// Shape: one poller goroutine claims due rows in batches and feeds a channel;
// N worker goroutines drain it. Backpressure is natural -- the poller blocks on
// the channel send when every worker is busy, so the process never holds more
// claimed rows than it can actually deliver.
type Dispatcher struct {
	db        *repository.DB
	webhooks  *repository.WebhookRepo
	merchants *repository.MerchantRepo
	cipher    *secrets.Cipher
	client    *http.Client
	cfg       config.WebhookConfig
	log       *slog.Logger

	mu  sync.Mutex
	rnd *rand.Rand
}

// NewDispatcher builds a Dispatcher.
func NewDispatcher(
	db *repository.DB,
	webhooks *repository.WebhookRepo,
	merchants *repository.MerchantRepo,
	cipher *secrets.Cipher,
	cfg config.WebhookConfig,
	log *slog.Logger,
) *Dispatcher {
	if log == nil {
		log = slog.Default()
	}
	return &Dispatcher{
		db:        db,
		webhooks:  webhooks,
		merchants: merchants,
		cipher:    cipher,
		cfg:       cfg,
		log:       log,
		client: &http.Client{
			Timeout: cfg.RequestTimeout,
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 10,
				IdleConnTimeout:     90 * time.Second,
			},
		},
		rnd: rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

// Run blocks until ctx is cancelled, then drains in-flight work.
//
// Shutdown order matters: stop the poller first so no new rows are claimed,
// close the channel so workers finish what they hold, then wait -- bounded by
// ShutdownTimeout. A job already claimed is either completed or released, never
// abandoned half-done.
func (d *Dispatcher) Run(ctx context.Context) error {
	jobs := make(chan *domain.WebhookDelivery)

	var workers sync.WaitGroup
	for i := 0; i < d.cfg.Workers; i++ {
		workers.Add(1)
		go func(id int) {
			defer workers.Done()
			d.worker(ctx, id, jobs)
		}(i)
	}

	var poller sync.WaitGroup
	poller.Add(1)
	go func() {
		defer poller.Done()
		defer close(jobs)
		d.poll(ctx, jobs)
	}()

	<-ctx.Done()
	d.log.Info("webhook dispatcher shutting down", "timeout", d.cfg.ShutdownTimeout.String())

	poller.Wait()

	done := make(chan struct{})
	go func() {
		workers.Wait()
		close(done)
	}()

	select {
	case <-done:
		d.log.Info("webhook dispatcher stopped cleanly")
		return nil
	case <-time.After(d.cfg.ShutdownTimeout):
		return fmt.Errorf("webhook dispatcher: %d workers did not finish within %s",
			d.cfg.Workers, d.cfg.ShutdownTimeout)
	}
}

func (d *Dispatcher) poll(ctx context.Context, jobs chan<- *domain.WebhookDelivery) {
	ticker := time.NewTicker(d.cfg.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		if released, err := d.webhooks.ReleaseStale(ctx, d.db.Pool, staleClaimTimeout, time.Now()); err != nil {
			d.log.ErrorContext(ctx, "release stale webhook claims", "error", err)
		} else if released > 0 {
			d.log.WarnContext(ctx, "released stale webhook claims", "count", released)
		}

		batch, err := d.webhooks.ClaimDue(ctx, d.db.Pool, d.cfg.Workers*2, time.Now())
		if err != nil {
			d.log.ErrorContext(ctx, "claim due webhooks", "error", err)
			continue
		}
		for _, delivery := range batch {
			select {
			case jobs <- delivery:
			case <-ctx.Done():
				// Hand the row back so a restarted worker picks it up at once.
				d.release(delivery)
				return
			}
		}
	}
}

func (d *Dispatcher) worker(ctx context.Context, id int, jobs <-chan *domain.WebhookDelivery) {
	log := d.log.With("worker", id)
	for delivery := range jobs {
		// Detach from ctx: once a job is claimed it runs to completion even
		// while the process is shutting down. The HTTP client timeout keeps
		// that bounded.
		jobCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), d.cfg.RequestTimeout+5*time.Second)
		d.deliver(jobCtx, log, delivery)
		cancel()
	}
}

func (d *Dispatcher) deliver(ctx context.Context, log *slog.Logger, delivery *domain.WebhookDelivery) {
	log = log.With(
		"delivery_id", delivery.ID.String(),
		"event", string(delivery.EventType),
		"attempt", delivery.AttemptCount+1,
	)

	merchant, err := d.merchants.GetByID(ctx, d.db.Pool, delivery.MerchantID)
	if err != nil {
		d.fail(ctx, log, delivery, fmt.Sprintf("load merchant: %v", err))
		return
	}
	if merchant.WebhookURL == "" {
		// Nothing to deliver to. Park it instead of retrying six times.
		delivery.AttemptCount++
		delivery.Status = domain.DeliveryDead
		delivery.LastError = "merchant has no webhook_url configured"
		delivery.UpdatedAt = time.Now()
		d.persist(ctx, log, delivery)
		return
	}

	secret, err := d.cipher.Decrypt(merchant.WebhookSecretEnc)
	if err != nil {
		d.fail(ctx, log, delivery, fmt.Sprintf("decrypt webhook secret: %v", err))
		return
	}

	status, err := d.post(ctx, merchant.WebhookURL, secret, delivery)
	if err != nil {
		d.fail(ctx, log, delivery, err.Error())
		return
	}

	delivery.RecordSuccess(time.Now())
	d.persist(ctx, log, delivery)
	log.InfoContext(ctx, "webhook delivered", "status", status)
}

func (d *Dispatcher) post(ctx context.Context, url, secret string, delivery *domain.WebhookDelivery) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(delivery.Payload))
	if err != nil {
		return 0, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "mini-payment-gateway/1.0")
	req.Header.Set("X-Webhook-Id", delivery.ID.String())
	req.Header.Set("X-Event-Type", string(delivery.EventType))
	req.Header.Set(SignatureHeader, Sign(secret, time.Now(), delivery.Payload))

	resp, err := d.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("post webhook: %w", err)
	}
	defer func() {
		// Drain before closing so the connection can be reused.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return resp.StatusCode, fmt.Errorf("endpoint returned %d", resp.StatusCode)
	}
	return resp.StatusCode, nil
}

func (d *Dispatcher) fail(ctx context.Context, log *slog.Logger, delivery *domain.WebhookDelivery, reason string) {
	d.mu.Lock()
	delivery.RecordFailure(reason, d.cfg.MaxAttempts, time.Now(), d.rnd)
	d.mu.Unlock()

	d.persist(ctx, log, delivery)
	if delivery.Status == domain.DeliveryDead {
		log.ErrorContext(ctx, "webhook delivery dead", "reason", reason, "attempts", delivery.AttemptCount)
		return
	}
	log.WarnContext(ctx, "webhook delivery failed, will retry",
		"reason", reason, "next_attempt_at", delivery.NextAttemptAt.Format(time.RFC3339))
}

func (d *Dispatcher) persist(ctx context.Context, log *slog.Logger, delivery *domain.WebhookDelivery) {
	if err := d.webhooks.UpdateAttempt(ctx, d.db.Pool, delivery); err != nil && !errors.Is(err, domain.ErrNotFound) {
		log.ErrorContext(ctx, "persist webhook attempt", "error", err)
	}
}

// release puts a claimed delivery back on the queue during shutdown.
func (d *Dispatcher) release(delivery *domain.WebhookDelivery) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	delivery.Status = domain.DeliveryFailed
	delivery.NextAttemptAt = time.Now()
	if err := d.webhooks.UpdateAttempt(ctx, d.db.Pool, delivery); err != nil {
		d.log.Error("release claimed webhook", "delivery_id", delivery.ID.String(), "error", err)
	}
}
