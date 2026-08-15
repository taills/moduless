package hostsvc

import (
	"context"
	"log"
	"strings"

	"github.com/taills/moduless/core/event"
)

// BusEvents adapts Core's in-process bus to the EventBackend interface.
//
// This bus is best-effort by design: a subscriber that cannot keep up misses
// events rather than stalling the publisher. Anything that must not be lost
// belongs on the durable queue instead, and the docs say so where plugin
// authors will see it.
type BusEvents struct {
	bus *event.EventBus

	// SubscriberBuffer is how many events a subscriber may fall behind before
	// its events start being dropped.
	SubscriberBuffer int
}

func NewBusEvents(bus *event.EventBus) *BusEvents {
	return &BusEvents{bus: bus, SubscriberBuffer: 64}
}

// eventTopic namespaces an event name with the publishing plugin, so a plugin
// cannot impersonate another's events. Subscribers name the publisher they
// want to hear from, which keeps the cross-plugin boundary explicit rather
// than implied by a shared string.
func eventTopic(sourcePluginKey, name string) string {
	return sourcePluginKey + ":" + name
}

func (b *BusEvents) Publish(pluginKey string, ev Event) error {
	b.bus.Publish(eventTopic(pluginKey, ev.Name), ev.Data)
	return nil
}

// Subscribe delivers events until the context is cancelled.
//
// The event name may be "plugin:event" to hear from another plugin, or a plain
// name to hear the subscriber's own events.
func (b *BusEvents) Subscribe(ctx context.Context, pluginKey, eventName string, deliver func(Event) error) error {
	source := pluginKey
	name := eventName
	if publisher, rest, ok := strings.Cut(eventName, ":"); ok {
		source, name = publisher, rest
	}
	topic := eventTopic(source, name)

	size := b.SubscriberBuffer
	if size <= 0 {
		size = 64
	}
	ch := make(chan event.Event, size)
	b.bus.Subscribe(topic, ch)
	defer b.bus.Unsubscribe(topic, ch)

	for {
		select {
		case <-ctx.Done():
			return nil
		case ev := <-ch:
			if err := deliver(Event{
				Name:            name,
				Data:            ev.Data,
				SourcePluginKey: source,
			}); err != nil {
				return err
			}
		}
	}
}

// LogObservability writes plugin logs and metrics into Core's own log.
//
// Every record carries the plugin key and the trace id, which is what lets a
// plugin's own account of a request be lined up with Core's — the whole point
// of threading the trace id across the process boundary.
type LogObservability struct {
	// MinLevel filters records below it: "debug", "info", "warn", "error".
	MinLevel string
}

func NewLogObservability(minLevel string) *LogObservability {
	if minLevel == "" {
		minLevel = "info"
	}
	return &LogObservability{MinLevel: minLevel}
}

var levelRank = map[string]int{"debug": 0, "info": 1, "warn": 2, "error": 3}

func (o *LogObservability) Log(pluginKey string, rec LogRecord) {
	if levelRank[rec.Level] < levelRank[o.MinLevel] {
		return
	}

	var b strings.Builder
	b.WriteString("[plugin:")
	b.WriteString(pluginKey)
	b.WriteString("] ")
	b.WriteString(strings.ToUpper(rec.Level))
	b.WriteString(" ")
	b.WriteString(rec.Message)
	if rec.TraceID != "" {
		b.WriteString(" trace=")
		b.WriteString(rec.TraceID)
	}
	for k, v := range rec.Fields {
		b.WriteString(" ")
		b.WriteString(k)
		b.WriteString("=")
		b.WriteString(v)
	}
	log.Print(b.String())
}

// RecordMetric currently logs. It is a seam: swapping in Prometheus or OTLP is
// a change here rather than in every plugin.
func (o *LogObservability) RecordMetric(pluginKey string, m Metric) {
	if levelRank["debug"] < levelRank[o.MinLevel] {
		return
	}
	log.Printf("[plugin:%s] metric %s{%v} = %v", pluginKey, m.Name, m.Labels, m.Value)
}
