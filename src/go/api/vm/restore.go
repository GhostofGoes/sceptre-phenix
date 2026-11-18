package vm

import (
	"encoding/json"
	"fmt"
	"path/filepath"

	"phenix/api/experiment"
	"phenix/util/mm"
)

// qemuChainImage is the part of qemu-img's JSON description of an image of a
// backing chain that checkSnapshotDisk reads.
type qemuChainImage struct {
	Filename       string `json:"filename"`
	Format         string `json:"format"`
	FormatSpecific struct {
		Data struct {
			DataFile string `json:"data-file"`
		} `json:"data"`
	} `json:"format-specific"`
}

// checkSnapshotDisk returns an error wrapping ErrInvalidDisk unless disk, the
// path of a snapshot disk Restore gives VM vmName of experiment expName, is a
// qcow2 image whose backing chain phenix could have made: a snapshot disk
// backs onto the disk the VM ran on, which is a snapshot disk of the
// experiment, the VM's disk snapshot (<headnode>_<experiment>_<vm>_snapshot)
// or an image a VM may be given, whose own chain is left alone. qemu-img
// inspects the chain on host, the VM's host, where the snapshot disk was
// saved.
//
// QEMU, which runs as root, opens the whole chain, and a qcow2 file names any
// host file as its backing file or data file, so a snapshot disk uploaded
// before the file server refused them would give the guest that host file.
func checkSnapshotDisk(expName, vmName, host, disk string) error {
	exp, err := experiment.Get(expName)
	if err != nil {
		return fmt.Errorf("getting experiment %s: %w", expName, err)
	}

	resp, err := mm.MeshShellResponse(host, "qemu-img info --force-share --backing-chain --output=json "+disk)
	if err != nil {
		return fmt.Errorf("inspecting snapshot disk %s of VM %s: %w", disk, vmName, err)
	}

	var chain []qemuChainImage

	if err := json.Unmarshal([]byte(resp), &chain); err != nil || len(chain) == 0 {
		return fmt.Errorf("inspecting snapshot disk %s of VM %s: qemu-img answered %q", disk, vmName, resp)
	}

	files := filepath.Join(filepath.Clean(mmFullPath("")), expName, "files")

	// The name v1.ExperimentSpec.SnapshotName gives the start's snapshot.
	diskSnapshot := filepath.Clean(mmFullPath(fmt.Sprintf("%s_%s_%s_snapshot", mm.Headnode(), expName, vmName)))

	for i, image := range chain {
		path := filepath.Clean(image.Filename)

		switch {
		case i == 0:
		case diskImageProblem(exp.Spec.Topology(), path) == "":
			return nil
		case filepath.Dir(path) == files && filepath.Ext(path) == ".hdd", path == diskSnapshot:
		default:
			return invalidDisk("snapshot disk", disk, vmName, fmt.Sprintf(
				"backs onto %q, which is neither a snapshot disk of the experiment, the VM's disk snapshot nor an "+
					"image a VM may be given", image.Filename))
		}

		switch {
		case image.Format != "qcow2":
			return invalidDisk("snapshot disk", disk, vmName, fmt.Sprintf(
				"holds %q, a %s image, where phenix saves qcow2 images", image.Filename, image.Format))
		case image.FormatSpecific.Data.DataFile != "":
			return invalidDisk("snapshot disk", disk, vmName, fmt.Sprintf(
				"holds %q, which keeps its data in %q", image.Filename, image.FormatSpecific.Data.DataFile))
		}
	}

	return nil
}
