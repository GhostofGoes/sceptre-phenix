package scorch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"

	"phenix/web/broker"
	bt "phenix/web/broker/brokertypes"
	"phenix/web/middleware"
	"phenix/web/rbac"
)

var startBroker sync.Once //nolint:gochecknoglobals // one broker per test binary

// brokerClient connects a websocket client with the given role to the
// broker, as phenix ui's /ws route does.
func brokerClient(t *testing.T, role rbac.Role) *gws.Conn {
	t.Helper()

	startBroker.Do(func() { go broker.Start() })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), middleware.ContextKeyRole, role)

		broker.ServeWS(w, r.WithContext(ctx))
	}))
	t.Cleanup(srv.Close)

	conn, resp, err := gws.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}

	_ = resp.Body.Close()

	t.Cleanup(func() { _ = conn.Close() })

	return conn
}

// brokerActions returns the actions of the apps/scorch messages about name
// that conn receives before the one with the given marker action, but for
// the registration messages.
func brokerActions(conn *gws.Conn, name, marker string) ([]string, error) {
	var actions []string

	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))

	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return actions, fmt.Errorf("reading broker messages: %w", err)
		}

		for line := range bytes.SplitSeq(data, []byte("\n")) {
			var msg bt.Publish

			if err := json.Unmarshal(line, &msg); err != nil || msg.Resource == nil {
				continue
			}

			if msg.Resource.Type != "apps/scorch" || msg.Resource.Name != name {
				continue
			}

			switch msg.Resource.Action {
			case marker:
				return actions, nil
			case "registered": // repeated until every client heard it
			default:
				actions = append(actions, msg.Resource.Action)
			}
		}
	}
}

// waitRegistered waits until each of conns hears a message for everyone
// about name, as the broker registers its clients asynchronously.
func waitRegistered(t *testing.T, name string, conns ...*gws.Conn) {
	t.Helper()

	for _, conn := range conns {
		registered := make(chan error, 1)

		go func() {
			_, err := brokerActions(conn, name, "registered")
			registered <- err
		}()

	wait:
		for {
			broker.Broadcast(nil, bt.NewResource("apps/scorch", name, "registered"), nil)

			select {
			case err := <-registered:
				if err != nil {
					t.Fatal(err)
				}

				break wait
			case <-time.After(50 * time.Millisecond):
			}
		}
	}
}

// TestTerminalBroadcastsNeedExperimentAccess is a regression test: the
// broker sent the creation and exit of every SCORCH web terminal, which
// names its experiment and component, to every connected client.
func TestTerminalBroadcastsNeedExperimentAccess(t *testing.T) {
	const exp = "bcast-exp"

	viewer := brokerClient(t, testRole([2]string{"vms", "list"}))
	watcher := brokerClient(t, testRole([2]string{"experiments", "get"}))

	waitRegistered(t, exp, viewer, watcher)

	term := testTerminal(t, exp)

	if err := KillTerminal(term); err != nil {
		t.Fatal(err)
	}

	broker.Broadcast(nil, bt.NewResource("apps/scorch", exp, "marker"), nil)

	got, err := brokerActions(watcher, exp, "marker")
	if err != nil || strings.Join(got, ",") != "terminal-create,terminal-exit" {
		t.Errorf("a client with the experiment heard %v (%v), want terminal-create and terminal-exit", got, err)
	}

	got, err = brokerActions(viewer, exp, "marker")
	if err != nil || len(got) != 0 {
		t.Errorf("a client without the experiment heard %v (%v), want nothing", got, err)
	}
}

// TestPipelineBroadcastsNeedExperimentAccess is a regression test: the
// broker sent every update of every SCORCH pipeline, which names its
// experiment, run, loop and components and their status, to every connected
// client.
func TestPipelineBroadcastsNeedExperimentAccess(t *testing.T) {
	const name = "pbcast-exp/0/0"

	viewer := brokerClient(t, testRole([2]string{"vms", "list"}))
	watcher := brokerClient(t, testRole([2]string{"experiments", "get"}))

	waitRegistered(t, name, viewer, watcher)

	broadcastPipeline("pbcast-exp", 0, 0, new(pipeline))
	broker.Broadcast(nil, bt.NewResource("apps/scorch", name, "marker"), nil)

	got, err := brokerActions(watcher, name, "marker")
	if err != nil || strings.Join(got, ",") != "pipeline-update" {
		t.Errorf("a client with the experiment heard %v (%v), want pipeline-update", got, err)
	}

	got, err = brokerActions(viewer, name, "marker")
	if err != nil || len(got) != 0 {
		t.Errorf("a client without the experiment heard %v (%v), want nothing", got, err)
	}
}
