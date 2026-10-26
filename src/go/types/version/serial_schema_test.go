package version

import (
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
)

// TestSerialDeviceDefault checks the serial interface device the schemas
// suggest. The serial app links a pseudo-terminal at that path in the guest,
// replacing what is there, so it must not be the guest's real /dev/ttyS0.
func TestSerialDeviceDefault(t *testing.T) {
	t.Parallel()

	const want = "/dev/ttyphenix0"

	for _, version := range []string{"v1", "v2"} {
		text, err := ReadSchemaFile(version)
		if err != nil {
			t.Fatalf("ReadSchemaFile(%s) error = %v", version, err)
		}

		doc, err := openapi3.NewLoader().LoadFromData(text)
		if err != nil {
			t.Fatalf("loading the %s schema: %v", version, err)
		}

		serial := doc.Components.Schemas["serial_iface"]
		if serial == nil || serial.Value == nil || serial.Value.Properties["device"] == nil {
			t.Fatalf("%s schema has no serial_iface device", version)
		}

		device := serial.Value.Properties["device"].Value

		if device.Default != want || device.Example != want {
			t.Errorf("%s serial_iface device default = %v, example = %v, want %s", version, device.Default, device.Example, want)
		}
	}
}
