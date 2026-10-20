package vm_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/activeshadow/libminimega/minicli"

	"phenix/api/vm"
	"phenix/util/common"
	"phenix/util/mm"
)

// resetDiskMM is the mm.MM side of ResetDiskState for a VM on the headnode.
type resetDiskMM struct {
	mm.MM

	started *bool
}

func (resetDiskMM) IsHeadnode(node string) bool {
	return node == "head"
}

func (m resetDiskMM) StartVM(...mm.Option) error {
	*m.started = true

	return nil
}

func (resetDiskMM) GetVMCaptures(...mm.Option) []mm.Capture {
	return nil
}

// TestResetDiskStateSnapshotsIntoInstanceDirectory checks that reset-disk
// creates the new instance disk where the VM uses it, because minimega records
// the snapshot's backing file relative to the destination unless it runs with
// -abssnapshot.
func TestResetDiskStateSnapshotsIntoInstanceDirectory(t *testing.T) {
	const orig = "/phenix/images/head_test-experiment_test-vm_snapshot"

	getTestExperiment(t, "test-vm", nil, "2026-10-18T00:00:00Z")

	original := mm.DefaultMM
	t.Cleanup(func() { mm.DefaultMM = original }) //nolint:reassign // restore test double

	var started bool

	mm.DefaultMM = resetDiskMM{started: &started} //nolint:reassign // install test double

	fake := useFakeMinimega(t, func(cmd string) minicli.Responses {
		if strings.HasSuffix(cmd, "vm info") {
			return mmTable(
				[]string{"name", "id", "state", "disks", "snapshot"},
				[]string{"test-vm", "5", "QUIT", orig + ",writeback", "true"},
			)
		}

		return minicli.Responses{{Host: "head"}}
	})

	if err := vm.ResetDiskState("test-experiment", "test-vm"); err != nil {
		t.Fatalf("ResetDiskState() = %v", err)
	}

	var (
		want  = "disk snapshot " + orig + " " + common.MinimegaBase + "/5/disk-0.qcow2"
		found bool
	)

	for _, cmd := range fake.sent() {
		switch {
		case strings.HasSuffix(cmd, want):
			found = true
		case strings.Contains(cmd, "shell mv"):
			t.Errorf("ResetDiskState() moved a disk: %q", cmd)
		}
	}

	if !found {
		t.Errorf("ResetDiskState() did not run %q; minimega got %q", want, fake.sent())
	}

	if !started {
		t.Error("ResetDiskState() did not start the VM again")
	}
}

// TestResetDiskStateKillsPausedVM is a regression test: reset-disk killed only
// a RUNNING VM, so for a PAUSED one `disk snapshot` failed on the write lock
// QEMU holds on the instance disk, and a failed `disk snapshot` left a VM it
// had killed powered off. An ERROR VM, which minimega does not kill, is reset
// without a kill.
func TestResetDiskStateKillsPausedVM(t *testing.T) {
	const orig = "/phenix/images/head_test-experiment_test-vm_snapshot"

	for _, tc := range []struct {
		name      string
		state     string
		snapshot  minicli.Responses
		wantKill  bool
		wantStart bool
		wantErr   string
	}{
		{name: "paused", state: "PAUSED", wantKill: true, wantStart: true},
		{name: "running", state: "RUNNING", wantKill: true, wantStart: true},
		{name: "building", state: "BUILDING", wantKill: true, wantStart: true},
		{name: "powered off", state: "QUIT", wantStart: true},
		// minimega answers `VM state error` to a kill of an ERROR VM, whose
		// QEMU is gone; `vm start` relaunches it.
		{name: "in error", state: "ERROR", wantStart: true},
		{
			name:      "the snapshot fails",
			state:     "RUNNING",
			snapshot:  mmError(`qemu-img: Could not open '` + orig + `': No such file or directory`),
			wantKill:  true,
			wantStart: true,
			wantErr:   "taking disk snapshot remotely for VM test-vm",
		},
		{
			name:     "the snapshot of a powered-off VM fails",
			state:    "QUIT",
			snapshot: mmError("qemu-img: exit status 1"),
			wantErr:  "taking disk snapshot remotely for VM test-vm",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			getTestExperiment(t, "test-vm", nil, "2026-10-18T00:00:00Z")

			original := mm.DefaultMM
			t.Cleanup(func() { mm.DefaultMM = original }) //nolint:reassign // restore test double

			var started bool

			mm.DefaultMM = resetDiskMM{started: &started} //nolint:reassign // install test double

			fake := useFakeMinimega(t, func(cmd string) minicli.Responses {
				switch {
				case strings.HasSuffix(cmd, "vm info"):
					return mmTable(
						[]string{"host", "name", "id", "state", "disks", "snapshot"},
						[]string{"head", "test-vm", "5", tc.state, orig + ",writeback", "true"},
					)
				case strings.Contains(cmd, "disk snapshot ") && tc.snapshot != nil:
					return tc.snapshot
				case strings.HasSuffix(cmd, "vm kill test-vm") && !slices.Contains([]string{"BUILDING", "RUNNING", "PAUSED"}, tc.state):
					return mmError("VM state error: test-vm")
				default:
					return minicli.Responses{{Host: "head"}}
				}
			})

			err := vm.ResetDiskState("test-experiment", "test-vm")

			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("ResetDiskState() = %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("ResetDiskState() = %v, want an error saying %q", err, tc.wantErr)
			}

			if got := slices.Contains(fake.sent(), "vm kill test-vm"); got != tc.wantKill {
				t.Errorf("ResetDiskState() killed test-vm = %v, want %v; minimega got %q", got, tc.wantKill, fake.sent())
			}

			if started != tc.wantStart {
				t.Errorf("ResetDiskState() started test-vm = %v, want %v", started, tc.wantStart)
			}
		})
	}
}
