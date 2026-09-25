package scorch

import (
	"context"
	"slices"
	"testing"

	"phenix/api/scorch/scorchmd"
	"phenix/store"
	"phenix/types"
	v1 "phenix/types/version/v1"
)

// recorder records which lifecycle stages it was called for.
type recorder struct {
	calls *[]Action
}

func (recorder) Init(...Option) error { return nil }
func (recorder) Type() string         { return "test-recorder" }

func (r recorder) Configure(context.Context) error {
	*r.calls = append(*r.calls, ActionConfigure)

	return nil
}

func (r recorder) Start(context.Context) error {
	*r.calls = append(*r.calls, ActionStart)

	return nil
}

func (r recorder) Stop(context.Context) error {
	*r.calls = append(*r.calls, ActionStop)

	return nil
}

func (r recorder) Cleanup(context.Context) error {
	*r.calls = append(*r.calls, ActionCleanup)

	return nil
}

func TestExecutorCleanupOnly(t *testing.T) {
	for _, tc := range []struct {
		name        string
		cleanupOnly bool
		want        []Action
	}{
		{"whole run", false, []Action{ActionConfigure, ActionStart, ActionStop, ActionCleanup}},
		{"cleanup only", true, []Action{ActionCleanup}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls []Action

			components["test-recorder"] = recorder{calls: &calls}
			t.Cleanup(func() { delete(components, "test-recorder") })

			specs := scorchmd.ComponentSpecMap{
				"rec": {Name: "rec", Type: "test-recorder"},
			}

			run := &scorchmd.Loop{
				Configure: []string{"rec"},
				Start:     []string{"rec"},
				Stop:      []string{"rec"},
				Cleanup:   []string{"rec"},
			}

			exp := types.Experiment{
				Metadata: store.ConfigMetadata{Name: "cleanup-test"},
				Spec:     &v1.ExperimentSpec{ExperimentNameF: "cleanup-test"},
			}

			opts := []Option{Experiment(exp), RunID(0), LoopCount(0)}
			if tc.cleanupOnly {
				opts = append(opts, CleanupOnly())
			}

			if err := executor(context.Background(), specs, run, opts...); err != nil {
				t.Fatalf("executor: %v", err)
			}

			if !slices.Equal(calls, tc.want) {
				t.Fatalf("stages run = %v, want %v", calls, tc.want)
			}
		})
	}
}
