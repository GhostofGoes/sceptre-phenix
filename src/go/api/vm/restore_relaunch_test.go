package vm_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/activeshadow/libminimega/minicli"

	"phenix/api/vm"
	"phenix/store"
	"phenix/util/file"
	"phenix/util/mm"
)

// restoreMM is the mm.MM side of a restore of test-vm in a namespace that also
// runs TEST-VM, which minimega's case-folding name= filter lists first.
type restoreMM struct {
	snapshotMM
}

func (restoreMM) GetVMInfo(...mm.Option) mm.VMs {
	return mm.VMs{
		{Name: "TEST-VM", UUID: "aaaa-0001", Host: "head", State: "RUNNING", Running: true},
		{Name: "test-vm", UUID: "bbbb-0002", Host: "head", State: "RUNNING", Running: true},
	}
}

// KillVM kills through minimega, which the fake answers, as phenix does.
func (snapshotMM) KillVM(opts ...mm.Option) error {
	return mm.Minimega{}.KillVM(opts...) //nolint:wrapcheck // passthrough
}

// restoreFiles lists the snapshot snap1 of test-vm.
type restoreFiles struct {
	file.ClusterFiles
}

func (restoreFiles) GetExperimentSnapshots(string) ([]string, error) {
	return []string{"test-vm__snap1"}, nil
}

// installRestoreTest makes test-vm of the running test-experiment restorable
// from test-vm__snap1, with minimega answering each command with answer.
func installRestoreTest(t *testing.T, answer func(cmd string) minicli.Responses) *fakeMinimega {
	t.Helper()

	originalStore, originalMM, originalFiles := store.DefaultStore, mm.DefaultMM, file.DefaultClusterFiles

	t.Cleanup(func() {
		store.DefaultStore = originalStore       //nolint:reassign // restore test double
		mm.DefaultMM = originalMM                //nolint:reassign // restore test double
		file.DefaultClusterFiles = originalFiles //nolint:reassign // restore test double
	})

	getTestExperiment(t, "test-vm", nil, "2026-10-20T00:00:00Z")

	mm.DefaultMM = restoreMM{}                //nolint:reassign // install test double
	file.DefaultClusterFiles = restoreFiles{} //nolint:reassign // install test double

	return useFakeMinimega(t, answer)
}

// sentCommand reports whether fake was sent a command ending in want.
func sentCommand(fake *fakeMinimega, want string) bool {
	return slices.ContainsFunc(fake.sent(), func(cmd string) bool { return strings.HasSuffix(cmd, want) })
}

// TestRestoreKeepsTheUUIDOfTheExactVM is a regression test: Restore gave the
// relaunched VM the UUID of the first VM minimega's case-folding name filter
// listed, so with VMs TEST-VM and test-vm a restore of test-vm took TEST-VM's
// UUID, and its miniccc client then answered as TEST-VM.
func TestRestoreKeepsTheUUIDOfTheExactVM(t *testing.T) {
	fake := installRestoreTest(t, func(string) minicli.Responses { return minicli.Responses{{Host: "head"}} })

	if err := vm.Restore("test-experiment", "test-vm", "test-vm__snap1"); err != nil {
		t.Fatalf("Restore() = %v", err)
	}

	if want := "vm config uuid bbbb-0002"; !sentCommand(fake, want) {
		t.Errorf("Restore() sent %q, want %q", fake.sent(), want)
	}
}

// TestRestoreRelaunchesPoweredOffVMs is a regression test: Restore always sent
// `vm kill`, which minimega refuses with `VM state error` for a QUIT or ERROR
// VM, whose QEMU is gone, so a VM shut down from the guest or failed by
// minimega could not be restored, and its `vm flush` failed on every
// namespace host that does not run the VM, which answers `vm not found`.
func TestRestoreRelaunchesPoweredOffVMs(t *testing.T) {
	for _, state := range []string{"QUIT", "ERROR", "RUNNING"} {
		gone := state != "RUNNING"

		t.Run(state, func(t *testing.T) {
			fake := installRestoreTest(t, func(cmd string) minicli.Responses {
				switch {
				case strings.HasSuffix(cmd, "vm info summary"):
					return mmTable([]string{"name", "state"}, []string{"test-vm", state})
				case strings.HasSuffix(cmd, "vm kill test-vm") && gone:
					return mmError("VM state error: test-vm")
				case strings.HasSuffix(cmd, "vm flush test-vm"):
					return minicli.Responses{{Host: "head"}, {Host: "compute1", Error: "vm not found: test-vm"}}
				default:
					return minicli.Responses{{Host: "head"}}
				}
			})

			if err := vm.Restore("test-experiment", "test-vm", "test-vm__snap1"); err != nil {
				t.Fatalf("Restore() of a %s VM = %v", state, err)
			}

			if killed := sentCommand(fake, "vm kill test-vm"); killed == gone {
				t.Errorf("Restore() of a %s VM sent vm kill: %v, want %v", state, killed, !gone)
			}

			for _, want := range []string{"vm flush test-vm", "vm launch kvm test-vm", "vm start test-vm"} {
				if !sentCommand(fake, want) {
					t.Errorf("Restore() of a %s VM sent %q, want %q", state, fake.sent(), want)
				}
			}
		})
	}
}

// restoreHostsMM is restoreMM with TEST-VM on compute1, listed first, and
// test-vm on the headnode, and it records the hosts qemu-img is asked on.
type restoreHostsMM struct {
	snapshotMM

	hosts *[]string
}

func (restoreHostsMM) GetVMInfo(...mm.Option) mm.VMs {
	return mm.VMs{
		{Name: "TEST-VM", UUID: "aaaa-0001", Host: "compute1", State: "RUNNING", Running: true},
		{Name: "test-vm", UUID: "bbbb-0002", Host: "head", State: "RUNNING", Running: true},
	}
}

func (m restoreHostsMM) MeshShellResponse(host, cmd string) (string, error) {
	*m.hosts = append(*m.hosts, host)

	return m.snapshotMM.MeshShellResponse(host, cmd) //nolint:wrapcheck // passthrough
}

// TestRestoreChecksTheDiskOnTheExactVMsHost is a regression test: Restore
// checked the snapshot disk's backing chain on the host of the first VM
// minimega's case-folding name filter listed, so with TEST-VM on another host
// a restore of test-vm ran qemu-img where test-vm's disk is not.
func TestRestoreChecksTheDiskOnTheExactVMsHost(t *testing.T) {
	installRestoreTest(t, func(string) minicli.Responses { return minicli.Responses{{Host: "head"}} })

	var hosts []string

	mm.DefaultMM = restoreHostsMM{hosts: &hosts} //nolint:reassign // install test double

	if err := vm.Restore("test-experiment", "test-vm", "test-vm__snap1"); err != nil {
		t.Fatalf("Restore() = %v", err)
	}

	if !slices.Equal(hosts, []string{"head"}) {
		t.Errorf("Restore() checked the snapshot disk on %q, want test-vm's host head", hosts)
	}
}
