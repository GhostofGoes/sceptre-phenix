package mm

import (
	"errors"
	"strings"
	"testing"

	"github.com/activeshadow/libminimega/minicli"
)

// caseMinimega is a fake minimega whose namespace holds VM1 and vm1, which
// minimega's `.filter name=vm1` both match: it folds case.
func caseMinimega(t *testing.T) {
	t.Helper()

	fake := &fakeMinimega{answer: func(cmd string) minicli.Responses {
		switch {
		case strings.Contains(cmd, "vm info"):
			header := []string{"name", "state", "uuid", "vnc_port"}

			return minicli.Responses{
				{Host: "h2", Header: header, Tabular: [][]string{{"VM1", "PAUSED", "aaaa-0001", "5901"}}},
				{Host: "h1", Header: header, Tabular: [][]string{{"vm1", "RUNNING", "bbbb-0002", "5900"}}},
			}
		case strings.Contains(cmd, "cc response"):
			return minicli.Responses{{Host: "head", Response: "1/aaaa-0001/stdout:\nupper\n1/bbbb-0002/stdout:\nlower\n"}}
		default:
			return minicli.Responses{{Host: "head"}}
		}
	}}
	fake.serve(t)
}

// TestVMLookupsPickTheVMOfTheExactName is a regression test: GetVMHost,
// GetVMState and GetC2Response took the first VM minimega's case-folding
// name filter listed, so with VMs VM1 and vm1 they answered for VM1 when asked
// about vm1, and GetVNCEndpoint took the last one, so the VNC console of VM1
// connected to vm1.
func TestVMLookupsPickTheVMOfTheExactName(t *testing.T) {
	caseMinimega(t)

	for _, tc := range []struct{ vm, host, state, output, vnc string }{
		{"vm1", "h1", "RUNNING", "lower", "h1:5900"},
		{"VM1", "h2", "PAUSED", "upper", "h2:5901"},
	} {
		if vnc, err := (Minimega{}).GetVNCEndpoint(NS("exp"), VMName(tc.vm)); err != nil || vnc != tc.vnc {
			t.Errorf("GetVNCEndpoint(%s) = %q, %v; want %q", tc.vm, vnc, err, tc.vnc)
		}

		if host, err := (Minimega{}).GetVMHost(NS("exp"), VMName(tc.vm)); err != nil || host != tc.host {
			t.Errorf("GetVMHost(%s) = %q, %v; want %q", tc.vm, host, err, tc.host)
		}

		if state, err := (Minimega{}).GetVMState(NS("exp"), VMName(tc.vm)); err != nil || state != tc.state {
			t.Errorf("GetVMState(%s) = %q, %v; want %q", tc.vm, state, err, tc.state)
		}

		out, err := Minimega{}.GetC2Response(C2NS("exp"), C2VM(tc.vm), C2CommandID("1"), C2ResponseTypeStdout())
		if err != nil || strings.TrimSpace(out) != tc.output {
			t.Errorf("GetC2Response(%s) = %q, %v; want %q", tc.vm, out, err, tc.output)
		}
	}
}

// TestKillAndRedeploySkipKillOfQuitVMs is a regression test: KillVM and
// RedeployVM always sent `vm kill`, which minimega refuses for a QUIT or ERROR
// VM, whose QEMU is gone, so such a VM could be neither deleted nor
// redeployed. Only a BUILDING, RUNNING or PAUSED VM is killed; every VM is
// flushed.
func TestKillAndRedeploySkipKillOfQuitVMs(t *testing.T) {
	for _, state := range []string{"QUIT", "ERROR", "RUNNING", "PAUSED"} {
		gone := state == "QUIT" || state == "ERROR"

		t.Run(state, func(t *testing.T) {
			fake := &fakeMinimega{answer: func(cmd string) minicli.Responses {
				switch {
				case strings.Contains(cmd, "vm info"):
					return minicli.Responses{{
						Host:    "head",
						Header:  []string{"name", "state", "type", "disks"},
						Tabular: [][]string{{"vm1", state, "kvm", ""}},
					}}
				case strings.Contains(cmd, "vm kill") && gone:
					return minicli.Responses{{Host: "head", Error: "VM state error: vm1"}}
				default:
					return minicli.Responses{{Host: "head"}}
				}
			}}
			fake.serve(t)

			if err := (Minimega{}).KillVM(NS("exp"), VMName("vm1")); err != nil {
				t.Errorf("KillVM of a %s VM = %v", state, err)
			}

			if err := (Minimega{}).RedeployVM(NS("exp"), VMName("vm1")); err != nil {
				t.Errorf("RedeployVM of a %s VM = %v", state, err)
			}

			var kills, flushes int

			for _, cmd := range fake.sent() {
				if strings.Contains(cmd, "vm kill vm1") {
					kills++
				}

				if strings.Contains(cmd, "vm flush vm1") {
					flushes++
				}
			}

			wantKills := 2
			if gone {
				wantKills = 0
			}

			if kills != wantKills || flushes != 2 {
				t.Errorf("a %s VM got %d kills and %d flushes, want %d and 2: %q", state, kills, flushes, wantKills, fake.sent())
			}
		})
	}
}

// TestVMCommandsReportVMsMinimegaDoesNotKnow checks that vm start, stop and
// kill of a name minimega does not know wrap ErrVMNotFound, which the VM
// routes answer with 400 instead of 500.
func TestVMCommandsReportVMsMinimegaDoesNotKnow(t *testing.T) {
	fake := &fakeMinimega{answer: func(cmd string) minicli.Responses {
		if strings.Contains(cmd, "vm info") {
			return minicli.Responses{{Host: "head", Header: []string{"name", "state"}}}
		}

		return minicli.Responses{{Host: "head", Error: "vm not found: vm9"}}
	}}
	fake.serve(t)

	for verb, call := range map[string]func(...Option) error{
		"start": Minimega{}.StartVM,
		"stop":  Minimega{}.StopVM,
		"kill":  Minimega{}.KillVM,
	} {
		if err := call(NS("exp"), VMName("vm9")); !errors.Is(err, ErrVMNotFound) {
			t.Errorf("vm %s of an unknown VM = %v, want ErrVMNotFound", verb, err)
		}
	}
}
