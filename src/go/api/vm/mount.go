package vm

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"phenix/api/experiment"
	"phenix/util/mm"
	"phenix/util/plog"
)

// MountTimeout is how long to wait for a mount/unmount C2 command to complete.
const MountTimeout = 5 * time.Second

func init() { //nolint:gochecknoinits // registering experiment lifecycle hooks
	// Make sure VM filesystem mounts don't outlive their experiment. Without
	// this, a forgotten mount can leave a stale 9p mount on the headnode
	// that requires a reboot to clear once the backing VM is gone.
	experiment.RegisterHook("stop", func(_, expName string) { unmountExperiment(expName) })
	experiment.RegisterHook("delete", func(_, expName string) { unmountExperiment(expName) })
}

// Mount mounts the filesystem of the given running VM to hostPath. If
// hostPath is empty, the default phenix mount path
// (<mount-dir>/<experiment>/<vm-name>) is used instead. It returns the
// resolved host path the VM was mounted to, along with any errors encountered.
//
// This is the same implementation used by the `POST
// /experiments/{exp}/vms/{name}/mount` API endpoint, so behavior is
// consistent between the CLI and web UI.
func Mount(expName, vmName, hostPath string) (string, error) {
	if expName == "" {
		return "", errors.New("no experiment name provided")
	}

	if vmName == "" {
		return "", errors.New("no VM name provided")
	}

	if hostPath == "" {
		hostPath = mm.GetLocalMountPath(expName, vmName)
	}

	hostPath, err := filepath.Abs(hostPath)
	if err != nil {
		return "", fmt.Errorf("resolving mount path: %w", err)
	}

	// minimega gets the path in `cc mount`, which it splits, expands and
	// parses again, so it must need no quoting.
	if !plainPathRegex.MatchString(hostPath) {
		return "", fmt.Errorf(
			"mount path %q for VM %s may hold only letters, digits, '.', '_', '+', '-' and '/'",
			hostPath, vmName,
		)
	}

	details, err := Get(expName, vmName)
	if err != nil {
		return "", fmt.Errorf("getting VM details: %w", err)
	}

	if !details.Running {
		return "", errors.New("vm is not running")
	}

	if err := mm.IsC2ClientActive(
		mm.C2NS(expName),
		mm.C2VM(vmName),
		mm.C2IDClientsByUUID(),
		mm.C2Timeout(MountTimeout),
	); err != nil {
		return "", fmt.Errorf("vm's C2 agent (miniccc) is not reachable: %w", err)
	}

	if err := os.MkdirAll(hostPath, 0o750); err != nil {
		return "", fmt.Errorf("creating mount directory %s: %w", hostPath, err)
	}

	if err := mountFilesystem(expName, vmName, hostPath); err != nil {
		return "", err
	}

	plog.Info(plog.TypeAction, "vm mounted", "exp", expName, "vm", vmName, "path", hostPath)

	return hostPath, nil
}

// Unmount unmounts the filesystem of the given VM. It returns any errors
// encountered while unmounting.
//
// This is the same implementation used by the `POST
// /experiments/{exp}/vms/{name}/unmount` API endpoint, so behavior is
// consistent between the CLI and web UI.
func Unmount(expName, vmName string) error {
	if expName == "" {
		return errors.New("no experiment name provided")
	}

	if vmName == "" {
		return errors.New("no VM name provided")
	}

	if err := UnmountFilesystem(expName, vmName); err != nil {
		return err
	}

	plog.Info(plog.TypeAction, "vm unmounted", "exp", expName, "vm", vmName)

	return nil
}

// MountFilesystem issues the C2 command to mount a VM's filesystem at the
// default phenix mount path, tolerating the case where it's already mounted.
// It is used by the `POST /experiments/{exp}/vms/{name}/mount` API endpoint;
// the CLI uses Mount, which also accepts another host path.
func MountFilesystem(expName, vmName string) error {
	if err := requireRunning(expName); err != nil {
		return err
	}

	return mountFilesystem(expName, vmName, "")
}

//nolint:gochecknoglobals // replaced by tests
var (
	// vmMounted reports whether a VM filesystem is mounted at a host path.
	vmMounted = Mounted

	// readMountPoints returns the mount points of this host with their types.
	readMountPoints = mountPoints

	// detachMount lazily detaches the mount at a path.
	detachMount = func(path string) error { return syscall.Unmount(path, syscall.MNT_DETACH) }
)

// mountFilesystem mounts the VM's filesystem at hostPath, or at the default
// phenix mount path when hostPath is empty.
func mountFilesystem(expName, vmName, hostPath string) error {
	_, err := mm.ExecC2Command(
		mm.C2NS(expName),
		mm.C2VM(vmName),
		mm.C2Mount(),
		mm.C2MountPath(hostPath),
		mm.C2IDClientsByUUID(),
		mm.C2Timeout(MountTimeout),
	)

	if err == nil {
		return nil
	}

	if !strings.Contains(err.Error(), "already connected") {
		return fmt.Errorf("mounting VM filesystem: %w", err)
	}

	// minimega answers so for any VM that has a mount, wherever it is, so the
	// VM counts as mounted only if its filesystem is at the path asked for.
	if hostPath == "" {
		hostPath = mm.GetLocalMountPath(expName, vmName)
	}

	if vmMounted(hostPath) {
		return nil
	}

	return fmt.Errorf(
		"mounting VM filesystem at %s: VM %s is already mounted elsewhere, so unmount it first: %w",
		hostPath, vmName, err,
	)
}

// UnmountFilesystem issues the C2 command to unmount a VM's filesystem. It is
// used by both the CLI (via Unmount) and the `POST
// /experiments/{exp}/vms/{name}/unmount` API endpoint.
func UnmountFilesystem(expName, vmName string) error {
	if err := requireRunning(expName); err != nil {
		return err
	}

	_, err := mm.ExecC2Command(
		mm.C2NS(expName),
		mm.C2VM(vmName),
		mm.C2Unmount(),
		mm.C2Timeout(MountTimeout),
		mm.C2SkipActiveClientCheck(true),
	)
	if err != nil {
		return fmt.Errorf("unmounting VM filesystem: %w", err)
	}

	return nil
}

// unmountExperiment cleans up the experiment's VM filesystem mounts on this
// host after the experiment stopped or was deleted. Errors are logged but not
// returned since this is invoked from experiment lifecycle hooks.
//
// The hooks run after phenix cleared the experiment's minimega namespace, and
// minimega unmounts every `cc mount` of a namespace when it destroys it, so
// this does not ask minimega to unmount: any minimega command naming the
// namespace would re-create it, empty, and leave it behind. It only detaches
// mount points minimega failed to unmount and removes the empty directories.
func unmountExperiment(expName string) {
	mountDir := mm.GetLocalMountPath(expName, "")

	entries, err := os.ReadDir(mountDir)
	if err != nil {
		return
	}

	mounted := readMountPoints()

	for _, entry := range entries {
		path := filepath.Join(mountDir, entry.Name())

		fsType, isMount := mounted[canonicalMountPath(path)]

		switch {
		case fsType == vmMountFSType:
			// Detach rather than unmount: the 9p server went away with the VM.
			if err := detachMount(path); err != nil {
				plog.Warn(plog.TypeSystem, "detaching vm mount during experiment cleanup", "exp", expName, "path", path, "err", err)

				continue
			}
		case isMount:
			// Not a VM mount, such as a share someone bind-mounted here: theirs.
			plog.Warn(plog.TypeSystem, "leaving non-VM mount during experiment cleanup", "exp", expName, "path", path, "fs", fsType)

			continue
		}

		// Remove, not RemoveAll: a directory that still holds files was not a
		// phenix mount point and is left for the user.
		if err := os.Remove(path); err != nil {
			plog.Warn(plog.TypeSystem, "removing vm mount directory", "exp", expName, "path", path, "err", err)
		}
	}

	if err := os.Remove(mountDir); err != nil {
		plog.Warn(plog.TypeSystem, "removing experiment mount directory", "exp", expName, "path", mountDir, "err", err)
	}
}

const (
	// mountInfoMountPointField is the index of the mount point field in a line
	// of /proc/self/mountinfo (see proc(5)).
	mountInfoMountPointField = 4
	// mountInfoOptionalFields is the index of the optional fields, which end
	// with a "-" field followed by the file system type.
	mountInfoOptionalFields = 6

	// vmMountFSType is the file system of a VM mount: minimega serves `cc mount`
	// over 9p.
	vmMountFSType = "9p"
)

// Mounted reports whether a VM filesystem is mounted at path on this host,
// whichever process mounted it, such as `phenix vm mount` or phenix ui.
func Mounted(path string) bool {
	return readMountPoints()[canonicalMountPath(path)] == vmMountFSType
}

// canonicalMountPath returns path as /proc/self/mountinfo lists a mount point
// at it: with the symlinks of its directory, such as a /phenix that links to
// /data/phenix, resolved. The last element is left as it is, since a stat of
// a VM mount whose 9p server went away can fail or hang.
func canonicalMountPath(path string) string {
	path = filepath.Clean(path)

	if dir, err := filepath.EvalSymlinks(filepath.Dir(path)); err == nil {
		return filepath.Join(dir, filepath.Base(path))
	}

	return path
}

// mountPoints returns the mount points listed in /proc/self/mountinfo, each
// with its file system type.
func mountPoints() map[string]string {
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return map[string]string{}
	}

	return parseMountInfo(string(data))
}

// parseMountInfo maps each mount point in mountinfo data to its file system
// type. Of several mounts on one point, the last one listed, which is on top,
// wins.
func parseMountInfo(data string) map[string]string {
	points := make(map[string]string)

	for line := range strings.SplitSeq(data, "\n") {
		// The mount point has spaces and other special characters
		// octal-escaped.
		fields := strings.Fields(line)
		if len(fields) <= mountInfoMountPointField {
			continue
		}

		fsType := ""

		if len(fields) > mountInfoOptionalFields {
			if i := slices.Index(fields[mountInfoOptionalFields:], "-"); i >= 0 {
				if typ := mountInfoOptionalFields + i + 1; typ < len(fields) {
					fsType = fields[typ]
				}
			}
		}

		points[unescapeMountInfo(fields[mountInfoMountPointField])] = fsType
	}

	return points
}

// unescapeMountInfo decodes the \ooo octal escapes the kernel uses for
// special characters in /proc/self/mountinfo paths.
func unescapeMountInfo(s string) string {
	if !strings.Contains(s, "\\") {
		return s
	}

	var b strings.Builder

	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3

				continue
			}
		}

		b.WriteByte(s[i])
	}

	return b.String()
}
