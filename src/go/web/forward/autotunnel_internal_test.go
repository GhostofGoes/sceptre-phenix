package forward

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/activeshadow/structs"
	"github.com/golang/mock/gomock"

	"phenix/app"
	"phenix/store"
	"phenix/util/mm"
	"phenix/util/plog"
	ft "phenix/web/forward/forwardtypes"

	v1 "phenix/types/version/v1"
)

const (
	atExp   = "at-exp"
	atStart = "2026-10-19T00:00:00Z"
)

// autotunnelMM stands in for minimega with one VM, web, whose C2 client is
// active or not, and which holds the tunnels phenix ui created to it.
type autotunnelMM struct {
	mm.MM

	mu        sync.Mutex
	active    bool
	checks    int
	created   int
	tunnels   []map[string]string
	createErr error
	calls     int // every call
}

func (m *autotunnelMM) IsC2ClientActive(...mm.C2Option) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.calls++
	m.checks++

	if m.active {
		return nil
	}

	return mm.ErrC2ClientNotActive
}

func (m *autotunnelMM) GetVMInfo(...mm.Option) mm.VMs {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.calls++

	return mm.VMs{{Name: "web", Host: "head"}}
}

func (m *autotunnelMM) GetVNCEndpoint(...mm.Option) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.calls++

	return "head:5901", nil
}

func (m *autotunnelMM) GetTunnels(...mm.Option) ([]map[string]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.calls++

	if len(m.tunnels) == 0 {
		// What minimega answers once the VM's miniccc client reconnected.
		return nil, errors.New("tunnel has not been initialized")
	}

	return append([]map[string]string(nil), m.tunnels...), nil
}

func (m *autotunnelMM) CreateTunnel(...mm.Option) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.calls++
	m.created++

	if m.createErr != nil {
		return m.createErr
	}

	m.tunnels = append(m.tunnels, map[string]string{"dst": "127.0.0.1", "dst port": "8080", "src port": "50001"})

	return nil
}

// set changes the double under its lock.
func (m *autotunnelMM) set(f func(m *autotunnelMM)) {
	m.mu.Lock()
	defer m.mu.Unlock()

	f(m)
}

// count reads a counter of the double under its lock.
func (m *autotunnelMM) count(f func(m *autotunnelMM) int) int {
	m.mu.Lock()
	defer m.mu.Unlock()

	return f(m)
}

// autotunnelStore stands in for the store, holding experiment atExp, whose VM
// web carries the given autotunnel annotation, with a start time a stop
// clears.
type autotunnelStore struct {
	mu         sync.Mutex
	start      string
	annotation []string
}

func (s *autotunnelStore) setStart(start string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.start = start
}

func (s *autotunnelStore) config() store.Config {
	s.mu.Lock()
	defer s.mu.Unlock()

	spec := &v1.ExperimentSpec{
		ExperimentNameF: atExp,
		TopologyF: &v1.TopologySpec{
			NodesF: []*v1.Node{{
				TypeF:        "VirtualMachine",
				GeneralF:     &v1.General{HostnameF: "web"},
				AnnotationsF: map[string]any{app.AutotunnelAnnotation: s.annotation},
			}},
		},
	}

	return store.Config{
		Version:  "phenix.sandia.gov/v1",
		Kind:     "Experiment",
		Metadata: store.ConfigMetadata{Name: atExp},
		Spec:     structs.MapDefaultCase(spec, structs.CASESNAKE),
		Status:   map[string]any{"startTime": s.start},
	}
}

// autotunnelEnv is the phenix ui state one autotunnel test starts from: no
// forwards and no autotunnels, with every interval shortened to the passes the
// test runs itself.
type autotunnelEnv struct {
	mm    *autotunnelMM
	store *autotunnelStore
	logs  *autotunnelLogs
	now   time.Time
}

func newAutotunnelEnv(t *testing.T, annotation ...string) *autotunnelEnv {
	t.Helper()

	env := &autotunnelEnv{
		mm:    new(autotunnelMM),
		store: &autotunnelStore{start: atStart, annotation: annotation},
		logs:  new(autotunnelLogs),
		now:   time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC),
	}

	ctrl := gomock.NewController(t)
	m := store.NewMockStore(ctrl)

	m.EXPECT().List(gomock.Any()).DoAndReturn(func(...string) (store.Configs, error) {
		return store.Configs{env.store.config()}, nil
	}).AnyTimes()

	m.EXPECT().Get(gomock.Any()).DoAndReturn(func(c *store.Config) error {
		if c.Metadata.Name != atExp {
			return store.ErrNotExist
		}

		*c = env.store.config()

		return nil
	}).AnyTimes()

	originalStore, originalMM := store.DefaultStore, mm.DefaultMM
	store.DefaultStore, mm.DefaultMM = m, env.mm //nolint:reassign // install test doubles

	forwardsMu.Lock()
	originalForwards := forwards
	forwards = make(map[string]ft.Listener)
	forwardsMu.Unlock()

	autotunnelsMu.Lock()
	originalAutotunnels := autotunnels
	autotunnels = make(map[string]*autotunnelState)
	autotunnelsMu.Unlock()

	slow, progress := autotunnelSlowInterval, autotunnelProgressInterval
	autotunnelSlowInterval, autotunnelProgressInterval = 15*time.Second, 5*time.Minute

	plog.AddHandler(t.Name(), slog.NewTextHandler(env.logs, nil))

	t.Cleanup(func() {
		plog.RemoveHandler(t.Name())
		experimentStopped(atExp)

		autotunnelSlowInterval, autotunnelProgressInterval = slow, progress

		autotunnelsMu.Lock()
		autotunnels = originalAutotunnels
		autotunnelsMu.Unlock()

		forwardsMu.Lock()
		forwards = originalForwards
		forwardsMu.Unlock()

		store.DefaultStore, mm.DefaultMM = originalStore, originalMM //nolint:reassign // restore defaults
	})

	return env
}

// pass runs one autotunnel pass d after the previous one.
func (e *autotunnelEnv) pass(d time.Duration) {
	e.now = e.now.Add(d)
	reconcileAutotunnels(e.now)
}

// held reports whether phenix ui holds the forward of the autotunnel to
// 127.0.0.1:dport of web.
func (e *autotunnelEnv) held(dport int) (ft.Listener, bool) {
	forwardsMu.Lock()
	defer forwardsMu.Unlock()

	l, ok := forwards[ft.Listener{
		Exp: atExp, VM: "web", DstHost: "127.0.0.1", DstPort: dport, Owner: app.AutotunnelOwner,
	}.ToKey()]

	return l, ok
}

type autotunnelLogs struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *autotunnelLogs) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p)
}

// count returns how many lines hold every needle.
func (b *autotunnelLogs) count(needles ...string) int {
	b.mu.Lock()
	defer b.mu.Unlock()

	n := 0

	for line := range strings.SplitSeq(b.buf.String(), "\n") {
		all := line != ""

		for _, needle := range needles {
			all = all && strings.Contains(line, needle)
		}

		if all {
			n++
		}
	}

	return n
}

func (b *autotunnelLogs) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}

func (e *autotunnelEnv) requireLogs(t *testing.T, n int, needles ...string) {
	t.Helper()

	if got := e.logs.count(needles...); got != n {
		t.Errorf("%d log lines hold %q, want %d:\n%s", got, needles, n, e.logs.String())
	}
}

// TestAutotunnelWaitsForC2ThenCreates covers a VM whose C2 client registers
// long after the experiment started, such as one delayed until a user starts
// it: phenix ui waits for as long as the experiment runs, logs the wait when
// it begins and then every progress interval, and checks less often once the
// wait is long.
func TestAutotunnelWaitsForC2ThenCreates(t *testing.T) {
	env := newAutotunnelEnv(t, "8080")

	env.pass(0)

	env.requireLogs(t, 1, "startup autotunnel requested", "exp="+atExp, "vm=web", "sport=8080", "dhost=127.0.0.1", "dport=8080")
	env.requireLogs(t, 1, "startup autotunnel waiting for the C2 client")

	// 12 minutes of passes 5 s apart.
	for range 12 * 12 {
		env.pass(5 * time.Second)
	}

	if _, ok := env.held(8080); ok {
		t.Fatal("phenix ui holds a forward to a VM without C2")
	}

	env.requireLogs(t, 1, "startup autotunnel waiting for the C2 client")
	env.requireLogs(t, 2, "startup autotunnel still waiting for the C2 client", "vm=web")
	env.requireLogs(t, 0, "startup autotunnel not created")

	// Every pass for 5 minutes, then every 15 s for 7.
	if got, want := env.mm.count(func(m *autotunnelMM) int { return m.checks }), 1+60+28; got < want-2 || got > want+2 {
		t.Errorf("checked C2 %d times in 12 minutes, want about %d", got, want)
	}

	env.mm.set(func(m *autotunnelMM) { m.active = true })

	env.pass(15 * time.Second)

	l, ok := env.held(8080)
	if !ok {
		t.Fatalf("no forward once the VM's C2 client is active:\n%s", env.logs.String())
	}

	if l.SrcPort != 8080 || l.ClusterHost != "head" || l.ClusterPort < forwardPortBase {
		t.Errorf("forward = %+v", l)
	}

	env.requireLogs(t, 1, "startup autotunnel created", "recreated=false", "adopted=false", "sport=8080")

	// The forward stays; nothing is created again.
	env.pass(5 * time.Second)
	env.pass(5 * time.Second)

	if got := env.mm.count(func(m *autotunnelMM) int { return m.created }); got != 1 {
		t.Errorf("created %d minimega tunnels, want 1", got)
	}
}

// TestAutotunnelIsRecreatedAfterItsTunnelDrops covers every way a VM goes
// down and comes back while the experiment runs: a guest reboot, a restart, a
// redeploy and a restored snapshot all reconnect the VM's miniccc client, and
// minimega gives it a new, empty tunnel, so the reaper drops the forward.
func TestAutotunnelIsRecreatedAfterItsTunnelDrops(t *testing.T) {
	env := newAutotunnelEnv(t, "8080")
	env.mm.set(func(m *autotunnelMM) { m.active = true })

	env.pass(0)

	if _, ok := env.held(8080); !ok {
		t.Fatalf("no forward:\n%s", env.logs.String())
	}

	// The VM reboots: its client and tunnel go, and it does not register
	// again for a while.
	env.mm.set(func(m *autotunnelMM) { m.active, m.tunnels = false, nil })

	env.pass(5 * time.Second)

	if _, ok := env.held(8080); ok {
		t.Fatal("phenix ui still holds the forward whose tunnel is gone")
	}

	env.requireLogs(t, 1, "port forward dropped: its minimega tunnel is gone", "owner=bot", "dport=8080")
	env.requireLogs(t, 1, "startup autotunnel down", "vm=web", "dport=8080")
	env.requireLogs(t, 1, "startup autotunnel waiting for the C2 client")

	env.pass(5 * time.Second)
	env.mm.set(func(m *autotunnelMM) { m.active = true })
	env.pass(5 * time.Second)

	if _, ok := env.held(8080); !ok {
		t.Fatalf("the forward was not created again:\n%s", env.logs.String())
	}

	env.requireLogs(t, 1, "startup autotunnel re-creating")
	env.requireLogs(t, 1, "startup autotunnel created", "recreated=true")

	if got := env.mm.count(func(m *autotunnelMM) int { return m.created }); got != 2 {
		t.Errorf("created %d minimega tunnels, want 2", got)
	}
}

// TestAutotunnelAdoptsAnOpenTunnel covers a phenix ui restart while the
// experiment runs: the new process holds no forwards, and adopts the minimega
// tunnel the earlier one left open instead of opening a second one.
func TestAutotunnelAdoptsAnOpenTunnel(t *testing.T) {
	env := newAutotunnelEnv(t, "8080")
	env.mm.set(func(m *autotunnelMM) {
		m.active = true
		m.tunnels = []map[string]string{{"dst": "127.0.0.1", "dst port": "8080", "src port": "61234"}}
	})

	env.pass(0)

	l, ok := env.held(8080)
	if !ok || l.ClusterPort != 61234 {
		t.Fatalf("forward = %+v (held %t), want it on the open tunnel's port 61234:\n%s", l, ok, env.logs.String())
	}

	if got := env.mm.count(func(m *autotunnelMM) int { return m.created }); got != 0 {
		t.Errorf("created %d minimega tunnels, want the open one adopted", got)
	}

	env.requireLogs(t, 1, "startup autotunnel created", "adopted=true", "clusterPort=61234")
}

// TestAutotunnelOfACLIStartedExperiment is a regression test: only the
// process that started an experiment asked phenix ui for its autotunnels, so
// one started with `phenix experiment start` got none. phenix ui now finds
// them in the store, whoever started the experiment.
func TestAutotunnelOfACLIStartedExperiment(t *testing.T) {
	env := newAutotunnelEnv(t, "8080", "5901:VNC")
	env.mm.set(func(m *autotunnelMM) { m.active = true })

	// Nothing but the store tells phenix ui about the experiment.
	env.pass(0)

	if _, ok := env.held(8080); !ok {
		t.Errorf("no forward to 8080:\n%s", env.logs.String())
	}

	if l, ok := env.held(5900); !ok || !l.QEMU {
		t.Errorf("VNC forward = %+v (held %t)", l, ok)
	}
}

// TestAutotunnelToVNCNeedsNoC2 covers a VNC autotunnel, which phenix ui
// creates while the VM's other autotunnels still wait for its C2 client.
func TestAutotunnelToVNCNeedsNoC2(t *testing.T) {
	env := newAutotunnelEnv(t, "8080", "5901:VNC")

	env.pass(0)

	if _, ok := env.held(5900); !ok {
		t.Errorf("no VNC forward:\n%s", env.logs.String())
	}

	if _, ok := env.held(8080); ok {
		t.Error("phenix ui holds a forward to a VM without C2")
	}

	env.requireLogs(t, 1, "startup autotunnel waiting for the C2 client", "dport=8080")
}

// TestAutotunnelCreationFailuresAreLoggedOnce covers a forward that cannot be
// created: phenix ui tries again every slow interval, and logs the same error
// again only every progress interval.
func TestAutotunnelCreationFailuresAreLoggedOnce(t *testing.T) {
	env := newAutotunnelEnv(t, "8080")
	env.mm.set(func(m *autotunnelMM) { m.active, m.createErr = true, errors.New("no such client") })

	env.pass(0)

	for range 12 {
		env.pass(5 * time.Second)
	}

	env.requireLogs(t, 1, "startup autotunnel not created: creating the port forward", "no such client", "dport=8080")

	if got := env.mm.count(func(m *autotunnelMM) int { return m.created }); got != 5 {
		t.Errorf("tried %d tunnels in a minute, want 5", got)
	}

	env.mm.set(func(m *autotunnelMM) { m.createErr = nil })
	env.pass(15 * time.Second)

	if _, ok := env.held(8080); !ok {
		t.Fatalf("no forward once the tunnel can be created:\n%s", env.logs.String())
	}
}

// TestAutotunnelEndsWithTheStoppingHook covers a stop in phenix ui: the
// stopping hook forgets the experiment's autotunnels, logs each, and no pass
// asks minimega about the experiment again until the stop ends.
func TestAutotunnelEndsWithTheStoppingHook(t *testing.T) {
	env := newAutotunnelEnv(t, "8080", "8081")
	env.pass(0)

	// 8081's forward exists, 8080's waits.
	env.mm.set(func(m *autotunnelMM) {
		m.active = true
		m.createErr = errors.New("connection refused")
	})

	env.pass(5 * time.Second)
	env.mm.set(func(m *autotunnelMM) { m.createErr = nil })

	autotunnelsMu.Lock()
	for _, st := range autotunnels {
		if st.Dport == "8081" {
			st.next = env.now
		}
	}
	autotunnelsMu.Unlock()

	env.pass(0)

	if _, ok := env.held(8081); !ok {
		t.Fatalf("no forward to 8081:\n%s", env.logs.String())
	}

	stopExperimentForwards(atExp)

	env.requireLogs(t, 1, "startup autotunnel removed: the experiment stopped", "dport=8081")
	env.requireLogs(t, 1, "startup autotunnel not created: the experiment stopped before the C2 client registered", "dport=8080")

	calls := env.mm.count(func(m *autotunnelMM) int { return m.calls })

	env.pass(time.Minute)

	if got := env.mm.count(func(m *autotunnelMM) int { return m.calls }); got != calls {
		t.Errorf("a pass called minimega %d times for an experiment being stopped", got-calls)
	}

	if _, ok := env.held(8081); ok {
		t.Error("phenix ui holds a forward of an experiment being stopped")
	}
}

// TestAutotunnelEndsWithAStopElsewhere covers a stop in another process, such
// as `phenix experiment stop`, whose stopping hook cannot reach phenix ui: the
// cleared start time ends the experiment's autotunnels and forwards without a
// further minimega call, and a later start gets them anew.
func TestAutotunnelEndsWithAStopElsewhere(t *testing.T) {
	env := newAutotunnelEnv(t, "8080")
	env.mm.set(func(m *autotunnelMM) { m.active = true })
	env.pass(0)

	env.store.setStart("")

	calls := env.mm.count(func(m *autotunnelMM) int { return m.calls })

	env.pass(5 * time.Second)

	if got := env.mm.count(func(m *autotunnelMM) int { return m.calls }); got != calls {
		t.Errorf("a pass called minimega %d times for a stopped experiment", got-calls)
	}

	if _, ok := env.held(8080); ok {
		t.Error("phenix ui holds a forward of a stopped experiment")
	}

	env.requireLogs(t, 1, "port forward removed: the experiment is not running", "dport=8080")
	env.requireLogs(t, 1, "startup autotunnel removed: the experiment stopped", "dport=8080")

	// The experiment starts again.
	env.mm.set(func(m *autotunnelMM) { m.tunnels = nil })
	env.store.setStart("2026-10-19T01:00:00Z")
	env.pass(5 * time.Second)

	if _, ok := env.held(8080); !ok {
		t.Fatalf("no forward in the new run:\n%s", env.logs.String())
	}

	env.requireLogs(t, 2, "startup autotunnel requested")
	env.requireLogs(t, 2, "startup autotunnel created", "recreated=false")
}

// TestAutotunnelOfAStartingExperimentIsLeftForTheStop covers a stop from
// another process between a pass's read of the store and its C2 check: the
// check reads the start time again first, and asks minimega nothing.
func TestAutotunnelOfAStartingExperimentIsLeftForTheStop(t *testing.T) {
	env := newAutotunnelEnv(t, "8080")

	state := &autotunnelState{
		Autotunnel: app.Autotunnel{Experiment: atExp, VM: "web", Sport: "8080", Dhost: "127.0.0.1", Dport: "8080"},
		start:      atStart,
	}

	env.store.setStart("")

	got := checkAutotunnelC2(map[string][]*autotunnelState{atExp + "/web": {state}})

	if len(got) != 0 {
		t.Errorf("checkAutotunnelC2() = %v, want the stopped experiment left out", got)
	}

	if n := env.mm.count(func(m *autotunnelMM) int { return m.calls }); n != 0 {
		t.Errorf("called minimega %d times for a stopped experiment", n)
	}
}
