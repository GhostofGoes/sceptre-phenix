package web

import (
	"encoding/json"
	"io/fs"
	"path"
	"slices"
	"testing"
)

// TestBuilderSerialDeviceDefault checks the serial interface device the
// Builder fills in for a new serial interface. The serial app links a
// pseudo-terminal at that path in the guest, replacing what is there, so it
// must not be the guest's real /dev/ttyS0. It matches the OpenAPI schemas.
func TestBuilderSerialDeviceDefault(t *testing.T) {
	const (
		dir  = "public/grapheditor/utils/schemas"
		want = "/dev/ttyphenix0"
	)

	files, err := fs.Glob(publicFS, dir+"/*.json")
	if err != nil {
		t.Fatalf("listing the Builder schemas: %v", err)
	}

	withDevice := make([]string, 0, len(files))

	for _, file := range files {
		data, err := publicFS.ReadFile(file)
		if err != nil {
			t.Fatalf("reading %s: %v", file, err)
		}

		var schema any
		if err := json.Unmarshal(data, &schema); err != nil {
			t.Fatalf("decoding %s: %v", file, err)
		}

		for _, device := range serialDevices(schema) {
			withDevice = append(withDevice, path.Base(file))

			examples, _ := device["examples"].([]any)

			if device["default"] != want || !slices.Equal(examples, []any{want}) {
				t.Errorf("%s: serial device default = %v, examples = %v, want %s", file, device["default"], examples, want)
			}
		}
	}

	// The schemas the Builder loads for new nodes carry the field.
	for _, file := range []string{"container_schema.json", "kvm_schema.json", "node_schema.json", "topo_schema.json"} {
		if !slices.Contains(withDevice, file) {
			t.Errorf("%s has no serial device property", file)
		}
	}
}

// serialDevices returns the "device" property schema of every serial
// interface schema, one with a baud_rate, at any depth of the decoded JSON
// schema v.
func serialDevices(v any) []map[string]any {
	var found []map[string]any

	switch v := v.(type) {
	case map[string]any:
		if props, ok := v["properties"].(map[string]any); ok && props["baud_rate"] != nil {
			if device, ok := props["device"].(map[string]any); ok {
				found = append(found, device)
			}
		}

		for _, child := range v {
			found = append(found, serialDevices(child)...)
		}
	case []any:
		for _, child := range v {
			found = append(found, serialDevices(child)...)
		}
	}

	return found
}
