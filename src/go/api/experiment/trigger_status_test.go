package experiment_test

import (
	"context"
	"testing"

	"phenix/api/experiment"
	"phenix/app"
	"phenix/store"
	"phenix/types"
)

// statusChangingApp is a scenario app whose stages call change.
type statusChangingApp struct {
	noopApp

	change func(app.Action, *types.Experiment)
}

func (a *statusChangingApp) Configure(_ context.Context, exp *types.Experiment) error {
	a.change(app.ActionConfigure, exp)

	return nil
}

func (a *statusChangingApp) Cleanup(_ context.Context, exp *types.Experiment) error {
	a.change(app.ActionCleanup, exp)

	return nil
}

// TestTriggerKeepsWhatOthersStoreDuringTheStage is a regression test: a
// triggered stage other than running ended by writing back the whole status
// read before the stage, so a SCORCH run that ended during a triggered
// cleanup had its record restored, and what other apps stored meanwhile was
// lost.
func TestTriggerKeepsWhatOthersStoreDuringTheStage(t *testing.T) {
	const (
		name    = "trigger-keeps-status"
		appName = "test-trigger-keeps-status"
	)

	if err := app.RegisterUserApp(appName, func() app.App {
		return &statusChangingApp{
			noopApp: noopApp{name: appName},
			change: func(stage app.Action, exp *types.Experiment) {
				if stage != app.ActionCleanup {
					return
				}

				exp.Spec.Scenario().App(appName).SetMetadata(map[string]any{"cleaned": true})

				// The app's running stage ends, and another app stores its status.
				modifyStoredStatus(t, name, func(status map[string]any) {
					status["apps"] = map[string]any{"other": "kept"}
					status["appRunningStageStatus"] = map[string]any{appName: false}
				})
			},
		}
	}); err != nil {
		t.Fatalf("registering user app: %v", err)
	}

	cfg := triggerTestConfig(name, appName)
	cfg.Status["apps"] = map[string]any{appName: map[string]any{"run": 0}}
	cfg.Status["appRunningStageStatus"] = map[string]any{appName: true}

	useBoltStore(t, cfg)

	if err := experiment.Trigger(context.Background(), name, app.ActionCleanup, appName); err != nil {
		t.Fatalf("triggering the cleanup stage: %v", err)
	}

	exp, err := experiment.Get(name)
	if err != nil {
		t.Fatalf("getting experiment: %v", err)
	}

	if got := exp.Status.AppStatus()["other"]; got != "kept" {
		t.Errorf("status.apps.other = %v after the trigger, want the status the other app stored", got)
	}

	if got, ok := exp.Status.AppStatus()[appName]; ok {
		t.Errorf("status.apps.%s = %v, the record of the run that ended", appName, got)
	}

	if exp.Status.AppRunning()[appName] {
		t.Error("the trigger set the running flag again after the app's running stage ended")
	}

	if got := exp.Spec.Scenario().App(appName).Metadata()["cleaned"]; got != true {
		t.Errorf("the scenario app's metadata.cleaned = %v, want the change the stage made", got)
	}
}

// TestTriggerStoresTheCleanupQueue checks that a triggered stage stores the
// cleanup it queues for a disabled app, and a triggered cleanup the end of
// it, since the trigger stores no status afterwards.
func TestTriggerStoresTheCleanupQueue(t *testing.T) {
	const (
		name    = "trigger-cleanup-queue"
		appName = "test-trigger-cleanup-queue"
	)

	if err := app.RegisterUserApp(appName, func() app.App {
		return &statusChangingApp{noopApp: noopApp{name: appName}, change: func(app.Action, *types.Experiment) {}}
	}); err != nil {
		t.Fatalf("registering user app: %v", err)
	}

	cfg := triggerTestConfig(name, appName)
	cfg.Spec["scenario"] = map[string]any{"apps": []map[string]any{{"name": appName, "disabled": true}}}

	useBoltStore(t, cfg)

	pending := func() bool {
		t.Helper()

		exp, err := experiment.Get(name)
		if err != nil {
			t.Fatalf("getting experiment: %v", err)
		}

		return exp.Status.AppCleanup()[appName]
	}

	if err := experiment.Trigger(context.Background(), name, app.ActionConfigure, appName); err != nil {
		t.Fatalf("triggering the configure stage: %v", err)
	}

	if !pending() {
		t.Fatal("the triggered configure stage did not store the app's pending cleanup")
	}

	if err := experiment.Trigger(context.Background(), name, app.ActionCleanup, appName); err != nil {
		t.Fatalf("triggering the cleanup stage: %v", err)
	}

	if pending() {
		t.Error("the triggered cleanup stage left the app's cleanup pending")
	}
}

// modifyStoredStatus changes the stored status of the named experiment, as
// another app or process would.
func modifyStoredStatus(t *testing.T, name string, change func(status map[string]any)) {
	t.Helper()

	c, _ := store.NewConfig("experiment/" + name)

	if err := store.Modify(c, func(c *store.Config) error {
		change(c.Status)

		return nil
	}); err != nil {
		t.Errorf("changing the stored status: %v", err)
	}
}
