package tmpl

import (
	"bytes"
	"strings"
	"testing"

	v1 "phenix/types/version/v1"
)

func TestLinuxInterfacesSetsMTU(t *testing.T) {
	node := &v1.Node{
		NetworkF: &v1.Network{
			InterfacesF: []*v1.Interface{
				{NameF: "eth0", ProtoF: "static", AddressF: "10.10.0.2", MaskF: 24, MTUF: 1400},
				{NameF: "eth1", ProtoF: "dhcp", MTUF: 9000},
				{NameF: "eth2", ProtoF: "static", AddressF: "10.30.0.2", MaskF: 24},
			},
		},
	}

	var buf bytes.Buffer

	if err := GenerateFromTemplate("linux_interfaces.tmpl", node, &buf); err != nil {
		t.Fatalf("GenerateFromTemplate() error = %v", err)
	}

	script := buf.String()

	for _, want := range []string{
		`ip link set dev "$dev" mtu 1400`,
		`ip link set dev "$dev" mtu 9000`,
		`ifconfig "$dev" mtu 1400`,
		`ifconfig "$dev" mtu 9000`,
	} {
		if n := strings.Count(script, want); n != 1 {
			t.Errorf("script has %d lines %q, want 1:\n%s", n, want, script)
		}
	}

	// An interface without an MTU keeps the guest's.
	if n := strings.Count(script, " mtu "); n != 4 {
		t.Errorf("script sets the MTU %d times, want 4 (eth2 has none):\n%s", n, script)
	}

	// The MTU is set while the link is down, before the address and the link
	// come up.
	mtu := strings.Index(script, `ip link set dev "$dev" mtu 1400`)
	addr := strings.Index(script, `ip addr add 10.10.0.2/24 dev "$dev"`)

	if mtu < 0 || addr < 0 || mtu > addr {
		t.Errorf("eth0's MTU (at %d) is not set before its address (at %d):\n%s", mtu, addr, script)
	}
}
