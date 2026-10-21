package forward

import (
	"errors"
	"sync"
	"testing"

	"github.com/activeshadow/structs"
	"github.com/golang/mock/gomock"

	"phenix/store"
	"phenix/types"
	"phenix/util/mm"
	ft "phenix/web/forward/forwardtypes"
)

type tunnelCountingMM struct {
	mm.MM

	mu    sync.Mutex
	calls int
}

func (m *tunnelCountingMM) GetTunnels(...mm.Option) ([]map[string]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.calls++

	return []map[string]string{{"dst": "127.0.0.1"}}, nil
}

// TestDeleteExperimentForwardsKeepsTheReaperOutOfTheExperiment checks the
// stopping hook: once it ran, the reaper no longer asks minimega about the
// stopped experiment's tunnels, which would create its cleared namespace
// again, and keeps the forwards of other experiments.
func TestDeleteExperimentForwardsKeepsTheReaperOutOfTheExperiment(t *testing.T) {
	var (
		stopped = ft.Listener{Exp: "stopped", VM: "vm", DstHost: "127.0.0.1", DstPort: 22, Owner: "admin"}
		running = ft.Listener{Exp: "running", VM: "vm", DstHost: "127.0.0.1", DstPort: 22, Owner: "admin"}
		fake    = new(tunnelCountingMM)
	)

	installExperimentStore(t, map[string]string{"stopped": "", "running": "2024-01-01T00:00:00Z"}, nil)

	originalMM := mm.DefaultMM

	forwardsMu.Lock()
	originalForwards := forwards
	forwards = map[string]ft.Listener{stopped.ToKey(): stopped, running.ToKey(): running}
	mm.DefaultMM = fake //nolint:reassign // install test double
	forwardsMu.Unlock()

	t.Cleanup(func() {
		forwardsMu.Lock()
		defer forwardsMu.Unlock()

		forwards = originalForwards
		mm.DefaultMM = originalMM //nolint:reassign // restore default
	})

	deleteExperimentForwards("stopped")

	forwardsMu.Lock()
	reapForwards()
	_, keptStopped := forwards[stopped.ToKey()]
	_, keptRunning := forwards[running.ToKey()]
	forwardsMu.Unlock()

	if keptStopped || !keptRunning {
		t.Fatalf(
			"expected only the running experiment's forward to remain, kept stopped=%t running=%t",
			keptStopped, keptRunning,
		)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()

	if fake.calls != 1 {
		t.Fatalf("expected the reaper to check only the running experiment's tunnel, got %d checks", fake.calls)
	}
}

// TestStopExperimentForwardsEndsAutotunnelWaitsFirst is a regression test:
// the stopping hooks run before the cleanup apps, whose startup app ended the
// autotunnel waits, so a wait that saw its VM's C2 client while the SCORCH
// hook waited for canceled runs created a forward after the stopping hook had
// dropped the experiment's forwards.
func TestStopExperimentForwardsEndsAutotunnelWaitsFirst(t *testing.T) {
	l := ft.Listener{Exp: "stopping", VM: "vm", DstHost: "127.0.0.1", DstPort: 22, Owner: "admin"}

	forwardsMu.Lock()
	originalForwards, originalEnd := forwards, endAutotunnelWaits
	forwards = map[string]ft.Listener{l.ToKey(): l}
	forwardsMu.Unlock()

	t.Cleanup(func() {
		forwardsMu.Lock()
		defer forwardsMu.Unlock()

		forwards, endAutotunnelWaits = originalForwards, originalEnd
	})

	var (
		ended          []string
		forwardAtEnded bool
	)

	endAutotunnelWaits = func(exp string) {
		forwardsMu.Lock()
		defer forwardsMu.Unlock()

		ended = append(ended, exp)
		_, forwardAtEnded = forwards[l.ToKey()]
	}

	stopExperimentForwards("stopping")

	forwardsMu.Lock()
	_, kept := forwards[l.ToKey()]
	forwardsMu.Unlock()

	if len(ended) != 1 || ended[0] != "stopping" || !forwardAtEnded {
		t.Errorf(
			"autotunnel waits ended for %q, forward still there %t; want them ended for stopping first",
			ended, forwardAtEnded,
		)
	}

	if kept {
		t.Error("the stopping hook kept the experiment's forward")
	}
}

// installExperimentStore installs a store double holding an experiment for
// each name in startTimes, with that start time. Getting a name in errs fails
// with its error, and getting any other name fails with store.ErrNotExist.
func installExperimentStore(t *testing.T, startTimes map[string]string, errs map[string]error) {
	t.Helper()

	ctrl := gomock.NewController(t)
	t.Cleanup(ctrl.Finish)

	m := store.NewMockStore(ctrl)
	m.EXPECT().Get(gomock.Any()).DoAndReturn(func(c *store.Config) error {
		name := c.Metadata.Name

		if err, ok := errs[name]; ok {
			return err
		}

		startTime, ok := startTimes[name]
		if !ok {
			return store.ErrNotExist
		}

		exp := types.NewExperiment(store.ConfigMetadata{Name: name})
		exp.Spec.SetExperimentName(name)
		exp.Status.SetStartTime(startTime)

		*c = store.Config{
			Version:  "phenix.sandia.gov/v1",
			Kind:     "Experiment",
			Metadata: store.ConfigMetadata{Name: name},
			Spec:     structs.MapDefaultCase(exp.Spec, structs.CASESNAKE),
			Status:   structs.MapDefaultCase(exp.Status, structs.CASESNAKE),
		}

		return nil
	}).AnyTimes()

	original := store.DefaultStore
	store.DefaultStore = m //nolint:reassign // install test double

	t.Cleanup(func() {
		store.DefaultStore = original //nolint:reassign // restore default
	})
}

// TestReapForwardsLeavesStoppedExperimentsOutOfMinimega is a regression test:
// a stop in another process, such as the CLI, never ran this process's
// stopping hook, so the reaper went on asking minimega about the stopped
// experiment's tunnels, which created its cleared namespace again.
func TestReapForwardsLeavesStoppedExperimentsOutOfMinimega(t *testing.T) {
	tests := []struct {
		name      string
		startTime string
		missing   bool
		storeErr  error
		wantKept  bool
		wantCalls int
	}{
		{name: "running", startTime: "2024-01-01T00:00:00Z", wantKept: true, wantCalls: 1},
		{name: "stopped", startTime: ""},
		{name: "dry run", startTime: "2024-01-01T00:00:00Z-DRYRUN"},
		{name: "deleted", missing: true},
		{name: "store error", storeErr: errors.New("store unavailable"), wantKept: true, wantCalls: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var (
				l          = ft.Listener{Exp: "exp", VM: "vm", DstHost: "127.0.0.1", DstPort: 22, Owner: "admin"}
				startTimes = map[string]string{"exp": tt.startTime}
				errs       map[string]error
				fake       = new(tunnelCountingMM)
			)

			if tt.missing {
				startTimes = nil
			}

			if tt.storeErr != nil {
				errs = map[string]error{"exp": tt.storeErr}
			}

			installExperimentStore(t, startTimes, errs)

			originalMM := mm.DefaultMM

			forwardsMu.Lock()
			originalForwards := forwards
			forwards = map[string]ft.Listener{l.ToKey(): l}
			mm.DefaultMM = fake //nolint:reassign // install test double
			forwardsMu.Unlock()

			t.Cleanup(func() {
				forwardsMu.Lock()
				defer forwardsMu.Unlock()

				forwards = originalForwards
				mm.DefaultMM = originalMM //nolint:reassign // restore default
			})

			forwardsMu.Lock()
			reapForwards()
			_, kept := forwards[l.ToKey()]
			forwardsMu.Unlock()

			if kept != tt.wantKept {
				t.Errorf("forward kept = %t, want %t", kept, tt.wantKept)
			}

			fake.mu.Lock()
			defer fake.mu.Unlock()

			if fake.calls != tt.wantCalls {
				t.Errorf("reaper asked minimega about %d tunnels, want %d", fake.calls, tt.wantCalls)
			}
		})
	}
}
