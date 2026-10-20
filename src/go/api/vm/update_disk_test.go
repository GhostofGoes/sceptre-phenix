package vm_test

import (
	"errors"
	"path"
	"strings"
	"testing"

	"github.com/golang/mock/gomock"

	"phenix/api/vm"
	"phenix/store"
	"phenix/types"
)

// useStoppedDiskExperiment makes the store hold the stopped test-experiment,
// whose nodes test-vm and peer-vm boot from test.qc2 and peer.qc2 with
// snapshot: true, with minimega's files directory at /srv/minimega/files,
// until the test ends. Each save replaces the experiment the store holds. It
// returns the images of test-vm the store saved.
func useStoppedDiskExperiment(t *testing.T) *[]string {
	t.Helper()

	const files = "/srv/minimega/files"

	vm.SetMMFullPath(t, func(p string) string {
		if path.IsAbs(p) {
			return p
		}

		return path.Join(files, p)
	})

	c := store.Config{
		Version:  "phenix.sandia.gov/v1",
		Kind:     "Experiment",
		Metadata: store.ConfigMetadata{Name: "test-experiment"},
		Spec: map[string]any{
			"experimentName": "test-experiment",
			"topology": map[string]any{"nodes": []map[string]any{
				{
					"type":    "VirtualMachine",
					"general": map[string]any{"hostname": "test-vm", "snapshot": true},
					"hardware": map[string]any{
						"os_type": "linux",
						"drives":  []map[string]any{{"image": "test.qc2"}},
					},
				},
				{
					"type":    "VirtualMachine",
					"general": map[string]any{"hostname": "peer-vm", "snapshot": true},
					"hardware": map[string]any{
						"os_type": "linux",
						"drives":  []map[string]any{{"image": "peer.qc2"}},
					},
				},
			}},
		},
	}

	var saved []string

	m := store.NewMockStore(gomock.NewController(t))
	m.EXPECT().Get(gomock.Any()).DoAndReturn(func(cfg *store.Config) error {
		*cfg = c

		return nil
	}).AnyTimes()
	m.EXPECT().Update(gomock.Any()).DoAndReturn(func(cfg *store.Config) error {
		exp, err := types.DecodeExperimentFromConfig(*cfg)
		if err != nil {
			return err
		}

		saved = append(saved, exp.Spec.Topology().FindNodeByName("test-vm").Hardware().Drives()[0].Image())
		c = *cfg

		return nil
	}).AnyTimes()

	original := store.DefaultStore
	t.Cleanup(func() { store.DefaultStore = original }) //nolint:reassign // restore test double

	store.DefaultStore = m //nolint:reassign // install test double

	return &saved
}

// TestUpdateRefusesDiskOutsideImages is a regression test: a disk change of
// a stopped experiment's VM stored any path, so a user allowed to patch VMs
// had the next start give QEMU, which runs as root, a host file such as
// /etc/shadow, an uploaded qcow2 file or another experiment's disk snapshot
// as the guest's disk.
func TestUpdateRefusesDiskOutsideImages(t *testing.T) {
	saved := useStoppedDiskExperiment(t)

	for _, disk := range []string{
		"/etc/shadow",
		"../../../../etc/shadow",
		"/srv/minimega/files/../../../etc/shadow",
		"test.qc2 /etc/shadow",
		"test.qc2,writeback",
		"test-experiment/files/evil.qcow2",
		"head_other_victim_snapshot",
		"$HOME/x.qc2",
	} {
		err := vm.Update(vm.UpdateExperiment("test-experiment"), vm.UpdateVM("test-vm"), vm.UpdateWithDisk(disk))
		if !errors.Is(err, vm.ErrInvalidDisk) || !strings.Contains(err.Error(), "VM test-vm") {
			t.Errorf("Update(disk %q) = %v, want vm.ErrInvalidDisk naming test-vm", disk, err)
		}
	}

	if len(*saved) != 0 {
		t.Fatalf("Update saved the disks %q it should have refused", *saved)
	}

	for _, disk := range []string{"other.qc2", "/srv/minimega/files/other.qc2", "test.qc2"} {
		err := vm.Update(vm.UpdateExperiment("test-experiment"), vm.UpdateVM("test-vm"), vm.UpdateWithDisk(disk))
		if err != nil {
			t.Errorf("Update(disk %q) = %v, want nil", disk, err)
		}
	}

	if want := []string{"other.qc2", "/srv/minimega/files/other.qc2", "test.qc2"}; strings.Join(*saved, " ") != strings.Join(want, " ") {
		t.Errorf("Update saved the disks %q, want %q", *saved, want)
	}
}

// TestUpdateRunsOnlyTopologyDisksWithoutSnapshot is a regression test: a
// VM with snapshot: false boots its disk writable, and a disk change could
// pick any image directly in minimega's files directory, such as one other
// experiments boot from, and turn snapshot off in the same or a later
// request, so the guest wrote that image.
func TestUpdateRunsOnlyTopologyDisksWithoutSnapshot(t *testing.T) {
	update := func(opts ...vm.UpdateOption) error {
		return vm.Update(append([]vm.UpdateOption{vm.UpdateExperiment("test-experiment"), vm.UpdateVM("test-vm")}, opts...)...)
	}

	refused := func(t *testing.T, err error) {
		t.Helper()

		if !errors.Is(err, vm.ErrInvalidDisk) || !strings.Contains(err.Error(), "VM test-vm") ||
			!strings.Contains(err.Error(), "snapshot: false") {
			t.Errorf("Update() = %v, want vm.ErrInvalidDisk naming test-vm and snapshot: false", err)
		}
	}

	t.Run("a picked disk and snapshot off at once", func(t *testing.T) {
		saved := useStoppedDiskExperiment(t)

		refused(t, update(vm.UpdateWithDisk("golden.qc2"), vm.UpdateWithSnapshot(false)))

		if len(*saved) != 0 {
			t.Errorf("Update saved %q", *saved)
		}
	})

	t.Run("snapshot off after a picked disk", func(t *testing.T) {
		saved := useStoppedDiskExperiment(t)

		if err := update(vm.UpdateWithDisk("golden.qc2")); err != nil {
			t.Fatalf("Update(disk golden.qc2) = %v", err)
		}

		refused(t, update(vm.UpdateWithSnapshot(false)))

		// Another change keeps the topology's disk recorded.
		if err := update(vm.UpdateWithDisk("other.qc2")); err != nil {
			t.Fatalf("Update(disk other.qc2) = %v", err)
		}

		refused(t, update(vm.UpdateWithSnapshot(false)))

		if want := []string{"golden.qc2", "other.qc2"}; strings.Join(*saved, " ") != strings.Join(want, " ") {
			t.Errorf("Update saved %q, want %q", *saved, want)
		}
	})

	t.Run("a picked disk after snapshot off", func(t *testing.T) {
		useStoppedDiskExperiment(t)

		if err := update(vm.UpdateWithSnapshot(false)); err != nil {
			t.Fatalf("Update(snapshot false) = %v", err)
		}

		refused(t, update(vm.UpdateWithDisk("golden.qc2")))
	})

	t.Run("topology disks", func(t *testing.T) {
		saved := useStoppedDiskExperiment(t)

		for _, opts := range [][]vm.UpdateOption{
			{vm.UpdateWithDisk("golden.qc2")},
			// Back on the topology's own disk, which the VM no longer had.
			{vm.UpdateWithDisk("/srv/minimega/files/test.qc2"), vm.UpdateWithSnapshot(false)},
			{vm.UpdateWithSnapshot(true)},
			// Another node's disk.
			{vm.UpdateWithDisk("peer.qc2"), vm.UpdateWithSnapshot(false)},
			{vm.UpdateWithDisk("test.qc2")},
		} {
			if err := update(opts...); err != nil {
				t.Errorf("Update() = %v, want nil", err)
			}
		}

		want := []string{
			"golden.qc2",
			"/srv/minimega/files/test.qc2",
			"/srv/minimega/files/test.qc2",
			"peer.qc2",
			"test.qc2",
		}

		if strings.Join(*saved, " ") != strings.Join(want, " ") {
			t.Errorf("Update saved %q, want %q", *saved, want)
		}
	})
}
