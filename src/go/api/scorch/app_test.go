package scorch

import (
	"context"
	"strings"
	"testing"

	"phenix/api/scorch/scorchexe"
	"phenix/store"
	"phenix/types"
	v2 "phenix/types/version/v2"
)

// newScorchTestExperiment returns an experiment whose scenario holds only the
// scorch app with the given metadata.
func newScorchTestExperiment(md map[string]any) *types.Experiment {
	exp := types.NewExperiment(store.ConfigMetadata{Name: "test-exp"})
	exp.Spec.SetExperimentName("test-exp")
	exp.Spec.SetScenario(&v2.ScenarioSpec{AppsF: []*v2.ScenarioApp{{NameF: "scorch", MetadataF: md}}})

	return exp
}

// typoMetadata defines one component and a run that also names an undefined
// one.
func typoMetadata() map[string]any {
	return map[string]any{
		"components": []any{map[string]any{"name": "wait1", "type": "pause"}},
		"runs":       []any{map[string]any{"name": "typo", "start": []any{"wait1", "nosuchcmp"}}},
	}
}

// TestUndefinedRunComponentIsRefused is a regression test: a run that named
// a component the app does not define was accepted, and running it looked
// for an external component of no type, "phenix-scorch-component-".
func TestUndefinedRunComponentIsRefused(t *testing.T) {
	const want = `scorch run 0 start stage names component "nosuchcmp"`

	err := Scorch{}.Configure(context.Background(), newScorchTestExperiment(typoMetadata()))
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("Configure() = %v, want an error containing %q", err, want)
	}

	// An experiment configured before runs were validated is refused when the
	// run starts, before anything runs.
	exp := newScorchTestExperiment(typoMetadata())

	err = new(Scorch).Running(scorchexe.SetRunID(context.Background(), 0), exp)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("Running() = %v, want an error containing %q", err, want)
	}
}

// TestCleanupKeepsRunStatus checks that the scorch app's cleanup stage keeps
// the record of a run, status.apps.scorch, so `phenix experiment trigger
// cleanup` on a running experiment does not drop the run ID and taps of a run
// that still executes. An experiment stop drops the record from its "stop"
// hook (dropRunRecord).
func TestCleanupKeepsRunStatus(t *testing.T) {
	exp := newScorchTestExperiment(typoMetadata())
	exp.Status.SetAppStatus("scorch", map[string]any{"runID": 4, "taps": map[string]any{}})

	if err := (Scorch{}).Cleanup(context.Background(), exp); err != nil {
		t.Fatalf("Cleanup() = %v", err)
	}

	var status map[string]any
	if err := exp.Status.ParseAppStatus("scorch", &status); err != nil {
		t.Errorf("after Cleanup, status.apps.scorch is gone (%v), want the run's record kept", err)
	}
}

// TestDropRunRecord checks that the "stop" hook drops the record of a run
// from the stored experiment, and keeps the rest of its status.
func TestDropRunRecord(t *testing.T) {
	s := installExperimentStore(t, store.Config{
		Version:  "phenix.sandia.gov/v1",
		Kind:     "Experiment",
		Metadata: store.ConfigMetadata{Name: "test-exp"},
		Spec:     map[string]any{"experimentName": "test-exp"},
		Status: map[string]any{
			"apps": map[string]any{"scorch": map[string]any{"runID": 0}, "soh": map[string]any{"initialized": true}},
		},
	})

	dropRunRecord("test-exp")

	apps, _ := s.load().Status["apps"].(map[string]any)
	if _, ok := apps["scorch"]; ok || apps["soh"] == nil {
		t.Errorf("after the stop hook, status.apps = %v, want only soh", apps)
	}
}
