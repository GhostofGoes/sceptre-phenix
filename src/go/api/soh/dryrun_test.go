package soh

import (
	"context"
	"fmt"
	"testing"
	"time"

	"phenix/app"
	"phenix/store"
	"phenix/types"
	"phenix/util/mm"
)

// dryRunMM is a minimega double whose every method panics, so a call reaching
// it fails the test.
type dryRunMM struct {
	mm.MM
}

// TestSOHLeavesMinimegaAloneInADryRun is a regression test: the soh app's
// cleanup cleared C2 responses, and its running stage ran its checks, in the
// experiment's minimega namespace even for a dry run, so stopping a dry run, or
// triggering its running stage, made minimega create the namespace the dry run
// never had.
func TestSOHLeavesMinimegaAloneInADryRun(t *testing.T) {
	original := mm.DefaultMM

	t.Cleanup(func() { mm.DefaultMM = original }) //nolint:reassign // restore test double

	mm.DefaultMM = dryRunMM{} //nolint:reassign // install test double

	exp, err := types.DecodeExperimentFromConfig(store.Config{
		Version:  "phenix.sandia.gov/v1",
		Kind:     "Experiment",
		Metadata: store.ConfigMetadata{Name: "soh-dry"},
		Spec: map[string]any{
			"experimentName": "soh-dry",
			"scenario": map[string]any{
				"apps": []map[string]any{{"name": "soh", "metadata": map[string]any{"c2Timeout": "1s"}}},
			},
		},
		Status: map[string]any{"startTime": "2026-10-18T00:00:00Z-DRYRUN"},
	})
	if err != nil {
		t.Fatalf("decoding experiment: %v", err)
	}

	s := newSOH()

	if err := s.Init(app.DryRun(true)); err != nil {
		t.Fatalf("Init: %v", err)
	}

	stages := []struct {
		name string
		run  func(context.Context, *types.Experiment) error
	}{
		{"cleanup", s.Cleanup},
		{"running", s.Running},
	}

	for _, stage := range stages {
		done := make(chan error, 1)

		go func() {
			defer func() {
				if r := recover(); r != nil {
					done <- fmt.Errorf("reached minimega: %v", r)
				}
			}()

			done <- stage.run(context.Background(), exp)
		}()

		select {
		case err := <-done:
			if err != nil {
				t.Errorf("the %s stage of a dry run: %v", stage.name, err)
			}
		case <-time.After(10 * time.Second):
			t.Errorf("the %s stage of a dry run still runs its checks after 10s", stage.name)
		}
	}
}
