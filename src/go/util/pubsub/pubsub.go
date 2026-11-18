package pubsub

import (
	"slices"
	"sync"
)

var (
	mu   sync.RWMutex                  //nolint:gochecknoglobals // package level subscription registry
	subs = make(map[string][]chan any) //nolint:gochecknoglobals // package level subscription registry
)

func Subscribe(topic string) chan any {
	mu.Lock()
	defer mu.Unlock()

	ch := make(chan any)

	subs[topic] = append(subs[topic], ch)

	return ch
}

// Unsubscribe removes a channel Subscribe returned for topic. The subscriber
// must keep reading the channel until Unsubscribe returns, since a Publish in
// progress may be blocked sending to it.
func Unsubscribe(topic string, ch chan any) {
	mu.Lock()
	defer mu.Unlock()

	subs[topic] = slices.DeleteFunc(subs[topic], func(c chan any) bool { return c == ch })
}

// HasSubscribers reports whether anything in this process subscribed to topic,
// that is, whether a message published to it is received at all.
func HasSubscribers(topic string) bool {
	mu.RLock()
	defer mu.RUnlock()

	return len(subs[topic]) > 0
}

func Publish(topic string, msg any) {
	mu.RLock()
	defer mu.RUnlock()

	for _, ch := range subs[topic] {
		ch <- msg
	}
}
