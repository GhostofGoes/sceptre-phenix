package pubsub

import (
	"testing"
	"time"
)

func TestHasSubscribers(t *testing.T) {
	const topic = "test-has-subscribers"

	if HasSubscribers(topic) {
		t.Fatal("HasSubscribers = true before anything subscribed")
	}

	ch := Subscribe(topic)

	if !HasSubscribers(topic) {
		t.Fatal("HasSubscribers = false after Subscribe")
	}

	go Publish(topic, "msg")

	select {
	case got := <-ch:
		if got != "msg" {
			t.Fatalf("received %v, want msg", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("subscriber did not receive the published message")
	}

	Unsubscribe(topic, ch)

	if HasSubscribers(topic) {
		t.Fatal("HasSubscribers = true after Unsubscribe")
	}

	// With no subscriber left, Publish must not block.
	Publish(topic, "dropped")
}
