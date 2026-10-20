package vm_test

import (
	"encoding/json"
	"errors"
	"path"
	"slices"
	"strings"
	"testing"

	"github.com/activeshadow/libminimega/minicli"

	"phenix/api/vm"
	"phenix/store"
	"phenix/util/file"
	"phenix/util/mm"
)

// snapshotFiles lists the snapshots of test-experiment as the given names.
type snapshotFiles struct {
	file.ClusterFiles

	snapshots []string
}

func (f snapshotFiles) GetExperimentSnapshots(string) ([]string, error) {
	return f.snapshots, nil
}

// TestRestoreRefusesNamesSnapshotWouldNotMake is a regression test: the
// snapshots of a VM are the <vm>__<name>.hdd and .state pairs found in the
// experiment's files, which users can upload, and a restore sent the name to
// minimega unchecked, so a pair named test-vm__a;reboot;b ran reboot as root
// through the `exec:cat` URI QEMU starts the restored VM with.
func TestRestoreRefusesNamesSnapshotWouldNotMake(t *testing.T) {
	originalStore, originalMM, originalFiles := store.DefaultStore, mm.DefaultMM, file.DefaultClusterFiles

	t.Cleanup(func() {
		store.DefaultStore = originalStore       //nolint:reassign // restore test double
		mm.DefaultMM = originalMM                //nolint:reassign // restore test double
		file.DefaultClusterFiles = originalFiles //nolint:reassign // restore test double
	})

	getTestExperiment(t, "test-vm", nil, "2026-10-19T00:00:00Z")

	mm.DefaultMM = snapshotMM{}                                   //nolint:reassign // install test double
	file.DefaultClusterFiles = snapshotFiles{snapshots: []string{ //nolint:reassign // install test double
		"test-vm__a;reboot;b",
		"test-vm__a;echo${IFS}x|sh;b",
		"test-vm__ok-1_x",
		"other__s1",
	}}

	got, err := vm.Snapshots("test-experiment", "test-vm")
	if err != nil || !slices.Equal(got, []string{"test-vm__ok-1_x"}) {
		t.Errorf("Snapshots() = %q, %v, want only test-vm__ok-1_x", got, err)
	}

	for _, snap := range []string{"test-vm__a;reboot;b", "test-vm__a;echo${IFS}x|sh;b", "test-vm__$(reboot)"} {
		fake := useFakeMinimega(t, nil)

		if err := vm.Restore("test-experiment", "test-vm", snap); !errors.Is(err, mm.ErrInvalidFileName) {
			t.Errorf("Restore(%q) = %v, want mm.ErrInvalidFileName", snap, err)
		}

		if sent := fake.sent(); len(sent) != 0 {
			t.Errorf("Restore(%q) sent %q to minimega", snap, sent)
		}
	}

	fake := useFakeMinimega(t, func(string) minicli.Responses { return minicli.Responses{{Host: "head"}} })

	if err := vm.Restore("test-experiment", "test-vm", "test-vm__ok-1_x"); err != nil {
		t.Fatalf("Restore(test-vm__ok-1_x) = %v", err)
	}

	if want := "vm config state test-experiment/files/test-vm__ok-1_x.state"; !slices.ContainsFunc(fake.sent(), func(cmd string) bool {
		return strings.HasSuffix(cmd, want)
	}) {
		t.Errorf("Restore(test-vm__ok-1_x) sent %q, want %q", fake.sent(), want)
	}
}

// restoreChainMM answers qemu-img's description of the backing chain of a
// snapshot disk with chain, and records the host and command it ran.
type restoreChainMM struct {
	snapshotMM

	chain []map[string]any
	err   error
	ran   *[]string
}

func (m restoreChainMM) MeshShellResponse(host, cmd string) (string, error) {
	*m.ran = append(*m.ran, host+": "+cmd)

	b, _ := json.Marshal(m.chain)

	return string(b), m.err
}

// TestRestoreRefusesDisksBackedOntoHostFiles is a regression test: a
// restore gave QEMU, which runs as root, the <vm>__<name>.hdd disk of any
// pair in the experiment's files, and a qcow2 file uploaded there before
// uploads refused snapshot files names any host file as its backing file or
// data file, so the guest read and wrote that host file.
func TestRestoreRefusesDisksBackedOntoHostFiles(t *testing.T) {
	const (
		files = "/srv/minimega/files"
		hdd   = files + "/test-experiment/files/test-vm__s1.hdd"
		own   = files + "/head_test-experiment_test-vm_snapshot"
	)

	originalStore, originalMM, originalFiles := store.DefaultStore, mm.DefaultMM, file.DefaultClusterFiles

	t.Cleanup(func() {
		store.DefaultStore = originalStore       //nolint:reassign // restore test double
		mm.DefaultMM = originalMM                //nolint:reassign // restore test double
		file.DefaultClusterFiles = originalFiles //nolint:reassign // restore test double
	})

	vm.SetMMFullPath(t, func(p string) string {
		if path.IsAbs(p) {
			return p
		}

		return path.Join(files, p)
	})

	getTestExperiment(t, "test-vm", nil, "2026-10-19T00:00:00Z")

	file.DefaultClusterFiles = snapshotFiles{snapshots: []string{"test-vm__s1"}} //nolint:reassign // install test double

	qcow2 := func(name string) map[string]any { return map[string]any{"filename": name, "format": "qcow2"} }

	restore := func(t *testing.T, chain ...map[string]any) ([]string, []string, error) {
		t.Helper()

		var ran []string

		mm.DefaultMM = restoreChainMM{chain: chain, ran: &ran} //nolint:reassign // install test double

		fake := useFakeMinimega(t, func(string) minicli.Responses { return minicli.Responses{{Host: "head"}} })
		err := vm.Restore("test-experiment", "test-vm", "test-vm__s1")

		return ran, fake.sent(), err
	}

	for name, chain := range map[string][]map[string]any{
		"a host file":                      {qcow2(hdd), qcow2("/etc/shadow")},
		"a host file behind a snapshot":    {qcow2(hdd), qcow2(files + "/test-experiment/files/test-vm__s0.hdd"), qcow2("/dev/sda")},
		"another experiment's snapshot":    {qcow2(hdd), qcow2(files + "/other/files/test-vm__s1.hdd")},
		"another experiment's disk":        {qcow2(hdd), qcow2(files + "/head_other_test-vm_snapshot")},
		"an upload of the experiment":      {qcow2(hdd), qcow2(files + "/test-experiment/files/evil.qcow2")},
		"a json: pseudo-protocol filename": {qcow2(hdd), qcow2(`json:{"driver":"file","filename":"/etc/shadow"}`)},
		"a data file": {{
			"filename":        hdd,
			"format":          "qcow2",
			"format-specific": map[string]any{"type": "qcow2", "data": map[string]any{"data-file": "/etc/shadow"}},
		}},
		"a vmdk with host extents": {{"filename": hdd, "format": "vmdk"}},
	} {
		t.Run(name, func(t *testing.T) {
			_, sent, err := restore(t, chain...)
			if !errors.Is(err, vm.ErrInvalidDisk) || !strings.Contains(err.Error(), "VM test-vm") {
				t.Errorf("Restore() = %v, want vm.ErrInvalidDisk naming test-vm", err)
			}

			if len(sent) != 0 {
				t.Errorf("Restore() sent %q to minimega", sent)
			}
		})
	}

	for name, chain := range map[string][]map[string]any{
		"a saved disk":             {qcow2(hdd), qcow2(own), qcow2(files + "/test.qc2"), qcow2(files + "/base/os.qc2")},
		"a disk saved after one":   {qcow2(hdd), qcow2(files + "/test-experiment/files/test-vm__s0.hdd"), qcow2(own), qcow2(files + "/test.qc2")},
		"a disk of another image":  {qcow2(hdd), qcow2(files + "/other.qc2"), qcow2("/data/base.qc2")},
		"a disk without a backing": {qcow2(hdd)},
	} {
		t.Run(name, func(t *testing.T) {
			ran, sent, err := restore(t, chain...)
			if err != nil {
				t.Fatalf("Restore() = %v", err)
			}

			want := "head: qemu-img info --force-share --backing-chain --output=json " + hdd
			if !slices.Equal(ran, []string{want}) {
				t.Errorf("Restore() ran %q, want %q", ran, want)
			}

			if !slices.Contains(sent, "vm config disk test-experiment/files/test-vm__s1.hdd,writeback") {
				t.Errorf("Restore() sent %q, want the snapshot disk configured", sent)
			}
		})
	}

	t.Run("qemu-img fails", func(t *testing.T) {
		var ran []string

		mm.DefaultMM = restoreChainMM{err: errors.New("exit status 1"), ran: &ran} //nolint:reassign // install test double

		fake := useFakeMinimega(t, func(string) minicli.Responses { return minicli.Responses{{Host: "head"}} })

		err := vm.Restore("test-experiment", "test-vm", "test-vm__s1")
		if err == nil || errors.Is(err, vm.ErrInvalidDisk) || !strings.Contains(err.Error(), "exit status 1") {
			t.Errorf("Restore() = %v, want the error of qemu-img", err)
		}

		if sent := fake.sent(); len(sent) != 0 {
			t.Errorf("Restore() sent %q to minimega", sent)
		}
	})
}
