package event

import (
	"testing"
	"time"
)

func TestEventBusPublishSubscribe(t *testing.T) {
	bus := NewEventBus()
	ch := make(chan Event, 1)
	bus.Subscribe("user.created", ch)

	bus.Publish("user.created", []byte("payload"))

	select {
	case e := <-ch:
		if e.Name != "user.created" || string(e.Data) != "payload" {
			t.Fatalf("unexpected event: %+v", e)
		}
	case <-time.After(time.Second):
		t.Fatal("did not receive published event")
	}
}

func TestEventBusMultiEventTagging(t *testing.T) {
	bus := NewEventBus()
	ch := make(chan Event, 2)
	bus.Subscribe("a", ch)
	bus.Subscribe("b", ch)

	bus.Publish("a", []byte("1"))
	bus.Publish("b", []byte("2"))

	got := map[string]string{}
	for i := 0; i < 2; i++ {
		select {
		case e := <-ch:
			got[e.Name] = string(e.Data)
		case <-time.After(time.Second):
			t.Fatal("missing event")
		}
	}
	if got["a"] != "1" || got["b"] != "2" {
		t.Fatalf("events mis-tagged: %v", got)
	}
}

func TestEventBusUnsubscribe(t *testing.T) {
	bus := NewEventBus()
	ch := make(chan Event, 1)
	bus.Subscribe("x", ch)
	if bus.SubscriberCount("x") != 1 {
		t.Fatal("expected 1 subscriber")
	}
	bus.Unsubscribe("x", ch)
	if bus.SubscriberCount("x") != 0 {
		t.Fatal("expected 0 subscribers after unsubscribe")
	}
}

func TestEventBusNonBlockingPublish(t *testing.T) {
	bus := NewEventBus()
	ch := make(chan Event) // unbuffered, no reader
	bus.Subscribe("y", ch)
	done := make(chan struct{})
	go func() {
		bus.Publish("y", []byte("z")) // must not block
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Publish blocked on slow subscriber")
	}
}
