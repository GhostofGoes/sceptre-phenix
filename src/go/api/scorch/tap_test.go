//nolint:testpackage // testing internals
package scorch

import (
	"context"
	"errors"
	"testing"

	"phenix/api/scorch/scorchmd"
	"phenix/store"
	"phenix/types"
	"phenix/util/mm"
	"phenix/util/tap"
)

type fakeTapMM struct {
	mm.Minimega

	tapVLANCalls int
}

func (f *fakeTapMM) TapVLAN(opts ...mm.TapOption) error {
	f.tapVLANCalls++

	return nil
}

func (*fakeTapMM) MeshShell(_, _ string) error {
	return nil
}

func (fakeTapMM) Headnode() string {
	return "headnode"
}

func (fakeTapMM) IsHeadnode(node string) bool {
	return node == "headnode"
}

type fakeStore struct {
	store.Store
}

func (fakeStore) Get(*store.Config) error {
	return nil
}

func (fakeStore) Update(*store.Config) error {
	return nil
}

func TestTapTypeAndInit(t *testing.T) {
	var tapComp Tap

	if err := tapComp.Init(); err != nil {
		t.Fatalf("unexpected error initializing Tap: %v", err)
	}

	if tapComp.Type() != "tap" {
		t.Fatalf("expected type 'tap', got '%s'", tapComp.Type())
	}

	if err := tapComp.Configure(context.Background()); err != nil {
		t.Fatalf("expected Configure to return nil, got: %v", err)
	}
}

func TestTapCleanupNoopWhenStatusMissing(t *testing.T) {
	exp := types.NewExperiment(store.ConfigMetadata{Name: "tap-cleanup-test"})
	exp.Spec.SetExperimentName("tap-cleanup-test")

	var tapComp Tap
	_ = tapComp.Init(Experiment(*exp), Name("test-tap"))

	if err := tapComp.Cleanup(context.Background()); err != nil {
		t.Fatalf("expected no error cleaning up tap with no recorded status, got: %v", err)
	}
}

func TestTapCleanupNoopWhenTapMissing(t *testing.T) {
	exp := types.NewExperiment(store.ConfigMetadata{Name: "tap-cleanup-test"})
	exp.Spec.SetExperimentName("tap-cleanup-test")

	status := scorchmd.ScorchStatus{
		RunID: 0,
		Taps:  make(map[string]*tap.Tap),
	}
	exp.Status.SetAppStatus("scorch", status)

	var tapComp Tap
	_ = tapComp.Init(Experiment(*exp), Name("test-tap"))

	if err := tapComp.Cleanup(context.Background()); err != nil {
		t.Fatalf("expected no error cleaning up non-existent tap, got: %v", err)
	}
}

func TestTapStopAndCleanupIdempotence(t *testing.T) {
	originalStore := store.DefaultStore
	store.DefaultStore = fakeStore{}                         //nolint:reassign // install test double
	t.Cleanup(func() { store.DefaultStore = originalStore }) //nolint:reassign // restore test double

	originalMM := mm.DefaultMM
	fake := &fakeTapMM{}
	mm.DefaultMM = fake                             //nolint:reassign // install test double
	t.Cleanup(func() { mm.DefaultMM = originalMM }) //nolint:reassign // restore test double

	exp := types.NewExperiment(store.ConfigMetadata{Name: "tap-cleanup-test"})
	exp.Spec.SetExperimentName("tap-cleanup-test")

	testTap := &tap.Tap{
		Name: "test-tapcomp",
		VLAN: "MGMT",
	}

	status := scorchmd.ScorchStatus{
		RunID: 0,
		Taps: map[string]*tap.Tap{
			"test-tap": testTap,
		},
	}
	exp.Status.SetAppStatus("scorch", status)

	var tapComp Tap
	_ = tapComp.Init(Experiment(*exp), Name("test-tap"))

	// Stop should delete the tap and remove it from status
	if err := tapComp.Stop(context.Background()); err != nil {
		t.Fatalf("unexpected error stopping tap: %v", err)
	}

	if fake.tapVLANCalls != 1 {
		t.Fatalf("expected 1 TapVLAN call, got %d", fake.tapVLANCalls)
	}

	var updatedStatus scorchmd.ScorchStatus
	if err := exp.Status.ParseAppStatus("scorch", &updatedStatus); err != nil {
		t.Fatalf("failed to parse updated status: %v", err)
	}

	if _, exists := updatedStatus.Taps["test-tap"]; exists {
		t.Fatalf("expected 'test-tap' to be removed from status.Taps")
	}

	// Subsequent Cleanup should be a no-op and not call TapVLAN again
	if err := tapComp.Cleanup(context.Background()); err != nil {
		t.Fatalf("unexpected error during idempotent cleanup: %v", err)
	}

	if fake.tapVLANCalls != 1 {
		t.Fatalf("expected TapVLAN calls to remain 1, got %d", fake.tapVLANCalls)
	}
}

func TestTapCleanupDirectly(t *testing.T) {
	originalStore := store.DefaultStore
	store.DefaultStore = fakeStore{}                         //nolint:reassign // install test double
	t.Cleanup(func() { store.DefaultStore = originalStore }) //nolint:reassign // restore test double

	originalMM := mm.DefaultMM
	fake := &fakeTapMM{}
	mm.DefaultMM = fake                             //nolint:reassign // install test double
	t.Cleanup(func() { mm.DefaultMM = originalMM }) //nolint:reassign // restore test double

	exp := types.NewExperiment(store.ConfigMetadata{Name: "tap-cleanup-test"})
	exp.Spec.SetExperimentName("tap-cleanup-test")

	testTap := &tap.Tap{
		Name: "test-tapcomp",
		VLAN: "MGMT",
	}

	status := scorchmd.ScorchStatus{
		RunID: 0,
		Taps: map[string]*tap.Tap{
			"test-tap": testTap,
		},
	}
	exp.Status.SetAppStatus("scorch", status)

	var tapComp Tap
	_ = tapComp.Init(Experiment(*exp), Name("test-tap"))

	// Cleanup called directly (without Stop) should delete the tap
	if err := tapComp.Cleanup(context.Background()); err != nil {
		t.Fatalf("unexpected error cleaning up tap: %v", err)
	}

	if fake.tapVLANCalls != 1 {
		t.Fatalf("expected 1 TapVLAN call, got %d", fake.tapVLANCalls)
	}

	var updatedStatus scorchmd.ScorchStatus
	if err := exp.Status.ParseAppStatus("scorch", &updatedStatus); err != nil {
		t.Fatalf("failed to parse updated status: %v", err)
	}

	if _, exists := updatedStatus.Taps["test-tap"]; exists {
		t.Fatalf("expected 'test-tap' to be removed from status.Taps")
	}
}

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

// TestDeleteRemainingTaps proves the end of a run deletes the taps its record
// still holds, such as one whose tap component failed to stop, and leaves out
// a tap its break component deleted itself and forgot.
func TestDeleteRemainingTaps(t *testing.T) {
	originalMM := mm.DefaultMM
	fake := &fakeTapMM{}
	mm.DefaultMM = fake                             //nolint:reassign // install test double
	t.Cleanup(func() { mm.DefaultMM = originalMM }) //nolint:reassign // restore test double

	exp := types.NewExperiment(store.ConfigMetadata{Name: "tap-remaining-test"})
	exp.Spec.SetExperimentName("tap-remaining-test")

	exp.Status.SetAppStatus("scorch", scorchmd.ScorchStatus{
		RunID: 0,
		Taps: map[string]*tap.Tap{
			"leaked": {Name: "leaked-tapcomp", VLAN: "MGMT"},
			"break":  {Name: "brk-tapbrk", VLAN: "MGMT"},
		},
	})

	forgetTap(*exp, "break")

	if err := deleteRemainingTaps(exp); err != nil {
		t.Fatalf("deleteRemainingTaps() error = %v", err)
	}

	if fake.tapVLANCalls != 1 {
		t.Fatalf("deleteRemainingTaps() made %d tap calls, want 1 for the leaked tap", fake.tapVLANCalls)
	}

	if err := deleteRemainingTaps(types.NewExperiment(store.ConfigMetadata{Name: "no-record"})); err != nil {
		t.Fatalf("deleteRemainingTaps() without a record error = %v, want nil", err)
	}
}
