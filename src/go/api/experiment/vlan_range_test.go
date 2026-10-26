package experiment_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/golang/mock/gomock"

	apiconfig "phenix/api/config"
	"phenix/api/experiment"
	"phenix/store"
)

// useStore installs m as the store for the test and restores the previous one.
func useStore(t *testing.T, m store.Store) {
	t.Helper()

	previous := store.DefaultStore
	store.DefaultStore = m //nolint:reassign // monkey patching for test

	t.Cleanup(func() { store.DefaultStore = previous }) //nolint:reassign // restore test double
}

// The errors CheckVLANRange returns for a range with one end set, and for a
// min that is not less than the max.
const (
	noMax    = "max VLAN ID is unset while min VLAN ID is %d; set both ends of the VLAN range, or neither"
	noMin    = "min VLAN ID is unset while max VLAN ID is %d; set both ends of the VLAN range, or neither"
	notBelow = "VLAN range %d-%d: min VLAN ID must be less than max VLAN ID, as minimega assigns VLAN IDs below the max"
)

// TestCheckVLANRange covers the VLAN ranges the minimega script does not
// render or minimega would refuse at start. minimega assigns from [min, max),
// so a min is 1-4095, a max 2-4096 and the min below the max, and the script
// renders no range with only one end set.
func TestCheckVLANRange(t *testing.T) {
	tests := []struct {
		name     string
		min, max int
		wantErr  string
	}{
		{name: "unset", min: 0, max: 0},
		{name: "the whole VLAN ID space", min: 1, max: 4096},
		{name: "a single VLAN", min: 350, max: 351},
		{name: "the highest VLAN alone", min: 4095, max: 4096},
		{name: "a low max with a min", min: 10, max: 100},
		{name: "a negative min", min: -1, max: 200, wantErr: "min VLAN ID -1 is outside 1-4095"},
		{name: "a min of 4096", min: 4096, max: 4096, wantErr: "min VLAN ID 4096 is outside 1-4095"},
		{name: "a max above 4096", min: 300, max: 4097, wantErr: "max VLAN ID 4097 is outside 2-4096"},
		{name: "a max of 1", min: 1, max: 1, wantErr: "max VLAN ID 1 is outside 2-4096"},
		{name: "a negative max", max: -5, wantErr: "max VLAN ID -5 is outside 2-4096"},
		{name: "a min alone", min: 320, wantErr: fmt.Sprintf(noMax, 320)},
		{name: "a max alone", max: 330, wantErr: fmt.Sprintf(noMin, 330)},
		{name: "equal ends", min: 350, max: 350, wantErr: fmt.Sprintf(notBelow, 350, 350)},
		{name: "inverted", min: 360, max: 350, wantErr: fmt.Sprintf(notBelow, 360, 350)},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := experiment.CheckVLANRange(test.min, test.max)

			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("CheckVLANRange(%d, %d) = %v, want nil", test.min, test.max, err)
				}

				return
			}

			if err == nil || err.Error() != test.wantErr {
				t.Fatalf("CheckVLANRange(%d, %d) = %v, want %q", test.min, test.max, err, test.wantErr)
			}
		})
	}
}

// TestCreateRejectsInvalidVLANRange proves a range the minimega script does
// not render or minimega would refuse is refused before the store is read, so
// no experiment is created with it, while a valid range or none passes the
// check and goes on to the topology lookup.
func TestCreateRejectsInvalidVLANRange(t *testing.T) {
	tests := []struct {
		name     string
		min, max int
		wantErr  string
	}{
		{name: "inverted", min: 360, max: 350, wantErr: fmt.Sprintf(notBelow, 360, 350)},
		{name: "equal", min: 350, max: 350, wantErr: fmt.Sprintf(notBelow, 350, 350)},
		{name: "max above 4096", min: 360, max: 5000, wantErr: "max VLAN ID 5000 is outside 2-4096"},
		{name: "min alone above 4095", min: 4096, wantErr: "min VLAN ID 4096 is outside 1-4095"},
		{name: "only min", min: 360, wantErr: fmt.Sprintf(noMax, 360)},
		{name: "only max", max: 350, wantErr: fmt.Sprintf(noMin, 350)},
		{name: "ascending", min: 350, max: 360, wantErr: "topology doesn't exist"},
		{name: "up to 4096", min: 4000, max: 4096, wantErr: "topology doesn't exist"},
		{name: "no range", wantErr: "topology doesn't exist"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			m := store.NewMockStore(ctrl)

			if !strings.Contains(test.wantErr, "VLAN ID") {
				m.EXPECT().Get(gomock.Any()).Return(store.ErrNotExist)
			}

			useStore(t, m)

			err := experiment.Create(
				context.Background(),
				experiment.CreateWithName("vinv"),
				experiment.CreateWithTopology("vinv"),
				experiment.CreateWithVLANMin(test.min),
				experiment.CreateWithVLANMax(test.max),
			)
			if err == nil || err.Error() != test.wantErr {
				t.Fatalf("Create() error = %v, want %q", err, test.wantErr)
			}
		})
	}
}

// TestStartRejectsInvalidVLANRange proves a start whose --vlan-min or
// --vlan-max leaves the stored range with its min not below its max, an end
// outside what minimega takes, or only one end set fails before any app runs
// or the spec is saved.
func TestStartRejectsInvalidVLANRange(t *testing.T) {
	const notLess = "min VLAN ID must be less than max VLAN ID"

	var (
		stored  = map[string]any{"min": 300, "max": 310}
		noRange = map[string]any{"aliases": map[string]any{}} // as Init stores it
	)

	tests := []struct {
		name     string
		stored   map[string]any // the stored spec.vlans range
		min, max int
		wantErr  string
	}{
		{name: "min above the stored max", stored: stored, min: 320, wantErr: notLess},
		{name: "min at the stored max", stored: stored, min: 310, wantErr: notLess},
		{name: "max below the stored min", stored: stored, max: 290, wantErr: notLess},
		{name: "both given inverted", stored: stored, min: 360, max: 350, wantErr: notLess},
		{name: "max above 4096", stored: stored, max: 4097, wantErr: "max VLAN ID 4097 is outside 2-4096"},
		{name: "negative min", stored: stored, min: -300, wantErr: "min VLAN ID -300 is outside 1-4095"},
		{name: "a min alone", stored: noRange, min: 300, wantErr: fmt.Sprintf(noMax, 300)},
		{name: "a max alone", stored: noRange, max: 310, wantErr: fmt.Sprintf(noMin, 310)},
		{name: "a stored min alone", stored: map[string]any{"min": 300}, wantErr: fmt.Sprintf(noMax, 300)},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			m := store.NewMockStore(ctrl)

			m.EXPECT().Get(gomock.Any()).DoAndReturn(func(c *store.Config) error {
				*c = store.Config{
					Version:  "phenix.sandia.gov/v1",
					Kind:     "Experiment",
					Metadata: store.ConfigMetadata{Name: "vinv"},
					Spec: map[string]any{
						"experimentName": "vinv",
						"vlans":          test.stored,
					},
				}

				return nil
			})

			useStore(t, m)

			err := experiment.Start(
				context.Background(),
				experiment.StartWithName("vinv"),
				experiment.StartWithVLANMin(test.min),
				experiment.StartWithVLANMax(test.max),
			)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("Start() error = %v, want it to contain %q", err, test.wantErr)
			}
		})
	}
}

// TestConfigHooksRejectInvalidVLANRange proves that creating or updating an
// experiment config, as phenix config create and edit and the REST config
// routes do, refuses a spec.vlans range the minimega script does not render
// or minimega would refuse at start before anything is stored.
func TestConfigHooksRejectInvalidVLANRange(t *testing.T) {
	ranges := []struct {
		name    string
		vlans   map[string]any
		wantErr string
	}{
		{
			name:    "min above 4095",
			vlans:   map[string]any{"min": 4096, "max": 4096},
			wantErr: "spec.vlans: min VLAN ID 4096 is outside 1-4095",
		},
		{
			name:    "max alone",
			vlans:   map[string]any{"max": 350},
			wantErr: "spec.vlans: " + fmt.Sprintf(noMin, 350),
		},
		{
			name:    "equal ends",
			vlans:   map[string]any{"min": 350, "max": 350},
			wantErr: "spec.vlans: " + fmt.Sprintf(notBelow, 350, 350),
		},
		{
			name:    "inverted",
			vlans:   map[string]any{"min": 360, "max": 350},
			wantErr: "spec.vlans: " + fmt.Sprintf(notBelow, 360, 350),
		},
	}

	config := func(t *testing.T, vlans map[string]any) *store.Config {
		t.Helper()

		return &store.Config{
			Version:  "phenix.sandia.gov/v1",
			Kind:     "Experiment",
			Metadata: store.ConfigMetadata{Name: "vbad"},
			Spec: map[string]any{
				"experimentName": "vbad",
				"baseDir":        t.TempDir(),
				"defaultBridge":  "phenix",
				"topology":       bridgeModeTopology(""),
				"vlans":          vlans,
			},
		}
	}

	for _, r := range ranges {
		t.Run("create/"+r.name, func(t *testing.T) {
			// The store mock fails the test on any call: nothing is stored.
			useStore(t, store.NewMockStore(gomock.NewController(t)))

			_, err := apiconfig.Create(apiconfig.CreateFromConfig(config(t, r.vlans)))
			if err == nil || !strings.Contains(err.Error(), r.wantErr) {
				t.Fatalf("config.Create() error = %v, want it to contain %q", err, r.wantErr)
			}
		})

		t.Run("update/"+r.name, func(t *testing.T) {
			current := config(t, nil)

			m := store.NewMockStore(gomock.NewController(t))
			m.EXPECT().Get(gomock.Any()).DoAndReturn(func(c *store.Config) error {
				*c = *current

				return nil
			}).AnyTimes()

			useStore(t, m)

			err := apiconfig.Update("experiment/vbad", config(t, r.vlans))
			if err == nil || !strings.Contains(err.Error(), r.wantErr) {
				t.Fatalf("config.Update() error = %v, want it to contain %q", err, r.wantErr)
			}
		})
	}
}
