package event

import "sync"

// Event is a delivered message carrying its originating event name so a single
// subscriber channel can multiplex several subscriptions correctly.
type Event struct {
	Name string
	Data []byte
}

// EventBus is an in-process publish/subscribe broker. It removes the need for
// an external broker (Redis/RabbitMQ) during local development; Core fans out
// events to subscribed extensions over their gRPC streams.
type EventBus struct {
	mu          sync.RWMutex
	subscribers map[string][]chan Event

	// onDrop, when set, is called for every event a subscriber was too slow
	// to receive, so a silently lagging consumer becomes visible.
	onDrop func(eventName string)
}

func NewEventBus() *EventBus {
	return &EventBus{subscribers: make(map[string][]chan Event)}
}

// Subscribe registers ch to receive events published under name.
func (b *EventBus) Subscribe(name string, ch chan Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.subscribers[name] = append(b.subscribers[name], ch)
}

// Unsubscribe removes ch from a named subscription.
func (b *EventBus) Unsubscribe(name string, ch chan Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	subs := b.subscribers[name]
	for i, sub := range subs {
		if sub == ch {
			b.subscribers[name] = append(subs[:i], subs[i+1:]...)
			break
		}
	}
	if len(b.subscribers[name]) == 0 {
		delete(b.subscribers, name)
	}
}

// Publish delivers data to every subscriber of name. Delivery is non-blocking:
// a slow subscriber drops the event rather than stalling the publisher.
func (b *EventBus) Publish(name string, data []byte) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, ch := range b.subscribers[name] {
		select {
		case ch <- Event{Name: name, Data: data}:
		default:
			// Dropping is deliberate: a slow subscriber must not stall the
			// publisher, and this bus is explicitly best-effort — anything
			// that must not be lost belongs on the durable queue.
			//
			// Dropping *silently* is not. A subscriber quietly missing events
			// looks identical to one receiving none, so the drop is reported
			// where a hook is installed.
			if b.onDrop != nil {
				b.onDrop(name)
			}
		}
	}
}

// OnDrop installs a callback invoked whenever an event is dropped because a
// subscriber could not keep up.
func (b *EventBus) OnDrop(fn func(eventName string)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.onDrop = fn
}

// SubscriberCount reports the number of live subscribers for a name (testing).
func (b *EventBus) SubscriberCount(name string) int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subscribers[name])
}
