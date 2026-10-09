package queue

import (
	"context"
	"log/slog"
	"time"

	"squared/internal/metrics"
)

// Outbox is the source of the relay.
type Outbox interface {
	PublishOutbox(ctx context.Context, limit int, publish func(ctx context.Context, payload []byte) error) (int, error)
}

// Relay moves tasks from the database outbox to RabbitMQ. More than one
// relay can run at the same time.
type Relay struct {
	Outbox    Outbox
	Publisher *Publisher
	Log       *slog.Logger
	Interval  time.Duration     // Time between polls when the outbox is empty.
	Metrics   *metrics.Registry // Optional.
}

// Run sends tasks until ctx is done.
func (r *Relay) Run(ctx context.Context) {
	interval := r.Interval
	if interval <= 0 {
		interval = 500 * time.Millisecond
	}
	reg := r.Metrics
	if reg == nil {
		reg = &metrics.Registry{}
	}
	published := reg.NewCounter("squared_outbox_published_total", "Tasks that the relay sent to RabbitMQ.")
	errorsTotal := reg.NewCounter("squared_outbox_errors_total", "Failed relay runs.")
	failures := 0
	for {
		n, err := r.Outbox.PublishOutbox(ctx, 100, r.Publisher.PublishTask)
		published.Add(float64(n))
		if err != nil && ctx.Err() == nil {
			errorsTotal.Inc()
		}
		wait := interval
		switch {
		case err != nil && ctx.Err() == nil:
			failures++
			wait = min(time.Duration(failures)*time.Second, 30*time.Second)
			r.Log.Error("relay outbox", "error", err, "published", n, "retry_in", wait)
		case n == 100:
			wait = 0 // There can be more rows.
			failures = 0
		default:
			failures = 0
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}
