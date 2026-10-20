package types_test

import (
	"errors"
	"path/filepath"
	"testing"

	"phenix/store"
	"phenix/types"
	ifaces "phenix/types/interfaces"
)

// useBoltStore makes a BoltDB in a temporary directory the store, holding the
// given configs, and restores the original store when the test ends.
func useBoltStore(t *testing.T, configs ...store.Config) {
	t.Helper()

	original := store.DefaultStore
	t.Cleanup(func() { store.DefaultStore = original }) //nolint:reassign // restore default

	db := store.NewBoltDB()

	if err := db.Init(store.Endpoint("bolt://" + filepath.Join(t.TempDir(), "store.bdb"))); err != nil {
		t.Fatalf("initializing BoltDB: %v", err)
	}

	store.DefaultStore = db //nolint:reassign // install test store

	for _, c := range configs {
		if err := store.Create(&c); err != nil {
			t.Fatalf("storing %s: %v", c.Metadata.Name, err)
		}
	}
}

func storedExperiment(t *testing.T, name string) *types.Experiment {
	t.Helper()

	c, _ := store.NewConfig("experiment/" + name)
	if err := store.Get(c); err != nil {
		t.Fatalf("getting experiment %s: %v", name, err)
	}

	exp, err := types.DecodeExperimentFromConfig(*c)
	if err != nil {
		t.Fatalf("decoding experiment %s: %v", name, err)
	}

	return exp
}

// TestWriteToStoreKeepsTheStoredRun is a regression test: a status write from
// a copy of the experiment read before another process stopped it, such as an
// app's around its post-start stage or a periodic app's, wrote the old start
// time back, so the stopped experiment was stored running again with no VMs,
// and a copy from before a restart replaced the new run's start time.
func TestWriteToStoreKeepsTheStoredRun(t *testing.T) {
	const name = "write-run-test"

	tests := []struct {
		name    string
		stored  string // the stored start time
		read    string // the start time of the copy being written
		refused bool
	}{
		{name: "same run", stored: "T1", read: "T1"},
		{name: "stopped experiment", stored: "", read: ""},
		{name: "stopped since it was read", stored: "", read: "T1", refused: true},
		{name: "started again since it was read", stored: "T2", read: "T1", refused: true},
		{name: "started since it was read", stored: "T1", read: "", refused: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			useBoltStore(t, store.Config{
				Version:  "phenix.sandia.gov/v1",
				Kind:     "Experiment",
				Metadata: store.ConfigMetadata{Name: name},
				Spec:     map[string]any{"experimentName": name},
				Status:   map[string]any{"startTime": tt.stored},
			})

			exp := storedExperiment(t, name)
			exp.Status.SetStartTime(tt.read)
			exp.Status.SetAppRunning("app", true)

			err := exp.WriteToStore(true)

			if tt.refused != errors.Is(err, types.ErrExperimentRunChanged) {
				t.Fatalf("WriteToStore() returned %v, want it refused: %t", err, tt.refused)
			}

			if !tt.refused && err != nil {
				t.Fatalf("WriteToStore() returned %v", err)
			}

			stored := storedExperiment(t, name)

			if got := stored.Status.StartTime(); got != tt.stored {
				t.Errorf("stored start time %q after the write, want %q", got, tt.stored)
			}

			if got := stored.Status.AppRunning()["app"]; got == tt.refused {
				t.Errorf("stored app running %t after the write, want %t", got, !tt.refused)
			}
		})
	}
}

// TestUpdateStatusKeepsWhatOthersStored checks that UpdateStatus changes only
// what its change sets, keeping a status another writer stored since the copy
// was read, and refuses, like WriteToStore, a run that changed since.
func TestUpdateStatusKeepsWhatOthersStored(t *testing.T) {
	const name = "update-status-test"

	useBoltStore(t, store.Config{
		Version:  "phenix.sandia.gov/v1",
		Kind:     "Experiment",
		Metadata: store.ConfigMetadata{Name: name},
		Spec:     map[string]any{"experimentName": name},
		Status:   map[string]any{"startTime": "T1"},
	})

	exp := storedExperiment(t, name)

	// Another writer stores its status after exp was read.
	other := storedExperiment(t, name)
	other.Status.SetAppStatus("other", "kept")

	if err := other.WriteToStore(true); err != nil {
		t.Fatalf("WriteToStore() returned %v", err)
	}

	err := exp.UpdateStatus(func(status ifaces.ExperimentStatus) error {
		status.SetAppRunning("app", true)

		return nil
	})
	if err != nil {
		t.Fatalf("UpdateStatus() returned %v", err)
	}

	for _, status := range []ifaces.ExperimentStatus{storedExperiment(t, name).Status, exp.Status} {
		if got := status.AppStatus()["other"]; got != "kept" || !status.AppRunning()["app"] {
			t.Errorf("status apps %v, running %v, want both writers' changes", status.AppStatus(), status.AppRunning())
		}
	}

	refused := errors.New("refused")

	if err := exp.UpdateStatus(func(status ifaces.ExperimentStatus) error {
		status.SetAppRunning("app", false)

		return refused
	}); !errors.Is(err, refused) || !storedExperiment(t, name).Status.AppRunning()["app"] {
		t.Errorf("UpdateStatus() with a failing change returned %v, want it and the status unchanged", err)
	}

	exp.Status.SetStartTime("T0")

	if err := exp.UpdateStatus(func(ifaces.ExperimentStatus) error { return nil }); !errors.Is(err, types.ErrExperimentRunChanged) {
		t.Errorf("UpdateStatus() of an older run returned %v, want it refused", err)
	}
}

// TestWriteSpecToStoreLeavesTheStatusAlone checks that WriteSpecToStore stores
// the spec and keeps a status another writer stored since the copy was read,
// and refuses, like WriteToStore, a run that changed since.
func TestWriteSpecToStoreLeavesTheStatusAlone(t *testing.T) {
	const name = "write-spec-test"

	useBoltStore(t, store.Config{
		Version:  "phenix.sandia.gov/v1",
		Kind:     "Experiment",
		Metadata: store.ConfigMetadata{Name: name},
		Spec:     map[string]any{"experimentName": name},
		Status:   map[string]any{"startTime": "T1"},
	})

	exp := storedExperiment(t, name)
	exp.Spec.SetDefaultBridge("changed")
	exp.Status.SetAppStatus("stale", "dropped")

	// Another writer stores its status after exp was read.
	other := storedExperiment(t, name)
	other.Status.SetAppStatus("other", "kept")

	if err := other.WriteToStore(true); err != nil {
		t.Fatalf("WriteToStore() returned %v", err)
	}

	if err := exp.WriteSpecToStore(); err != nil {
		t.Fatalf("WriteSpecToStore() returned %v", err)
	}

	stored := storedExperiment(t, name)

	if got := stored.Spec.DefaultBridge(); got != "changed" {
		t.Errorf("stored default bridge %q, want the spec written", got)
	}

	if got := stored.Status.AppStatus(); got["other"] != "kept" || got["stale"] != nil {
		t.Errorf("stored status apps %v, want only the other writer's status", got)
	}

	exp.Status.SetStartTime("T0")

	if err := exp.WriteSpecToStore(); !errors.Is(err, types.ErrExperimentRunChanged) {
		t.Errorf("WriteSpecToStore() of an older run returned %v, want it refused", err)
	}
}
