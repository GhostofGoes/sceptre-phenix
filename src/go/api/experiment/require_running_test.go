package experiment_test

import (
	"errors"
	"testing"

	"github.com/golang/mock/gomock"

	"phenix/api/experiment"
	"phenix/store"
)

// TestRequireRunning checks the guard VM operations use to stay out of
// minimega, which creates the namespace of any command sent to it, for an
// experiment whose VMs are not there.
func TestRequireRunning(t *testing.T) {
	tests := []struct {
		name      string
		startTime string
		missing   bool
		want      string // the error; "" for none
	}{
		{name: "running", startTime: "2024-01-01T00:00:00Z"},
		{name: "stopped", want: "experiment not running: require-test"},
		{name: "dry run", startTime: "2024-01-01T00:00:00Z-DRYRUN", want: "experiment not running: require-test is a dry run"},
		{name: "missing", missing: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := triggerTestConfig("require-test")
			cfg.Status["startTime"] = tt.startTime

			m := store.NewMockStore(gomock.NewController(t))
			m.EXPECT().Get(gomock.Any()).DoAndReturn(func(c *store.Config) error {
				if tt.missing {
					return store.ErrNotExist
				}

				*c = cfg

				return nil
			}).AnyTimes()

			original := store.DefaultStore

			t.Cleanup(func() { store.DefaultStore = original }) //nolint:reassign // restore test double

			store.DefaultStore = m //nolint:reassign // install test double

			err := experiment.RequireRunning("require-test")

			switch {
			case tt.missing:
				if !errors.Is(err, store.ErrNotExist) {
					t.Fatalf("RequireRunning = %v, want the store's not-exist error", err)
				}
			case tt.want == "":
				if err != nil {
					t.Fatalf("RequireRunning = %v, want nil", err)
				}
			default:
				if !errors.Is(err, experiment.ErrExperimentNotRunning) || err.Error() != tt.want {
					t.Fatalf("RequireRunning = %v, want %q wrapping ErrExperimentNotRunning", err, tt.want)
				}
			}
		})
	}
}
