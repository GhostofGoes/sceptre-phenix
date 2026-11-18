package vm

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	ifaces "phenix/types/interfaces"
)

var (
	// ErrInvalidDisk is wrapped by the error Redeploy, Update and
	// ChangeOpticalDisc return for a disk or an optical disc they refuse to
	// give a VM.
	ErrInvalidDisk = errors.New("invalid disk image")

	// ErrNotRedeployable is wrapped by the error Redeploy returns for a node
	// it cannot redeploy as asked, such as an external node.
	ErrNotRedeployable = errors.New("VM cannot be redeployed")
)

// TopologyDiskAnnotation is the node annotation in which Update records the
// image the topology gave the node's first drive once a disk change replaces
// it, so that a VM runs with snapshot: false only on a disk of the topology.
const TopologyDiskAnnotation = "phenix/topology-disk"

var (
	// plainPathRegex is the allow-list of the characters of a host path, such
	// as a redeploy disk or a mount path, that phenix passes to minimega.
	plainPathRegex = regexp.MustCompile(`^[A-Za-z0-9._+/-]+$`)

	// diskInterfaces are the interfaces of a minimega diskspec.
	diskInterfaces = []string{"ahci", "ide", "scsi", "sd", "mtd", "floppy", "pflash", "virtio"} //nolint:gochecknoglobals // constant list

	// diskCacheModes are the cache modes of a minimega diskspec.
	diskCacheModes = []string{"none", "writeback", "unsafe", "directsync", "writethrough"} //nolint:gochecknoglobals // constant list
)

// checkRedeployDisk returns an error wrapping ErrInvalidDisk, naming vmName,
// unless disk, a minimega diskspec <image>[,<interface>][,<cache mode>],
// names an image diskImageProblem accepts. The disk of a REST redeploy comes
// from the request.
func checkRedeployDisk(topo ifaces.TopologySpec, vmName, disk string) error {
	fields := strings.Split(disk, ",")
	image, opts := fields[0], fields[1:]

	switch {
	case len(opts) == 0:
	case len(opts) == 1 && (slices.Contains(diskInterfaces, opts[0]) || slices.Contains(diskCacheModes, opts[0])):
	case len(opts) == 2 && slices.Contains(diskInterfaces, opts[0]) && slices.Contains(diskCacheModes, opts[1]):
	default:
		return invalidDisk("disk", disk, vmName,
			"is not <image>[,<interface>][,<cache mode>] with an interface ("+strings.Join(diskInterfaces, ", ")+
				") and a cache mode ("+strings.Join(diskCacheModes, ", ")+") minimega knows",
		)
	}

	if problem := diskImageProblem(topo, image); problem != "" {
		return invalidDisk("disk", disk, vmName, problem)
	}

	return nil
}

// checkDiskImage returns an error wrapping ErrInvalidDisk, naming what and
// vmName, unless diskImageProblem accepts image for a VM of topo.
func checkDiskImage(topo ifaces.TopologySpec, what, vmName, image string) error {
	if problem := diskImageProblem(topo, image); problem != "" {
		return invalidDisk(what, image, vmName, problem)
	}

	return nil
}

// AssignableDisk reports whether a VM of topo may be given image as its disk
// or its optical disc: whether Update, Redeploy and ChangeOpticalDisc accept
// it. A VM with snapshot: false may run only on a disk the topology gave a
// node, which they check too.
func AssignableDisk(topo ifaces.TopologySpec, image string) bool {
	return diskImageProblem(topo, image) == ""
}

// invalidDisk returns the error wrapping ErrInvalidDisk that refuses value
// as the what of VM vmName for problem.
func invalidDisk(what, value, vmName, problem string) error {
	return fmt.Errorf("%s %q for VM %s %s: %w", what, value, vmName, problem, ErrInvalidDisk)
}

// diskImageProblem returns why image may not be given to a VM of topo, or ""
// when it may: it must be written with plain characters and name a disk of a
// node of topo, or a file directly in minimega's files directory that is not
// a disk snapshot phenix makes at start (<headnode>_<experiment>_<vm>_snapshot).
//
// QEMU, which runs as root, opens whatever file the image names, so a host
// file such as /etc/shadow would be given to the guest. phenix keeps each
// experiment's VM snapshots, captures, memory dumps and commit copies under
// <experiment>/ in the files directory, and users upload files to
// <experiment>/files/; a qcow2 file there names its own backing file, which
// may be any host file. A disk snapshot holds the injections of its VM. The
// image ends up in minimega commands, which a space splits into several disks.
func diskImageProblem(topo ifaces.TopologySpec, image string) string {
	if !plainPathRegex.MatchString(image) || strings.HasPrefix(image, "-") {
		return "may hold only letters, digits, '.', '_', '+', '-' and '/', and may not start with '-'"
	}

	if slices.Contains(strings.Split(image, "/"), "..") {
		return `holds ".."`
	}

	path := filepath.Clean(mmFullPath(image))

	for _, node := range topo.Nodes() {
		for _, drive := range node.Hardware().Drives() {
			if drive.Image() != "" && filepath.Clean(mmFullPath(drive.Image())) == path {
				return ""
			}
		}
	}

	if filepath.Dir(path) != filepath.Clean(mmFullPath("")) {
		return "is neither directly in minimega's files directory nor a disk of the experiment's topology"
	}

	if strings.HasSuffix(path, "_snapshot") {
		return "is a disk snapshot phenix makes for a VM at start"
	}

	return ""
}

// writableDiskProblem returns why a VM of topo that runs with snapshot: false
// may not have image as its disk, or "" when it may: QEMU then writes the
// image itself, so it must be a disk the topology gave a node (topologyDisk),
// not one that only a disk change picked, such as an image in minimega's
// files directory that other experiments boot from.
func writableDiskProblem(topo ifaces.TopologySpec, image string) string {
	path := filepath.Clean(mmFullPath(image))

	for _, node := range topo.Nodes() {
		for i, drive := range node.Hardware().Drives() {
			disk := drive.Image()
			if i == 0 {
				disk = topologyDisk(node)
			}

			if disk != "" && filepath.Clean(mmFullPath(disk)) == path {
				return ""
			}
		}
	}

	return "is no disk the topology gave a node, and a VM with snapshot: false writes to its disk"
}

// topologyDisk returns the image the topology gave the first drive of node:
// the one TopologyDiskAnnotation records once a disk change replaced it, or
// else the drive's image. It returns "" for a node without drives.
func topologyDisk(node ifaces.NodeSpec) string {
	drives := node.Hardware().Drives()
	if len(drives) == 0 {
		return ""
	}

	if image, ok := node.Annotations()[TopologyDiskAnnotation].(string); ok && image != "" {
		return image
	}

	return drives[0].Image()
}
