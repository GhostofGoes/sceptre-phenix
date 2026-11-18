package experiment_test

import (
	"context"
	"testing"

	"phenix/api/experiment"
	ifaces "phenix/types/interfaces"
	"phenix/util/notes"
)

// TestSyncStartKeepsStatusStoredDuringPostStart is a regression test: a
// synchronous start stored its whole in-memory status once the post-start
// apps ended, although each app's stage stores its own status changes, so
// what another process stored meanwhile was reverted, such as a SCORCH run's
// running flag, which then no longer refused a second run.
func TestSyncStartKeepsStatusStoredDuringPostStart(t *testing.T) {
	fake := new(startMM)
	installStartDoubles(t, fake, startTestConfig(t, false, false))
	useBoltStore(t, startTestConfig(t, false, false))

	rec := new(startRecord)
	rec.onPostStart = func() {
		// Another process, such as phenix ui running a SCORCH run.
		other, err := experiment.Get(startTestExp)
		if err != nil {
			t.Errorf("getting the experiment as another process: %v", err)

			return
		}

		err = other.UpdateStatus(func(status ifaces.ExperimentStatus) error {
			status.SetAppStatus("other-app", "stored elsewhere")
			status.SetAppRunning("scorch", true)

			return nil
		})
		if err != nil {
			t.Errorf("storing status as another process: %v", err)
		}
	}

	registerStartApp(t, rec)

	err := experiment.Start(
		notes.Context(context.Background(), false),
		experiment.StartWithName(startTestExp),
	)
	if err != nil {
		t.Fatalf("Start returned %v", err)
	}

	exp, err := experiment.Get(startTestExp)
	if err != nil {
		t.Fatalf("getting experiment: %v", err)
	}

	if got := exp.Status.AppStatus()["other-app"]; got != "stored elsewhere" {
		t.Errorf("stored status of other-app = %v, want what the other process stored", got)
	}

	if !exp.Status.AppRunning()["scorch"] {
		t.Error("the start cleared the running flag another process stored during post-start")
	}

	if !exp.Running() {
		t.Error("the experiment is not stored running after the start")
	}
}
