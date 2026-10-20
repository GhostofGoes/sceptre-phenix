package scorchexe_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"phenix/api/scorch/scorchexe"
	"phenix/store"
	"phenix/types"
)

// TestExecuteRefusesARunStartedSinceTheCopyWasRead is a regression test:
// Execute checked the running flag of its copy of the experiment and then
// wrote the whole status with the flag set, so a run started from another
// process after the copy was read was not refused, and the write lost what
// was stored since.
func TestExecuteRefusesARunStartedSinceTheCopyWasRead(t *testing.T) {
	const name = "execute-flag-test"

	original := store.DefaultStore
	t.Cleanup(func() { store.DefaultStore = original }) //nolint:reassign // restore default

	db := store.NewBoltDB()

	if err := db.Init(store.Endpoint("bolt://" + filepath.Join(t.TempDir(), "store.bdb"))); err != nil {
		t.Fatalf("initializing BoltDB: %v", err)
	}

	store.DefaultStore = db //nolint:reassign // install test store

	c := store.Config{
		Version:  "phenix.sandia.gov/v1",
		Kind:     "Experiment",
		Metadata: store.ConfigMetadata{Name: name},
		Spec: map[string]any{
			"experimentName": name,
			"scenario": map[string]any{
				"apps": []any{map[string]any{
					"name":     "scorch",
					"metadata": map[string]any{"runs": []any{map[string]any{}}},
				}},
			},
		},
		Status: map[string]any{"startTime": "2026-10-19T00:00:00Z"},
	}

	if err := store.Create(&c); err != nil {
		t.Fatalf("storing experiment: %v", err)
	}

	exp := readExperiment(t, name)

	// Another process starts a run, with its record, after exp was read.
	stored, _ := store.NewConfig("experiment/" + name)

	if err := store.Modify(stored, func(c *store.Config) error {
		c.Status["appRunningStageStatus"] = map[string]any{"scorch": true}
		c.Status["apps"] = map[string]any{"scorch": map[string]any{"runID": 0}}

		return nil
	}); err != nil {
		t.Fatalf("starting the other run: %v", err)
	}

	if err := scorchexe.Execute(context.Background(), exp, 0); !errors.Is(err, scorchexe.ErrAlreadyRunning) {
		t.Fatalf("Execute() = %v, want %v", err, scorchexe.ErrAlreadyRunning)
	}

	status := readExperiment(t, name).Status

	if !status.AppRunning()["scorch"] {
		t.Error("the refused run cleared the other run's running flag")
	}

	if _, ok := status.AppStatus()["scorch"]; !ok {
		t.Error("the refused run dropped the other run's record")
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
