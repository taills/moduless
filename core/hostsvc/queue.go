package hostsvc

import (
	"context"
	"fmt"
	"sync"
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

	// MaxDepth bounds how many messages one plugin may have waiting.
	//
	// The queue is a shared table on a shared disk, so a producer in a loop
	// fills the disk for everyone rather than only for itself. The bound is
	// high because a legitimate batch is large; what it stops is the case with
	// no ceiling at all.
	MaxDepth int64

	// depth is what the maintenance loop last measured, per plugin, and the
	// flag Enqueue reads.
	//
	// Checked on a timer rather than per enqueue on purpose: counting rows on
	// every write would make the common path pay for the rare failure, and a
	// producer that has run away is not going to be stopped any better by
	// catching it a few seconds earlier.
	depthMu sync.RWMutex
	depth   map[string]int64
	// dead is what the last maintenance pass found parked. Cached the same
	// way as depth: it is read per plugin for the console and measured once.
	dead map[string]int64
}

// DefaultMaxQueueDepth is the per-plugin ceiling on waiting messages.
const DefaultMaxQueueDepth = 100_000

func NewPGQueue(q *db.Queue) *PGQueue {
	return &PGQueue{
		q:            q,
		PollInterval: 500 * time.Millisecond,
		MaxDepth:     DefaultMaxQueueDepth,
		depth:        map[string]int64{},
		dead:         map[string]int64{},
	}
}

// Depth reports the last measured backlog for a plugin, for the console.
func (p *PGQueue) Depth(pluginKey string) int64 {
	p.depthMu.RLock()
	defer p.depthMu.RUnlock()
	return p.depth[pluginKey]
}

// Dead reports how many messages this plugin has given up on, as of the last
// maintenance pass.
func (p *PGQueue) Dead(pluginKey string) int64 {
	p.depthMu.RLock()
	defer p.depthMu.RUnlock()
	return p.dead[pluginKey]
}

func (p *PGQueue) Enqueue(ctx context.Context, pluginKey, topic string, payload []byte, opts EnqueueOptions) (int64, bool, error) {
	if err := p.checkDepth(pluginKey); err != nil {
		return 0, false, err
	}
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
				if depth, err := p.q.PendingDepth(ctx); err == nil {
					p.depthMu.Lock()
					p.depth = depth
					p.depthMu.Unlock()
					for key, n := range depth {
						if limit := p.maxDepth(); limit > 0 && n >= limit {
							logf("queue: plugin %s has %d messages waiting, at the limit of %d; "+
								"further enqueues are refused until it drains", key, n, limit)
						}
					}
				}
				if dead, err := p.q.DeadDepth(ctx); err == nil {
					p.depthMu.Lock()
					p.dead = dead
					p.depthMu.Unlock()
				}
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

// checkDepth refuses a plugin that is at or over its backlog limit.
func (p *PGQueue) checkDepth(pluginKey string) error {
	limit := p.maxDepth()
	if limit <= 0 {
		return nil
	}
	if depth := p.Depth(pluginKey); depth >= limit {
		return fmt.Errorf("plugin %s has %d messages waiting, at or over the limit of %d; "+
			"the queue is a shared table, so a backlog this size is everyone's problem",
			pluginKey, depth, limit)
	}
	return nil
}

// setDepthForTest overrides the measured backlog. Test-only.
func (p *PGQueue) setDepthForTest(pluginKey string, n int64) {
	p.depthMu.Lock()
	defer p.depthMu.Unlock()
	p.depth[pluginKey] = n
}

func (p *PGQueue) maxDepth() int64 {
	if p.MaxDepth > 0 {
		return p.MaxDepth
	}
	return DefaultMaxQueueDepth
}

// logf is a seam so this package does not hard-depend on a logger.
var logf = func(string, ...any) {}

// SetLogger installs the log sink for background queue maintenance.
func SetLogger(fn func(format string, args ...any)) {
	if fn != nil {
		logf = fn
	}
}
