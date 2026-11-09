//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// wsMessage is one message phenix publishes on /api/v1/ws.
type wsMessage struct {
	Resource struct {
		Type   string `json:"type"`
		Name   string `json:"name"`
		Action string `json:"action"`
	} `json:"resource"`
	Result json.RawMessage `json:"result"`
}

// wsRecorder records every message phenix publishes on the websocket from
// shortly after it connects until the test ends or the socket closes. phenix
// registers the client with its broker only after the upgrade, so a message
// published at that instant can be missed.
type wsRecorder struct {
	mu   sync.Mutex
	msgs []wsMessage

	// err is why recording stopped (the broker drops a client that falls
	// behind); undecoded counts the lines that were not a message.
	err       error
	undecoded int
	lastBad   string
}

// recordWS connects to phenix's websocket and records its messages.
func recordWS(t *testing.T) *wsRecorder {
	t.Helper()

	conn, _, err := websocket.DefaultDialer.Dial("ws://"+cfg.uiAddr+"/api/v1/ws", nil)
	if err != nil {
		t.Fatalf("connecting to the phenix websocket: %v", err)
	}

	rec := &wsRecorder{}
	done := make(chan struct{})

	go func() {
		defer close(done)

		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				rec.mu.Lock()
				rec.err = err
				rec.mu.Unlock()

				return
			}

			// phenix's broker writes every message it has queued into one
			// frame, one message per line, as the UI client expects.
			for line := range bytes.SplitSeq(data, []byte{'\n'}) {
				if len(bytes.TrimSpace(line)) == 0 {
					continue
				}

				var m wsMessage

				err := json.Unmarshal(line, &m)

				rec.mu.Lock()

				if err != nil {
					rec.undecoded++
					rec.lastBad = string(line[:min(len(line), 200)])
				} else {
					rec.msgs = append(rec.msgs, m)
				}

				rec.mu.Unlock()
			}
		}
	}()

	t.Cleanup(func() {
		_ = conn.Close()
		<-done
	})

	return rec
}

// messages returns the recorded messages whose resource type and name match.
// An empty argument matches anything.
func (r *wsRecorder) messages(resourceType, name string) []wsMessage {
	r.mu.Lock()
	defer r.mu.Unlock()

	var out []wsMessage

	for _, m := range r.msgs {
		if (resourceType == "" || m.Resource.Type == resourceType) && (name == "" || m.Resource.Name == name) {
			out = append(out, m)
		}
	}

	return out
}

// actions returns the actions of the recorded messages that match.
func (r *wsRecorder) actions(resourceType, name string) []string {
	msgs := r.messages(resourceType, name)
	out := make([]string, 0, len(msgs))

	for _, m := range msgs {
		out = append(out, m.Resource.Action)
	}

	return out
}

// waitAction waits until a message with the given action is recorded, and
// fails at once when the socket closed without one.
func (r *wsRecorder) waitAction(t *testing.T, timeout time.Duration, resourceType, name, action string) wsMessage {
	t.Helper()

	var found wsMessage

	what := "websocket " + resourceType + " " + name + " " + action

	eventually(t, timeout, 200*time.Millisecond, what, func() (bool, string) {
		// Read before the messages: the recorder sets err only after it
		// recorded every message it read, so none can arrive unseen.
		r.mu.Lock()
		err, undecoded, lastBad := r.err, r.undecoded, r.lastBad
		r.mu.Unlock()

		for _, m := range r.messages(resourceType, name) {
			if m.Resource.Action == action {
				found = m

				return true, ""
			}
		}

		detail := "seen " + strings.Join(r.actions(resourceType, name), ",")
		if undecoded > 0 {
			detail += fmt.Sprintf("; %d lines were not messages, the last %q", undecoded, lastBad)
		}

		if err != nil {
			t.Fatalf("the websocket stopped recording before %s: %v (%s)", what, err, detail)
		}

		return false, detail
	})

	return found
}
