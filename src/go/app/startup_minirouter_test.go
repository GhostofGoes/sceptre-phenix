package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestStartupPreStartMinirouterHostname is a regression test: the startup app
// skipped every Router node, so a minirouter kept its image's hostname
// "phenix" and hostname-mode C2 (SOH, delay.c2, autotunnel) never matched it.
// A minirouter gets only the hostname script, since minirouter configures its
// interfaces itself.
func TestStartupPreStartMinirouterHostname(t *testing.T) {
	tests := []struct {
		name            string
		nodeType        string
		osType          string
		injectPartition int
		annotations     map[string]any
		wantHostname    bool
		wantC2          bool
	}{
		{name: "minirouter router", nodeType: "Router", osType: osMinirouter, injectPartition: 1, wantHostname: true},
		{name: "minirouter router over C2", nodeType: "Router", osType: osMinirouter, wantHostname: true, wantC2: true},
		{name: "minirouter firewall", nodeType: "Firewall", osType: osMinirouter, injectPartition: 1, wantHostname: true},
		{
			name: "minirouter with default apps off", nodeType: "Router", osType: osMinirouter, injectPartition: 1,
			annotations: map[string]any{defaultAppsAnnotation: false},
		},
		{name: "vyatta router", nodeType: "Router", osType: "vyatta", injectPartition: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			node := newStartupTestNode(t, "rtr", tt.osType, true, tt.injectPartition)
			node.TypeF = tt.nodeType
			node.AnnotationsF = tt.annotations

			exp, mmDir := newStartupTestExperiment(t, node, nil)
			runStartupPreStart(t, exp, mmDir)

			if got := hasInjection(node, linuxHostnameInjectDst); got != tt.wantHostname {
				t.Errorf("hostname script injected = %v, want %v", got, tt.wantHostname)
			}

			for _, dst := range []string{linuxTimezoneInjectDst, linuxIfaceInjectDst, linuxDomainInjectDst} {
				if hasInjection(node, dst) {
					t.Errorf("router has %s injected", dst)
				}
			}

			hostnameFile := filepath.Join(exp.Spec.BaseDir(), "startup", "rtr-hostname.sh")

			script, err := os.ReadFile(hostnameFile)
			switch {
			case tt.wantHostname && err != nil:
				t.Errorf("reading hostname script: %v", err)
			case tt.wantHostname && !strings.Contains(string(script), "hostnamectl set-hostname rtr"):
				t.Errorf("hostname script does not set hostname rtr:\n%s", script)
			case !tt.wantHostname && err == nil:
				t.Errorf("hostname script rendered for a node that gets none:\n%s", script)
			}

			send, exec, _ := startupC2Commands(exp, hostnameFile, startupC2Linux)

			if got := hasCommand(node, send) && hasCommand(node, exec); got != tt.wantC2 {
				t.Errorf("hostname script sent over C2 = %v, want %v (commands %q)", got, tt.wantC2, node.CommandsF)
			}

			if !tt.wantC2 && len(node.CommandsF) != 0 {
				t.Errorf("router has C2 commands %q, want none", node.CommandsF)
			}

			if tt.wantC2 && len(node.CommandsF) != 2 {
				t.Errorf("router has C2 commands %q, want only the hostname script's", node.CommandsF)
			}
		})
	}
}
