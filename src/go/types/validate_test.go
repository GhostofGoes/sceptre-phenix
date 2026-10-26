package types_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"

	"phenix/store"
	"phenix/types"
	"phenix/types/version"
)

// TestValidateSchemaAccepts proves values are converted to JSON types before
// validation, so Go ints, typed maps and tagged structs validate like a
// decoded JSON document. Both values have a spec, which the Workflow schema
// requires.
func TestValidateSchemaAccepts(t *testing.T) {
	cases := []struct {
		name  string
		value any
	}{
		{
			name: "map holding Go ints",
			value: map[string]any{
				"apiVersion": "phenix.sandia.gov/v0",
				"kind":       "Workflow",
				"spec": map[string]any{
					"vlans":     map[string]int{"EXP": 101},
					"vlanRange": map[string]any{"min": 100, "max": 200},
				},
			},
		},
		{
			name: "struct with JSON tags",
			value: store.Config{
				Version: "phenix.sandia.gov/v0",
				Kind:    "Workflow",
				Spec:    map[string]any{"topology": "helloworld"},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := types.ValidateSchema("Workflow", "v0", tc.value); err != nil {
				t.Fatalf("ValidateSchema() error = %v", err)
			}
		})
	}
}

// assertSchemaError checks that err is a validation failure whose schema error
// has the given JSON pointer and breaks the given schema keyword.
func assertSchemaError(t *testing.T, err error, pointer []string, field string) {
	t.Helper()

	if !errors.Is(err, types.ErrValidationFailed) {
		t.Fatalf("error = %v, want ErrValidationFailed", err)
	}

	var schemaErr *openapi3.SchemaError
	if !errors.As(err, &schemaErr) {
		t.Fatalf("error %v carries no *openapi3.SchemaError", err)
	}

	if got := schemaErr.JSONPointer(); !slices.Equal(got, pointer) {
		t.Errorf("JSONPointer() = %v, want %v", got, pointer)
	}

	if schemaErr.SchemaField != field {
		t.Errorf("SchemaField = %q, want %q", schemaErr.SchemaField, field)
	}
}

// TestValidateSchemaRejects checks the chain of validation failures, whose
// pointers are relative to the validated value.
func TestValidateSchemaRejects(t *testing.T) {
	cases := []struct {
		name        string
		value       any
		wantPointer []string
		wantField   string
	}{
		{
			name:        "required property missing",
			value:       map[string]any{"apiVersion": "phenix.sandia.gov/v0"},
			wantPointer: []string{"kind"},
			wantField:   "required",
		},
		{
			name: "nested property of the wrong type",
			value: map[string]any{
				"apiVersion": "phenix.sandia.gov/v0",
				"kind":       "Workflow",
				"spec":       map[string]any{"auto": map[string]any{"update": "yes"}},
			},
			wantPointer: []string{"spec", "auto", "update"},
			wantField:   "type",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertSchemaError(t, types.ValidateSchema("Workflow", "v0", tc.value), tc.wantPointer, tc.wantField)
		})
	}
}

// TestValidateSchemaLookupErrors proves a missing schema is a lookup failure,
// not a validation failure, so callers can report it as an internal error.
func TestValidateSchemaLookupErrors(t *testing.T) {
	cases := []struct {
		name        string
		component   string
		ver         string
		wantPrefix  string
		invalidKind bool
	}{
		{
			name:        "unknown component",
			component:   "Nope",
			ver:         "v0",
			wantPrefix:  "getting validator for config: invalid kind: no schema definition found for version v0 of Nope",
			invalidKind: true,
		},
		{
			name:        "Workflow is not in the latest schemas",
			component:   "Workflow",
			ver:         version.LATEST_VERSION,
			wantPrefix:  "getting validator for config: invalid kind: no schema definition found for version v2 of Workflow",
			invalidKind: true,
		},
		{
			name:        "unknown version",
			component:   "Workflow",
			ver:         "v9",
			wantPrefix:  "getting validator for config: reading embedded OpenAPI schema schemas/v9.yaml: ",
			invalidKind: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := types.ValidateSchema(tc.component, tc.ver, map[string]any{})
			if err == nil || !strings.HasPrefix(err.Error(), tc.wantPrefix) {
				t.Fatalf("ValidateSchema() error = %v, want prefix %q", err, tc.wantPrefix)
			}

			if errors.Is(err, types.ErrValidationFailed) {
				t.Errorf("ValidateSchema() error %v wraps ErrValidationFailed", err)
			}

			if got := errors.Is(err, version.ErrInvalidKind); got != tc.invalidKind {
				t.Errorf("error wraps ErrInvalidKind = %t, want %t", got, tc.invalidKind)
			}
		})
	}
}

// TestValidateConfigSpecErrorChain checks a per-kind schema failure, whose
// pointer is relative to the spec, as ValidateConfigSpec reports it through
// ValidateSchema.
func TestValidateConfigSpecErrorChain(t *testing.T) {
	cfg := store.Config{
		Version:  "phenix.sandia.gov/v2",
		Kind:     "Scenario",
		Metadata: store.ConfigMetadata{Name: "apps"},
		Spec:     map[string]any{"apps": []any{map[string]any{}}},
	}

	assertSchemaError(t, types.ValidateConfigSpec(cfg), []string{"apps", "0", "name"}, "required")
}

// TestValidateConfigSpecRejectsWorkflowKind proves a Workflow document cannot
// pass config validation, so phenix config create and POST /configs, which
// validate through config.Create, reject one.
func TestValidateConfigSpecRejectsWorkflowKind(t *testing.T) {
	cfg := store.Config{
		Version:  "phenix.sandia.gov/v0",
		Kind:     "Workflow",
		Metadata: store.ConfigMetadata{Name: "wf"},
		Spec:     map[string]any{"topology": "helloworld"},
	}

	assertSchemaError(t, types.ValidateConfigSpec(cfg), []string{"kind"}, "enum")
}

// TestValidateConfigSpecRefusesOtherVersions proves a config of a kind phenix
// decodes by version is validated only at the stored version of that kind,
// the one experiment create decodes, since phenix has no config upgraders: a
// v1 Scenario is refused whatever the shape of its apps, as is a v0 Topology,
// and the error names the apiVersion the kind needs. A kind without a
// versioned spec, such as Image, is validated against the latest schema
// whatever its apiVersion.
func TestValidateConfigSpecRefusesOtherVersions(t *testing.T) {
	app := map[string]any{"name": "e2e-probe", "metadata": map[string]any{"k": "v"}}

	node := map[string]any{
		"type":    "VirtualMachine",
		"general": map[string]any{"hostname": "host-a"},
		"hardware": map[string]any{
			"os_type": "linux",
			"drives":  []any{map[string]any{"image": "ubuntu.qc2"}},
		},
	}

	image := map[string]any{
		"format": "qcow2", "mirror": "http://us.archive.ubuntu.com/ubuntu/",
		"release": "noble", "size": "10G", "variant": "minbase",
	}

	cases := []struct {
		name    string
		version string
		kind    string
		spec    map[string]any
		wantErr string // empty when the config is valid
	}{
		{
			name:    "v2 scenario with a list of apps",
			version: "v2",
			kind:    "Scenario",
			spec:    map[string]any{"apps": []any{app}},
		},
		{
			name:    "v2 scenario with the v1 map of apps",
			version: "v2",
			kind:    "Scenario",
			spec:    map[string]any{"apps": map[string]any{"experiment": []any{app}}},
			wantErr: `Error at "/apps": value must be an array`,
		},
		{
			name:    "v1 scenario with experiment and host apps",
			version: "v1",
			kind:    "Scenario",
			spec: map[string]any{"apps": map[string]any{
				"experiment": []any{app},
				"host": []any{map[string]any{
					"name":  "protonuke",
					"hosts": []any{map[string]any{"hostname": "h1", "metadata": map[string]any{}}},
				}},
			}},
			wantErr: "unknown scenario version v1: Scenario configs need apiVersion phenix.sandia.gov/v2",
		},
		{
			name:    "v1 scenario with the v2 list of apps",
			version: "v1",
			kind:    "Scenario",
			spec:    map[string]any{"apps": []any{app}},
			wantErr: "unknown scenario version v1",
		},
		{
			name:    "scenario in a later version",
			version: "v3",
			kind:    "Scenario",
			spec:    map[string]any{"apps": []any{app}},
			wantErr: "unknown scenario version v3",
		},
		{
			name:    "v1 topology",
			version: "v1",
			kind:    "Topology",
			spec:    map[string]any{"nodes": []any{node}},
		},
		{
			name:    "v0 topology",
			version: "v0",
			kind:    "Topology",
			spec:    map[string]any{"nodes": []any{node}},
			wantErr: "unknown topology version v0: Topology configs need apiVersion phenix.sandia.gov/v1",
		},
		{
			name:    "v2 experiment",
			version: "v2",
			kind:    "Experiment",
			spec:    map[string]any{"topology": map[string]any{"nodes": []any{node}}},
			wantErr: "unknown experiment version v2",
		},
		{
			name:    "image in another version",
			version: "v2",
			kind:    "Image",
			spec:    image,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := store.Config{
				Version:  "phenix.sandia.gov/" + tc.version,
				Kind:     tc.kind,
				Metadata: store.ConfigMetadata{Name: "probe"},
				Spec:     tc.spec,
			}

			err := types.ValidateConfigSpec(cfg)

			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("ValidateConfigSpec() error = %v, want nil", err)
				}

				return
			}

			if !errors.Is(err, types.ErrValidationFailed) {
				t.Fatalf("ValidateConfigSpec() error = %v, want ErrValidationFailed", err)
			}

			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("ValidateConfigSpec() error = %q, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

// TestDecodeScenarioFromConfigV1 checks that experiment create refuses a v1
// Scenario that validation refuses, whatever the shape of its apps, with the
// same unknown version, and decodes the v2 Scenario that validation accepts.
func TestDecodeScenarioFromConfigV1(t *testing.T) {
	cfg := store.Config{
		Version:  "phenix.sandia.gov/v1",
		Kind:     "Scenario",
		Metadata: store.ConfigMetadata{Name: "v1map"},
	}

	for _, apps := range []any{
		map[string]any{"experiment": []any{map[string]any{"name": "e2e-probe"}}},
		[]any{map[string]any{"name": "e2e-probe"}},
	} {
		cfg.Spec = map[string]any{"apps": apps}

		_, err := types.DecodeScenarioFromConfig(cfg)
		if err == nil || !strings.Contains(err.Error(), "unknown scenario version v1") {
			t.Errorf("DecodeScenarioFromConfig(v1, apps %v) error = %v, want an unknown scenario version", apps, err)
		}
	}

	cfg.Version = "phenix.sandia.gov/v2"

	if err := types.ValidateConfigSpec(cfg); err != nil {
		t.Fatalf("ValidateConfigSpec(v2) error = %v", err)
	}

	spec, err := types.DecodeScenarioFromConfig(cfg)
	if err != nil {
		t.Fatalf("DecodeScenarioFromConfig(v2) error = %v", err)
	}

	if apps := spec.Apps(); len(apps) != 1 || apps[0].Name() != "e2e-probe" {
		t.Errorf("DecodeScenarioFromConfig(v2) apps = %v, want one app named e2e-probe", apps)
	}
}
