//go:build e2e

package e2e

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Hollow disk fixtures. A hollow VM boots QEMU with no operating system, which
// is enough for every check that does not need a guest: VM rows, taps, VLANs,
// QMP, snapshots, captures. phenix's default startup app injects files into
// partition 1 of a Linux node's disk, so the ext4 fixture carries an msdos
// label and one ext4 partition, laid out like the images phenix builds.
const (
	blankImage = "e2e-blank.qc2"
	ext4Image  = "e2e-ext4.qc2"
	ext4Marker = "e2e-base-marker"
)

// hollowFixtures builds the hollow disk fixtures once per test binary and
// keeps the result, so that every caller, not only the first, fails when the
// build failed.
var hollowFixtures = sync.OnceValue(makeHollowImages) //nolint:gochecknoglobals // one fixture build per binary

// hollowImages creates the hollow disk fixtures once per test binary.
func hollowImages(t *testing.T) {
	t.Helper()

	if err := hollowFixtures(); err != nil {
		t.Fatalf("creating hollow disk fixtures: %v", err)
	}
}

func run(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput() //nolint:gosec // test harness
	if err != nil {
		return fmt.Errorf("%s %s: %w\n%s", name, strings.Join(args, " "), err, out)
	}

	return nil
}

func makeHollowImages() error {
	if err := run("qemu-img", "create", "-q", "-f", "qcow2", filepath.Join(cfg.imagesDir, blankImage), "32M"); err != nil {
		return err
	}

	raw := filepath.Join(cfg.workDir, "e2e-ext4.raw")

	if err := run("truncate", "-s", "64M", raw); err != nil {
		return err
	}

	defer os.Remove(raw)

	sfdisk := exec.Command("sfdisk", "-q", raw)
	sfdisk.Stdin = strings.NewReader("label: dos\n,,83\n")

	if out, err := sfdisk.CombinedOutput(); err != nil {
		return fmt.Errorf("sfdisk: %w\n%s", err, out)
	}

	out, err := exec.Command("losetup", "-fP", "--show", raw).Output()
	if err != nil {
		return fmt.Errorf("losetup: %w", err)
	}

	loop := strings.TrimSpace(string(out))

	defer func() { _ = run("losetup", "-d", loop) }()

	if err := run("mkfs.ext4", "-q", "-L", "e2e", loop+"p1"); err != nil {
		return err
	}

	mnt, err := os.MkdirTemp(cfg.workDir, "mnt")
	if err != nil {
		return err
	}

	defer os.Remove(mnt)

	if err := run("mount", loop+"p1", mnt); err != nil {
		return err
	}

	writeErr := os.WriteFile(filepath.Join(mnt, ext4Marker), []byte("base\n"), 0o644) //nolint:gosec // guest file
	if writeErr == nil {
		writeErr = os.MkdirAll(filepath.Join(mnt, "etc"), 0o755) //nolint:gosec // guest dir
	}

	if err := run("umount", mnt); err != nil {
		return err
	}

	if writeErr != nil {
		return writeErr
	}

	return run("qemu-img", "convert", "-q", "-f", "raw", "-O", "qcow2", raw, filepath.Join(cfg.imagesDir, ext4Image))
}

// nbdDevice is the device the disk inspections use. minimega's GetDevice
// takes the first nbd device, in /dev's name order (nbd0, nbd1, nbd10 to
// nbd15, nbd2 to nbd9), whose pid and lock files are absent
// (internal/nbd/nbd.go), so it skips this one while a test holds it and
// reaches it only when every other device is busy.
const nbdDevice = "/dev/nbd9"

const (
	// nbdReleaseTimeout bounds the wait for the previous connection of
	// nbdDevice to go away, which normally takes milliseconds.
	nbdReleaseTimeout = 30 * time.Second

	// nbdStartTimeout bounds the wait for a new connection to start.
	nbdStartTimeout = 5 * time.Second

	// nbdConnectAttempts is how often nbdConnect connects a device that does
	// not start, as minimega's ConnectImage retries.
	nbdConnectAttempts = 3
)

var nbdMu sync.Mutex //nolint:gochecknoglobals // guards the one nbd device

// inspectDisk mounts partition 1 of a qcow2 image read-only and calls fn with
// the mount point. The image may be in use by a VM or minimega: the mount is
// read-only and qcow2 locking is disabled.
func inspectDisk(t *testing.T, image string, fn func(root string)) {
	t.Helper()

	mountDisk(t, image, 1, "-r", "ro,noload", fn)
}

// mountDisk connects a qcow2 image to nbdDevice with qemu-nbd mode (-r or
// --snapshot), mounts its partition part with the options mountOpts, and calls
// fn with the mount point.
func mountDisk(t *testing.T, image string, part int, mode, mountOpts string, fn func(root string)) {
	t.Helper()

	nbdMu.Lock()
	defer nbdMu.Unlock()

	nbdConnect(t, image, mode)

	defer nbdDisconnect(t)

	dev := fmt.Sprintf("%sp%d", nbdDevice, part)

	waitNBDPartition(t, dev)

	mnt := t.TempDir()

	if err := run("mount", "-o", mountOpts, dev, mnt); err != nil {
		t.Fatal(err)
	}

	defer func() {
		if err := run("umount", mnt); err != nil {
			t.Errorf("unmounting %s from %s: %v", image, mnt, err)
		}
	}()

	fn(mnt)
}

// nbdDisconnect drops nbdDevice's connection and waits until the kernel has
// released it, so that a connection a test leaves behind fails that test, not
// the next one to inspect a disk.
func nbdDisconnect(t *testing.T) {
	t.Helper()

	if err := run("qemu-nbd", "-d", nbdDevice); err != nil {
		t.Errorf("disconnecting %s: %v", nbdDevice, err)
	}

	waitNBDReleased(t, nbdDevice)
}

// waitNBDPartition waits for partition device part of nbdDevice, which
// qemu-nbd has just connected. The kernel's partition scan on connect can run
// before the device is ready and find nothing, so every poll rereads the
// partition table, as minimega's disk inject does (minimega
// cmd/minimega/disk.go). On a timeout the detail tells a missed scan (a size
// above 0 and a pid) from a device qemu-nbd does not serve.
func waitNBDPartition(t *testing.T, part string) {
	t.Helper()

	sys := filepath.Join("/sys/block", filepath.Base(nbdDevice))

	eventually(t, 10*time.Second, 100*time.Millisecond, part+" to appear", func() (bool, string) {
		_ = run("blockdev", "--rereadpt", nbdDevice)

		_, err := os.Stat(part)
		if err == nil {
			return true, ""
		}

		size, sizeErr := os.ReadFile(filepath.Join(sys, "size")) //nolint:gosec // fixed sysfs path
		_, pidErr := os.Stat(filepath.Join(sys, "pid"))

		return false, fmt.Sprintf("%v; %s/size %q (%v); %s/pid present: %t",
			err, sys, strings.TrimSpace(string(size)), sizeErr, sys, pidErr == nil)
	})
}

// nbdConnect exports image on nbdDevice with qemu-nbd, mode being -r
// (read-only) or --snapshot (writes go to a throwaway overlay), and waits
// until the kernel serves it: the pid file appears only once qemu-nbd's
// NBD_DO_IT started the device, and after waitNBDReleased it can only be the
// new connection's. A connection that does not start is dropped and made
// again.
func nbdConnect(t *testing.T, image, mode string) {
	t.Helper()

	opts := "driver=qcow2,file.driver=file,file.locking=off,file.filename=" + image
	pid := filepath.Join("/sys/block", filepath.Base(nbdDevice), "pid")

	for attempt := 1; ; attempt++ {
		waitNBDReleased(t, nbdDevice)

		if err := run("qemu-nbd", mode, "-c", nbdDevice, "--image-opts", opts); err != nil {
			t.Fatal(err)
		}

		for deadline := time.Now().Add(nbdStartTimeout); time.Now().Before(deadline); {
			if _, err := os.Stat(pid); err == nil {
				return
			}

			time.Sleep(50 * time.Millisecond)
		}

		_ = run("qemu-nbd", "-d", nbdDevice)

		if attempt == nbdConnectAttempts {
			t.Fatalf("%s did not start serving %s in %d connections", nbdDevice, image, attempt)
		}

		t.Logf("%s did not start serving %s within %v; connecting again", nbdDevice, image, nbdStartTimeout)
	}
}

// waitNBDReleased waits until no connection of nbd device dev is alive, and
// logs whether one was and how long it took to go. qemu-nbd -d returns before
// the kernel drops the old connection: the exiting qemu-nbd and udev's probes
// of the disconnect still hold it, and its pid stays set. A qemu-nbd -c that
// opens the device in that window joins the old connection, so its NBD_DO_IT
// fails with EBUSY (kernel nbd.c nbd_start_device) after qemu-nbd -c already
// exited 0, and the device never gets a size or a pid. The kernel removes the
// pid file and zeroes the size when the last holder lets go, and the old
// qemu-nbd removes its socket under /var/lock when it exits; minimega's
// GetDevice checks the same files (internal/nbd/nbd.go).
func waitNBDReleased(t *testing.T, dev string) {
	t.Helper()

	var (
		name  = filepath.Base(dev)
		pid   = filepath.Join("/sys/block", name, "pid")
		size  = filepath.Join("/sys/block", name, "size")
		lock  = filepath.Join("/var/lock", "qemu-nbd-"+name)
		start = time.Now()
	)

	before := "absent"
	if data, err := os.ReadFile(pid); err == nil { //nolint:gosec // fixed sysfs path
		before = fmt.Sprintf("present (%s)", strings.TrimSpace(string(data)))
	}

	eventually(t, nbdReleaseTimeout, 20*time.Millisecond, dev+" to be released", func() (bool, string) {
		pidData, pidErr := os.ReadFile(pid)    //nolint:gosec // fixed sysfs path
		sizeData, sizeErr := os.ReadFile(size) //nolint:gosec // fixed sysfs path
		_, lockErr := os.Stat(lock)

		sectors := strings.TrimSpace(string(sizeData))
		idle := errors.Is(pidErr, fs.ErrNotExist) && sizeErr == nil && sectors == "0" && errors.Is(lockErr, fs.ErrNotExist)

		return idle, fmt.Sprintf("%s %q (%v); %s %q (%v); %s: %v",
			pid, strings.TrimSpace(string(pidData)), pidErr, size, sectors, sizeErr, lock, lockErr)
	})

	t.Logf("%s: pid file %s at first look; released after %v", dev, before, time.Since(start).Round(time.Millisecond))
}

// readGuestFile reads a file inside a mounted guest filesystem.
func readGuestFile(t *testing.T, root, path string) string {
	t.Helper()

	data, err := os.ReadFile(filepath.Join(root, path))
	if err != nil {
		t.Fatalf("reading %s from the guest disk: %v", path, err)
	}

	return string(data)
}

// guestFile reads a file of a mounted guest filesystem, reporting false when it
// is absent.
func guestFile(root, path string) (string, bool) {
	data, err := os.ReadFile(filepath.Join(root, path)) //nolint:gosec // guest disk
	if err != nil {
		return "", false
	}

	return string(data), true
}

// staticIface returns a static /24 interface for a node's interfaces list.
func staticIface(name, vlan, address string) string {
	return fmt.Sprintf(`
      - name: %s
        type: ethernet
        proto: static
        vlan: %s
        address: %s
        mask: 24`, name, vlan, address)
}

// hollowNode returns a hollow Linux node on the ext4 fixture with one static
// interface; extra is appended verbatim under the node (indent 4).
func hollowNode(hostname, vlan, address, extra string) string {
	return fmt.Sprintf(`
  - type: VirtualMachine
    general:
      hostname: %s
    hardware:
      vcpus: 1
      memory: 256
      os_type: linux
      drives:
      - image: %s
    network:
      interfaces:
      - name: eth0
        type: ethernet
        proto: static
        vlan: %s
        address: %s
        mask: 24%s`, hostname, ext4Image, vlan, address, extra)
}
