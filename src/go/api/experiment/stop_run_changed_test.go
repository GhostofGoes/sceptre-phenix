package experiment_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"phenix/api/experiment"
	"phenix/types"
	"phenix/util/notes"
)

// stopHookRecord records the stopping, stop and stop-end hooks of the start
// test experiment while a test sets onStopping.
type stopHookRecord struct {
	mu         sync.Mutex
	onStopping func()
	stages     []string
}

//nolint:gochecknoglobals // the hook registry is global, so the hooks are registered once
var (
	stopHooksOnce sync.Once
	stopHooks     stopHookRecord
)

// recordStopHooks records the start test experiment's stop hooks until the
// test ends, and makes its stopping hook call onStopping.
func recordStopHooks(t *testing.T, onStopping func()) {
	t.Helper()

	stopHooksOnce.Do(func() {
		for _, stage := range []string{"stopping", "stop", "stop-end"} {
			experiment.RegisterHook(stage, func(stage, name string) {
				if name != startTestExp {
					return
				}

				stopHooks.mu.Lock()
				active, hook := stopHooks.onStopping != nil, stopHooks.onStopping
				if active {
					stopHooks.stages = append(stopHooks.stages, stage)
				}
				stopHooks.mu.Unlock()

				if active && stage == "stopping" {
					hook()
				}
			})
		}
	})

	stopHooks.mu.Lock()
	stopHooks.onStopping, stopHooks.stages = onStopping, nil
	stopHooks.mu.Unlock()

	t.Cleanup(func() {
		stopHooks.mu.Lock()
		defer stopHooks.mu.Unlock()

		stopHooks.onStopping = nil
	})
}

// stopAndStartElsewhere stores the start test experiment as another
// process's stop, then its start, leave it.
func stopAndStartElsewhere(t *testing.T) {
	t.Helper()

	setStoredStart(t, "")
	setStoredStart(t, otherRun)
}

func recordedStopHooks() string {
	stopHooks.mu.Lock()
	defer stopHooks.mu.Unlock()

	return strings.Join(stopHooks.stages, " ")
}

// TestFailedStartStopLeavesARunStartedWhileStopping is a regression test: the
// stop of a failed start checked that the stored run was its own once, before
// its stopping hooks, which can wait minutes for SCORCH runs, and then
// reloaded the experiment, so when another process stopped the experiment and
// started it again meanwhile, it ran the cleanup apps against the newer run,
// cleared its namespace and stored it as stopped.
func TestFailedStartStopLeavesARunStartedWhileStopping(t *testing.T) {
	rec := &startRecord{postStartErr: errors.New("post-start failed")}
	registerStartApp(t, rec)

	fake := new(startMM)
	installStartDoubles(t, fake, startTestConfig(t, false, false))
	useBoltStore(t, startTestConfig(t, false, false))

	recordStopHooks(t, func() { stopAndStartElsewhere(t) })

	errs := newStartErrors()

	err := experiment.Start(
		notes.Context(context.Background(), false),
		experiment.StartWithName(startTestExp),
		experiment.StartWithErrorChannel(errs.ch),
	)
	if err != nil {
		t.Fatalf("Start returned %v", err)
	}

	got := errors.Join(errs.wait(t)...)
	if got == nil || !strings.Contains(got.Error(), "was stopped while it was starting") {
		t.Errorf("Start reported %v, want the stop to report the experiment stopped elsewhere", got)
	}

	if start := storedStart(t); start != otherRun {
		t.Errorf("stored start time %q, want the newer run %q", start, otherRun)
	}

	if rec.cleanups != 0 || fake.cleared != 0 {
		t.Errorf("cleanup apps ran %d times and the namespace was cleared %d times, want neither: the stop tore down the newer run",
			rec.cleanups, fake.cleared)
	}

	if hooks := recordedStopHooks(); hooks != "stopping stop-end" {
		t.Errorf("stop hooks ran: %q, want %q", hooks, "stopping stop-end")
	}
}

// TestStopLeavesARunStartedWhileStopping is a regression test: Stop reloaded
// the experiment after its stopping hooks and tore down whatever run was then
// stored, one that another process started while the hooks ran, too.
func TestStopLeavesARunStartedWhileStopping(t *testing.T) {
	rec := new(startRecord)
	registerStartApp(t, rec)

	cfg := startTestConfig(t, false, false)
	cfg.Status = map[string]any{"startTime": "2026-10-19T00:00:00Z"}

	fake := new(startMM)
	installStartDoubles(t, fake, cfg)
	useBoltStore(t, cfg)

	recordStopHooks(t, func() { stopAndStartElsewhere(t) })

	err := experiment.Stop(startTestExp)
	if !errors.Is(err, types.ErrExperimentRunChanged) {
		t.Errorf("Stop returned %v, want ErrExperimentRunChanged", err)
	}

	if start := storedStart(t); start != otherRun {
		t.Errorf("stored start time %q, want the newer run %q", start, otherRun)
	}

	if rec.cleanups != 0 || fake.cleared != 0 {
		t.Errorf("cleanup apps ran %d times and the namespace was cleared %d times, want neither", rec.cleanups, fake.cleared)
	}

	if hooks := recordedStopHooks(); hooks != "stopping stop-end" {
		t.Errorf("stop hooks ran: %q, want %q", hooks, "stopping stop-end")
	}
}

// TestStopDoesNotStoreARunStartedDuringCleanup checks that the stop's final
// write, too, refuses to store the experiment as stopped once another process
// has started it again, here while the cleanup apps ran.
func TestStopDoesNotStoreARunStartedDuringCleanup(t *testing.T) {
	rec := new(startRecord)
	rec.onCleanup = func() { stopAndStartElsewhere(t) }

	registerStartApp(t, rec)

	cfg := startTestConfig(t, false, false)
	cfg.Status = map[string]any{"startTime": "2026-10-19T00:00:00Z"}

	installStartDoubles(t, new(startMM), cfg)
	useBoltStore(t, cfg)

	recordStopHooks(t, func() {})

	err := experiment.Stop(startTestExp)
	if !errors.Is(err, types.ErrExperimentRunChanged) {
		t.Errorf("Stop returned %v, want ErrExperimentRunChanged", err)
	}

	if start := storedStart(t); start != otherRun {
		t.Errorf("stored start time %q, want the newer run %q", start, otherRun)
	}

	if hooks := recordedStopHooks(); hooks != "stopping stop-end" {
		t.Errorf("stop hooks ran: %q, want %q", hooks, "stopping stop-end")
	}
}

// TestStopRunsStopEndHooksLast checks the hook order of an ordinary stop.
func TestStopRunsStopEndHooksLast(t *testing.T) {
	registerStartApp(t, new(startRecord))

	cfg := startTestConfig(t, false, false)
	cfg.Status = map[string]any{"startTime": "2026-10-19T00:00:00Z"}

	installStartDoubles(t, new(startMM), cfg)
	useBoltStore(t, cfg)

	recordStopHooks(t, func() {})

	if err := experiment.Stop(startTestExp); err != nil {
		t.Fatalf("Stop returned %v", err)
	}

	if hooks := recordedStopHooks(); hooks != "stopping stop stop-end" {
		t.Errorf("stop hooks ran: %q, want %q", hooks, "stopping stop stop-end")
	}

	if start := storedStart(t); start != "" {
		t.Errorf("stored start time %q, want the experiment stopped", start)
	}
}

// panickingStartMM panics when the namespace is cleared.
type panickingStartMM struct{ *startMM }

func (panickingStartMM) ClearNamespace(string) error { panic("clearing the namespace") }

// TestStopRunsStopEndHooksAfterAPanic checks that a panic after the stopping
// hooks, which net/http recovers from, still runs the stop-end hooks, so that
// SCORCH ends its refusal of the experiment's runs.
func TestStopRunsStopEndHooksAfterAPanic(t *testing.T) {
	registerStartApp(t, new(startRecord))

	cfg := startTestConfig(t, false, false)
	cfg.Status = map[string]any{"startTime": "2026-10-19T00:00:00Z"}

	installStartDoubles(t, panickingStartMM{new(startMM)}, cfg)
	useBoltStore(t, cfg)

	recordStopHooks(t, func() {})

	func() {
		defer func() {
			if recover() == nil {
				t.Error("Stop did not panic")
			}
		}()

		_ = experiment.Stop(startTestExp)
	}()

	if hooks := recordedStopHooks(); hooks != "stopping stop-end" {
		t.Errorf("stop hooks ran: %q, want %q", hooks, "stopping stop-end")
	}
}

// TestStopRunsStopEndHooksAfterAStoppingHookPanics is a regression test: the
// stop-end hooks were deferred after the stopping hooks had run, so a panic in
// a stopping hook skipped them, and the refusal of SCORCH runs an earlier
// stopping hook began lasted until phenix ui restarted.
func TestStopRunsStopEndHooksAfterAStoppingHookPanics(t *testing.T) {
	registerStartApp(t, new(startRecord))

	cfg := startTestConfig(t, false, false)
	cfg.Status = map[string]any{"startTime": "2026-10-19T00:00:00Z"}

	installStartDoubles(t, new(startMM), cfg)
	useBoltStore(t, cfg)

	recordStopHooks(t, func() { panic("stopping hook") })

	func() {
		defer func() {
			if recover() == nil {
				t.Error("Stop did not panic")
			}
		}()

		_ = experiment.Stop(startTestExp)
	}()

	if hooks := recordedStopHooks(); hooks != "stopping stop-end" {
		t.Errorf("stop hooks ran: %q, want %q", hooks, "stopping stop-end")
	}
}
