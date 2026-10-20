package soh

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"phenix/app"
	"phenix/store"
	"phenix/types"
	"phenix/util/mm"
)

// TestSOHStartupDelayEndsWithTheStart is a regression test: the soh app's
// post-start stage slept through its startupDelay whatever its context, so a
// stop of the starting experiment waited for it until it gave up, and the app
// then ran its checks, whose commands made minimega create the cleared
// namespace again.
func TestSOHStartupDelayEndsWithTheStart(t *testing.T) {
	original := mm.DefaultMM

	t.Cleanup(func() { mm.DefaultMM = original }) //nolint:reassign // restore test double

	mm.DefaultMM = dryRunMM{} //nolint:reassign // install test double

	exp, err := types.DecodeExperimentFromConfig(store.Config{
		Version:  "phenix.sandia.gov/v1",
		Kind:     "Experiment",
		Metadata: store.ConfigMetadata{Name: "soh-delay"},
		Spec: map[string]any{
			"experimentName": "soh-delay",
			"scenario": map[string]any{
				"apps": []map[string]any{{"name": "soh", "metadata": map[string]any{"startupDelay": "1h"}}},
			},
		},
		Status: map[string]any{"startTime": "2026-10-19T00:00:00Z"},
	})
	if err != nil {
		t.Fatalf("decoding experiment: %v", err)
	}

	s := newSOH()

	if err := s.Init(app.Name("soh")); err != nil {
		t.Fatalf("Init: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)

	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- fmt.Errorf("reached minimega: %v", r)
			}
		}()

		done <- s.PostStart(ctx, exp)
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("the canceled post-start stage returned %v, want the cancellation", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the post-start stage still waits its startup delay after the start was canceled")
	}
}

// TestSOHPostStartEndsWithAStopElsewhere is a regression test: the soh app's
// startup delay ended only when the start's context was canceled, which a
// stop from another process, such as phenix experiment stop while phenix ui
// starts the experiment, does not do, so the app ran its checks once the
// delay passed and their commands made minimega create the cleared namespace
// again.
func TestSOHPostStartEndsWithAStopElsewhere(t *testing.T) {
	originalMM, originalStore, originalInterval := mm.DefaultMM, store.DefaultStore, storedRunInterval

	t.Cleanup(func() {
		mm.DefaultMM = originalMM          //nolint:reassign // restore test double
		store.DefaultStore = originalStore //nolint:reassign // restore default
		storedRunInterval = originalInterval
	})

	mm.DefaultMM = dryRunMM{} //nolint:reassign // install test double
	storedRunInterval = 10 * time.Millisecond

	db := store.NewBoltDB()
	if err := db.Init(store.Endpoint("bolt://" + filepath.Join(t.TempDir(), "store.bdb"))); err != nil {
		t.Fatalf("initializing BoltDB: %v", err)
	}

	store.DefaultStore = db //nolint:reassign // install test store

	cfg := store.Config{
		Version:  "phenix.sandia.gov/v1",
		Kind:     "Experiment",
		Metadata: store.ConfigMetadata{Name: "soh-elsewhere"},
		Spec: map[string]any{
			"experimentName": "soh-elsewhere",
			"scenario": map[string]any{
				"apps": []map[string]any{{"name": "soh", "metadata": map[string]any{"startupDelay": "1h"}}},
			},
		},
		Status: map[string]any{"startTime": "2026-10-19T00:00:00Z"},
	}

	if err := store.Create(&cfg); err != nil {
		t.Fatalf("storing experiment: %v", err)
	}

	exp, err := types.DecodeExperimentFromConfig(cfg)
	if err != nil {
		t.Fatalf("decoding experiment: %v", err)
	}

	s := newSOH()

	if err := s.Init(app.Name("soh")); err != nil {
		t.Fatalf("Init: %v", err)
	}

	done := make(chan error, 1)

	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- fmt.Errorf("reached minimega: %v", r)
			}
		}()

		done <- s.PostStart(context.Background(), exp)
	}()

	time.Sleep(50 * time.Millisecond)

	// The other process's stop.
	c, _ := store.NewConfig("experiment/soh-elsewhere")

	if err := store.Modify(c, func(c *store.Config) error {
		c.Status["startTime"] = ""

		return nil
	}); err != nil {
		t.Fatalf("storing the stop: %v", err)
	}

	select {
	case err := <-done:
		if !errors.Is(err, types.ErrExperimentRunChanged) {
			t.Errorf("the post-start stage of an experiment stopped elsewhere returned %v, want ErrExperimentRunChanged", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the post-start stage still waits its startup delay after another process stopped the experiment")
	}
}
