package experiment

import (
	"errors"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang/mock/gomock"

	"phenix/store"
	"phenix/util/mm"
)

func TestParseNetflowLine(t *testing.T) {
	tests := []struct {
		name string
		line string
		ok   bool
		src  string
		dst  string
	}{
		{
			name: "IPv4 flow",
			line: "0 0 6 10.0.0.1:1234 <-> 10.0.0.2:443 2 2048",
			ok:   true,
			src:  "10.0.0.1",
			dst:  "10.0.0.2",
		},
		{
			name: "IPv6 flow",
			line: "0 0 17 [2001:db8::1]:53 <-> [2001:db8::2]:5353 3 4096",
			ok:   true,
			src:  "2001:db8::1",
			dst:  "2001:db8::2",
		},
		{name: "truncated flow", line: "0 0 6 10.0.0.1:1234", ok: false},
		{name: "wrong delimiter", line: "0 0 6 10.0.0.1:1234 => 10.0.0.2:443 2 2048", ok: false},
		{name: "invalid endpoint", line: "0 0 6 broken <-> 10.0.0.2:443 2 2048", ok: false},
		{name: "non-IP hostname endpoint", line: "0 0 6 example.com:80 <-> 10.0.0.2:443 2 2048", ok: false},
		{name: "out of range port", line: "0 0 6 10.0.0.1:70000 <-> 10.0.0.2:443 2 2048", ok: false},
		{name: "negative port", line: "0 0 6 10.0.0.1:-1 <-> 10.0.0.2:443 2 2048", ok: false},
		{name: "invalid proto", line: "0 0 300 10.0.0.1:1234 <-> 10.0.0.2:443 2 2048", ok: false},
		{name: "negative packet count", line: "0 0 6 10.0.0.1:1234 <-> 10.0.0.2:443 -1 2048", ok: false},
		{name: "invalid count", line: "0 0 6 10.0.0.1:1234 <-> 10.0.0.2:443 packets 2048", ok: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			flow, ok := parseNetflowLine(test.line)
			if ok != test.ok {
				t.Fatalf("parseNetflowLine() ok = %v, want %v", ok, test.ok)
			}
			if !ok {
				return
			}
			if flow["src"] != test.src || flow["dst"] != test.dst {
				t.Fatalf("parseNetflowLine() endpoints = %v -> %v, want %s -> %s", flow["src"], flow["dst"], test.src, test.dst)
			}
		})
	}
}

func TestNetflowPublishDoesNotBlockOnSlowConsumer(t *testing.T) {
	flow := NewNetflow("bridge", &net.UDPConn{})
	consumer := flow.NewChannel("slow")
	for range netflowChannelBufferSize {
		consumer <- map[string]any{"bytes": 1}
	}

	done := make(chan struct{})
	go func() {
		flow.Publish(map[string]any{"bytes": 2})
		close(done)
	}()

	select {
	case <-done:
		// The newest message is deliberately dropped for this slow consumer.
	case <-time.After(time.Second):
		t.Fatal("Publish blocked on a full consumer channel")
	}

	if len(consumer) != netflowChannelBufferSize {
		t.Fatalf("consumer queue length = %d, want %d", len(consumer), netflowChannelBufferSize)
	}
}

// netflowMM records the commands netflow sends to minimega.
type netflowMM struct {
	mm.MM

	mu   sync.Mutex
	cmds []string
}

func (*netflowMM) Headnode() string { return "headnode" }

func (m *netflowMM) MeshSend(_, _, cmd string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.cmds = append(m.cmds, cmd)

	return nil
}

func (m *netflowMM) sent(prefix string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	return slices.ContainsFunc(m.cmds, func(cmd string) bool { return strings.HasPrefix(cmd, prefix) })
}

// netflowRun is the start time of the one experiment a netflow test's store
// holds. Setting it stands in for another process, such as the CLI, starting
// ("" to stop) the experiment.
type netflowRun struct {
	mu        sync.Mutex
	startTime string
}

func (r *netflowRun) set(startTime string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.startTime = startTime
}

// installNetflowTest installs a store holding the experiment name, running on
// a non-phenix bridge since startTime, and a minimega double. Its cleanup drops
// the experiment's netflow and waits for the watchers of every flow in flows.
func installNetflowTest(t *testing.T, name, startTime string, flows *[]*Netflow) (*netflowRun, *netflowMM) {
	t.Helper()

	var (
		ctrl = gomock.NewController(t)
		run  = &netflowRun{startTime: startTime}
		fake = new(netflowMM)
		m    = store.NewMockStore(ctrl)
	)

	m.EXPECT().Get(gomock.Any()).DoAndReturn(func(c *store.Config) error {
		run.mu.Lock()
		defer run.mu.Unlock()

		*c = store.Config{
			Version:  "phenix.sandia.gov/v1",
			Kind:     "Experiment",
			Metadata: store.ConfigMetadata{Name: name},
			Spec:     map[string]any{"experimentName": name, "defaultBridge": "nfbr"},
			Status: map[string]any{
				"startTime": run.startTime,
				"schedules": map[string]any{"h1": "headnode"},
			},
		}

		return nil
	}).AnyTimes()

	originalStore, originalMM := store.DefaultStore, mm.DefaultMM
	store.DefaultStore = m //nolint:reassign // install test double
	mm.DefaultMM = fake    //nolint:reassign // install test double

	t.Cleanup(func() {
		netflowMu.Lock()
		if flow, ok := netflows[name]; ok {
			flow.Close()
			delete(netflows, name)
		}
		netflowMu.Unlock()

		// The watchers read the store, so they must return before it is restored.
		for _, flow := range *flows {
			<-flow.watched
		}

		store.DefaultStore = originalStore //nolint:reassign // restore default
		mm.DefaultMM = originalMM          //nolint:reassign // restore default
	})

	return run, fake
}

// TestStartNetflowReplacesStaleEntry is a regression test: stop hooks run only
// in the process that stops an experiment, so after a CLI stop phenix ui kept
// the netflow of the run that ended, reported it as started, and refused to
// start netflow again once the experiment was started again.
func TestStartNetflowReplacesStaleEntry(t *testing.T) {
	const name = "netflow-stale-test"

	flows := make([]*Netflow, 0, 2)

	run, fake := installNetflowTest(t, name, "2024-01-01T00:00:00Z", &flows)

	if err := StartNetflow(name); err != nil {
		t.Fatalf("starting netflow: %v", err)
	}

	first := GetNetflow(name)
	if first == nil {
		t.Fatal("expected the started netflow")
	}

	flows = append(flows, first)
	ws := first.NewChannel("ws")

	if err := StartNetflow(name); !errors.Is(err, ErrNetflowAlreadyStarted) {
		t.Fatalf("starting netflow twice in one run: got %v, want %v", err, ErrNetflowAlreadyStarted)
	}

	run.set("") // stopped by another process

	if flow := GetNetflow(name); flow != nil {
		t.Fatal("expected no netflow for a stopped experiment")
	}

	if _, open := <-ws; open {
		t.Fatal("expected the websocket channel of the dropped netflow to be closed")
	}

	run.set("2024-01-01T01:00:00Z") // started again

	if err := StartNetflow(name); err != nil {
		t.Fatalf("starting netflow in a new run: %v", err)
	}

	second := GetNetflow(name)
	if second == nil || second == first {
		t.Fatalf("expected a new netflow for the new run, got %p (old %p)", second, first)
	}

	flows = append(flows, second)

	run.set("2024-01-01T02:00:00Z") // restarted while this process looked away

	if err := StopNetflow(name); !errors.Is(err, ErrNetflowNotStarted) {
		t.Fatalf("stopping the netflow of an ended run: got %v, want %v", err, ErrNetflowNotStarted)
	}

	if fake.sent("capture netflow delete") {
		t.Fatal("expected no minimega command for the netflow of an ended run")
	}
}

// TestStartNetflowRefusesDryRun is a regression test: a dry run counts as
// running, so starting its netflow sent capture commands to minimega, which
// created the namespace the dry run never had.
func TestStartNetflowRefusesDryRun(t *testing.T) {
	const name = "netflow-dry-test"

	var flows []*Netflow

	_, fake := installNetflowTest(t, name, "2024-01-01T00:00:00Z-DRYRUN", &flows)

	if err := StartNetflow(name); !errors.Is(err, ErrExperimentNotRunning) {
		t.Fatalf("starting the netflow of a dry run: got %v, want %v", err, ErrExperimentNotRunning)
	}

	if fake.sent("capture netflow") {
		t.Fatal("expected no minimega command for the netflow of a dry run")
	}
}

// TestWatchNetflowDropsFlowOfStoppedExperiment checks that a netflow closes
// its websockets on its own once another process stops the experiment.
func TestWatchNetflowDropsFlowOfStoppedExperiment(t *testing.T) {
	const name = "netflow-watch-test"

	var (
		flow  = NewNetflow("nfbr", &net.UDPConn{})
		flows = []*Netflow{flow}
	)

	run, _ := installNetflowTest(t, name, "2024-01-01T00:00:00Z", &flows)

	flow.startTime = "2024-01-01T00:00:00Z"
	ws := flow.NewChannel("ws")

	netflowMu.Lock()
	netflows[name] = flow
	netflowMu.Unlock()

	go watchNetflow(name, flow, time.Millisecond)

	run.set("")

	select {
	case <-flow.watched:
	case <-time.After(5 * time.Second):
		t.Fatal("the netflow watcher did not drop the netflow of a stopped experiment")
	}

	if _, open := <-ws; open {
		t.Fatal("expected the websocket channel to be closed")
	}

	netflowMu.Lock()
	_, kept := netflows[name]
	netflowMu.Unlock()

	if kept {
		t.Fatal("expected the netflow of a stopped experiment to be dropped")
	}
}
