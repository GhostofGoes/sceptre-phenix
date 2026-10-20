package forward

import (
	"errors"
	"testing"

	"phenix/api/experiment"
	"phenix/types"
	"phenix/util/mm"
	ft "phenix/web/forward/forwardtypes"
)

// stoppingMM is a minimega double that runs the experiment's stopping hook
// while the forward service queries the VM, and fails the test when a tunnel
// is created.
type stoppingMM struct {
	mm.MM

	t *testing.T
}

func (m stoppingMM) GetVMInfo(...mm.Option) mm.VMs {
	stopExperimentForwards("stopping-exp")

	return mm.VMs{{Name: "vm", Host: "compute-0"}}
}

func (m stoppingMM) CreateTunnel(...mm.Option) error {
	m.t.Error("created a minimega tunnel for an experiment being stopped")

	return nil
}

// TestCreatePortForwardRefusesAnExperimentBeingStopped is a regression test:
// the forward service could still hold an autotunnel the startup app handed
// it before the stopping hook ended the waits, and created the forward and
// its minimega tunnel after the hook had dropped the experiment's forwards.
func TestCreatePortForwardRefusesAnExperimentBeingStopped(t *testing.T) {
	installExperimentStore(t, map[string]string{"stopping-exp": "2026-10-19T00:00:00Z"}, nil)

	forwardsMu.Lock()
	originalForwards, originalEnd, originalMM := forwards, endAutotunnelWaits, mm.DefaultMM
	forwards = make(map[string]ft.Listener)
	mm.DefaultMM = stoppingMM{t: t} //nolint:reassign // install test double
	forwardsMu.Unlock()

	t.Cleanup(func() {
		experimentStopped("stopping-exp")

		forwardsMu.Lock()
		defer forwardsMu.Unlock()

		forwards, endAutotunnelWaits = originalForwards, originalEnd
		mm.DefaultMM = originalMM //nolint:reassign // restore default
	})

	var during error

	endAutotunnelWaits = func(exp string) {
		// The service handles an autotunnel handed over before the waits ended.
		during = createPortForward(exp, "vm", "8080", "", "22", "admin")
	}

	// The stopping hook runs while the service queries minimega.
	if err := createPortForward("stopping-exp", "vm", "8080", "", "22", "admin"); !errors.Is(err, errExperimentStopping) {
		t.Errorf("forward whose VM query overlapped the stopping hook = %v, want it refused", err)
	}

	if !errors.Is(during, errExperimentStopping) {
		t.Errorf("forward handled while the stopping hook ran = %v, want it refused", during)
	}

	forwardsMu.Lock()
	n := len(forwards)
	forwardsMu.Unlock()

	if n != 0 {
		t.Errorf("phenix ui holds %d forwards of the experiment being stopped", n)
	}

	experimentStopped("stopping-exp")

	forwardsMu.Lock()
	err := checkNotStopping("stopping-exp", "vm")
	forwardsMu.Unlock()

	if err != nil {
		t.Errorf("after the stop, checkNotStopping = %v, want the mark cleared", err)
	}
}

// TestStopOfAChangedRunEndsTheStoppingMark is a regression test: the mark
// the stopping hook sets was cleared only by the stop hooks, which a stop
// skips when another process stopped or restarted the experiment while the
// stopping hooks ran, so phenix ui refused every forward to the newer run,
// its autotunnels too, until it restarted.
func TestStopOfAChangedRunEndsTheStoppingMark(t *testing.T) {
	const name = "changed-run-exp"

	starts := map[string]string{name: "2026-10-19T00:00:00Z"}
	installExperimentStore(t, starts, nil)

	original := endAutotunnelWaits
	endAutotunnelWaits = func(string) {}

	t.Cleanup(func() {
		endAutotunnelWaits = original

		experimentStopped(name)
	})

	// Registered after this package's stopping hook, so it runs after the
	// mark is set: another process restarts the experiment meanwhile.
	experiment.RegisterHook("stopping", func(_, exp string) {
		if exp == name {
			starts[name] = "2026-10-19T00:05:00Z"
		}
	})

	if err := experiment.Stop(name); !errors.Is(err, types.ErrExperimentRunChanged) {
		t.Fatalf("Stop = %v, want ErrExperimentRunChanged", err)
	}

	forwardsMu.Lock()
	err := checkNotStopping(name, "vm")
	forwardsMu.Unlock()

	if err != nil {
		t.Errorf("after a stop that found a newer run, checkNotStopping = %v, want the mark cleared", err)
	}
}
