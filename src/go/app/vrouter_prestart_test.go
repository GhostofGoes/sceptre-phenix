package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	v1 "phenix/types/version/v1"
	v2 "phenix/types/version/v2"
)

// newVyattaTestNode builds a Vyatta router with a static LAN interface and a
// DHCP WAN interface that the topology masquerades the LAN out of.
func newVyattaTestNode() *v1.Node {
	return &v1.Node{
		TypeF:    "Router",
		GeneralF: &v1.General{HostnameF: "v1", DoNotBootF: boolPtr(false)},
		HardwareF: &v1.Hardware{
			OSTypeF: "vyatta",
			DrivesF: []*v1.Drive{{ImageF: "vyatta.qc2"}},
		},
		NetworkF: &v1.Network{
			InterfacesF: []*v1.Interface{
				{NameF: "eth0", TypeF: "ethernet", ProtoF: "static", VLANF: "LAN", AddressF: "10.60.0.254", MaskF: 24},
				{NameF: "eth1", TypeF: "ethernet", ProtoF: "dhcp", VLANF: "WAN"},
			},
			NATF: []v1.NAT{{InF: []string{"eth0"}, OutF: "eth1"}},
		},
	}
}

// TestVrouterPreStartNATPrecedence is a regression test: the topology NAT
// fallback tested a "nat" key the app never sets, so a router's topology NAT
// replaced the snat rules of its vrouter app metadata.
func TestVrouterPreStartNATPrecedence(t *testing.T) {
	tests := []struct {
		name     string
		metadata map[string]any // nil: v1 is not a host of the vrouter app
		want     []string
		lacks    []string
	}{
		{
			name: "app snat replaces topology nat",
			metadata: map[string]any{
				"snat": []any{
					map[string]any{"interface": "eth1", "srcAddr": "10.60.0.0/24", "translation": "192.0.2.10"},
				},
			},
			want:  []string{"address 192.0.2.10"},
			lacks: []string{"address masquerade"},
		},
		{
			name: "topology nat without app metadata",
			want: []string{"outbound-interface eth1", "address 10.60.0.0/24", "address masquerade"},
		},
		{
			name: "topology nat beside app dnat",
			metadata: map[string]any{
				"dnat": []any{
					map[string]any{
						"interface": "eth1", "dstAddr": "192.0.2.1", "dstPort": 80,
						"protocol": "tcp", "translation": "10.60.0.10:8080",
					},
				},
			},
			want: []string{"address masquerade", "inbound-interface eth1", "address 10.60.0.10"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var scenario *v2.ScenarioSpec

			if tt.metadata != nil {
				scenario = &v2.ScenarioSpec{AppsF: []*v2.ScenarioApp{{
					NameF:  appNameVrouter,
					HostsF: []*v2.ScenarioAppHost{{HostnameF: "v1", MetadataF: tt.metadata}},
				}}}
			}

			exp, _ := newStartupTestExperiment(t, newVyattaTestNode(), scenario)

			if err := new(Vrouter).PreStart(context.Background(), exp); err != nil {
				t.Fatalf("running vrouter PreStart: %v", err)
			}

			data, err := os.ReadFile(filepath.Join(exp.Spec.BaseDir(), "vrouter", "v1.boot"))
			if err != nil {
				t.Fatalf("reading rendered config: %v", err)
			}

			boot := string(data)

			for _, want := range tt.want {
				if !strings.Contains(boot, want) {
					t.Errorf("v1.boot lacks %q:\n%s", want, boot)
				}
			}

			for _, lack := range tt.lacks {
				if strings.Contains(boot, lack) {
					t.Errorf("v1.boot contains %q:\n%s", lack, boot)
				}
			}
		})
	}
}
