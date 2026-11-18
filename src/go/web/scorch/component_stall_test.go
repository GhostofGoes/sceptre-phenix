package scorch

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"golang.org/x/net/websocket"

	"phenix/web/middleware"
	"phenix/web/weberror"
)

// stallBound bounds how long a component update or an output request may
// take while a stream client has stopped reading.
const stallBound = 2 * time.Second

// stallOutput is more output than the socket buffers between phenix and a
// client hold, so writing it to a client that does not read stalls.
var stallOutput = bytes.Repeat([]byte("0123456789abcdef"), 1<<20) //nolint:gochecknoglobals // 16 MiB, shared

// streamTestServer serves the component output routes as web/server.go does,
// for a role allowed everything. ended receives each time a stream handler
// returns, which is when the server closes that client's connection.
func streamTestServer(t *testing.T) (*httptest.Server, <-chan struct{}) {
	t.Helper()

	var (
		ended = make(chan struct{}, 16)
		admin = testRole([2]string{"*", "*"}, [2]string{"*/*", "*"})
	)

	router := mux.NewRouter()
	api := router.PathPrefix("/api/v1").Subrouter()

	api.Use(func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := context.WithValue(r.Context(), middleware.ContextKeyRole, admin)
			ctx = context.WithValue(ctx, middleware.ContextKeyUser, "admin")

			h.ServeHTTP(w, r.WithContext(ctx))
		})
	})

	api.Handle(
		"/experiments/{name}/scorch/components/{run}/{loop}/{stage}/{cmp}",
		weberror.ErrorHandler(GetComponentOutput),
	).Methods("GET")
	api.HandleFunc(
		"/experiments/{name}/scorch/components/{run}/{loop}/{stage}/{cmp}/ws",
		func(w http.ResponseWriter, r *http.Request) {
			StreamComponentOutput(w, r)

			ended <- struct{}{}
		},
	).Methods("GET")

	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)

	return srv, ended
}

// stalledClient opens the output stream of the component cmp of exp as a
// client with a small receive buffer that reads the start of the first
// frame, showing phenix has begun writing to it, and nothing after, so
// writes to it stall once the socket buffers are full. It returns the
// client's connection.
func stalledClient(t *testing.T, srv *httptest.Server, exp string) net.Conn {
	t.Helper()

	addr := strings.TrimPrefix(srv.URL, "http://")

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = conn.Close() })

	if err := conn.(*net.TCPConn).SetReadBuffer(4096); err != nil { //nolint:forcetypeassert // TCP dial
		t.Fatal(err)
	}

	cfg, err := websocket.NewConfig("ws://"+addr+"/api/v1/experiments/"+exp+"/scorch/components/0/0/start/cmp/ws", srv.URL)
	if err != nil {
		t.Fatal(err)
	}

	stream, err := websocket.NewClient(cfg, conn)
	if err != nil {
		t.Fatalf("opening the component output stream: %v", err)
	}

	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))

	if _, err := stream.Read(make([]byte, 64)); err != nil {
		t.Fatalf("reading the first frame of the stream: %v", err)
	}

	return conn
}

// within fails the test unless f returns within stallBound.
func within(t *testing.T, what string, f func()) {
	t.Helper()

	done := make(chan struct{})

	go func() {
		defer close(done)

		f()
	}()

	select {
	case <-done:
	case <-time.After(stallBound):
		t.Errorf("%s did not return within %s", what, stallBound)
	}
}

// assertNotStalled checks that, while a stream client has stopped reading,
// an update of an unrelated experiment's component and a request for its
// output both return within stallBound.
func assertNotStalled(t *testing.T, srv *httptest.Server) {
	t.Helper()

	within(t, "UpdateComponent of an unrelated experiment", func() {
		UpdateComponent(ComponentUpdate{
			Exp: "unrelated-exp", CmpName: "cmp", CmpType: "user", Stage: "start", Status: statusStart,
		})
	})

	client := &http.Client{Timeout: stallBound}

	resp, err := client.Get(srv.URL + "/api/v1/experiments/unrelated-exp/scorch/components/0/0/start/cmp")
	if err != nil {
		t.Errorf("GET component output of an unrelated experiment: %v", err)

		return
	}

	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET component output of an unrelated experiment: status %d, want 200", resp.StatusCode)
	}
}

// assertStreamEnded checks that the stream of the stalled client ended, and
// so its connection closed, soon after streamWriteWait.
func assertStreamEnded(t *testing.T, ended <-chan struct{}) {
	t.Helper()

	select {
	case <-ended:
	case <-time.After(streamWriteWait + stallBound):
		t.Errorf("the stream of the stalled client did not end within %s", streamWriteWait+stallBound)
	}
}

// updateComponent sends the update of exp's component cmp with status and
// output, within stallBound.
func updateComponent(t *testing.T, exp, status string, output []byte) {
	t.Helper()

	within(t, "UpdateComponent "+status+" of "+exp, func() {
		UpdateComponent(ComponentUpdate{
			Exp: exp, CmpName: "cmp", CmpType: "user", Stage: "start", Status: status, Output: output,
		})
	})
}

// TestStalledStreamOfCompletedComponent is a regression test: the component
// processor, the one goroutine every SCORCH run's component updates and
// output requests go through, wrote the whole output of a completed
// component to a client opening its stream, so a client that stopped
// reading held up every SCORCH run of every experiment.
func TestStalledStreamOfCompletedComponent(t *testing.T) {
	useProcessors()

	srv, ended := streamTestServer(t)

	updateComponent(t, "stall-completed", statusStart, nil)
	updateComponent(t, "stall-completed", statusRunning, stallOutput)
	updateComponent(t, "stall-completed", statusSuccess, nil)

	stalledClient(t, srv, "stall-completed")
	assertNotStalled(t, srv)
	assertStreamEnded(t, ended)
}

// TestStalledStreamOfRunningComponent is TestStalledStreamOfCompletedComponent
// for the output a running component has printed so far, which the web UI
// streams.
func TestStalledStreamOfRunningComponent(t *testing.T) {
	useProcessors()

	srv, ended := streamTestServer(t)

	updateComponent(t, "stall-running", statusStart, nil)
	updateComponent(t, "stall-running", statusRunning, stallOutput)

	stalledClient(t, srv, "stall-running")
	assertNotStalled(t, srv)
	assertStreamEnded(t, ended)

	updateComponent(t, "stall-running", statusSuccess, nil)
}

// TestStalledStreamOfLiveOutput is a regression test: the component
// processor wrote each component update's output, and the notice that the
// component finished, to every client streaming it, so one client that
// stopped reading held up the update and every SCORCH run after it.
func TestStalledStreamOfLiveOutput(t *testing.T) {
	useProcessors()

	srv, ended := streamTestServer(t)

	updateComponent(t, "stall-live", statusStart, nil)
	updateComponent(t, "stall-live", statusRunning, []byte("first\n"))

	// The client reads the replay of the first line, so it is subscribed
	// and the next update is live output, not part of a replay.
	stalledClient(t, srv, "stall-live")

	updateComponent(t, "stall-live", statusRunning, stallOutput)
	assertNotStalled(t, srv)
	updateComponent(t, "stall-live", statusSuccess, nil)
	assertStreamEnded(t, ended)
}

// TestStreamEndsWhenClientLeaves is a regression test: a stream ended only
// when a write to its client failed or its component finished, so a client
// that left a running component that printed nothing more kept its stream
// goroutine and connection until the component finished.
func TestStreamEndsWhenClientLeaves(t *testing.T) {
	useProcessors()

	srv, ended := streamTestServer(t)

	updateComponent(t, "left-idle", statusStart, nil)
	updateComponent(t, "left-idle", statusRunning, []byte("first\n"))

	conn := stalledClient(t, srv, "left-idle")
	_ = conn.Close()

	select {
	case <-ended:
	case <-time.After(stallBound):
		t.Errorf("the stream of the client that left did not end within %s", stallBound)
	}

	updateComponent(t, "left-idle", statusSuccess, nil)
}
