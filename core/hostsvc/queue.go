package hostsvc

import (
	"context"
	"time"

	"github.com/taills/moduless/core/db"
)

// PGQueue adapts the PostgreSQL queue to the QueueBackend interface.
type PGQueue struct {
	q *db.Queue

	// PollInterval is how long Consume waits before looking again when the
	// topic is empty. A queue backed by polling trades a little latency for
	// not needing a broker; this is the knob for that trade.
	PollInterval time.Duration
}

func NewPGQueue(q *db.Queue) *PGQueue {
	return &PGQueue{q: q, PollInterval: 500 * time.Millisecond}
}

func (p *PGQueue) Enqueue(ctx context.Context, pluginKey, topic string, payload []byte, opts EnqueueOptions) (int64, bool, error) {
	return p.q.Enqueue(ctx, pluginKey, topic, payload, db.EnqueueOptions{
		Delay:       opts.Delay,
		DedupKey:    opts.DedupKey,
		Priority:    opts.Priority,
		MaxAttempts: opts.MaxAttempts,
		TraceID:     opts.TraceID,
	})
}

// Consume delivers messages until the context is cancelled.
//
// Messages are handed over as soon as they are claimed; acknowledgement is a
// separate call from the plugin. That means a plugin that dies mid-handler
// simply lets the visibility timeout lapse and the message is redelivered,
// rather than the delivery being lost with the connection.
func (p *PGQueue) Consume(ctx context.Context, pluginKey, topic string, prefetch int, visibility time.Duration, deliver func(Message) error) error {
	if prefetch <= 0 {
		prefetch = 1
	}
	if visibility <= 0 {
		visibility = 30 * time.Second
	}
	interval := p.PollInterval
	if interval <= 0 {
		interval = 500 * time.Millisecond
	}

	for {
		if err := ctx.Err(); err != nil {
			return nil // the consumer went away; not an error
		}

		msgs, err := p.q.Claim(ctx, pluginKey, topic, prefetch, visibility)
		if err != nil {
			return err
		}
		if len(msgs) == 0 {
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(interval):
			}
			continue
		}

		for _, m := range msgs {
			if err := deliver(Message{
				ID:            m.ID,
				Topic:         m.Topic,
				Payload:       m.Payload,
				Attempt:       m.Attempt,
				MaxAttempts:   m.MaxAttempts,
				ParentTraceID: m.ParentTraceID,
			}); err != nil {
				// The stream broke. Release what we already claimed so the
				// messages become visible again promptly instead of waiting
				// out their visibility timeout.
				for _, pending := range msgs {
					_ = p.q.Nack(context.WithoutCancel(ctx), pluginKey, pending.ID, "consumer disconnected", 0)
				}
				return err
			}
		}
	}
}

func (p *PGQueue) Ack(ctx context.Context, pluginKey string, id int64) error {
	return p.q.Ack(ctx, pluginKey, id)
}

func (p *PGQueue) Nack(ctx context.Context, pluginKey string, id int64, reason string, retryAfter time.Duration) error {
	return p.q.Nack(ctx, pluginKey, id, reason, retryAfter)
}

// StartMaintenance runs the periodic housekeeping a polling queue needs:
// returning messages whose consumer vanished, and clearing out completed rows
// so the table does not grow without bound.
func (p *PGQueue) StartMaintenance(ctx context.Context, interval, retention time.Duration) {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	if retention <= 0 {
		retention = 24 * time.Hour
	}

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if n, err := p.q.ReapExpired(ctx); err == nil && n > 0 {
					logf("queue: returned %d message(s) whose consumer stopped responding", n)
				}
				if _, err := p.q.PurgeDone(ctx, retention); err != nil {
					logf("queue: purge failed: %v", err)
				}
			}
		}
	}()
}

// logf is a seam so this package does not hard-depend on a logger.
var logf = func(string, ...any) {}

// SetLogger installs the log sink for background queue maintenance.
func SetLogger(fn func(format string, args ...any)) {
	if fn != nil {
		logf = fn
	}
}
