package vm

import (
	"errors"
	"path"
	"testing"

	v1 "phenix/types/version/v1"
)

// TestCheckRedeployDisk checks the disks a redeploy may give a VM: the REST
// redeploy passed its disk to minimega verbatim, so an Experiment User could
// attach any host file, such as /etc/shadow, another experiment's VM snapshot
// or disk snapshot, or an uploaded qcow2 file that backs onto a host file, to
// a guest as root. A diskspec may name minimega's interfaces.
func TestCheckRedeployDisk(t *testing.T) {
	const files = "/srv/minimega/files"

	SetMMFullPath(t, func(p string) string {
		if path.IsAbs(p) {
			return p
		}

		return path.Join(files, p)
	})

	topo := &v1.TopologySpec{NodesF: []*v1.Node{
		{HardwareF: &v1.Hardware{DrivesF: []*v1.Drive{{ImageF: "/data/images/win10.qcow2"}}}},
		{HardwareF: &v1.Hardware{DrivesF: []*v1.Drive{{ImageF: "images/sub/ubuntu.qc2"}}}},
		{}, // an external node without hardware
	}}

	for _, disk := range []string{
		"ubuntu.qc2",
		"ubuntu.qc2,writeback",
		// A topology disk may be in a subdirectory.
		"images/sub/ubuntu.qc2",
		files + "/images/sub/ubuntu.qc2,virtio",
		files + "/ubuntu.qc2",
		files + "/./ubuntu+tools.qc2",
		"/data/images/win10.qcow2",
		"/data/images/win10.qcow2,none",
		"ubuntu.qc2,virtio",
		"ubuntu.qc2,ide,writeback",
	} {
		if err := checkRedeployDisk(topo, "vm1", disk); err != nil {
			t.Errorf("checkRedeployDisk(%q) = %v, want nil", disk, err)
		}
	}

	for _, disk := range []string{
		"/etc/shadow",
		"ubuntu.qc2 /etc/shadow",
		"../../etc/shadow",
		files + "/../../etc/shadow",
		files,
		"/data/images/other.qcow2",
		"ubuntu.qc2,writeback,none",
		"ubuntu.qc2,writeback,virtio",
		"ubuntu.qc2,virtio,ide",
		"ubuntu.qc2,ide,writeback,none",
		"ubuntu.qc2,",
		// Another experiment's VM snapshot would boot its guest's disk.
		"exp2/files/vm1__s1.hdd",
		files + "/exp2/files/sub/x.qc2",
		// Users upload files to the experiment's own files directory.
		"exp1/files/vm1__s1.hdd",
		files + "/exp1/files/evil.qcow2",
		"exp2/tmp/snapshot-0.qc2",
		"images/other/x.qc2",
		// The disk snapshots phenix makes at start hold their VMs' injections.
		"head_exp2_victim_snapshot",
		files + "/head_exp1_vm2_snapshot,writeback",
		"-drive",
		"file:/etc/shadow",
		"$HOME/x.qc2",
		`a"b.qc2`,
		"",
	} {
		err := checkRedeployDisk(topo, "vm1", disk)
		if !errors.Is(err, ErrInvalidDisk) {
			t.Errorf("checkRedeployDisk(%q) = %v, want ErrInvalidDisk", disk, err)
		}
	}
}
