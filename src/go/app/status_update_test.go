package app_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"phenix/app"
	"phenix/store"
	"phenix/types"
	"phenix/util/pubsub"
)

// hookApp is a user app whose stages call the hook set for its name.
type hookApp struct{ name string }

//nolint:gochecknoglobals // the app registry is global, so the apps are registered once
var (
	hookAppsOnce sync.Once
	hookMu       sync.Mutex
	hooks        = make(map[string]func(app.Action, *types.Experiment) error)
)

var hookAppNames = []string{"hook-a", "hook-b", "hook-c", "hook-d"} //nolint:gochecknoglobals // test apps

func (a hookApp) call(stage app.Action, exp *types.Experiment) error {
	hookMu.Lock()
	hook := hooks[a.name]
	hookMu.Unlock()

	if hook == nil {
		return nil
	}

	return hook(stage, exp)
}

func (a hookApp) Init(...app.Option) error { return nil }
func (a hookApp) Name() string             { return a.name }

func (a hookApp) Configure(ctx context.Context, exp *types.Experiment) error {
	return a.call(app.ActionConfigure, exp)
}

func (a hookApp) PreStart(ctx context.Context, exp *types.Experiment) error {
	return a.call(app.ActionPreStart, exp)
}

func (a hookApp) PostStart(ctx context.Context, exp *types.Experiment) error {
	return a.call(app.ActionPostStart, exp)
}

func (a hookApp) Running(ctx context.Context, exp *types.Experiment) error {
	return a.call(app.ActionRunning, exp)
}

func (a hookApp) Cleanup(ctx context.Context, exp *types.Experiment) error {
	return a.call(app.ActionCleanup, exp)
}

// useHookApps registers the hook apps and sets hook for each named app until
// the test ends.
func useHookApps(t *testing.T, hook func(name string, stage app.Action, exp *types.Experiment) error) {
	t.Helper()

	hookAppsOnce.Do(func() {
		for _, name := range hookAppNames {
			if err := app.RegisterUserApp(name, func() app.App { return hookApp{name: name} }); err != nil {
				t.Fatalf("registering test app %s: %v", name, err)
			}
		}
	})

	hookMu.Lock()
	defer hookMu.Unlock()

	for _, name := range hookAppNames {
		hooks[name] = func(stage app.Action, exp *types.Experiment) error { return hook(name, stage, exp) }
	}

	t.Cleanup(func() {
		hookMu.Lock()
		defer hookMu.Unlock()

		clear(hooks)
	})
}

// modifyStatus changes the stored status of the named experiment, as another
// app or process would.
func modifyStatus(t *testing.T, name string, change func(status map[string]any)) {
	t.Helper()

	c, _ := store.NewConfig("experiment/" + name)

	if err := store.Modify(c, func(c *store.Config) error {
		change(c.Status)

		return nil
	}); err != nil {
		t.Errorf("changing the stored status: %v", err)
	}
}

// waitForPeriodicRuns cancels the periodic runs and waits until they end.
func waitForPeriodicRuns(t *testing.T, cancel context.CancelFunc, wg *sync.WaitGroup) {
	t.Helper()

	cancel()

	done := make(chan struct{})

	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the periodic runs did not end with their context")
	}
}

// TestPeriodicTickKeepsWhatOthersStored is a regression test: a periodic
// tick wrote back the whole status it read when it began, so what other apps
// stored while the app ran, such as their status, running flag or pending
// cleanup, was lost.
func TestPeriodicTickKeepsWhatOthersStored(t *testing.T) {
	t.Cleanup(app.SetMinPeriod(time.Millisecond))

	const name = "periodic-keep-test"

	var (
		once     sync.Once
		started  = make(chan struct{})
		released = make(chan struct{})
	)

	useHookApps(t, func(appName string, stage app.Action, exp *types.Experiment) error {
		if appName != "hook-a" || stage != app.ActionRunning {
			return nil
		}

		first := false

		once.Do(func() { first = true })

		if !first {
			return nil
		}

		close(started)
		<-released

		exp.Status.SetAppStatus("hook-a", map[string]any{"ticks": 1})

		return nil
	})

	useBoltStore(t, store.Config{
		Version:  "phenix.sandia.gov/v1",
		Kind:     "Experiment",
		Metadata: store.ConfigMetadata{Name: name},
		Spec: map[string]any{
			"experimentName": name,
			"scenario": map[string]any{
				"apps": []map[string]any{{"name": "hook-a", "runPeriodically": "50ms"}},
			},
		},
		Status: map[string]any{"startTime": "2026-10-19T00:00:00Z"},
	})

	ctx, cancel := context.WithCancel(context.Background())

	var wg sync.WaitGroup

	t.Cleanup(func() { waitForPeriodicRuns(t, cancel, &wg) })

	if err := app.PeriodicallyRunApps(ctx, &wg, readExperiment(t, name)); err != nil {
		t.Fatalf("PeriodicallyRunApps: %v", err)
	}

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the periodic app did not run")
	}

	// While the tick runs, another app stores its status.
	modifyStatus(t, name, func(status map[string]any) {
		status["apps"] = map[string]any{"other": "kept"}
		status["appRunningStageStatus"] = map[string]any{"hook-a": true, "other": true}
		status["appCleanupStagePending"] = map[string]any{"other": true}
	})

	close(released)

	waitFor(t, "the tick's status", func() bool {
		_, ok := readExperiment(t, name).Status.AppStatus()["hook-a"]

		return ok
	})

	status := readExperiment(t, name).Status

	if got := status.AppStatus()["other"]; got != "kept" {
		t.Errorf("status.apps.other = %v after the tick, want the status the other app stored", got)
	}

	if !status.AppRunning()["other"] {
		t.Error("appRunningStageStatus.other was cleared by the tick")
	}

	if !status.AppCleanup()["other"] {
		t.Error("appCleanupStagePending.other was cleared by the tick")
	}
}

// TestPeriodicAppsKeepEachOthersFrequency is a regression test: each periodic
// app wrote the whole status when it was scheduled, so apps scheduled together
// overwrote each other's frequency.
func TestPeriodicAppsKeepEachOthersFrequency(t *testing.T) {
	const name = "periodic-frequency-test"

	useHookApps(t, func(string, app.Action, *types.Experiment) error { return nil })

	apps := make([]map[string]any, len(hookAppNames))
	for i, appName := range hookAppNames {
		apps[i] = map[string]any{"name": appName, "runPeriodically": "1h"}
	}

	useBoltStore(t, store.Config{
		Version:  "phenix.sandia.gov/v1",
		Kind:     "Experiment",
		Metadata: store.ConfigMetadata{Name: name},
		Spec:     map[string]any{"experimentName": name, "scenario": map[string]any{"apps": apps}},
		Status:   map[string]any{"startTime": "2026-10-19T00:00:00Z"},
	})

	ctx, cancel := context.WithCancel(context.Background())

	var wg sync.WaitGroup

	t.Cleanup(func() { waitForPeriodicRuns(t, cancel, &wg) })

	if err := app.PeriodicallyRunApps(ctx, &wg, readExperiment(t, name)); err != nil {
		t.Fatalf("PeriodicallyRunApps: %v", err)
	}

	waitFor(t, "every app's frequency", func() bool {
		return len(readExperiment(t, name).Status.AppFrequency()) == len(hookAppNames)
	})
}

// TestRunningStageStoresNothingIntoANewRun is a regression test: the running
// stage reloaded the experiment before its last write, which adopted the
// start time of a run started meanwhile, so the old run's app status was
// stored into the new run.
func TestRunningStageStoresNothingIntoANewRun(t *testing.T) {
	const name = "running-new-run-test"

	useHookApps(t, func(appName string, stage app.Action, exp *types.Experiment) error {
		if appName != "hook-a" || stage != app.ActionRunning {
			return nil
		}

		exp.Status.SetAppStatus("hook-a", map[string]any{"run": "old"})

		// Another process stops the experiment and starts it again.
		modifyStatus(t, name, func(status map[string]any) {
			status["startTime"] = "2026-10-19T01:00:00Z"
			status["apps"] = map[string]any{}
			status["appRunningStageStatus"] = map[string]any{}
		})

		return nil
	})

	useBoltStore(t, store.Config{
		Version:  "phenix.sandia.gov/v1",
		Kind:     "Experiment",
		Metadata: store.ConfigMetadata{Name: name},
		Spec: map[string]any{
			"experimentName": name,
			"scenario":       map[string]any{"apps": []map[string]any{{"name": "hook-a"}}},
		},
		Status: map[string]any{"startTime": "2026-10-19T00:00:00Z"},
	})

	err := app.ApplyApps(
		context.Background(), readExperiment(t, name), app.Stage(app.ActionRunning), app.FilterApp("hook-a"),
	)
	if err != nil {
		t.Fatalf("ApplyApps: %v", err)
	}

	status := readExperiment(t, name).Status

	if got, ok := status.AppStatus()["hook-a"]; ok {
		t.Errorf("the new run's status.apps.hook-a = %v, the old run's status", got)
	}

	if _, ok := status.AppRunning()["hook-a"]; ok {
		t.Error("the old run wrote its running flag into the new run")
	}
}

// TestTriggeredStageKeepsTheRunningFlag is a regression test: every stage
// other than running cleared the app's running flag when it ended, so a
// cleanup triggered while a SCORCH run executed let another process start a
// second run. A start or a stop still clears the flag.
func TestTriggeredStageKeepsTheRunningFlag(t *testing.T) {
	const name = "triggered-flag-test"

	useHookApps(t, func(string, app.Action, *types.Experiment) error { return nil })

	for _, stage := range []app.Action{app.ActionConfigure, app.ActionPreStart, app.ActionPostStart, app.ActionCleanup} {
		t.Run(string(stage), func(t *testing.T) {
			useBoltStore(t, store.Config{
				Version:  "phenix.sandia.gov/v1",
				Kind:     "Experiment",
				Metadata: store.ConfigMetadata{Name: name},
				Spec: map[string]any{
					"experimentName": name,
					"scenario":       map[string]any{"apps": []map[string]any{{"name": "hook-a"}}},
				},
				Status: map[string]any{
					"startTime":             "2026-10-19T00:00:00Z",
					"appRunningStageStatus": map[string]any{"hook-a": true},
				},
			})

			err := app.ApplyApps(
				context.Background(), readExperiment(t, name), app.Stage(stage), app.FilterApp("hook-a"), app.Trigger(),
			)
			if err != nil {
				t.Fatalf("ApplyApps: %v", err)
			}

			if !readExperiment(t, name).Status.AppRunning()["hook-a"] {
				t.Errorf("the triggered %s stage cleared the running flag of the app's running stage", stage)
			}

			if err := app.ApplyApps(
				context.Background(), readExperiment(t, name), app.Stage(stage), app.FilterApp("hook-a"),
			); err != nil {
				t.Fatalf("ApplyApps: %v", err)
			}

			if readExperiment(t, name).Status.AppRunning()["hook-a"] {
				t.Errorf("the %s stage of a start or a stop kept the app's running flag", stage)
			}
		})
	}
}

// TestTriggeredStageLeavesTheEndOfARunAlone is a regression test: a
// triggered stage wrote back the whole status it read, with the running flag
// it found, so when the app's running stage, such as a SCORCH run, ended
// during the stage, the flag stayed set and the run's status came back, and
// every later run was refused until the experiment stopped.
func TestTriggeredStageLeavesTheEndOfARunAlone(t *testing.T) {
	const name = "triggered-run-end-test"

	useHookApps(t, func(appName string, stage app.Action, _ *types.Experiment) error {
		if appName == "hook-a" && stage == app.ActionCleanup {
			// The app's running stage ends and clears its flag and status.
			modifyStatus(t, name, func(status map[string]any) {
				status["appRunningStageStatus"] = map[string]any{"hook-a": false}
				status["apps"] = map[string]any{}
			})
		}

		return nil
	})

	useBoltStore(t, store.Config{
		Version:  "phenix.sandia.gov/v1",
		Kind:     "Experiment",
		Metadata: store.ConfigMetadata{Name: name},
		Spec: map[string]any{
			"experimentName": name,
			"scenario":       map[string]any{"apps": []map[string]any{{"name": "hook-a"}}},
		},
		Status: map[string]any{
			"startTime":             "2026-10-19T00:00:00Z",
			"apps":                  map[string]any{"hook-a": map[string]any{"run": 0}},
			"appRunningStageStatus": map[string]any{"hook-a": true},
		},
	})

	exp := readExperiment(t, name)

	err := app.ApplyApps(
		context.Background(), exp, app.Stage(app.ActionCleanup), app.FilterApp("hook-a"), app.Trigger(),
	)
	if err != nil {
		t.Fatalf("ApplyApps: %v", err)
	}

	status := readExperiment(t, name).Status

	if status.AppRunning()["hook-a"] {
		t.Error("the triggered stage set the running flag again after the app's running stage ended")
	}

	if got, ok := status.AppStatus()["hook-a"]; ok {
		t.Errorf("status.apps.hook-a = %v, the status of the run that ended", got)
	}

	if exp.Status.AppRunning()["hook-a"] {
		t.Error("the experiment's copy keeps the running flag the stored status cleared")
	}
}

// TestTriggeredStageKeepsTheFlagOfARunStartedMeanwhile is a regression test:
// a triggered stage took the running flag it would keep from its copy of the
// experiment, so a run that started after the copy was read lost the flag as
// the stage ended, and a second run could start beside it.
func TestTriggeredStageKeepsTheFlagOfARunStartedMeanwhile(t *testing.T) {
	const name = "triggered-run-start-test"

	useHookApps(t, func(string, app.Action, *types.Experiment) error { return nil })

	useBoltStore(t, store.Config{
		Version:  "phenix.sandia.gov/v1",
		Kind:     "Experiment",
		Metadata: store.ConfigMetadata{Name: name},
		Spec: map[string]any{
			"experimentName": name,
			"scenario":       map[string]any{"apps": []map[string]any{{"name": "hook-a"}}},
		},
		Status: map[string]any{"startTime": "2026-10-19T00:00:00Z"},
	})

	exp := readExperiment(t, name)

	// The app's running stage starts after the copy was read.
	modifyStatus(t, name, func(status map[string]any) {
		status["appRunningStageStatus"] = map[string]any{"hook-a": true}
	})

	err := app.ApplyApps(
		context.Background(), exp, app.Stage(app.ActionCleanup), app.FilterApp("hook-a"), app.Trigger(),
	)
	if err != nil {
		t.Fatalf("ApplyApps: %v", err)
	}

	if !readExperiment(t, name).Status.AppRunning()["hook-a"] {
		t.Error("the triggered stage cleared the running flag of a run that started after its copy was read")
	}
}

// TestStageStoresOnlyItsOwnStatus is a regression test: the stages other than
// running wrote back the whole status they read, so what other apps stored
// while the stage ran, such as their status or running flag, was lost.
func TestStageStoresOnlyItsOwnStatus(t *testing.T) {
	const name = "stage-own-status-test"

	useHookApps(t, func(appName string, _ app.Action, exp *types.Experiment) error {
		if appName != "hook-a" {
			return nil
		}

		exp.Status.SetAppStatus("hook-a", map[string]any{"stage": "done"})

		// Another app stores its status and running flag meanwhile.
		modifyStatus(t, name, func(status map[string]any) {
			status["apps"] = map[string]any{"other": "kept"}
			status["appRunningStageStatus"] = map[string]any{"hook-a": true, "other": true}
		})

		return nil
	})

	for _, trigger := range []bool{false, true} {
		t.Run(fmt.Sprintf("trigger=%t", trigger), func(t *testing.T) {
			useBoltStore(t, store.Config{
				Version:  "phenix.sandia.gov/v1",
				Kind:     "Experiment",
				Metadata: store.ConfigMetadata{Name: name},
				Spec: map[string]any{
					"experimentName": name,
					"scenario":       map[string]any{"apps": []map[string]any{{"name": "hook-a"}}},
				},
				Status: map[string]any{"startTime": "2026-10-19T00:00:00Z"},
			})

			opts := []app.Option{app.Stage(app.ActionPostStart), app.FilterApp("hook-a")}
			if trigger {
				opts = append(opts, app.Trigger())
			}

			if err := app.ApplyApps(context.Background(), readExperiment(t, name), opts...); err != nil {
				t.Fatalf("ApplyApps: %v", err)
			}

			status := readExperiment(t, name).Status

			if got, ok := status.AppStatus()["hook-a"].(map[string]any); !ok || got["stage"] != "done" {
				t.Errorf("status.apps.hook-a = %v, want the status the stage set", status.AppStatus()["hook-a"])
			}

			if got := status.AppStatus()["other"]; got != "kept" {
				t.Errorf("status.apps.other = %v after the stage, want the status the other app stored", got)
			}

			if !status.AppRunning()["other"] {
				t.Error("appRunningStageStatus.other was cleared by the stage")
			}

			if status.AppRunning()["hook-a"] {
				t.Error("the stage kept its own running flag")
			}
		})
	}
}

// TestPeriodicAppsRefuseAStoppedCopy is a regression test: periodic apps
// scheduled from a copy of a stopped experiment, such as one read just after
// a stop from another process, compared the empty start time with the stored
// one and ran against the stopped experiment until canceled.
func TestPeriodicAppsRefuseAStoppedCopy(t *testing.T) {
	t.Cleanup(app.SetMinPeriod(time.Millisecond))

	const name = "periodic-stopped-test"

	var runs atomic.Int32

	useHookApps(t, func(_ string, stage app.Action, _ *types.Experiment) error {
		if stage == app.ActionRunning {
			runs.Add(1)
		}

		return nil
	})

	useBoltStore(t, store.Config{
		Version:  "phenix.sandia.gov/v1",
		Kind:     "Experiment",
		Metadata: store.ConfigMetadata{Name: name},
		Spec: map[string]any{
			"experimentName": name,
			"scenario": map[string]any{
				"apps": []map[string]any{{"name": "hook-a", "runPeriodically": "10ms"}},
			},
		},
		Status: map[string]any{"startTime": ""},
	})

	ctx, cancel := context.WithCancel(context.Background())

	var wg sync.WaitGroup

	t.Cleanup(func() { waitForPeriodicRuns(t, cancel, &wg) })

	if err := app.PeriodicallyRunApps(ctx, &wg, readExperiment(t, name)); err == nil {
		t.Error("PeriodicallyRunApps scheduled the apps of a stopped experiment")
	}

	time.Sleep(100 * time.Millisecond)

	if n := runs.Load(); n != 0 {
		t.Errorf("the periodic app ran %d times for a stopped experiment", n)
	}
}

// TestPeriodicAppsSkipPeriodsBelowTheMinimum is a regression test: a
// runPeriodically of 0s, a negative duration or a tiny one such as 1ms parsed,
// and the app then ran again and again almost without a pause.
func TestPeriodicAppsSkipPeriodsBelowTheMinimum(t *testing.T) {
	const name = "periodic-zero-test"

	var runs atomic.Int32

	useHookApps(t, func(_ string, stage app.Action, _ *types.Experiment) error {
		if stage == app.ActionRunning {
			runs.Add(1)
		}

		return nil
	})

	useBoltStore(t, store.Config{
		Version:  "phenix.sandia.gov/v1",
		Kind:     "Experiment",
		Metadata: store.ConfigMetadata{Name: name},
		Spec: map[string]any{
			"experimentName": name,
			"scenario": map[string]any{
				"apps": []map[string]any{
					{"name": "hook-a", "runPeriodically": "0s"},
					{"name": "hook-b", "runPeriodically": "-5m"},
					{"name": "hook-c", "runPeriodically": "1ms"},
					{"name": "hook-d", "runPeriodically": "1ns"},
				},
			},
		},
		Status: map[string]any{"startTime": "2026-10-19T00:00:00Z"},
	})

	ctx, cancel := context.WithCancel(context.Background())

	var wg sync.WaitGroup

	t.Cleanup(func() { waitForPeriodicRuns(t, cancel, &wg) })

	if err := app.PeriodicallyRunApps(ctx, &wg, readExperiment(t, name)); err != nil {
		t.Fatalf("PeriodicallyRunApps: %v", err)
	}

	time.Sleep(200 * time.Millisecond)

	if n := runs.Load(); n != 0 {
		t.Errorf("apps with a period below the minimum ran %d times", n)
	}
}

// TestPeriodicAppEndsSoonAfterAStop is a regression test: a periodic app
// noticed a stop from another process only at its next scheduled run, so
// phenix experiment start --honor-run-periodically kept running for up to one
// period after the stop.
func TestPeriodicAppEndsSoonAfterAStop(t *testing.T) {
	const name = "periodic-poll-test"

	t.Cleanup(app.SetStopPollInterval(20 * time.Millisecond))

	useHookApps(t, func(string, app.Action, *types.Experiment) error { return nil })

	useBoltStore(t, store.Config{
		Version:  "phenix.sandia.gov/v1",
		Kind:     "Experiment",
		Metadata: store.ConfigMetadata{Name: name},
		Spec: map[string]any{
			"experimentName": name,
			"scenario": map[string]any{
				"apps": []map[string]any{{"name": "hook-a", "runPeriodically": "1h"}},
			},
		},
		Status: map[string]any{"startTime": "2026-10-19T00:00:00Z"},
	})

	var wg sync.WaitGroup

	if err := app.PeriodicallyRunApps(context.Background(), &wg, readExperiment(t, name)); err != nil {
		t.Fatalf("PeriodicallyRunApps: %v", err)
	}

	waitFor(t, "the app to be scheduled", func() bool {
		return readExperiment(t, name).Status.AppFrequency()["hook-a"] == "1h"
	})

	// Another process stops the experiment.
	modifyStatus(t, name, func(status map[string]any) { status["startTime"] = "" })

	done := make(chan struct{})

	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the periodic app did not end after the stop")
	}
}

// TestPeriodicRunReportsAFailureOnlyAsAFailure is a regression test: a failed
// periodic run published an error and then a success for the app, so the UI
// showed the failed run as a success.
func TestPeriodicRunReportsAFailureOnlyAsAFailure(t *testing.T) {
	t.Cleanup(app.SetMinPeriod(time.Millisecond))

	const name = "periodic-failure-test"

	useHookApps(t, func(_ string, stage app.Action, _ *types.Experiment) error {
		if stage == app.ActionRunning {
			return errors.New("running stage failed")
		}

		return nil
	})

	useBoltStore(t, store.Config{
		Version:  "phenix.sandia.gov/v1",
		Kind:     "Experiment",
		Metadata: store.ConfigMetadata{Name: name},
		Spec: map[string]any{
			"experimentName": name,
			"scenario": map[string]any{
				"apps": []map[string]any{{"name": "hook-a", "runPeriodically": "20ms"}},
			},
		},
		Status: map[string]any{"startTime": "2026-10-19T00:00:00Z"},
	})

	events := pubsub.Subscribe("trigger-app")

	var (
		mu     sync.Mutex
		states []string
		read   = make(chan struct{})
	)

	go func() {
		defer close(read)

		for msg := range events {
			if pub, ok := msg.(app.TriggerPublication); ok && pub.Experiment == name {
				mu.Lock()
				states = append(states, pub.State)
				mu.Unlock()
			}
		}
	}()

	t.Cleanup(func() {
		pubsub.Unsubscribe("trigger-app", events)
		close(events)
		<-read
	})

	ctx, cancel := context.WithCancel(context.Background())

	var wg sync.WaitGroup

	t.Cleanup(func() { waitForPeriodicRuns(t, cancel, &wg) })

	if err := app.PeriodicallyRunApps(ctx, &wg, readExperiment(t, name)); err != nil {
		t.Fatalf("PeriodicallyRunApps: %v", err)
	}

	got := func() []string {
		mu.Lock()
		defer mu.Unlock()

		return slices.Clone(states)
	}

	// The second run's start follows the first run's last event.
	waitFor(t, "two failed periodic runs", func() bool {
		starts := 0

		for _, state := range got() {
			if state == "start" {
				starts++
			}
		}

		return starts >= 2
	})

	if slices.Contains(got(), "success") || !slices.Contains(got(), "error") {
		t.Errorf("failed periodic runs published %v, want only start and error", got())
	}
}
