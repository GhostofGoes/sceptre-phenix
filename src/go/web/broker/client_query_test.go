package broker

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	v1 "phenix/types/version/v1"
	bt "phenix/web/broker/brokertypes"
	"phenix/web/rbac"
)

func TestParseVMListQuery(t *testing.T) {
	tests := map[string]struct {
		payload string
		want    vmListQuery
	}{
		"empty payload": {
			payload: `{}`,
			want:    vmListQuery{sortAsc: true},
		},
		"null payload": {
			payload: `null`,
			want:    vmListQuery{sortAsc: true},
		},
		"only filter": {
			payload: `{"filter": "name:foo"}`,
			want:    vmListQuery{filter: "name:foo", sortAsc: true},
		},
		"everything": {
			payload: `{"filter": "x", "show_dnb": true, "sort_column": "name",
				"sort_asc": false, "page_number": 2, "page_size": 10}`,
			want: vmListQuery{
				filter: "x", showDNB: true, sortCol: "name", sortAsc: false, page: 2, size: 10,
			},
		},
		"wrong types": {
			payload: `{"filter": 1, "show_dnb": "yes", "sort_column": true,
				"sort_asc": "no", "page_number": "2", "page_size": "10"}`,
			want: vmListQuery{sortAsc: true},
		},
		"page without size": {
			payload: `{"page_number": 2}`,
			want:    vmListQuery{sortAsc: true},
		},
		"zero page": {
			payload: `{"page_number": 0, "page_size": 10}`,
			want:    vmListQuery{sortAsc: true},
		},
		"negative page": {
			payload: `{"page_number": -1, "page_size": 10}`,
			want:    vmListQuery{sortAsc: true},
		},
		"fractional size": {
			payload: `{"page_number": 1, "page_size": 2.5}`,
			want:    vmListQuery{sortAsc: true},
		},
		"huge size": {
			payload: `{"page_number": 1e300, "page_size": 1e300}`,
			want:    vmListQuery{sortAsc: true},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			var payload map[string]any
			if err := json.Unmarshal([]byte(tc.payload), &payload); err != nil {
				t.Fatal(err)
			}

			if got := parseVMListQuery(payload); got != tc.want {
				t.Fatalf("parseVMListQuery() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestValidScreenshotSize(t *testing.T) {
	for size, want := range map[string]bool{
		"200":       true,
		"1":         true,
		"4096":      true,
		"0":         false,
		"-5":        false,
		"4097":      false,
		"":          false,
		"200 extra": false,
		"12.5":      false,
	} {
		if got := validScreenshotSize(size); got != want {
			t.Errorf("validScreenshotSize(%q) = %v, want %v", size, got, want)
		}
	}
}

// dialTestClient starts a client's read loop on a test WebSocket server and
// returns the server side client and the dialed connection.
func dialTestClient(t *testing.T) (*Client, *websocket.Conn) {
	t.Helper()

	clients := make(chan *Client, 1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrading connection: %v", err)

			return
		}

		// a role that may do nothing, so list requests stop at the RBAC check
		c := NewClient(rbac.Role{Spec: &v1.RoleSpec{}}, conn)

		go c.read()

		clients <- c
	}))
	t.Cleanup(srv.Close)

	conn, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dialing websocket: %v", err)
	}

	t.Cleanup(func() { _ = resp.Body.Close() })
	t.Cleanup(func() { _ = conn.Close() })

	c := <-clients
	t.Cleanup(c.Stop)

	return c, conn
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)

	for time.Now().Before(deadline) {
		if cond() {
			return
		}

		time.Sleep(10 * time.Millisecond)
	}

	t.Fatal(what)
}

func TestScreenshotSizeIsPerClient(t *testing.T) {
	a, connA := dialTestClient(t)
	b, _ := dialTestClient(t)

	msg := `{"resource": {"type": "metadata/screenshot", "action": "resize"}, "request": {"size": "400"}}`
	if err := connA.WriteMessage(websocket.TextMessage, []byte(msg)); err != nil {
		t.Fatalf("writing resize message: %v", err)
	}

	waitFor(t, "screenshot size was not updated", func() bool { return a.screenshotSize() == "400" })

	if got := b.screenshotSize(); got != defaultBrokerScreenshotSize {
		t.Fatalf("other client's screenshot size = %q, want %q", got, defaultBrokerScreenshotSize)
	}

	bad := `{"resource": {"type": "metadata/screenshot", "action": "resize"}, "request": {"size": "1; vm kill all"}}`
	if err := connA.WriteMessage(websocket.TextMessage, []byte(bad)); err != nil {
		t.Fatalf("writing resize message: %v", err)
	}

	// the read loop handles messages in order, so once a later message is
	// handled the bad size has been seen too
	a.vmMu.Lock()
	a.vms = []vmScope{{exp: "exp", name: "vm1"}}
	a.vmMu.Unlock()

	unsub := `{"resource": {"type": "experiment/vms", "name": "exp", "action": "unsubscribe"}}`
	if err := connA.WriteMessage(websocket.TextMessage, []byte(unsub)); err != nil {
		t.Fatalf("writing unsubscribe message: %v", err)
	}

	waitFor(t, "unsubscribe was not handled", func() bool {
		a.vmMu.RLock()
		defer a.vmMu.RUnlock()

		return len(a.vms) == 0
	})

	if got := a.screenshotSize(); got != "400" {
		t.Fatalf("screenshot size = %q after invalid resize, want %q", got, "400")
	}
}

func TestReadSurvivesMalformedRequests(t *testing.T) {
	c, conn := dialTestClient(t)

	c.vmMu.Lock()
	c.vms = []vmScope{{exp: "exp", name: "vm1"}}
	c.vmMu.Unlock()

	for _, msg := range []string{
		`{}`,
		`{"resource": null}`,
		`{"resource": {"type": "experiment/vms", "name": "exp", "action": "list"}}`,
		`{"resource": {"type": "experiment/vms", "name": "exp", "action": "list"}, "request": {}}`,
		`{"resource": {"type": "metadata/screenshot"}, "request": {"size": 5}}`,
		`{"resource": {"type": "experiment/vms", "name": "exp", "action": "unsubscribe"}}`,
	} {
		if err := conn.WriteMessage(websocket.TextMessage, []byte(msg)); err != nil {
			t.Fatalf("writing message %s: %v", msg, err)
		}
	}

	waitFor(t, "read loop stopped before handling the last message", func() bool {
		c.vmMu.RLock()
		defer c.vmMu.RUnlock()

		return len(c.vms) == 0
	})
}

func TestSendGivesUpOnceClientIsDone(t *testing.T) {
	c := &Client{
		publish: make(chan any), // unbuffered and never drained
		done:    make(chan struct{}),
	}

	sent := make(chan bool)

	go func() {
		sent <- c.send(bt.Publish{})
	}()

	close(c.done)

	select {
	case ok := <-sent:
		if ok {
			t.Fatal("send reported success with no reader")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("send blocked after client was done")
	}
}
