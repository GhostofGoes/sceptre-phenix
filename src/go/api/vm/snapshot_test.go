package vm_test

import (
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/activeshadow/libminimega/minicli"

	"phenix/api/vm"
	"phenix/store"
	"phenix/util/mm"
	"phenix/util/mm/mmcli"
)

// snapshotMM is the mm.MM side of a running VM named test-vm on the headnode.
type snapshotMM struct {
	mm.MM
}

func (snapshotMM) GetVMInfo(...mm.Option) mm.VMs {
	return mm.VMs{{Name: "test-vm", Host: "head", State: "RUNNING", Running: true}}
}

func (m snapshotMM) GetVMInfoErr(opts ...mm.Option) (mm.VMs, error) {
	return m.GetVMInfo(opts...), nil
}

func (snapshotMM) IsHeadnode(node string) bool {
	return node == "head"
}

func (snapshotMM) Headnode() string {
	return "head"
}

// MeshShellResponse answers qemu-img's description of a snapshot disk that
// backs onto nothing, which a restore accepts.
func (snapshotMM) MeshShellResponse(string, string) (string, error) {
	return `[{"filename": "/srv/minimega/files/test-experiment/files/test-vm__s1.hdd", "format": "qcow2"}]`, nil
}

// snapshotMinimega answers the commands of vm.Snapshot for test-vm. Each
// field is the answer to one command; a nil field answers with no error.
type snapshotMinimega struct {
	info   []string          // the snapshot and disks of vm info; nil for a disk snapshot
	save   minicli.Responses // vm save test-vm <file>
	status minicli.Responses // QEMU's query-migrate of test-vm; nil for no migration
	state  string            // the state vm info reports after a failed save
	start  minicli.Responses // vm start test-vm
	mkdir  minicli.Responses // shell mkdir -p <experiment files directory>
	mvMem  minicli.Responses // shell mv <saved>.state <experiment files directory>
	mvDisk minicli.Responses // shell mv <saved>.hdd <experiment files directory>
	rm     minicli.Responses // shell rm -f <files of a failed snapshot>
}

func (m snapshotMinimega) answer(cmd string) minicli.Responses {
	switch {
	case strings.HasSuffix(cmd, "vm info") && strings.Contains(cmd, `"state"`):
		return mmTable([]string{"state"}, []string{m.state})
	case strings.HasSuffix(cmd, "vm info"):
		info := m.info
		if info == nil {
			info = []string{"true", "/phenix/images/head_test-experiment_test-vm_snapshot"}
		}

		return mmTable([]string{"snapshot", "disks"}, info)
	case strings.HasPrefix(cmd, "vm save test-vm "):
		return m.save
	case strings.HasSuffix(cmd, "vm save"):
		// minimega's status table of every VM, which one powered-off VM fails.
		return mmError("write unix @->/tmp/minimega/7/qmp: write: broken pipe")
	case strings.Contains(cmd, `"query-migrate"`):
		if m.status == nil {
			return qmpMigrate("")
		}

		return m.status
	case strings.HasSuffix(cmd, "vm start test-vm"):
		return m.start
	}

	shell := strings.TrimSpace(cmd)

	switch {
	case strings.HasPrefix(shell, "shell mkdir "):
		return m.mkdir
	case strings.HasPrefix(shell, "shell mv ") && strings.HasSuffix(shell, ".state"):
		return m.mvMem
	case strings.HasPrefix(shell, "shell mv ") && strings.HasSuffix(shell, ".hdd"):
		return m.mvDisk
	case strings.HasPrefix(shell, "shell rm "):
		return m.rm
	default:
		return nil
	}
}

// qmpMigrate is QEMU's answer to query-migrate for a migration with status,
// 40% of its memory copied, or for no migration when status is "".
func qmpMigrate(status string) minicli.Responses {
	resp := `{"return": {}}`
	if status != "" {
		resp = `{"return": {"status": "` + status + `", "ram": {"total": 1000, "transferred": 400}}}`
	}

	return minicli.Responses{{Host: "head", Response: resp}}
}

func installSnapshotTest(t *testing.T, m snapshotMinimega) *fakeMinimega {
	t.Helper()

	originalStore, originalMM := store.DefaultStore, mm.DefaultMM

	t.Cleanup(func() {
		store.DefaultStore = originalStore //nolint:reassign // restore test double
		mm.DefaultMM = originalMM          //nolint:reassign // restore test double
	})

	getTestExperiment(t, "test-vm", nil, "2026-10-18T00:00:00Z")

	mm.DefaultMM = snapshotMM{} //nolint:reassign // install test double

	return useFakeMinimega(t, func(cmd string) minicli.Responses {
		if resps := m.answer(cmd); resps != nil {
			return resps
		}

		return minicli.Responses{{Host: "head"}}
	})
}

// TestSnapshotResumesVMAfterFailedSave checks that a failed snapshot does not
// leave the VM paused: minimega pauses a VM before it saves its memory, and
// phenix returned the error without starting it again.
func TestSnapshotResumesVMAfterFailedSave(t *testing.T) {
	failed := qmpMigrate("failed")

	const cancel = `vm qmp test-vm '{ "execute": "migrate_cancel" }'`

	for _, tc := range []struct {
		name       string
		minimega   snapshotMinimega
		wantErr    []string
		wantResume bool
		wantCancel bool
	}{
		{
			name:       "the save fails after pausing the VM",
			minimega:   snapshotMinimega{status: failed, state: "PAUSED"},
			wantErr:    []string{"memory snapshot for VM test-vm failed"},
			wantResume: true,
		},
		{
			name:       "the save does not complete in time",
			minimega:   snapshotMinimega{status: qmpMigrate("active"), state: "PAUSED"},
			wantErr:    []string{"memory snapshot for VM test-vm did not complete within"},
			wantResume: true,
			wantCancel: true,
		},
		{
			name: "the save fails before pausing the VM",
			minimega: snapshotMinimega{
				save:  mmError("no space left on device"),
				state: "RUNNING",
			},
			wantErr: []string{"starting memory snapshot for VM test-vm", "no space left on device"},
		},
		{
			name: "the resume fails too",
			minimega: snapshotMinimega{
				status: failed,
				state:  "PAUSED",
				start:  mmError("vm not running: 5"),
			},
			wantErr:    []string{"memory snapshot for VM test-vm failed", "resuming VM test-vm", "vm not running: 5"},
			wantResume: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vm.SetVMSaveTimeout(t, 0)

			fake := installSnapshotTest(t, tc.minimega)

			err := vm.Snapshot("test-experiment", "test-vm", "s1", nil)
			if err == nil {
				t.Fatal("Snapshot() = nil, want the error of the failed save")
			}

			for _, want := range tc.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("Snapshot() = %q, want it to contain %q", err, want)
				}
			}

			sent := fake.sent()

			if got := slices.Contains(sent, "vm start test-vm"); got != tc.wantResume {
				t.Errorf("Snapshot() resumed test-vm = %v, want %v; minimega got %q", got, tc.wantResume, sent)
			}

			// QEMU pauses the VM again when a running save completes, but a
			// cancel of a finished one leaves QEMU's status cancelled, which
			// makes minimega's `vm save` status table panic.
			i := slices.Index(sent, cancel)

			switch {
			case tc.wantCancel && (i < 0 || i > slices.Index(sent, "vm start test-vm")):
				t.Errorf("Snapshot() did not cancel the running save before resuming test-vm; minimega got %q", sent)
			case !tc.wantCancel && i >= 0:
				t.Errorf("Snapshot() canceled a save QEMU no longer runs; minimega got %q", sent)
			}
		})
	}
}

// TestSnapshotBoundsCleanupOfFailedSave is a regression test: the cancel
// and the resume of a failed save were sent without a deadline, and minimega
// waits for QEMU's QMP answer without one, so a QEMU that stopped answering
// held the request and the VM lock for ever after the polls timed out.
func TestSnapshotBoundsCleanupOfFailedSave(t *testing.T) {
	const cancel = `vm qmp test-vm '{ "execute": "migrate_cancel" }'`

	for _, tc := range []struct {
		name     string
		minimega snapshotMinimega
		hang     string
		wantErr  string
	}{
		{
			name:     "the resume hangs",
			minimega: snapshotMinimega{status: qmpMigrate("failed"), state: "PAUSED"},
			hang:     "vm start test-vm",
			wantErr:  "resuming VM test-vm after it",
		},
		{
			name:     "the cancel hangs",
			minimega: snapshotMinimega{status: qmpMigrate("active"), state: "PAUSED"},
			hang:     cancel,
			wantErr:  "canceling the memory snapshot of VM test-vm",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vm.SetVMSaveTimeout(t, 0)
			vm.SetSaveCommandTimeout(t, 100*time.Millisecond)

			installSnapshotTest(t, tc.minimega)

			release := make(chan struct{})
			t.Cleanup(func() { close(release) })

			useFakeMinimega(t, func(cmd string) minicli.Responses {
				if strings.TrimSpace(cmd) == tc.hang {
					// QEMU answers only long after the save gave up on it.
					select {
					case <-release:
					case <-time.After(3 * time.Second):
					}
				}

				if resps := tc.minimega.answer(cmd); resps != nil {
					return resps
				}

				return minicli.Responses{{Host: "head"}}
			})

			start := time.Now()

			err := vm.Snapshot("test-experiment", "test-vm", "s1", nil)
			if !errors.Is(err, mmcli.ErrTimeout) || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("Snapshot() = %v, want a timeout %s", err, tc.wantErr)
			}

			if took := time.Since(start); took > 2*time.Second {
				t.Errorf("Snapshot() returned after %v, want about 100ms", took)
			}
		})
	}
}

// TestSnapshotRefusesUnsafeName checks that a snapshot name is refused
// before minimega is asked anything: a name with whitespace failed `vm save`
// and turned the cleanup into a root `shell rm -f` of the words in the name,
// shell metacharacters ran in QEMU's `exec:cat > <name>.state` migration, and
// an escaped quote made minimega exit when it parsed `vm save` again.
func TestSnapshotRefusesUnsafeName(t *testing.T) {
	for _, name := range []string{
		"x /phenix/images/ubuntu.qc2 y.z",
		"x -rf /var/lib/phenix y",
		"a;reboot;true${IFS}",
		`a\"b`,
		"../../../../etc/x",
		"",
	} {
		t.Run(name, func(t *testing.T) {
			fake := installSnapshotTest(t, snapshotMinimega{})

			err := vm.Snapshot("test-experiment", "test-vm", name, nil)
			if !errors.Is(err, mm.ErrInvalidFileName) || !strings.Contains(fmt.Sprint(err), "VM test-vm") {
				t.Fatalf("Snapshot(%q) = %v, want mm.ErrInvalidFileName naming test-vm", name, err)
			}

			if sent := fake.sent(); len(sent) != 0 {
				t.Errorf("Snapshot(%q) sent %q to minimega", name, sent)
			}
		})
	}
}

// TestSnapshotRefusesVMWithoutDiskSnapshot checks that a VM whose disk
// minimega cannot save is refused before anything is saved: minimega saved
// its memory, and phenix then failed to move the disk it never wrote.
func TestSnapshotRefusesVMWithoutDiskSnapshot(t *testing.T) {
	for _, tc := range []struct {
		name    string
		info    []string
		want    error
		wantErr string
	}{
		{
			name:    "snapshot false",
			info:    []string{"false", "/phenix/images/test.qc2"},
			wantErr: "cannot snapshot VM test-vm: it runs with snapshot: false",
		},
		{
			name:    "no disk",
			info:    []string{"true", ""},
			wantErr: "cannot snapshot VM test-vm: it has no disk",
		},
		{
			// minimega wrote <name>.hdd.1 too, which phenix neither moved nor
			// removed, and a restore gave the VM the first disk only.
			name:    "two disks",
			info:    []string{"true", "/phenix/images/a_snapshot,writeback /phenix/images/data.qc2,virtio"},
			want:    vm.ErrMultiDiskSnapshot,
			wantErr: "cannot snapshot VM test-vm: it has 2 disks",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := installSnapshotTest(t, snapshotMinimega{info: tc.info})

			want := tc.want
			if want == nil {
				want = vm.ErrNoDiskSnapshot
			}

			err := vm.Snapshot("test-experiment", "test-vm", "s1", nil)
			if !errors.Is(err, want) || !strings.Contains(fmt.Sprint(err), tc.wantErr) {
				t.Fatalf("Snapshot() = %v, want %v saying %q", err, want, tc.wantErr)
			}

			for _, cmd := range fake.sent() {
				if strings.HasPrefix(cmd, "vm save") {
					t.Errorf("Snapshot() sent %q for a VM it refused", cmd)
				}
			}
		})
	}
}

// TestSnapshotMovesFilesFromMinimegaFilesDirectory checks that the saved
// files are moved from minimega's files directory, which phenix assumed was
// <phenix base>/images.
func TestSnapshotMovesFilesFromMinimegaFilesDirectory(t *testing.T) {
	const files = "/srv/minimega/files"

	vm.SetMMFullPath(t, func(p string) string { return path.Join(files, p) })

	fake := installSnapshotTest(t, snapshotMinimega{status: qmpMigrate("completed")})

	if err := vm.Snapshot("test-experiment", "test-vm", "s1.snap", nil); err != nil {
		t.Fatalf("Snapshot() = %v", err)
	}

	var shell []string

	for _, cmd := range fake.sent() {
		if cmd = strings.TrimSpace(cmd); strings.HasPrefix(cmd, "shell ") {
			shell = append(shell, cmd)
		}
	}

	want := []string{
		"shell mkdir -p " + files + "/test-experiment/files",
		"shell mv " + files + "/test-experiment_test-vm__s1.state " + files + "/test-experiment/files/test-vm__s1.state",
		"shell mv " + files + "/test-experiment_test-vm__s1.hdd " + files + "/test-experiment/files/test-vm__s1.hdd",
	}

	if !slices.Equal(shell, want) {
		t.Errorf("Snapshot() ran\n%s\nwant\n%s", strings.Join(shell, "\n"), strings.Join(want, "\n"))
	}
}

// TestSnapshotRemovesFilesOfFailedSnapshot checks that a snapshot that fails
// once minimega has started saving resumes the VM and leaves no files behind:
// a `vm save` whose status minimega never reported left the VM paused and its
// files in minimega's files directory, and a failure after the save left the
// files there, or the memory snapshot alone in the experiment's.
func TestSnapshotRemovesFilesOfFailedSnapshot(t *testing.T) {
	const (
		files  = "/srv/minimega/files"
		saved  = files + "/test-experiment_test-vm__s1"
		moved  = files + "/test-experiment/files/test-vm__s1"
		cancel = `vm qmp test-vm '{ "execute": "migrate_cancel" }'`
	)

	var (
		completed = qmpMigrate("completed")
		failed    = qmpMigrate("failed")
		removeAll = "shell rm -f " + saved + ".state " + saved + ".hdd"
	)

	for _, tc := range []struct {
		name       string
		minimega   snapshotMinimega
		wantErr    []string
		wantResume bool
		wantCancel bool
		wantRemove string
	}{
		{
			name:       "minimega refuses the save",
			minimega:   snapshotMinimega{save: mmError("open " + saved + ".hdd: permission denied"), state: "RUNNING"},
			wantErr:    []string{"starting memory snapshot for VM test-vm", "permission denied"},
			wantRemove: removeAll,
		},
		{
			name:       "the save fails",
			minimega:   snapshotMinimega{status: failed, state: "PAUSED"},
			wantErr:    []string{"memory snapshot for VM test-vm failed"},
			wantResume: true,
			wantRemove: removeAll,
		},
		{
			name:       "QEMU reports no save",
			minimega:   snapshotMinimega{state: "PAUSED"},
			wantErr:    []string{"QEMU reported no memory snapshot for VM test-vm"},
			wantResume: true,
			wantRemove: removeAll,
		},
		{
			name:       "the save status cannot be read",
			minimega:   snapshotMinimega{status: mmError("vm 5 is not running"), state: "PAUSED"},
			wantErr:    []string{"querying the memory snapshot of VM test-vm", "vm 5 is not running"},
			wantResume: true,
			wantRemove: removeAll,
		},
		{
			name:       "the save does not complete in time",
			minimega:   snapshotMinimega{status: qmpMigrate("active"), state: "PAUSED"},
			wantErr:    []string{"did not complete within"},
			wantResume: true,
			wantCancel: true,
			wantRemove: removeAll,
		},
		{
			name:       "the experiment files directory cannot be created",
			minimega:   snapshotMinimega{status: completed, mkdir: mmError("exit status 1")},
			wantErr:    []string{"ensuring experiment files directory exists"},
			wantRemove: removeAll,
		},
		{
			name:       "the memory snapshot cannot be moved",
			minimega:   snapshotMinimega{status: completed, mvMem: mmError("exit status 1")},
			wantErr:    []string{"moving memory snapshot to experiment files directory"},
			wantRemove: removeAll,
		},
		{
			name:       "the disk snapshot cannot be moved",
			minimega:   snapshotMinimega{status: completed, mvDisk: mmError("exit status 1")},
			wantErr:    []string{"moving disk snapshot to experiment files directory"},
			wantRemove: "shell rm -f " + moved + ".state " + saved + ".hdd",
		},
		{
			name:     "the files cannot be removed",
			minimega: snapshotMinimega{status: failed, state: "PAUSED", rm: mmError("exit status 1")},
			wantErr: []string{
				"memory snapshot for VM test-vm failed",
				"removing the files of the failed snapshot",
				"exit status 1",
			},
			wantResume: true,
			wantRemove: removeAll,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vm.SetMMFullPath(t, func(p string) string { return path.Join(files, p) })
			vm.SetVMSaveStatusTimeout(t, 0)
			vm.SetVMSaveTimeout(t, 0)

			fake := installSnapshotTest(t, tc.minimega)

			err := vm.Snapshot("test-experiment", "test-vm", "s1", nil)
			if err == nil {
				t.Fatal("Snapshot() = nil, want the error of the failed snapshot")
			}

			for _, want := range tc.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("Snapshot() = %q, want it to contain %q", err, want)
				}
			}

			sent := fake.sent()
			for i := range sent {
				sent[i] = strings.TrimSpace(sent[i])
			}

			if len(sent) == 0 || sent[len(sent)-1] != tc.wantRemove {
				t.Errorf("Snapshot() did not end with %q; minimega got %q", tc.wantRemove, sent)
			}

			if got := slices.Index(sent, cancel) >= 0; got != tc.wantCancel {
				t.Errorf("Snapshot() canceled the save of test-vm = %v, want %v; minimega got %q", got, tc.wantCancel, sent)
			}

			if tc.wantResume && !slices.Contains(sent, "vm start test-vm") {
				t.Errorf("Snapshot() did not resume test-vm; minimega got %q", sent)
			}
		})
	}
}

// TestSnapshotKeepsCompletedSnapshotWhenResumeFails is a regression test: a
// snapshot whose save completed but whose VM minimega failed to start again
// removed the complete snapshot.
func TestSnapshotKeepsCompletedSnapshotWhenResumeFails(t *testing.T) {
	const files = "/srv/minimega/files"

	vm.SetMMFullPath(t, func(p string) string { return path.Join(files, p) })

	fake := installSnapshotTest(t, snapshotMinimega{
		status: qmpMigrate("completed"),
		start:  mmError("vm not running: 5"),
	})

	err := vm.Snapshot("test-experiment", "test-vm", "s1", nil)
	if err == nil || !strings.Contains(err.Error(), "resuming VM test-vm after its snapshot test-vm__s1 was saved") ||
		!strings.Contains(err.Error(), "vm not running: 5") {
		t.Fatalf("Snapshot() = %v, want the error of the resume naming the saved snapshot", err)
	}

	var shell []string

	for _, cmd := range fake.sent() {
		if cmd = strings.TrimSpace(cmd); strings.HasPrefix(cmd, "shell ") {
			shell = append(shell, cmd)
		}
	}

	want := []string{
		"shell mkdir -p " + files + "/test-experiment/files",
		"shell mv " + files + "/test-experiment_test-vm__s1.state " + files + "/test-experiment/files/test-vm__s1.state",
		"shell mv " + files + "/test-experiment_test-vm__s1.hdd " + files + "/test-experiment/files/test-vm__s1.hdd",
	}

	if !slices.Equal(shell, want) {
		t.Errorf("Snapshot() ran\n%s\nwant\n%s", strings.Join(shell, "\n"), strings.Join(want, "\n"))
	}
}

// TestSnapshotReadsTheVMsOwnSaveStatus is a regression test: the save was
// polled through minimega's `vm save` status table, which fails for every VM
// of the namespace on a host while one of them is powered off, so the
// snapshot failed after a minute and removed a complete save; minimega also
// panics building that table for a VM whose migration QEMU reports setup or
// cancelled.
func TestSnapshotReadsTheVMsOwnSaveStatus(t *testing.T) {
	var progress []string

	fake := installSnapshotTest(t, snapshotMinimega{status: qmpMigrate("completed")})

	if err := vm.Snapshot("test-experiment", "test-vm", "s1", func(p string) { progress = append(progress, p) }); err != nil {
		t.Fatalf("Snapshot() = %v, want nil while another VM on the host is powered off", err)
	}

	sent := fake.sent()

	if slices.Contains(sent, "vm save") {
		t.Errorf("Snapshot() asked minimega's `vm save` status table; minimega got %q", sent)
	}

	if !slices.Contains(sent, `vm qmp test-vm '{ "execute": "query-migrate" }'`) {
		t.Errorf("Snapshot() did not ask QEMU for the save status of test-vm; minimega got %q", sent)
	}

	if !slices.Equal(progress, []string{"completed"}) {
		t.Errorf("Snapshot() reported progress %q, want completed", progress)
	}
}
