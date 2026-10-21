package vm_test

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/activeshadow/libminimega/minicli"
	"github.com/golang/mock/gomock"

	"phenix/api/vm"
	"phenix/store"
	"phenix/util/mm"
)

// redeploySnapshot is the phenix disk snapshot test-vm runs on.
const redeploySnapshot = "/tmp/minimega/files/head_test-experiment_test-vm_snapshot"

// useRedeployExperiment makes the store hold a running test-experiment, with
// base directory baseDir and the single node test-vm on test.qc2 with
// injections, until the test ends.
func useRedeployExperiment(t *testing.T, baseDir string, injections []map[string]any) {
	t.Helper()

	useRedeployNode(t, baseDir, true, injections)
}

// useRedeployNode is useRedeployExperiment for a test-vm that runs with
// snapshot set to snapshot.
func useRedeployNode(t *testing.T, baseDir string, snapshot bool, injections []map[string]any) {
	t.Helper()

	ctrl := gomock.NewController(t)

	node := map[string]any{
		"type":    "VirtualMachine",
		"general": map[string]any{"hostname": "test-vm", "snapshot": snapshot},
		"hardware": map[string]any{
			"vcpus":   1,
			"memory":  512,
			"os_type": "linux",
			"drives":  []map[string]any{{"image": "test.qc2", "inject_partition": 1}},
		},
	}

	if len(injections) > 0 {
		node["injections"] = injections
	}

	c := store.Config{
		Version:  "phenix.sandia.gov/v1",
		Kind:     "Experiment",
		Metadata: store.ConfigMetadata{Name: "test-experiment"},
		Spec: map[string]any{
			"experimentName": "test-experiment",
			"baseDir":        baseDir,
			"topology":       map[string]any{"nodes": []map[string]any{node}},
		},
		Status: map[string]any{"startTime": "2026-10-18T00:00:00Z"},
	}

	m := store.NewMockStore(ctrl)
	m.EXPECT().Get(gomock.Any()).DoAndReturn(func(cfg *store.Config) error {
		*cfg = c

		return nil
	}).AnyTimes()

	original := store.DefaultStore
	t.Cleanup(func() { store.DefaultStore = original }) //nolint:reassign // restore test double

	store.DefaultStore = m //nolint:reassign // install test double
}

// redeployMinimega runs the redeploy through the minimega implementation of
// mm against a fake minimega on which test-vm runs on redeploySnapshot, and
// returns the commands the fake got.
func redeployMinimega(t *testing.T, opts ...vm.RedeployOption) []string {
	t.Helper()

	return redeployMinimegaDisks(t, redeploySnapshot+",writeback", opts...)
}

// redeployMinimegaDisks is redeployMinimega for a test-vm whose disks minimega
// reports as disks.
func redeployMinimegaDisks(t *testing.T, disks string, opts ...vm.RedeployOption) []string {
	t.Helper()

	original := mm.DefaultMM
	t.Cleanup(func() { mm.DefaultMM = original }) //nolint:reassign // restore test double

	mm.DefaultMM = new(mm.Minimega) //nolint:reassign // the implementation under test

	fake := useFakeMinimega(t, func(cmd string) minicli.Responses {
		if strings.HasSuffix(cmd, "vm info") {
			return mmTable(
				[]string{"name", "id", "state", "type", "disks", "snapshot"},
				[]string{"test-vm", "5", "RUNNING", "kvm", disks, "true"},
			)
		}

		return minicli.Responses{{Host: "head"}}
	})

	if err := vm.Redeploy("test-experiment", "test-vm", opts...); err != nil {
		t.Fatalf("Redeploy() = %v", err)
	}

	return fake.sent()
}

// TestRedeployInjectsFilesAsTheStart is a regression test: a redeploy that
// replicates injections injected each file as a bare src:dst pair, so a
// relative source was not looked up in the experiment's base directory and
// the injection's permissions were not applied, as the start-time script does.
func TestRedeployInjectsFilesAsTheStart(t *testing.T) {
	baseDir := t.TempDir()
	src := filepath.Join(baseDir, "motd")

	if err := os.WriteFile(src, []byte("v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	useRedeployExperiment(t, baseDir, []map[string]any{
		{"src": "motd", "dst": "/etc/motd", "permissions": "0644"},
	})

	sent := redeployMinimega(t, vm.Inject(true))

	want := `disk inject ` + filepath.Base(redeploySnapshot) + `:1 files "` + src + `":"/etc/motd"`
	if !slices.Contains(sent, want) {
		t.Errorf("Redeploy() did not run %q; minimega got %q", want, sent)
	}

	fi, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}

	if mode := fi.Mode().Perm(); mode != 0o644 {
		t.Errorf("injection source has mode %o after the redeploy, want the injection's 644", mode)
	}
}

// TestRedeployRecreatesSnapshotWithoutInjections is a regression test: a
// redeploy that replicates injections of a node with no injections and no
// deletions relaunched the VM on the node's image instead of recreating its
// disk snapshot.
func TestRedeployRecreatesSnapshotWithoutInjections(t *testing.T) {
	useRedeployExperiment(t, t.TempDir(), nil)

	sent := redeployMinimega(t, vm.Inject(true))

	for _, want := range []string{
		"disk snapshot test.qc2 " + filepath.Base(redeploySnapshot),
		"vm config disk " + redeploySnapshot + ",writeback",
	} {
		if !slices.Contains(sent, want) {
			t.Errorf("Redeploy() did not run %q; minimega got %q", want, sent)
		}
	}

	for _, cmd := range sent {
		if strings.HasPrefix(cmd, "disk inject") || strings.HasPrefix(cmd, "vm config disk test.qc2") {
			t.Errorf("Redeploy() ran %q for a node without injections", cmd)
		}
	}
}

// TestRedeployKeepsDiskWithoutReplicatingInjections checks that a redeploy
// that does not replicate injections leaves the disk of the VM alone.
func TestRedeployKeepsDiskWithoutReplicatingInjections(t *testing.T) {
	useRedeployExperiment(t, t.TempDir(), nil)

	for _, cmd := range redeployMinimega(t) {
		if strings.HasPrefix(cmd, "disk ") || strings.HasPrefix(cmd, "vm config disk") {
			t.Errorf("Redeploy() ran %q, want the VM's disk kept", cmd)
		}
	}
}

// TestRedeployRefusesDiskOutsideImages checks that a redeploy refuses a disk
// outside minimega's files directory and the topology before minimega is
// asked anything: the disk reached `vm config disk` verbatim, attaching the
// host file to the guest.
func TestRedeployRefusesDiskOutsideImages(t *testing.T) {
	useRedeployExperiment(t, t.TempDir(), nil)

	original := mm.DefaultMM
	t.Cleanup(func() { mm.DefaultMM = original }) //nolint:reassign // restore test double

	mm.DefaultMM = new(mm.Minimega) //nolint:reassign // the implementation under test

	for _, disk := range []string{"/etc/shadow", "test.qc2 /etc/shadow", "../../etc/shadow"} {
		fake := useFakeMinimega(t, nil)

		err := vm.Redeploy("test-experiment", "test-vm", vm.Disk(disk), vm.Inject(true))
		if !errors.Is(err, vm.ErrInvalidDisk) || !strings.Contains(err.Error(), "VM test-vm") {
			t.Errorf("Redeploy(disk %q) = %v, want vm.ErrInvalidDisk naming test-vm", disk, err)
		}

		if sent := fake.sent(); len(sent) != 0 {
			t.Errorf("Redeploy(disk %q) sent %q to minimega", disk, sent)
		}
	}
}

// TestRedeployRunsOnlyTopologyDisksWithoutSnapshot is a regression test: a
// VM keeps its snapshot flag when it is redeployed, so a snapshot: false VM
// redeployed onto an image directly in minimega's files directory, such as
// one other experiments boot from, wrote that image.
func TestRedeployRunsOnlyTopologyDisksWithoutSnapshot(t *testing.T) {
	useRedeployNode(t, t.TempDir(), false, nil)

	original := mm.DefaultMM
	t.Cleanup(func() { mm.DefaultMM = original }) //nolint:reassign // restore test double

	mm.DefaultMM = new(mm.Minimega) //nolint:reassign // the implementation under test

	fake := useFakeMinimega(t, nil)

	err := vm.Redeploy("test-experiment", "test-vm", vm.Disk("golden.qc2,writeback"))
	if !errors.Is(err, vm.ErrInvalidDisk) || !strings.Contains(err.Error(), "snapshot: false") {
		t.Errorf("Redeploy(disk golden.qc2) of a snapshot: false node = %v, want vm.ErrInvalidDisk", err)
	}

	if sent := fake.sent(); len(sent) != 0 {
		t.Errorf("Redeploy(disk golden.qc2) of a snapshot: false node sent %q to minimega", sent)
	}

	sent := redeployMinimegaDisks(t, "/tmp/minimega/files/test.qc2", vm.Disk("test.qc2,writeback"))
	if !slices.Contains(sent, "vm config disk test.qc2,writeback") {
		t.Errorf("Redeploy(disk test.qc2) of a snapshot: false node sent %q, want its topology disk", sent)
	}
}

// TestRedeployReplicateNeedsDiskSnapshot is a regression test: a redeploy
// that replicates injections of a snapshot: false node, which runs on its own
// image, deleted that image across the cluster when its name held
// "_snapshot", and then failed to recreate it.
func TestRedeployReplicateNeedsDiskSnapshot(t *testing.T) {
	useRedeployNode(t, t.TempDir(), false, nil)

	original := mm.DefaultMM
	t.Cleanup(func() { mm.DefaultMM = original }) //nolint:reassign // restore test double

	mm.DefaultMM = new(mm.Minimega) //nolint:reassign // the implementation under test

	fake := useFakeMinimega(t, nil)

	err := vm.Redeploy("test-experiment", "test-vm", vm.Inject(true))
	if !errors.Is(err, vm.ErrNotRedeployable) || !strings.Contains(err.Error(), "snapshot: false") {
		t.Errorf("Redeploy(-r) of a snapshot: false node = %v, want vm.ErrNotRedeployable", err)
	}

	if sent := fake.sent(); len(sent) != 0 {
		t.Errorf("Redeploy(-r) of a snapshot: false node sent %q to minimega", sent)
	}
}

// TestRedeployRecreatesOnlyThePhenixSnapshot is a regression test: a
// redeploy that recreates the disk snapshot took any VM whose disks held
// "_snapshot" anywhere for one running on its phenix snapshot, and deleted
// the first path of the whole disks column, so it deleted an image of the
// user named *_snapshot, or a later disk, across the cluster.
func TestRedeployRecreatesOnlyThePhenixSnapshot(t *testing.T) {
	const data = "/tmp/minimega/files/data_snapshot.qc2,virtio"

	t.Run("an image named like a snapshot", func(t *testing.T) {
		for _, disks := range []string{
			"/tmp/minimega/files/win10_snapshot.qcow2",
			"/tmp/minimega/files/a.qc2 /tmp/minimega/files/data_snapshot.qc2",
		} {
			useRedeployExperiment(t, t.TempDir(), nil)

			for _, cmd := range redeployMinimegaDisks(t, disks, vm.Inject(true)) {
				if strings.Contains(cmd, "file delete") || strings.HasPrefix(cmd, "disk snapshot") {
					t.Errorf("Redeploy() ran %q for a VM on %q, which is not its phenix snapshot", cmd, disks)
				}
			}
		}
	})

	t.Run("a second disk named like a snapshot", func(t *testing.T) {
		useRedeployExperiment(t, t.TempDir(), nil)

		sent := redeployMinimegaDisks(t, redeploySnapshot+",writeback "+data, vm.Inject(true))

		for _, want := range []string{
			"mesh send all file delete " + filepath.Base(redeploySnapshot),
			"disk snapshot test.qc2 " + filepath.Base(redeploySnapshot),
			"vm config disk " + redeploySnapshot + ",writeback " + data,
		} {
			if !slices.Contains(sent, want) {
				t.Errorf("Redeploy() did not run %q; minimega got %q", want, sent)
			}
		}

		for _, cmd := range sent {
			if strings.Contains(cmd, "file delete") && strings.Contains(cmd, "data_snapshot") {
				t.Errorf("Redeploy() ran %q, deleting the VM's second disk", cmd)
			}
		}
	})
}
