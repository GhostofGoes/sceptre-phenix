package soh

import (
	"context"
	"fmt"
	"testing"

	"phenix/app"
	"phenix/store"
	"phenix/types"
	"phenix/util/mm"
)

// TestSOHCleanupOfARefusedStartLeavesMinimegaAlone is a regression test: a
// start refused after its pre-start apps, before it stored its run or sent
// minimega anything, ran the soh app's cleanup, which cleared C2 responses in
// the experiment's namespace and so made minimega create it; nothing cleared
// it afterwards. The stored experiment is then stopped, or a dry run that a
// real start replaces.
func TestSOHCleanupOfARefusedStartLeavesMinimegaAlone(t *testing.T) {
	original := mm.DefaultMM

	t.Cleanup(func() { mm.DefaultMM = original }) //nolint:reassign // restore test double

	mm.DefaultMM = dryRunMM{} //nolint:reassign // install test double

	for _, start := range []string{"", "2026-10-18T00:00:00Z-DRYRUN"} {
		t.Run(fmt.Sprintf("stored start %q", start), func(t *testing.T) {
			exp, err := types.DecodeExperimentFromConfig(store.Config{
				Version:  "phenix.sandia.gov/v1",
				Kind:     "Experiment",
				Metadata: store.ConfigMetadata{Name: "soh-refused"},
				Spec: map[string]any{
					"experimentName": "soh-refused",
					"scenario":       map[string]any{"apps": []map[string]any{{"name": "soh"}}},
				},
				Status: map[string]any{"startTime": start},
			})
			if err != nil {
				t.Fatalf("decoding experiment: %v", err)
			}

			s := newSOH()

			if err := s.Init(app.Name("soh")); err != nil {
				t.Fatalf("Init: %v", err)
			}

			defer func() {
				if r := recover(); r != nil {
					t.Errorf("the cleanup reached minimega: %v", r)
				}
			}()

			if err := s.Cleanup(context.Background(), exp); err != nil {
				t.Errorf("Cleanup: %v", err)
			}
		})
	}
}
