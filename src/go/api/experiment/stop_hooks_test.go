package experiment_test

import (
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/golang/mock/gomock"

	"phenix/api/experiment"
	"phenix/store"
	"phenix/util/mm"
)

type orderRecordingMM struct {
	mm.MM

	events *[]string
}

func (m orderRecordingMM) ClearNamespace(string) error {
	*m.events = append(*m.events, "clear namespace")

	return nil
}

// TestStopRunsStoppingHooksBeforeClearingNamespace is a regression test: the
// port forwards of an experiment were dropped only by a stop hook, after its
// namespace had been cleared, so a reaper tick in between asked minimega about
// their tunnels and created the namespace again.
func TestStopRunsStoppingHooksBeforeClearingNamespace(t *testing.T) {
	const name = "stopping-hook-test"

	var events []string

	for _, stage := range []string{"stopping", "stop"} {
		experiment.RegisterHook(stage, func(stage, exp string) {
			if exp == name {
				events = append(events, stage)
			}
		})
	}

	ctrl := gomock.NewController(t)

	cfg := triggerTestConfig(name)

	m := store.NewMockStore(ctrl)
	m.EXPECT().Get(gomock.Any()).DoAndReturn(func(c *store.Config) error {
		*c = cfg
		return nil
	}).AnyTimes()
	m.EXPECT().Update(gomock.Any()).Return(nil).AnyTimes()

	originalStore, originalMM := store.DefaultStore, mm.DefaultMM
	t.Cleanup(func() {
		store.DefaultStore = originalStore //nolint:reassign // restore default
		mm.DefaultMM = originalMM          //nolint:reassign // restore default
	})

	store.DefaultStore = m                           //nolint:reassign // install test double
	mm.DefaultMM = orderRecordingMM{events: &events} //nolint:reassign // install test double

	if err := experiment.Stop(name, experiment.StopWithKeepInjectionSnapshots(true)); err != nil {
		t.Fatalf("stopping experiment: %v", err)
	}

	want := []string{"stopping", "clear namespace", "stop"}
	if !slices.Equal(events, want) {
		t.Fatalf("expected %v, got %v", want, events)
	}
}

// TestStopEndsRunsBeforeCleanupAndKeepsTheirStatus is a regression test: Stop
// ended the SCORCH runs (its stopping hooks) only after the cleanup apps had
// torn down what their stop and cleanup stages use, such as the tap app's
// taps, and then wrote back the status it read before the runs ended, so a
// cleanup app's failure left a stopped experiment recording a SCORCH run.
func TestStopEndsRunsBeforeCleanupAndKeepsTheirStatus(t *testing.T) {
	const name = "stop-order-test"

	var (
		mu     sync.Mutex
		events []string
		active bool
	)

	record := func(event string) {
		mu.Lock()
		defer mu.Unlock()

		events = append(events, event)
	}

	experiment.RegisterHook("stopping", func(_, exp string) {
		if exp != name || !active {
			return
		}

		record("stopping")

		// The run ends and clears its record, as scorchexe.Execute does.
		run, err := experiment.Get(name)
		if err != nil {
			t.Errorf("getting experiment: %v", err)

			return
		}

		run.Status.SetAppRunning("scorch", false)
		run.Status.SetAppStatus("scorch", nil)

		if err := run.WriteToStore(true); err != nil {
			t.Errorf("storing the ended run: %v", err)
		}
	})

	rec := &startRecord{cleanupErr: errors.New("cleanup failed")}
	rec.onCleanup = func() { record("cleanup") }

	registerStartApp(t, rec)

	cfg := startTestConfig(t, false, false)
	cfg.Metadata.Name = name
	cfg.Spec["experimentName"] = name
	cfg.Status = map[string]any{
		"startTime":             "2026-10-19T00:00:00Z",
		"appRunningStageStatus": map[string]any{"scorch": true},
		"apps":                  map[string]any{"scorch": map[string]any{"runID": 0}},
	}

	fake := &failedStartMM{startMM: new(startMM), events: new(failedStartEvents)}
	installStartDoubles(t, fake, cfg)
	useBoltStore(t, cfg)

	active = true

	t.Cleanup(func() { active = false })

	err := experiment.Stop(name)
	if err == nil || !strings.Contains(err.Error(), "cleanup failed") {
		t.Errorf("Stop returned %v, want the cleanup app's failure", err)
	}

	for _, event := range fake.events.events {
		record(event)
	}

	if want := []string{"stopping", "cleanup", "clear namespace " + name}; !slices.Equal(events, want) {
		t.Errorf("Stop ran %v, want %v", events, want)
	}

	exp, err := experiment.Get(name)
	if err != nil {
		t.Fatalf("getting experiment: %v", err)
	}

	if exp.Status.StartTime() != "" {
		t.Errorf("stored start time %q, want the experiment stopped", exp.Status.StartTime())
	}

	if exp.Status.AppRunning()["scorch"] {
		t.Error("the stopped experiment still records SCORCH running")
	}

	if _, ok := exp.Status.AppStatus()["scorch"]; ok {
		t.Errorf("the stopped experiment still records a SCORCH run: %v", exp.Status.AppStatus()["scorch"])
	}
}
