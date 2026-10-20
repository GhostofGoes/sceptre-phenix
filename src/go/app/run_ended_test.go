package app_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"phenix/app"
	"phenix/store"
	"phenix/types"
	"phenix/util/pubsub"
)

// TestApplyAppsStopsOnceTheRunEnded is a regression test: the post-start apps
// of a start kept running after another process had stopped the experiment,
// and the commands they sent to minimega created its cleared namespace again.
func TestApplyAppsStopsOnceTheRunEnded(t *testing.T) {
	const name = "run-ended-test"

	var ran []string

	useHookApps(t, func(appName string, stage app.Action, _ *types.Experiment) error {
		if stage != app.ActionPostStart {
			return nil
		}

		ran = append(ran, appName)

		// Another process stops the experiment.
		modifyStatus(t, name, func(status map[string]any) { status["startTime"] = "" })

		return nil
	})

	useBoltStore(t, store.Config{
		Version:  "phenix.sandia.gov/v1",
		Kind:     "Experiment",
		Metadata: store.ConfigMetadata{Name: name},
		Spec: map[string]any{
			"experimentName": name,
			"scenario": map[string]any{
				"apps": []map[string]any{{"name": "hook-a"}, {"name": "hook-b"}},
			},
		},
		Status: map[string]any{"startTime": "2026-10-19T00:00:00Z"},
	})

	err := app.ApplyApps(context.Background(), readExperiment(t, name), app.Stage(app.ActionPostStart))
	if !errors.Is(err, types.ErrExperimentRunChanged) {
		t.Errorf("ApplyApps returned %v, want it to report the run ended", err)
	}

	if len(ran) != 1 {
		t.Errorf("post-start ran for %v, want only the first app: the run ended after it", ran)
	}

	if got := readExperiment(t, name).Status.StartTime(); got != "" {
		t.Errorf("stored start time %q, want the experiment stopped", got)
	}
}

// useBoltStore makes a BoltDB in a temporary directory, holding c, the
// store until the test ends.
func useBoltStore(t *testing.T, c store.Config) {
	t.Helper()

	original := store.DefaultStore
	t.Cleanup(func() { store.DefaultStore = original }) //nolint:reassign // restore default

	db := store.NewBoltDB()

	if err := db.Init(store.Endpoint("bolt://" + filepath.Join(t.TempDir(), "store.bdb"))); err != nil {
		t.Fatalf("initializing BoltDB: %v", err)
	}

	store.DefaultStore = db //nolint:reassign // install test store

	if err := store.Create(&c); err != nil {
		t.Fatalf("storing experiment: %v", err)
	}
}

func readExperiment(t *testing.T, name string) *types.Experiment {
	t.Helper()

	c, _ := store.NewConfig("experiment/" + name)
	if err := store.Get(c); err != nil {
		t.Fatalf("getting experiment: %v", err)
	}

	exp, err := types.DecodeExperimentFromConfig(*c)
	if err != nil {
		t.Fatalf("decoding experiment: %v", err)
	}

	return exp
}

// TestStartSkipsDefaultPostStartAppsOnceTheRunEnded is a regression test: a
// stop from another process skipped a start's scenario post-start apps not yet
// run, but not its default ones, such as vrouter, whose commands to minimega
// created the cleared namespace again.
func TestStartSkipsDefaultPostStartAppsOnceTheRunEnded(t *testing.T) {
	const name = "run-ended-default-test"

	useBoltStore(t, store.Config{
		Version:  "phenix.sandia.gov/v1",
		Kind:     "Experiment",
		Metadata: store.ConfigMetadata{Name: name},
		Spec:     map[string]any{"experimentName": name},
		Status:   map[string]any{"startTime": "2026-10-19T00:00:00Z"},
	})

	exp := readExperiment(t, name)

	// Another process stops the experiment.
	modifyStatus(t, name, func(status map[string]any) { status["startTime"] = "" })

	events := pubsub.Subscribe("trigger-app")

	var (
		started []string
		read    = make(chan struct{})
	)

	go func() {
		defer close(read)

		for msg := range events {
			if pub, ok := msg.(app.TriggerPublication); ok && pub.Experiment == name && pub.State == "start" {
				started = append(started, pub.App)
			}
		}
	}()

	err := app.ApplyApps(context.Background(), exp, app.Stage(app.ActionPostStart), app.DryRun(true))

	pubsub.Unsubscribe("trigger-app", events)
	close(events)
	<-read

	if !errors.Is(err, types.ErrExperimentRunChanged) {
		t.Errorf("ApplyApps returned %v, want it to report the run ended", err)
	}

	if len(started) != 0 {
		t.Errorf("default apps %v ran their post-start stage after the stop", started)
	}
}
