package experiment_test

import (
	"strings"
	"testing"

	"phenix/api/config"
	"phenix/store"
)

const runningStart = "2026-10-19T10:00:00Z"

// runningTestConfig returns the start test experiment as a run that started
// at runningStart stores it.
func runningTestConfig(t *testing.T) store.Config {
	t.Helper()

	cfg := startTestConfig(t, false, false)
	cfg.Status = map[string]any{"startTime": runningStart}

	return cfg
}

// TestUpdateRefusesStoredRunningExperiment is a regression test: the update
// hook checked the status of the config it was given, not the stored one, so
// a config update of a running experiment whose body had no status was
// accepted and stored the experiment as stopped while its VMs kept running.
func TestUpdateRefusesStoredRunningExperiment(t *testing.T) {
	useBoltStore(t, runningTestConfig(t))

	c, _ := store.NewConfig("experiment/" + startTestExp)
	if err := store.Get(c); err != nil {
		t.Fatalf("getting experiment: %v", err)
	}

	c.Status = nil

	err := config.Update("experiment/"+startTestExp, c)
	if err == nil || !strings.Contains(err.Error(), "cannot update running experiment") {
		t.Errorf("updating running experiment returned %v, want it refused", err)
	}

	if got := storedStart(t); got != runningStart {
		t.Errorf("stored start time after the refused update = %q, want %q", got, runningStart)
	}
}

// TestForcedUpdateOfRunningExperiment checks that phenix config edit --force,
// which passes UpdateWithForce, still replaces the config of a running
// experiment.
func TestForcedUpdateOfRunningExperiment(t *testing.T) {
	useBoltStore(t, runningTestConfig(t))

	c, _ := store.NewConfig("experiment/" + startTestExp)
	if err := store.Get(c); err != nil {
		t.Fatalf("getting experiment: %v", err)
	}

	c.Status = nil

	if err := config.Update("experiment/"+startTestExp, c, config.UpdateWithForce(true)); err != nil {
		t.Fatalf("forced update of running experiment: %v", err)
	}

	if got := storedStart(t); got != "" {
		t.Errorf("stored start time after the forced update = %q, want the body's empty status", got)
	}
}

// TestForcedUpdateKeepsRunningStatus is a regression test: the update hook
// refused any body whose own status said the experiment ran, so phenix config
// edit --force, which edits the stored config with its status, never updated
// a running experiment.
func TestForcedUpdateKeepsRunningStatus(t *testing.T) {
	useBoltStore(t, runningTestConfig(t))

	c, _ := store.NewConfig("experiment/" + startTestExp)
	if err := store.Get(c); err != nil {
		t.Fatalf("getting experiment: %v", err)
	}

	c.Metadata.Annotations = map[string]string{"edited": "yes"}

	if err := config.Update("experiment/"+startTestExp, c, config.UpdateWithForce(true)); err != nil {
		t.Fatalf("forced update of running experiment: %v", err)
	}

	if got := storedStart(t); got != runningStart {
		t.Errorf("stored start time after the forced update = %q, want %q", got, runningStart)
	}

	stored, _ := store.NewConfig("experiment/" + startTestExp)
	if err := store.Get(stored); err != nil {
		t.Fatalf("getting experiment: %v", err)
	}

	if got := stored.Metadata.Annotations["edited"]; got != "yes" {
		t.Errorf("annotation after the forced update = %q, want %q", got, "yes")
	}
}

// TestUpdateRefusesRunningBody checks that an update whose body says the
// experiment runs is refused while the stored experiment is stopped.
func TestUpdateRefusesRunningBody(t *testing.T) {
	useBoltStore(t, startTestConfig(t, false, false))

	c, _ := store.NewConfig("experiment/" + startTestExp)
	if err := store.Get(c); err != nil {
		t.Fatalf("getting experiment: %v", err)
	}

	c.Status = map[string]any{"startTime": runningStart}

	err := config.Update("experiment/"+startTestExp, c)
	if err == nil || !strings.Contains(err.Error(), "cannot update running experiment") {
		t.Errorf("updating with a running status returned %v, want it refused", err)
	}

	if got := storedStart(t); got != "" {
		t.Errorf("stored start time after the refused update = %q, want the experiment stopped", got)
	}
}

// TestForcedUpdateRefusesRenamingRunningExperiment checks that a forced update
// does not rename a running experiment, whose VMs would stay in the minimega
// namespace of the old name.
func TestForcedUpdateRefusesRenamingRunningExperiment(t *testing.T) {
	useBoltStore(t, runningTestConfig(t))

	c, _ := store.NewConfig("experiment/" + startTestExp)
	if err := store.Get(c); err != nil {
		t.Fatalf("getting experiment: %v", err)
	}

	c.Metadata.Name = "renamed"

	err := config.Update("experiment/"+startTestExp, c, config.UpdateWithForce(true))
	if err == nil || !strings.Contains(err.Error(), "cannot rename running experiment") {
		t.Errorf("forced rename of running experiment returned %v, want it refused", err)
	}

	if got := storedStart(t); got != runningStart {
		t.Errorf("stored start time after the refused rename = %q, want %q", got, runningStart)
	}
}
