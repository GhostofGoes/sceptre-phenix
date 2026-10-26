package app

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/activeshadow/structs"
	"github.com/golang/mock/gomock"

	"phenix/store"
	"phenix/types"
	"phenix/util/mm"
	"phenix/util/plog"
	"phenix/util/pubsub"
)

const autotunnelTestStart = "2026-10-19T00:00:00Z"

// autotunnelTestMM answers the C2 check the startup app makes before it asks
// phenix ui for an autotunnel, and counts those checks. Any other call panics.
type autotunnelTestMM struct {
	mm.MM

	c2Err  error
	checks *atomic.Int32
}

func (m autotunnelTestMM) IsC2ClientActive(...mm.C2Option) error {
	if m.checks != nil {
		m.checks.Add(1)
	}

	return m.c2Err
}

// autotunnelTestStore stands in for the store, which holds the experiment
// with its start time until a stop clears it.
type autotunnelTestStore struct {
	mu    sync.Mutex
	start string
}

func (s *autotunnelTestStore) stop() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.start = ""
}

func installAutotunnelTestStore(t *testing.T, exp *types.Experiment) *autotunnelTestStore {
	t.Helper()

	var (
		spec = structs.MapDefaultCase(exp.Spec, structs.CASESNAKE)
		s    = &autotunnelTestStore{start: exp.Status.StartTime()}
		ctrl = gomock.NewController(t)
		m    = store.NewMockStore(ctrl)
	)

	m.EXPECT().Get(gomock.Any()).DoAndReturn(func(c *store.Config) error {
		s.mu.Lock()
		defer s.mu.Unlock()

		if c.Kind != "Experiment" || c.Metadata.Name != exp.Metadata.Name {
			return store.ErrNotExist
		}

		c.Spec = spec
		c.Status = map[string]any{"startTime": s.start}

		return nil
	}).AnyTimes()

	original := store.DefaultStore
	t.Cleanup(func() { store.DefaultStore = original }) //nolint:reassign // restore test double

	store.DefaultStore = m //nolint:reassign // install test double

	return s
}

// shortenAutotunnelWaits makes the startup app check for C2 every few
// milliseconds and give up after timeout, and ends the test's autotunnel
// waits before it restores the defaults.
func shortenAutotunnelWaits(t *testing.T, exp *types.Experiment, timeout time.Duration) {
	t.Helper()

	interval, limit := autotunnelC2Interval, autotunnelC2Timeout

	t.Cleanup(func() { autotunnelC2Interval, autotunnelC2Timeout = interval, limit })

	autotunnelC2Interval, autotunnelC2Timeout = 5*time.Millisecond, timeout

	t.Cleanup(func() { EndAutotunnelWaits(exp.Metadata.Name) })
}

// syncBuffer is a [bytes.Buffer] the log handler may write from the startup
// app's goroutines while the test reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}

func captureAutotunnelLogs(t *testing.T) *syncBuffer {
	t.Helper()

	logs := new(syncBuffer)

	plog.AddHandler(t.Name(), slog.NewTextHandler(logs, nil))
	t.Cleanup(func() { plog.RemoveHandler(t.Name()) })

	return logs
}

func installAutotunnelTestMM(t *testing.T, fake mm.MM) {
	t.Helper()

	original := mm.DefaultMM
	t.Cleanup(func() { mm.DefaultMM = original }) //nolint:reassign // restore test double

	mm.DefaultMM = fake //nolint:reassign // install test double
}

// subscribeAutotunnels stands in for phenix ui's port forward service.
func subscribeAutotunnels(t *testing.T) chan any {
	t.Helper()

	ch := pubsub.Subscribe(CreateTunnelTopic)

	t.Cleanup(func() {
		done := make(chan struct{})

		// keep reading, in case an unexpected Publish is blocked on ch
		go func() {
			for {
				select {
				case <-ch:
				case <-done:
					return
				}
			}
		}()

		pubsub.Unsubscribe(CreateTunnelTopic, ch)
		close(done)
	})

	return ch
}

func newAutotunnelExperiment(t *testing.T, tunnel string) *types.Experiment {
	t.Helper()

	node := newStartupTestNode(t, "web", "linux", true, 1)
	node.AnnotationsF = map[string]any{"phenix/startup-autotunnel": []string{tunnel}}

	exp, _ := newStartupTestExperiment(t, node, nil)
	exp.Status.SetStartTime(autotunnelTestStart)

	return exp
}

// waitForLog waits for the startup app's goroutine to log want.
func waitForLog(t *testing.T, logs *syncBuffer, want string) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)

	for !strings.Contains(logs.String(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("no log containing %q; logs:\n%s", want, logs.String())
		}

		time.Sleep(10 * time.Millisecond)
	}
}

// TestStartupAutotunnelWithoutPhenixUI covers `phenix experiment start`: no
// process will create the autotunnel, so the startup app warns that it was not
// created.
func TestStartupAutotunnelWithoutPhenixUI(t *testing.T) {
	if pubsub.HasSubscribers(CreateTunnelTopic) {
		t.Fatal("something in the test binary already subscribes to autotunnels")
	}

	installAutotunnelTestMM(t, autotunnelTestMM{})

	logs := captureAutotunnelLogs(t)

	if err := (Startup{}).PostStart(context.Background(), newAutotunnelExperiment(t, "8080")); err != nil {
		t.Fatalf("running startup PostStart: %v", err)
	}

	for _, want := range []string{
		"startup autotunnel not created: autotunnels are created only for experiments started through phenix ui",
		"exp=exp1",
		"vm=web",
		"tunnel=8080",
	} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("logs lack %q:\n%s", want, logs.String())
		}
	}
}

func TestStartupAutotunnelPublishesToPhenixUI(t *testing.T) {
	for _, tc := range []struct {
		name   string
		tunnel string
		c2Err  error
		want   *CreateTunnel // nil when nothing may be published
	}{
		{
			name:   "port only",
			tunnel: "8080",
			want: &CreateTunnel{
				Experiment: "exp1", VM: "web", Sport: "8080", Dhost: "127.0.0.1", Dport: "8080", User: "bot",
			},
		},
		{
			name:   "host and port",
			tunnel: "8080:10.0.0.5:80",
			want: &CreateTunnel{
				Experiment: "exp1", VM: "web", Sport: "8080", Dhost: "10.0.0.5", Dport: "80", User: "bot",
			},
		},
		{
			name:   "VNC needs no C2",
			tunnel: "5901:VNC",
			c2Err:  mm.ErrC2ClientNotActive,
			want: &CreateTunnel{
				Experiment: "exp1", VM: "web", Sport: "5901", Dhost: "127.0.0.1", Dport: "VNC", User: "bot",
			},
		},
		{
			name:   "C2 never active",
			tunnel: "8080",
			c2Err:  mm.ErrC2ClientNotActive,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			installAutotunnelTestMM(t, autotunnelTestMM{c2Err: tc.c2Err})

			var (
				exp  = newAutotunnelExperiment(t, tc.tunnel)
				logs = captureAutotunnelLogs(t)
				ch   = subscribeAutotunnels(t)
			)

			installAutotunnelTestStore(t, exp)
			shortenAutotunnelWaits(t, exp, 50*time.Millisecond)

			if err := (Startup{}).PostStart(context.Background(), exp); err != nil {
				t.Fatalf("running startup PostStart: %v", err)
			}

			if tc.want == nil {
				waitForLog(t, logs, "startup autotunnel not created: waiting for the C2 client")

				select {
				case got := <-ch:
					t.Fatalf("published %+v although the C2 client never became active", got)
				default:
				}

				return
			}

			select {
			case got := <-ch:
				if got != *tc.want {
					t.Fatalf("published %+v, want %+v", got, *tc.want)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("no autotunnel published")
			}

			if strings.Contains(logs.String(), "startup autotunnel not created") {
				t.Errorf("unexpected warning:\n%s", logs.String())
			}
		})
	}
}

// TestStartupAutotunnelSkipsDryRuns is a regression test: the startup app
// asked minimega about a dry run's VM, which made minimega create the
// experiment's namespace.
func TestStartupAutotunnelSkipsDryRuns(t *testing.T) {
	for _, tc := range []struct {
		name   string
		dryRun bool
		start  string
	}{
		{name: "dry-run option", dryRun: true, start: autotunnelTestStart},
		{name: "dry-run start time", start: autotunnelTestStart + "-DRYRUN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checks := new(atomic.Int32)
			installAutotunnelTestMM(t, autotunnelTestMM{checks: checks})

			exp := newAutotunnelExperiment(t, "8080")
			exp.Status.SetStartTime(tc.start)

			ch := subscribeAutotunnels(t)

			installAutotunnelTestStore(t, exp)
			shortenAutotunnelWaits(t, exp, time.Minute)

			app := new(Startup)
			if err := app.Init(DryRun(tc.dryRun)); err != nil {
				t.Fatalf("initializing startup app: %v", err)
			}

			if err := app.PostStart(context.Background(), exp); err != nil {
				t.Fatalf("running startup PostStart: %v", err)
			}

			select {
			case got := <-ch:
				t.Fatalf("published %+v for a dry run", got)
			case <-time.After(100 * time.Millisecond):
			}

			if n := checks.Load(); n != 0 {
				t.Fatalf("asked minimega about a dry run's VM %d times", n)
			}
		})
	}
}

// waitForChecks waits until the startup app checked for C2 at least n times.
func waitForChecks(t *testing.T, checks *atomic.Int32, n int32) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)

	for checks.Load() < n {
		if time.Now().After(deadline) {
			t.Fatalf("the startup app checked for C2 %d times, want at least %d", checks.Load(), n)
		}

		time.Sleep(time.Millisecond)
	}
}

// requireNoMoreChecks fails when the startup app checks for C2 again.
func requireNoMoreChecks(t *testing.T, checks *atomic.Int32, ch chan any) {
	t.Helper()

	n := checks.Load()

	select {
	case got := <-ch:
		t.Fatalf("published %+v for a stopped experiment", got)
	case <-time.After(100 * time.Millisecond):
	}

	if got := checks.Load(); got != n {
		t.Fatalf("the startup app checked for C2 %d more times after the stop", got-n)
	}
}

// TestStartupCleanupEndsAutotunnelWaits is a regression test: an autotunnel
// kept waiting for its VM's C2 client after the experiment stopped, and each
// check made minimega create the namespace stop had just cleared.
func TestStartupCleanupEndsAutotunnelWaits(t *testing.T) {
	checks := new(atomic.Int32)
	installAutotunnelTestMM(t, autotunnelTestMM{c2Err: mm.ErrC2ClientNotActive, checks: checks})

	var (
		exp  = newAutotunnelExperiment(t, "8080")
		logs = captureAutotunnelLogs(t)
		ch   = subscribeAutotunnels(t)
	)

	installAutotunnelTestStore(t, exp)
	shortenAutotunnelWaits(t, exp, time.Minute)

	if err := (Startup{}).PostStart(context.Background(), exp); err != nil {
		t.Fatalf("running startup PostStart: %v", err)
	}

	waitForChecks(t, checks, 2)

	// Stop runs the cleanup apps before it clears the namespace, so the
	// wait must have ended when the startup app's cleanup returns.
	if err := (Startup{}).Cleanup(context.Background(), exp); err != nil {
		t.Fatalf("running startup Cleanup: %v", err)
	}

	requireNoMoreChecks(t, checks, ch)
	waitForLog(t, logs, "the experiment stopped before the C2 client registered")
}

// TestStartupAutotunnelWaitEndsWhenStoppedElsewhere covers a stop from
// another process, such as phenix experiment stop, whose cleanup cannot reach
// this process's waits: the stored start time it clears ends them.
func TestStartupAutotunnelWaitEndsWhenStoppedElsewhere(t *testing.T) {
	checks := new(atomic.Int32)
	installAutotunnelTestMM(t, autotunnelTestMM{c2Err: mm.ErrC2ClientNotActive, checks: checks})

	var (
		exp  = newAutotunnelExperiment(t, "8080")
		logs = captureAutotunnelLogs(t)
		ch   = subscribeAutotunnels(t)
	)

	stored := installAutotunnelTestStore(t, exp)
	shortenAutotunnelWaits(t, exp, time.Minute)

	if err := (Startup{}).PostStart(context.Background(), exp); err != nil {
		t.Fatalf("running startup PostStart: %v", err)
	}

	waitForChecks(t, checks, 2)
	stored.stop()
	waitForLog(t, logs, "the experiment stopped before the C2 client registered")
	requireNoMoreChecks(t, checks, ch)
}
