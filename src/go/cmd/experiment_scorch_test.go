package cmd

import (
	"testing"

	"phenix/api/experiment"
	"phenix/util/mm/mmtest"
	"phenix/util/plog/plogtest"
)

// TestExpCmdScorchRun checks that experiment scorch runs the Scorch run that
// --run names in a running experiment, logs that it ran and leaves Scorch
// stopped, and refuses a run that the configuration does not have.
func TestExpCmdScorchRun(t *testing.T) { //nolint:paralleltest // replaces the phenix store and minimega
	tests := []struct {
		run       string
		humanized string // "" for success
	}{
		{run: "1"},
		{run: "2", humanized: "Unable to start Scorch run 2 for exp experiment"},
	}

	for _, tt := range tests {
		t.Run("run "+tt.run, func(t *testing.T) {
			cfg := vmCmdExperiment(t, "exp", true)
			cfg.Spec["scenario"] = map[string]any{"apps": []any{map[string]any{
				"name":     "scorch",
				"metadata": map[string]any{"components": []any{}, "runs": []any{map[string]any{}, map[string]any{}}},
			}}}

			expCmdUseStore(t, cfg)
			mmtest.Use(t, nil)

			logs := plogtest.Capture(t)
			err := expCmdRun("scorch", "exp", "--run", tt.run)

			if msg := expCmdCheckErr(err, "", tt.humanized); msg != "" {
				t.Fatalf("phenix experiment scorch exp --run %s %s", tt.run, msg)
			}

			exp, err := experiment.Get("exp")
			if err != nil {
				t.Fatalf("experiment.Get(exp) error = %v", err)
			}

			if _, ok := exp.Status.AppStatus()["scorch"]; ok || exp.Status.AppRunning()["scorch"] {
				t.Errorf("Scorch after the run has status %t, running %t; want neither", ok, exp.Status.AppRunning()["scorch"])
			}

			records := logs.Records(t, plogtest.Message("scorch run triggered"))
			if want := map[bool]int{true: 1, false: 0}[tt.humanized == ""]; len(records) != want {
				t.Errorf(
					"phenix experiment scorch exp --run %s logged %d scorch run triggered records, want %d",
					tt.run,
					len(records),
					want,
				)
			}
		})
	}
}
