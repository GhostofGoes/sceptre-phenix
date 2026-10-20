package scorch

import (
	"context"
	"errors"
	"testing"

	"phenix/store"
	"phenix/types"
	"phenix/util/mm"
)

// TestTapAndBreakKeepStoredStatus is a regression test: the tap and break
// components recorded their tap by writing the run's copy of the whole
// experiment status, read when the run began, back to the store, which
// reverted what was stored since, such as a soh component's results.
func TestTapAndBreakKeepStoredStatus(t *testing.T) {
	fake := new(tapCountingMM)
	originalMM := mm.DefaultMM
	mm.DefaultMM = fake //nolint:reassign // install test double

	t.Cleanup(func() { mm.DefaultMM = originalMM }) //nolint:reassign // restore test double

	tapMeta := map[string]any{"bridge": "phenix", "vlan": "LAN", "ip": "10.10.0.210/24"}

	for typ, start := range tapComponentStarts(tapMeta) {
		t.Run(typ, func(t *testing.T) {
			const name = "status-exp"

			s := installExperimentStore(t, store.Config{
				Version:  "phenix.sandia.gov/v1",
				Kind:     "Experiment",
				Metadata: store.ConfigMetadata{Name: name},
				Spec:     map[string]any{"experimentName": name, "defaultBridge": "phenix"},
				Status: map[string]any{
					"startTime": "2024-01-01T00:00:00Z",
					"apps": map[string]any{
						"soh":    map[string]any{"round": 0},
						"scorch": map[string]any{"runID": 0, "taps": map[string]any{}},
					},
				},
			})

			// The run's copy of the experiment.
			exp, err := types.DecodeExperimentFromConfig(s.load())
			if err != nil {
				t.Fatal(err)
			}

			// A soh component stores its results after the run began.
			c := s.load()
			c.Status["apps"].(map[string]any)["soh"] = map[string]any{"round": 7} //nolint:forcetypeassert // test data
			s.save(c)

			if err := start(NewOptions(Experiment(*exp), Name("cmp1"))); err != nil {
				t.Fatalf("starting the %s component: %v", typ, err)
			}

			apps, _ := s.load().Status["apps"].(map[string]any)

			if soh, _ := apps["soh"].(map[string]any); soh["round"] != float64(7) {
				t.Errorf("stored soh status %v after the %s component, want round 7", apps["soh"], typ)
			}

			scorch, _ := apps["scorch"].(map[string]any)
			if taps, _ := scorch["taps"].(map[string]any); taps["cmp1"] == nil {
				t.Errorf("stored scorch status %v after the %s component, want its tap recorded", scorch, typ)
			}
		})
	}
}

// tapComponentStarts starts the component of each type with a tap given by
// tapMeta, as a run does.
func tapComponentStarts(tapMeta map[string]any) map[string]func(Options) error {
	return map[string]func(Options) error{
		"tap": func(o Options) error {
			var c Tap

			_ = c.Init(Experiment(o.Exp), Name(o.Name), Type("tap"), Metadata(tapMeta))

			return c.Start(context.Background())
		},
		"break": func(o Options) error {
			var c Break

			_ = c.Init(Experiment(o.Exp), Name(o.Name), Type("break"), Metadata(map[string]any{"tap": tapMeta}))

			return c.Start(context.Background())
		},
	}
}

// TestTapAndBreakRefuseChangedRun checks that the tap and break components,
// which record their tap in the stored experiment, refuse to record it when
// the experiment was stopped or started again since the run began, so a run
// that ended does not write its record over the next run's.
func TestTapAndBreakRefuseChangedRun(t *testing.T) {
	fake := new(tapCountingMM)
	originalMM := mm.DefaultMM
	mm.DefaultMM = fake //nolint:reassign // install test double

	t.Cleanup(func() { mm.DefaultMM = originalMM }) //nolint:reassign // restore test double

	tapMeta := map[string]any{"bridge": "phenix", "vlan": "LAN", "ip": "10.10.0.210/24"}
	changes := map[string]string{"stopped": "", "started again": "2024-01-02T00:00:00Z"}

	for typ, start := range tapComponentStarts(tapMeta) {
		for change, startTime := range changes {
			t.Run(typ+" "+change, func(t *testing.T) {
				const name = "changed-exp"

				s := installExperimentStore(t, store.Config{
					Version:  "phenix.sandia.gov/v1",
					Kind:     "Experiment",
					Metadata: store.ConfigMetadata{Name: name},
					Spec:     map[string]any{"experimentName": name, "defaultBridge": "phenix"},
					Status: map[string]any{
						"startTime": "2024-01-01T00:00:00Z",
						"apps":      map[string]any{"scorch": map[string]any{"runID": 0, "taps": map[string]any{}}},
					},
				})

				// The run's copy of the experiment.
				exp, err := types.DecodeExperimentFromConfig(s.load())
				if err != nil {
					t.Fatal(err)
				}

				c := s.load()
				c.Status["startTime"] = startTime
				s.save(c)

				err = start(NewOptions(Experiment(*exp), Name("cmp1")))
				if !errors.Is(err, types.ErrExperimentRunChanged) {
					t.Errorf("starting the %s component after the experiment was %s: %v, want %v",
						typ, change, err, types.ErrExperimentRunChanged)
				}

				stored := s.load().Status

				if stored["startTime"] != startTime {
					t.Errorf("stored start time %v after the %s component, want %q", stored["startTime"], typ, startTime)
				}

				apps, _ := stored["apps"].(map[string]any)
				scorch, _ := apps["scorch"].(map[string]any)

				if taps, _ := scorch["taps"].(map[string]any); len(taps) != 0 {
					t.Errorf("stored scorch status %v after the %s component, want no tap recorded", scorch, typ)
				}
			})
		}
	}
}

// TestTapAndBreakValidateTaps is a regression test: the tap and break
// components created their tap without the checks the tap app makes, so a
// VLAN, bridge or address of several words, or with a '$' that minimega
// expands, reached minimega's tap create and the host's `ip addr add`.
func TestTapAndBreakValidateTaps(t *testing.T) {
	fake := new(tapCountingMM)
	originalMM := mm.DefaultMM
	mm.DefaultMM = fake //nolint:reassign // install test double

	t.Cleanup(func() { mm.DefaultMM = originalMM }) //nolint:reassign // restore test double

	const name = "valid-exp"

	s := installExperimentStore(t, store.Config{
		Version:  "phenix.sandia.gov/v1",
		Kind:     "Experiment",
		Metadata: store.ConfigMetadata{Name: name},
		Spec:     map[string]any{"experimentName": name, "defaultBridge": "phenix"},
		Status: map[string]any{
			"startTime": "2024-01-01T00:00:00Z",
			"apps":      map[string]any{"scorch": map[string]any{"runID": 0, "taps": map[string]any{}}},
		},
	})

	exp, err := types.DecodeExperimentFromConfig(s.load())
	if err != nil {
		t.Fatal(err)
	}

	invalid := map[string]map[string]any{
		"several-word VLAN":    {"bridge": "phenix", "vlan": "LAN bridge evil", "ip": "10.10.0.210/24"},
		"VLAN with a '$'":      {"bridge": "phenix", "vlan": "$HOME", "ip": "10.10.0.210/24"},
		"several-word bridge":  {"bridge": "phenix name x", "vlan": "LAN", "ip": "10.10.0.210/24"},
		"several-word address": {"bridge": "phenix", "vlan": "LAN", "ip": "10.0.0.1/24 dev lo"},
		"not an address":       {"bridge": "phenix", "vlan": "LAN", "ip": "not an ip"},
	}

	for what, meta := range invalid {
		var (
			tc Tap
			bc Break
		)

		_ = tc.Init(Experiment(*exp), Name("tap1"), Type("tap"), Metadata(meta))
		_ = bc.Init(Experiment(*exp), Name("brk1"), Type("break"), Metadata(map[string]any{"tap": meta}))

		if err := tc.Start(context.Background()); err == nil {
			t.Errorf("a tap component with a %s started", what)
		}

		if err := bc.Start(context.Background()); err == nil {
			t.Errorf("a break component with a %s tap started", what)
		}
	}

	if got := fake.calls(); got != 0 {
		t.Errorf("the refused components sent %d tap commands to minimega, want 0", got)
	}
}
