package app_test

import (
	"bytes"
	"strings"
	"testing"

	"phenix/tmpl"
	v1 "phenix/types/version/v1"
)

// TestVyattaTemplateHostnameVerbatim verifies that vyatta.tmpl sets the router's
// system host-name to the topology hostname exactly as written, so the guest
// hostname matches the minimega VM name used by cc filters and tag matching.
func TestVyattaTemplateHostnameVerbatim(t *testing.T) {
	const hostname = "Branch-Router-01"

	tests := []struct {
		name string
		vyos bool
		want string
	}{
		{name: "vyos script", vyos: true, want: "set system host-name " + hostname + "\n"},
		{name: "vyatta config.boot", vyos: false, want: "host-name " + hostname + "\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			node := &v1.Node{
				TypeF:    "Router",
				GeneralF: &v1.General{HostnameF: hostname},
				NetworkF: &v1.Network{},
			}

			data := map[string]any{"node": node, "vyos": tt.vyos}

			var buf bytes.Buffer

			if err := tmpl.GenerateFromTemplate("vyatta.tmpl", data, &buf); err != nil {
				t.Fatal(err)
			}

			if !strings.Contains(buf.String(), tt.want) {
				t.Fatalf("expected %q in rendered config:\n%s", tt.want, buf.String())
			}
		})
	}
}

// TestVyattaTemplateIPSecKeysPerSite verifies that a VyOS router with several
// IPsec sites gets each site's pre-shared key under its own name, and that the
// IKE group gets its proposal.
func TestVyattaTemplateIPSecKeysPerSite(t *testing.T) {
	node := &v1.Node{
		TypeF:    "Router",
		GeneralF: &v1.General{HostnameF: "rtr-site"},
		NetworkF: &v1.Network{},
	}

	site := func(peer, remote, secret string) map[string]any {
		return map[string]any{
			"Local": "10.0.40.2", "Remote": remote, "Peer": peer, "PresharedKey": secret,
			"Tunnels": []map[string]any{{"Local": "10.40.0.0/16", "Remote": "10.10.0.0/16"}},
		}
	}

	data := map[string]any{
		"node": node,
		"vyos": true,
		"ipsec": map[string]any{
			"Interfaces": map[string]string{"eth0": "eth0"},
			"Sites": []map[string]any{
				site("rtr-owop", "10.0.10.2", "owop-secret"),
				site("rtr-grid", "10.0.20.2", "grid-secret"),
			},
		},
	}

	var buf bytes.Buffer

	if err := tmpl.GenerateFromTemplate("vyatta.tmpl", data, &buf); err != nil {
		t.Fatal(err)
	}

	got := buf.String()

	for _, want := range []string{
		"set vpn ipsec authentication psk PSK-rtr-owop secret 'owop-secret'\n",
		"set vpn ipsec authentication psk PSK-rtr-owop id '10.0.10.2'\n",
		"set vpn ipsec authentication psk PSK-rtr-grid secret 'grid-secret'\n",
		"set vpn ipsec authentication psk PSK-rtr-grid id '10.0.20.2'\n",
		"set vpn ipsec ike-group IKE-1W proposal 1 encryption aes256\n",
		"set vpn ipsec ike-group IKE-1W proposal 1 hash sha1\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("expected %q in rendered config:\n%s", want, got)
		}
	}

	if strings.Contains(got, "esp-group IKE-1W") {
		t.Errorf("the IKE group's settings went to an ESP group:\n%s", got)
	}
}
