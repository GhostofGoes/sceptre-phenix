package app

import (
	"strings"
	"testing"

	"phenix/store"
	"phenix/types"
	v1 "phenix/types/version/v1"
)

func TestDefaultAppsEnabled(t *testing.T) {
	tests := []struct {
		name        string
		annotations map[string]any
		want        bool
	}{
		{name: "annotation omitted", want: true},
		{
			name:        "explicitly enabled",
			annotations: map[string]any{defaultAppsAnnotation: true},
			want:        true,
		},
		{
			name:        "explicitly disabled",
			annotations: map[string]any{defaultAppsAnnotation: false},
			want:        false,
		},
		{
			name:        "string false disables",
			annotations: map[string]any{defaultAppsAnnotation: "false"},
			want:        false,
		},
		{
			name:        "string false in any case disables",
			annotations: map[string]any{defaultAppsAnnotation: " FALSE "},
			want:        false,
		},
		{
			name:        "string zero disables",
			annotations: map[string]any{defaultAppsAnnotation: "0"},
			want:        false,
		},
		{
			name:        "string true enables",
			annotations: map[string]any{defaultAppsAnnotation: "True"},
			want:        true,
		},
		{
			name:        "zero from YAML disables",
			annotations: map[string]any{defaultAppsAnnotation: 0},
			want:        false,
		},
		{
			name:        "zero from JSON disables",
			annotations: map[string]any{defaultAppsAnnotation: float64(0)},
			want:        false,
		},
		{
			name:        "one enables",
			annotations: map[string]any{defaultAppsAnnotation: 1},
			want:        true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			node := &v1.Node{
				AnnotationsF: tt.annotations,
			}

			if got := defaultAppsEnabled(node); got != tt.want {
				t.Fatalf("defaultAppsEnabled() = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestDefaultAppsSettingRejectsUnreadableValues(t *testing.T) {
	for _, value := range []any{"no", "", nil, []any{"false"}} {
		node := &v1.Node{
			AnnotationsF: map[string]any{defaultAppsAnnotation: value},
			GeneralF:     &v1.General{HostnameF: "odd-apps"},
		}

		_, err := defaultAppsSetting(node)
		if err == nil {
			t.Errorf("defaultAppsSetting(%#v) returned no error", value)

			continue
		}

		if !strings.Contains(err.Error(), "node odd-apps: annotation phenix/default-apps is ") {
			t.Errorf("defaultAppsSetting(%#v) error = %q, want it to name the node and the annotation", value, err)
		}

		// Stages that do not check the annotation, such as cleanup, keep the
		// default apps.
		if !defaultAppsEnabled(node) {
			t.Errorf("defaultAppsEnabled(%#v) = false, want true", value)
		}
	}
}

// TestApplyAppsRejectsUnreadableDefaultApps checks that experiment create
// (configure) and start (pre-start) refuse an annotation the default apps
// cannot read, and that cleanup still runs.
func TestApplyAppsRejectsUnreadableDefaultApps(t *testing.T) {
	exp := &types.Experiment{
		Metadata: store.ConfigMetadata{Name: "default-apps-unreadable"},
		Spec: &v1.ExperimentSpec{
			BaseDirF: t.TempDir(),
			TopologyF: &v1.TopologySpec{NodesF: []*v1.Node{{
				AnnotationsF: map[string]any{defaultAppsAnnotation: "no"},
				GeneralF:     &v1.General{HostnameF: "odd-apps"},
			}}},
		},
		Status: &v1.ExperimentStatus{},
	}

	want := `node odd-apps: annotation phenix/default-apps is "no", want true or false`

	for _, stage := range []Action{ActionConfigure, ActionPreStart} {
		err := ApplyApps(t.Context(), exp, Stage(stage), DryRun(true))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("ApplyApps(%s) error = %v, want one containing %q", stage, err, want)
		}
	}

	if err := ApplyApps(t.Context(), exp, Stage(ActionCleanup), DryRun(true)); err != nil {
		t.Errorf("ApplyApps(cleanup) error = %v", err)
	}
}

func TestStartupSkipsNodeWithDefaultAppsDisabled(t *testing.T) {
	node := &v1.Node{
		AnnotationsF: map[string]any{defaultAppsAnnotation: false},
		GeneralF: &v1.General{
			HostnameF: "unmanaged-apps",
		},
		HardwareF: &v1.Hardware{
			OSTypeF: "linux",
			DrivesF: []*v1.Drive{{ImageF: "test.qc2"}},
		},
	}
	exp := &types.Experiment{
		Metadata: store.ConfigMetadata{Name: "default-apps-disabled"},
		Spec: &v1.ExperimentSpec{
			BaseDirF:  t.TempDir(),
			TopologyF: &v1.TopologySpec{NodesF: []*v1.Node{node}},
		},
	}

	if err := (Startup{}).PreStart(t.Context(), exp); err != nil {
		t.Fatalf("PreStart() applied startup app to disabled node: %v", err)
	}

	if len(node.Injections()) != 0 {
		t.Fatalf("PreStart() added %d injections to disabled node", len(node.Injections()))
	}
}

// TestStartupChecksAlwaysRunForNodeWithDefaultAppsDisabled verifies that
// checks/normalization that should always run (e.g. disk image presence),
// regardless of the default-apps annotation, are not skipped by the
// early bailout for disabled nodes.
func TestStartupChecksAlwaysRunForNodeWithDefaultAppsDisabled(t *testing.T) {
	node := &v1.Node{
		AnnotationsF: map[string]any{defaultAppsAnnotation: false},
		GeneralF: &v1.General{
			HostnameF: "unmanaged-apps-no-drives",
		},
	}
	exp := &types.Experiment{
		Metadata: store.ConfigMetadata{Name: "default-apps-disabled-no-drives"},
		Spec: &v1.ExperimentSpec{
			BaseDirF:  t.TempDir(),
			TopologyF: &v1.TopologySpec{NodesF: []*v1.Node{node}},
		},
	}

	err := (Startup{}).PreStart(t.Context(), exp)
	if err == nil {
		t.Fatal("PreStart() did not return an error for disabled node missing drives")
	}
}
