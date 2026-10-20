package scorch

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"phenix/api/scorch/scorchmd"
	"phenix/store"
	"phenix/types"
	"phenix/util/common"
)

// installSOHExperiment backs the store with the running experiment
// "test-exp", whose state of health app is configured and initialized, and
// executing its running stage if sohRunning is set, and returns the
// experiment.
func installSOHExperiment(t *testing.T, sohRunning bool) *types.Experiment {
	t.Helper()

	status := map[string]any{
		"startTime": "2024-01-01T00:00:00Z",
		"apps":      map[string]any{"soh": map[string]any{"initialized": true}},
	}

	if sohRunning {
		status["appRunningStageStatus"] = map[string]any{"soh": true}
	}

	s := installExperimentStore(t, store.Config{
		Version:  "phenix.sandia.gov/v1",
		Kind:     "Experiment",
		Metadata: store.ConfigMetadata{Name: "test-exp"},
		Spec: map[string]any{
			"experimentName": "test-exp",
			"defaultBridge":  "phenix",
			"topology":       map[string]any{"nodes": []any{}},
			"scenario": map[string]any{
				"apps": []any{map[string]any{"name": "soh", "metadata": map[string]any{}}},
			},
		},
		Status: status,
	})

	exp, err := types.DecodeExperimentFromConfig(s.load())
	if err != nil {
		t.Fatal(err)
	}

	return exp
}

// TestSOHFailOnErrorWhileSOHRuns is a regression test: the soh component read
// failOnError before decoding its metadata, so a run with failOnError set
// succeeded while the state of health app was already running.
func TestSOHFailOnErrorWhileSOHRuns(t *testing.T) {
	tests := []struct {
		name    string
		meta    map[string]any
		wantErr string
	}{
		{"failOnError", map[string]any{"failOnError": true}, "app already running"},
		{"without failOnError", map[string]any{}, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var cmp SOH

			_ = cmp.Init(
				Experiment(*installSOHExperiment(t, true)),
				Name("soh1"),
				Type("soh"),
				Metadata(tt.meta),
			)

			err := cmp.Start(context.Background())

			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("Start() = %v, want nil", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Fatalf("Start() = %v, want an error containing %q", err, tt.wantErr)
			}
		})
	}
}

// TestBackgroundedSOHKeepsRunStatus is a regression test: a backgrounded soh
// component ran the state of health app on the experiment status that all of
// the run's components share, so its writes raced with the components that
// read that status at the same time (run with -race), and the status it
// wrote back to the store was the run's copy from when the run started.
func TestBackgroundedSOHKeepsRunStatus(t *testing.T) {
	bin := t.TempDir()

	//nolint:gosec // test script must be executable
	if err := os.WriteFile(filepath.Join(bin, "phenix-scorch-component-reader"), []byte("#!/bin/sh\ncat >/dev/null\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	phenixBase, mmBase := common.PhenixBase, common.MinimegaBase
	common.PhenixBase = t.TempDir()                              //nolint:reassign // test files directory
	common.MinimegaBase = filepath.Join(t.TempDir(), "minimega") //nolint:reassign // no minimega to reach

	t.Cleanup(func() {
		common.PhenixBase, common.MinimegaBase = phenixBase, mmBase //nolint:reassign // restore globals
	})

	exp := installSOHExperiment(t, false)

	components := scorchmd.ComponentSpecMap{
		"soh1": {Name: "soh1", Type: "soh", Background: true, Metadata: scorchmd.ComponentMetadata{"checks": []string{"files"}}},
		"r1":   {Name: "r1", Type: "reader"},
		"r2":   {Name: "r2", Type: "reader"},
		"r3":   {Name: "r3", Type: "reader"},
	}

	loop := &scorchmd.Loop{Start: []string{"soh1", "r1", "r2", "r3"}, Stop: []string{"soh1"}}

	if err := executor(context.Background(), components, loop, Experiment(*exp), RunID(0)); err != nil {
		t.Fatalf("executor() = %v", err)
	}

	// The soh app marks itself running, then not, in the status it works on.
	if running, ok := exp.Status.AppRunning()["soh"]; ok {
		t.Errorf("the soh component worked on the run's shared status (soh running %t), want a copy of its own", running)
	}
}

// TestSOHAfterStop is a regression test: a soh component that ran after an
// experiment stop, as a run outliving the stop's wait could, ran the state of
// health app on the run's copy of the experiment and wrote its status back,
// start time included, so the stopped experiment read as running again.
func TestSOHAfterStop(t *testing.T) {
	exp := installSOHExperiment(t, false)

	// The stop clears the stored start time; the run's copy still has it.
	s := installExperimentStore(t, store.Config{
		Version:  "phenix.sandia.gov/v1",
		Kind:     "Experiment",
		Metadata: store.ConfigMetadata{Name: "test-exp"},
		Spec:     map[string]any{"experimentName": "test-exp"},
		Status:   map[string]any{"startTime": ""},
	})

	var cmp SOH

	_ = cmp.Init(Experiment(*exp), Name("soh1"), Type("soh"))

	if err := cmp.Stop(context.Background()); err == nil || !strings.Contains(err.Error(), "not running") {
		t.Errorf("Stop() after the experiment stopped = %v, want an error saying it is not running", err)
	}

	if start, _ := s.load().Status["startTime"].(string); start != "" {
		t.Errorf("after the soh component, the stored start time is %q, want it still cleared", start)
	}
}
