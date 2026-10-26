package experiment_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/activeshadow/structs"
	"github.com/golang/mock/gomock"

	"phenix/api/experiment"
	"phenix/app"
	"phenix/store"
	"phenix/types"
	"phenix/util/common"
	"phenix/util/notes"
)

// TestStartSyncGivesUpWhenStoppedElsewhere is a regression test: phenix
// experiment start waits for delayed VMs, and a stop from another process
// cleared the namespace under it; the wait then sent its VM start to minimega,
// which created the namespace again, cleared it a second time and stopped the
// experiment again, or a later run of it. The start reports the stop and sends
// nothing more to minimega.
func TestStartSyncGivesUpWhenStoppedElsewhere(t *testing.T) {
	startAppOnce.Do(func() {
		err := app.RegisterUserApp(startTestApp, func() app.App { return startApp{rec: startAppRec} })
		if err != nil {
			t.Fatalf("registering test app: %v", err)
		}
	})

	rec := new(startRecord)
	startAppRec = rec

	fake := new(startMM)

	cfg := startTestConfig(t, false, true)

	topo, _ := cfg.Spec["topology"].(map[string]any)
	nodes, _ := topo["nodes"].([]map[string]any)
	nodes[1]["delay"] = map[string]any{"timer": "1s"}

	installStartDoubles(t, fake, cfg)

	// Another process stops the experiment as soon as the start stores it
	// running.
	var (
		mu      sync.Mutex
		current = cfg
		m       = store.NewMockStore(gomock.NewController(t))
	)

	m.EXPECT().Get(gomock.Any()).DoAndReturn(func(c *store.Config) error {
		mu.Lock()
		defer mu.Unlock()

		*c = current

		return nil
	}).AnyTimes()
	m.EXPECT().Update(gomock.Any()).DoAndReturn(func(c *store.Config) error {
		mu.Lock()
		defer mu.Unlock()

		exp, err := types.DecodeExperimentFromConfig(*c)
		if err != nil {
			t.Errorf("decoding stored experiment: %v", err)

			return err
		}

		exp.Status.SetStartTime("")

		current = *c
		current.Status = structs.MapDefaultCase(exp.Status, structs.CASESNAKE)

		return nil
	}).AnyTimes()

	store.DefaultStore = m //nolint:reassign // install test double, restored by installStartDoubles

	// A VM start the wait must not send fails here instead of reaching a
	// minimega that runs on this host.
	originalBase := common.MinimegaBase

	t.Cleanup(func() { common.MinimegaBase = originalBase }) //nolint:reassign // restore test fixture

	common.MinimegaBase = t.TempDir() //nolint:reassign // test fixture

	err := experiment.Start(notes.Context(context.Background(), false), experiment.StartWithName(startTestExp))

	if err == nil || !strings.Contains(err.Error(), "experiment "+startTestExp+" was stopped while it was starting") {
		t.Fatalf("Start returned %v, want it to report the stop from elsewhere", err)
	}

	if strings.Contains(err.Error(), "starting VM timer") {
		t.Errorf("Start sent the timer VM's start to minimega after the stop: %v", err)
	}

	if fake.cleared != 0 {
		t.Errorf("the namespace was cleared %d times, want none: the stop from elsewhere cleared it", fake.cleared)
	}

	if rec.cleanups != 0 {
		t.Errorf("cleanup apps ran %d times, want none: the stop from elsewhere ran them", rec.cleanups)
	}
}
