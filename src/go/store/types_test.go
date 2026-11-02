package store

import (
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"pgregory.net/rapid"

	"phenix/types/version"
)

func TestNewConfigKindCaseInsensitive(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		expectKind string
	}{
		{name: "lowercase", input: "topology/test-topo", expectKind: "Topology"},
		{name: "capitalized", input: "Topology/test-topo", expectKind: "Topology"},
		{name: "uppercase", input: "TOPOLOGY/test-topo", expectKind: "Topology"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := NewConfig(tt.input)
			if err != nil {
				t.Fatalf("NewConfig(%q) returned error: %v", tt.input, err)
			}

			if cfg.Kind != tt.expectKind {
				t.Fatalf("NewConfig(%q) kind = %q, want %q", tt.input, cfg.Kind, tt.expectKind)
			}
		})
	}
}

func TestConfigFullNameKindCaseInsensitive(t *testing.T) {
	tests := []struct {
		name   string
		input  []string
		expect string
	}{
		{name: "single-arg lowercase", input: []string{"topology/test-topo"}, expect: "Topology/test-topo"},
		{name: "single-arg uppercase", input: []string{"TOPOLOGY/test-topo"}, expect: "Topology/test-topo"},
		{name: "two-arg lowercase", input: []string{"topology", "test-topo"}, expect: "Topology/test-topo"},
		{name: "two-arg uppercase", input: []string{"TOPOLOGY", "test-topo"}, expect: "Topology/test-topo"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ConfigFullName(tt.input...)
			if got != tt.expect {
				t.Fatalf("ConfigFullName(%v) = %q, want %q", tt.input, got, tt.expect)
			}
		})
	}
}

func TestNewConfigRejectsUnknownKind(t *testing.T) {
	_, err := NewConfig("notakind/test")
	if err == nil {
		t.Fatal("NewConfig should reject unknown kinds")
	}
}

// TestWorkflowIsNotAStoredKind proves the Workflow schema did not make
// workflow configs addressable in the store.
func TestWorkflowIsNotAStoredKind(t *testing.T) {
	for _, name := range []string{"workflow/wf", "Workflow/wf", "WORKFLOW/wf"} {
		if _, err := NewConfig(name); err == nil {
			t.Errorf("NewConfig(%q) returned no error", name)
		}

		if got := ConfigFullName(name); got != "" {
			t.Errorf("ConfigFullName(%q) = %q, want empty", name, got)
		}
	}

	if got := ConfigFullName("Workflow", "wf"); got != "" {
		t.Errorf(`ConfigFullName("Workflow", "wf") = %q, want empty`, got)
	}
}

// Clients cannot set a config's revision: the store assigns it.
func TestNewConfigClearsRevision(t *testing.T) {
	body := `{"apiVersion":"phenix.sandia.gov/v1","kind":"Topology","metadata":{"name":"a","revision":7}}`

	c, err := NewConfigFromJSON([]byte(body))
	if err != nil {
		t.Errorf("NewConfigFromJSON(revision 7) error = %v", err)
	} else if c.Metadata.Revision != 0 {
		t.Errorf("NewConfigFromJSON(revision 7) revision = %d, want 0", c.Metadata.Revision)
	}

	c, err = NewConfigFromYAML([]byte("apiVersion: phenix.sandia.gov/v1\nkind: Topology\nmetadata:\n  name: a\n  revision: 7\n"))
	if err != nil {
		t.Errorf("NewConfigFromYAML(revision 7) error = %v", err)
	} else if c.Metadata.Revision != 0 {
		t.Errorf("NewConfigFromYAML(revision 7) revision = %d, want 0", c.Metadata.Revision)
	}
}

// genTypesKindSpelling spells a stored kind in any mix of cases.
func genTypesKindSpelling(kind string) *rapid.Generator[string] {
	return rapid.Custom(func(t *rapid.T) string {
		var b strings.Builder

		for _, r := range kind {
			if rapid.Bool().Draw(t, "upper") {
				b.WriteString(strings.ToUpper(string(r)))
			} else {
				b.WriteString(strings.ToLower(string(r)))
			}
		}

		return b.String()
	})
}

// NewConfig and ConfigFullName agree on which names are configs: a stored kind in any case,
// "/", and a name with no "/" in it. Both name the config by its canonical kind.
func TestConfigNameProperties(t *testing.T) {
	t.Parallel()

	kinds := slices.Sorted(maps.Keys(version.StoredVersion))

	rapid.Check(t, func(t *rapid.T) {
		kind := rapid.SampledFrom(kinds).Draw(t, "kind")
		spelled := genTypesKindSpelling(kind).Draw(t, "spelled")

		if rapid.IntRange(0, 4).Draw(t, "unknown") == 0 {
			kind, spelled = "", spelled+rapid.SampledFrom([]string{"s", "x", "_"}).Draw(t, "suffix")
		}

		name := rapid.StringMatching(`[a-z/-]{0,4}`).Draw(t, "name")
		full := spelled + "/" + name

		want := ""
		if kind != "" && !strings.Contains(name, "/") {
			want = kind + "/" + name
		}

		if got := ConfigFullName(full); got != want {
			t.Fatalf("ConfigFullName(%q) = %q, want %q", full, got, want)
		}

		if want != "" {
			if got := ConfigFullName(spelled, name); got != kind+"/"+name {
				t.Fatalf("ConfigFullName(%q, %q) = %q, want %q", spelled, name, got, kind+"/"+name)
			}
		}

		c, err := NewConfig(full)
		if (err == nil) != (want != "") {
			t.Fatalf("NewConfig(%q) error = %v, want error %t", full, err, want == "")
		}

		if err == nil && (c.FullName() != want || c.APIVersion() != version.StoredVersion[kind] || c.APIGroup() != APIGroup) {
			t.Fatalf("NewConfig(%q) = %s %s, want %s at %s/%s", full, c.FullName(), c.Version, want, APIGroup, version.StoredVersion[kind])
		}
	})
}

// NewConfigFromFile parses a config by its file's extension, expanding
// environment placeholders, and clears the fields only the store sets.
func TestNewConfigFromFile(t *testing.T) {
	t.Parallel()

	const (
		jsonBody = `{"apiVersion":"phenix.sandia.gov/v1","kind":"Topology",` +
			`"metadata":{"name":"${PHENIX_TYPES_TEST_UNSET:a}","created":"x","updated":"y","revision":7}}`
		yamlBody = "apiVersion: phenix.sandia.gov/v1\nkind: Topology\n" +
			"metadata:\n  name: ${PHENIX_TYPES_TEST_UNSET:a}\n  created: x\n  updated: y\n  revision: 7\n"
	)

	want := &Config{Version: "phenix.sandia.gov/v1", Kind: "Topology", Metadata: ConfigMetadata{Name: "a"}}

	for name, tc := range map[string]struct {
		file, body string
		want       *Config
		wantErr    error
		wantMsg    string
	}{
		"json":         {file: "a.json", body: jsonBody, want: want},
		"yaml":         {file: "a.yaml", body: yamlBody, want: want},
		"yml":          {file: "a.yml", body: yamlBody, want: want},
		"bad json":     {file: "a.json", body: "{", wantErr: ErrInvalidFormat},
		"bad yaml":     {file: "a.yaml", body: "kind: [", wantErr: ErrInvalidFormat},
		"no extension": {file: "a", body: jsonBody, wantMsg: "invalid config extension"},
		"other":        {file: "a.txt", body: jsonBody, wantMsg: "invalid config extension"},
		"missing":      {file: "missing.json", wantErr: os.ErrNotExist, wantMsg: "cannot read config"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(t.TempDir(), tc.file)

			if tc.body != "" {
				if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			got, err := NewConfigFromFile(path)
			if (err != nil) != (tc.want == nil) || (tc.wantErr != nil && !errors.Is(err, tc.wantErr)) {
				t.Fatalf("NewConfigFromFile(%s) error = %v, want error %t (%v)", tc.file, err, tc.want == nil, tc.wantErr)
			}

			if tc.wantMsg != "" && (err == nil || !strings.Contains(err.Error(), tc.wantMsg)) {
				t.Fatalf("NewConfigFromFile(%s) error = %v, want it to contain %q", tc.file, err, tc.wantMsg)
			}

			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("NewConfigFromFile(%s) mismatch (-want +got):\n%s", tc.file, diff)
			}
		})
	}
}

// APIGroup and APIVersion split a config's version at its "/".
func TestConfigAPIVersion(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ version, group, apiVersion string }{
		{"phenix.sandia.gov/v1", "phenix.sandia.gov", "v1"},
		{"phenix.sandia.gov/v2/extra", "phenix.sandia.gov", "v2"},
		{"v1", "", "v1"},
		{"", "", ""},
		{"/", "", ""},
	} {
		t.Run(tc.version, func(t *testing.T) {
			t.Parallel()

			c := Config{Version: tc.version}

			if got := c.APIGroup(); got != tc.group {
				t.Errorf("Config{Version: %q}.APIGroup() = %q, want %q", tc.version, got, tc.group)
			}

			if got := c.APIVersion(); got != tc.apiVersion {
				t.Errorf("Config{Version: %q}.APIVersion() = %q, want %q", tc.version, got, tc.apiVersion)
			}
		})
	}
}

func TestConfigHasAnnotation(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		annotations Annotations
		want        bool
	}{
		"none":                {want: false},
		"empty":               {annotations: Annotations{}, want: false},
		"another":             {annotations: Annotations{"other": "x"}, want: false},
		"set":                 {annotations: Annotations{"phenix.io/key": "x"}, want: true},
		"set to empty string": {annotations: Annotations{"phenix.io/key": ""}, want: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			c := Config{Metadata: ConfigMetadata{Annotations: tc.annotations}}
			if got := c.HasAnnotation("phenix.io/key"); got != tc.want {
				t.Errorf("HasAnnotation(phenix.io/key) with %v = %t, want %t", tc.annotations, got, tc.want)
			}
		})
	}
}

// FullName joins kind and name as given; ConfigFullName takes one full name
// or a kind and a name, and nothing else.
func TestConfigFullNames(t *testing.T) {
	t.Parallel()

	if got := (Config{Kind: "Topology", Metadata: ConfigMetadata{Name: "a"}}).FullName(); got != "Topology/a" {
		t.Errorf("Config{Topology, a}.FullName() = %q, want %q", got, "Topology/a")
	}

	if got := (Config{}).FullName(); got != "/" {
		t.Errorf("Config{}.FullName() = %q, want %q", got, "/")
	}

	for _, args := range [][]string{nil, {"topology", "a", "b"}, {"notakind", "a"}, {"topology"}} {
		if got := ConfigFullName(args...); got != "" {
			t.Errorf("ConfigFullName(%q) = %q, want empty", args, got)
		}
	}
}
