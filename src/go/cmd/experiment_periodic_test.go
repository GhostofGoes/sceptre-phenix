package cmd

import (
	"bytes"
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"phenix/app"
	"phenix/store"
	"phenix/types"
	"phenix/util/mm"
)

const cliPeriodicApp = "cli-periodic-test-app"

// cliPeriodicAppOnce registers cliPeriodicApp once; cliPeriodicRuns counts
// its running stages.
//
//nolint:gochecknoglobals // the app registry is global
var (
	cliPeriodicAppOnce sync.Once
	cliPeriodicRuns    struct {
		mu sync.Mutex
		n  int
	}
)

type cliPeriodicTestApp struct{}

func (cliPeriodicTestApp) Init(...app.Option) error                           { return nil }
func (cliPeriodicTestApp) Name() string                                       { return cliPeriodicApp }
func (cliPeriodicTestApp) Configure(context.Context, *types.Experiment) error { return nil }
func (cliPeriodicTestApp) PreStart(context.Context, *types.Experiment) error  { return nil }
func (cliPeriodicTestApp) PostStart(context.Context, *types.Experiment) error { return nil }
func (cliPeriodicTestApp) Cleanup(context.Context, *types.Experiment) error   { return nil }

func (cliPeriodicTestApp) Running(context.Context, *types.Experiment) error {
	cliPeriodicRuns.mu.Lock()
	defer cliPeriodicRuns.mu.Unlock()

	cliPeriodicRuns.n++

	return nil
}

// TestExperimentStartRunsPeriodicAppsForTheNewRun is a regression test:
// phenix experiment start --honor-run-periodically gave the periodic apps the
// experiment it read before starting it, whose status writes then replaced
// the new run's start time with the old one (none, or a dry run's), so the
// started experiment was stored stopped, or as a dry run whose stop leaked its
// VMs.
func TestExperimentStartRunsPeriodicAppsForTheNewRun(t *testing.T) {
	cliPeriodicAppOnce.Do(func() {
		if err := app.RegisterUserApp(cliPeriodicApp, func() app.App { return cliPeriodicTestApp{} }); err != nil {
			t.Fatalf("registering test app: %v", err)
		}
	})

	cliPeriodicRuns.mu.Lock()
	cliPeriodicRuns.n = 0
	cliPeriodicRuns.mu.Unlock()

	const name = "periodic-exp"

	cfg := newRunningDryRunExperimentConfig(name, t.TempDir())
	cfg.Spec["scenario"] = map[string]any{
		"apps": []map[string]any{{"name": cliPeriodicApp, "runPeriodically": "1s"}},
	}

	originalMM, originalStore := mm.DefaultMM, store.DefaultStore
	t.Cleanup(func() {
		mm.DefaultMM = originalMM          //nolint:reassign // restore test double
		store.DefaultStore = originalStore //nolint:reassign // restore test double
	})

	mm.DefaultMM = fakeMM{} //nolint:reassign // install test double

	db := store.NewBoltDB()
	if err := db.Init(store.Endpoint("bolt://" + filepath.Join(t.TempDir(), "store.bdb"))); err != nil {
		t.Fatalf("initializing BoltDB: %v", err)
	}

	store.DefaultStore = db //nolint:reassign // install test store

	if err := store.Create(&cfg); err != nil {
		t.Fatalf("storing experiment: %v", err)
	}

	root := &cobra.Command{Use: "phenix", SilenceUsage: true}
	experimentCmd := newExperimentCmd()
	experimentCmd.AddCommand(newExperimentStartCmd())
	root.AddCommand(experimentCmd)
	root.SetArgs([]string{"experiment", "start", "--dry-run", "--honor-run-periodically", name})

	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&output)

	done := make(chan error, 1)

	go func() {
		_, err := root.ExecuteC()
		done <- err
	}()

	deadline := time.Now().Add(5 * time.Second)

	for {
		cliPeriodicRuns.mu.Lock()
		n := cliPeriodicRuns.n
		cliPeriodicRuns.mu.Unlock()

		if n > 0 {
			break
		}

		if time.Now().After(deadline) {
			t.Fatalf("the periodic app never ran for the started experiment\n%s", output.String())
		}

		time.Sleep(5 * time.Millisecond)
	}

	c, _ := store.NewConfig("experiment/" + name)
	if err := store.Get(c); err != nil {
		t.Fatalf("getting experiment: %v", err)
	}

	if start, _ := c.Status["startTime"].(string); start == "2024-01-01T00:00:00-DRYRUN" {
		t.Errorf("stored start time %q, want the new run's", start)
	}

	// Stop it, as phenix experiment stop from another shell does; the command
	// then returns.
	if err := store.Modify(c, func(c *store.Config) error {
		c.Status["startTime"] = ""

		return nil
	}); err != nil {
		t.Fatalf("stopping the experiment: %v", err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("starting the experiment: %v\n%s", err, output.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("phenix experiment start kept running the periodic app after the experiment stopped")
	}
}
