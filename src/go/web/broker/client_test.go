package broker

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"phenix/web/rbac"
)

func TestUnsubscribeClearsScreenshotVMs(t *testing.T) {
	clients := make(chan *Client, 1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrading connection: %v", err)

			return
		}

		c := NewClient(rbac.Role{}, conn)
		c.vms = []vmScope{{exp: "exp", name: "vm1"}, {exp: "exp", name: "vm2"}}

		go c.read()

		clients <- c
	}))
	defer srv.Close()

	conn, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dialing websocket: %v", err)
	}

	defer func() { _ = resp.Body.Close() }()
	defer func() { _ = conn.Close() }()

	c := <-clients
	defer c.Stop()

	msg := `{"resource": {"type": "experiment/vms", "name": "exp", "action": "unsubscribe"}}`
	if err := conn.WriteMessage(websocket.TextMessage, []byte(msg)); err != nil {
		t.Fatalf("writing unsubscribe message: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)

	for time.Now().Before(deadline) {
		c.vmMu.RLock()
		n := len(c.vms)
		c.vmMu.RUnlock()

		if n == 0 {
			return
		}

		time.Sleep(10 * time.Millisecond)
	}

	t.Fatal("screenshot VMs were not cleared after unsubscribe")
}
