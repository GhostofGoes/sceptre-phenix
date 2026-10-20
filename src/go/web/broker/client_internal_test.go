package broker

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang/mock/gomock"
	"github.com/gorilla/websocket"

	"phenix/store"
	"phenix/util/plog"
	webcache "phenix/web/cache"
)

// TestScreenshotTickerForgetsStoppedExperiments is a regression test: a
// client still showing the VMs of an experiment that was stopped, or of a dry
// run, logged an error for each of them on every 5 s screenshot tick.
func TestScreenshotTickerForgetsStoppedExperiments(t *testing.T) {
	var reads int

	m := store.NewMockStore(gomock.NewController(t))
	m.EXPECT().Get(gomock.Any()).DoAndReturn(func(c *store.Config) error {
		reads++

		*c = store.Config{
			Version:  "phenix.sandia.gov/v1",
			Kind:     "Experiment",
			Metadata: store.ConfigMetadata{Name: c.Metadata.Name},
			Spec:     map[string]any{"experimentName": c.Metadata.Name},
			Status:   map[string]any{"startTime": ""}, // stopped
		}

		return nil
	}).AnyTimes()

	original := store.DefaultStore
	t.Cleanup(func() { store.DefaultStore = original }) //nolint:reassign // restore test double

	store.DefaultStore = m //nolint:reassign // install test double

	var logs bytes.Buffer

	plog.AddHandler(t.Name(), slog.NewTextHandler(&logs, nil))
	t.Cleanup(func() { plog.RemoveHandler(t.Name()) })

	c := &Client{
		publish: make(chan any, 8),
		vms:     []vmScope{{exp: "stopped", name: "vm1"}, {exp: "stopped", name: "vm2"}},
	}

	c.updateScreenshots()
	c.updateScreenshots()

	if strings.Contains(logs.String(), "level=ERROR") {
		t.Errorf("the screenshot ticker logged errors for the stopped experiment:\n%s", logs.String())
	}

	if len(c.vms) != 0 || reads != 1 {
		t.Errorf("after two ticks the client has VMs %v in view and the store was read %d times, want none and 1", c.vms, reads)
	}
}

// TestScreenshotTickerKeepsVMsDuringStops is a regression test: the screenshot
// ticker forgot the VMs of an experiment while phenix ui stopped it, so after
// a stop that failed, the client got no screenshots of the still running
// experiment's VMs until it listed them again.
func TestScreenshotTickerKeepsVMsDuringStops(t *testing.T) {
	const exp = "ticker-stopping"

	if err := webcache.LockExperimentForStopping(exp); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { webcache.UnlockExperiment(exp) })

	c := &Client{publish: make(chan any, 8), vms: []vmScope{{exp: exp, name: "vm1"}}}

	c.updateScreenshots()

	if len(c.vms) != 1 || len(c.publish) != 0 {
		t.Errorf("during the stop the client has VMs %v in view and got %d screenshots, want vm1 and none", c.vms, len(c.publish))
	}
}

// TestVMListOptions is a regression test: the VM list request of a websocket
// client converted sort_asc, page_number and page_size without checking their
// types, so a request without them panicked the read goroutine, which ended
// phenix ui.
func TestVMListOptions(t *testing.T) {
	for _, tc := range []struct {
		payload    string
		sort       string
		asc        bool
		page, size int
	}{
		{`{}`, "", false, 0, 0},
		{`{"sort_column":"name","sort_asc":true,"page_number":2,"page_size":10}`, "name", true, 2, 10},
		{`{"sort_column":null,"page_size":5}`, "", false, 0, 5},
	} {
		var payload map[string]any
		if err := json.Unmarshal([]byte(tc.payload), &payload); err != nil {
			t.Fatal(err)
		}

		sort, asc, page, size, err := vmListOptions(payload)
		if err != nil || sort != tc.sort || asc != tc.asc || page != tc.page || size != tc.size {
			t.Errorf("vmListOptions(%s) = %q, %v, %d, %d, %v; want %q, %v, %d, %d",
				tc.payload, sort, asc, page, size, err, tc.sort, tc.asc, tc.page, tc.size)
		}
	}

	for _, payload := range []string{
		`{"sort_column":1}`,
		`{"sort_asc":"yes"}`,
		`{"page_number":"1"}`,
		`{"page_size":-1}`,
		`{"page_number":1.5}`,
		`{"page_number":1e300}`,
	} {
		var p map[string]any
		if err := json.Unmarshal([]byte(payload), &p); err != nil {
			t.Fatal(err)
		}

		if _, _, _, _, err := vmListOptions(p); err == nil {
			t.Errorf("vmListOptions(%s) accepted it, want an error", payload)
		}
	}
}

// TestScreenshotSizeIsPerClient is a regression test: the screenshot size was
// one package variable that every client's read goroutine wrote and the other
// clients' goroutines read without synchronization, a data race, and one
// client's choice applied to every client.
func TestScreenshotSizeIsPerClient(t *testing.T) {
	a, b := &Client{}, &Client{}

	done := make(chan struct{})

	go func() {
		defer close(done)

		for range 100 {
			a.setScreenshotSize("400")
		}
	}()

	for range 100 {
		_ = a.getScreenshotSize()
	}

	<-done

	if got := a.getScreenshotSize(); got != "400" {
		t.Errorf("client a's screenshot size = %q, want 400", got)
	}

	if got := b.getScreenshotSize(); got != defaultBrokerScreenshotSize {
		t.Errorf("client b's screenshot size = %q, want the default %s", got, defaultBrokerScreenshotSize)
	}
}

// panickingMessage panics when it is marshaled for a client.
type panickingMessage struct{}

func (panickingMessage) MarshalJSON() ([]byte, error) {
	panic("marshaling panickingMessage")
}

// TestClientWritePanicEndsOnlyTheClient is a regression test: only the read
// goroutine of a websocket client recovered from a panic, so a panic while
// publishing to the client, or while updating its screenshots, ended phenix
// ui.
func TestClientWritePanicEndsOnlyTheClient(t *testing.T) {
	var logs bytes.Buffer

	plog.AddHandler(t.Name(), slog.NewTextHandler(&logs, nil))
	t.Cleanup(func() { plog.RemoveHandler(t.Name()) })

	conns := make(chan *websocket.Conn, 1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var up websocket.Upgrader

		if conn, err := up.Upgrade(w, r, nil); err == nil {
			conns <- conn
		}
	}))
	t.Cleanup(srv.Close)

	peer, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}

	_ = resp.Body.Close()

	t.Cleanup(func() { _ = peer.Close() })

	c := &Client{conn: <-conns, publish: make(chan any, 1), done: make(chan struct{})}

	go c.write()

	c.publish <- panickingMessage{}

	select {
	case <-c.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the client was not stopped after a panic while publishing to it")
	}

	if !strings.Contains(logs.String(), "marshaling panickingMessage") {
		t.Errorf("the panic was not logged:\n%s", logs.String())
	}
}
