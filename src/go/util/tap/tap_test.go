package tap_test

import (
	"strings"
	"testing"

	"phenix/util/mm"
	"phenix/util/tap"
)

// tapTestMM is a test double for mm.MM that records the tap and shell
// commands a host tap sends.
type tapTestMM struct {
	mm.MM

	taps   int
	shells []string
}

func (m *tapTestMM) TapVLAN(...mm.TapOption) error {
	m.taps++

	return nil
}

func (m *tapTestMM) MeshShell(_, cmd string) error {
	m.shells = append(m.shells, cmd)

	return nil
}

func installTapTestMM(t *testing.T) *tapTestMM {
	t.Helper()

	original := mm.DefaultMM
	t.Cleanup(func() { mm.DefaultMM = original }) //nolint:reassign // restore test double

	fake := new(tapTestMM)
	mm.DefaultMM = fake //nolint:reassign // install test double

	return fake
}

// TestCreateRefusesExternalAccessWithoutAMask checks that a tap with external
// access and an address without a prefix length is refused with an error
// naming the address before anything is created on the host, instead of
// panicking after the tap and its namespace exist.
func TestCreateRefusesExternalAccessWithoutAMask(t *testing.T) {
	for _, ip := range []string{"10.10.0.201", ""} {
		t.Run(ip, func(t *testing.T) {
			fake := installTapTestMM(t)

			tp := &tap.Tap{VLAN: "LAN", IP: ip, Name: "abcd1234-tapapp"}
			tp.External.Enabled = true
			tp.Init("phenix", tap.Experiment("tap-test"))

			pair, err := tp.Create("compute-0")
			if err == nil {
				t.Fatalf("Create with external access and ip %q returned pair %v, want an error", ip, pair)
			}

			if !strings.Contains(err.Error(), `"`+ip+`"`) || !strings.Contains(err.Error(), "VLAN LAN") {
				t.Errorf("Create error = %q, want it to name VLAN LAN and the address %q", err, ip)
			}

			if fake.taps != 0 || len(fake.shells) != 0 {
				t.Errorf("Create sent %d tap and %d shell commands (%v), want none", fake.taps, len(fake.shells), fake.shells)
			}
		})
	}
}

// TestCreateExternalAccessWithAMask checks that an address with a prefix
// length still gets a tap and the first free veth pair.
func TestCreateExternalAccessWithAMask(t *testing.T) {
	fake := installTapTestMM(t)

	tp := &tap.Tap{VLAN: "LAN", IP: "10.10.0.201/24", Name: "abcd1234-tapapp"}
	tp.External.Enabled = true
	tp.Init("phenix", tap.Experiment("tap-test"))

	pair, err := tp.Create("compute-0")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if pair.String() != "10.213.47.0/30" || tp.Subnet != "10.213.47.0/30" {
		t.Errorf("Create pair = %v, tap subnet = %q, want 10.213.47.0/30", pair, tp.Subnet)
	}

	if fake.taps != 1 {
		t.Errorf("Create sent %d tap commands, want 1", fake.taps)
	}

	if want := "ip link add abcd1234-tapapp type veth peer name abcd1234-_right"; len(fake.shells) == 0 || fake.shells[0] != want {
		t.Errorf("first shell command = %v, want %q", fake.shells, want)
	}
}

// TestValidateChecksEveryTap is a regression test: only the address of a tap
// with external access was checked, so an invalid address, VLAN or bridge of
// any other tap reached minimega and `ip addr add` after the taps before it
// existed.
func TestValidateChecksEveryTap(t *testing.T) {
	for _, tc := range []struct {
		name string
		tap  tap.Tap
		want string // empty when the tap is valid
	}{
		{name: "address with a prefix", tap: tap.Tap{VLAN: "LAN", IP: "10.10.0.201/24"}},
		{name: "address without a prefix", tap: tap.Tap{VLAN: "LAN", IP: "10.10.0.201"}},
		{name: "no address", tap: tap.Tap{VLAN: "LAN"}},
		{name: "bridge", tap: tap.Tap{VLAN: "LAN", Bridge: "phenix", IP: "10.10.0.201/24"}},
		{name: "no VLAN", tap: tap.Tap{IP: "10.10.0.201/24"}, want: "needs a VLAN"},
		{name: "VLAN of two words", tap: tap.Tap{VLAN: "LAN WAN"}, want: `"LAN WAN"`},
		{name: "VLAN with a quote", tap: tap.Tap{VLAN: `LAN"`}, want: "one word"},
		{name: "VLAN with a dollar", tap: tap.Tap{VLAN: "$LAN"}, want: "one word"},
		{name: "bridge of two words", tap: tap.Tap{VLAN: "LAN", Bridge: "br0 name x"}, want: `"br0 name x"`},
		{name: "octet out of range", tap: tap.Tap{VLAN: "LAN", IP: "10.0.0.300/24"}, want: `"10.0.0.300/24"`},
		{name: "extra arguments", tap: tap.Tap{VLAN: "LAN", IP: "10.0.0.1/24 dev lo"}, want: `"10.0.0.1/24 dev lo"`},
		{name: "dhcp", tap: tap.Tap{VLAN: "LAN", IP: "dhcp"}, want: "VLAN LAN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.tap.Validate()

			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("Validate: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("Validate error = %v, want one containing %s", err, tc.want)
			}
		})
	}
}
