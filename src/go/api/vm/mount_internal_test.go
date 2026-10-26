package vm

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"phenix/util/common"
)

func TestUnescapeMountInfo(t *testing.T) {
	tests := map[string]string{
		"/phenix/mounts/exp/vm1":      "/phenix/mounts/exp/vm1",
		`/phenix/mounts/my\040exp/vm`: "/phenix/mounts/my exp/vm",
		`/tab\011and\134slash`:        "/tab\tand\\slash",
		`/trailing\04`:                `/trailing\04`,
	}

	for in, want := range tests {
		if got := unescapeMountInfo(in); got != want {
			t.Errorf("unescapeMountInfo(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseMountInfo(t *testing.T) {
	data := strings.Join([]string{
		`36 35 98:0 /mnt1 /mnt2 rw,noatime master:1 - ext3 /dev/root rw,errors=continue`,
		`100 30 0:52 / /phenix/mounts/exp/vm1 rw,relatime shared:60 - 9p 127.0.0.1 rw,trans=tcp,port=40125`,
		`101 30 0:53 / /phenix/mounts/my\040exp/vm rw,relatime - 9p 127.0.0.1 rw`,
		`102 30 0:54 / /stacked rw - tmpfs tmpfs rw`,
		`103 102 0:55 / /stacked rw - 9p 127.0.0.1 rw`,
		`104 30 0:56 / /truncated rw`,
		``,
	}, "\n")

	want := map[string]string{
		"/mnt2":                    "ext3",
		"/phenix/mounts/exp/vm1":   "9p",
		"/phenix/mounts/my exp/vm": "9p",
		"/stacked":                 "9p",
		"/truncated":               "",
	}

	if got := parseMountInfo(data); !maps.Equal(got, want) {
		t.Fatalf("parseMountInfo() = %v, want %v", got, want)
	}
}

func TestMountedIgnoresPlainDirectories(t *testing.T) {
	if Mounted(t.TempDir()) {
		t.Fatal("expected a plain directory not to count as a VM mount")
	}
}

func TestUnmountExperimentRemovesOnlyEmptyMountDirectories(t *testing.T) {
	original := common.MountBase
	common.MountBase = t.TempDir() //nolint:reassign // test configuration

	t.Cleanup(func() { common.MountBase = original }) //nolint:reassign // restore

	expDir := filepath.Join(common.MountBase, "exp")

	if err := os.MkdirAll(filepath.Join(expDir, "vm1"), 0o750); err != nil {
		t.Fatal(err)
	}

	// Not a mount point and not empty: someone else's files, kept.
	if err := os.MkdirAll(filepath.Join(expDir, "vm2"), 0o750); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(expDir, "vm2", "keep"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	unmountExperiment("exp")

	if _, err := os.Stat(filepath.Join(expDir, "vm1")); !os.IsNotExist(err) {
		t.Errorf("empty VM mount directory was not removed: %v", err)
	}

	if _, err := os.Stat(filepath.Join(expDir, "vm2", "keep")); err != nil {
		t.Errorf("file in a non-mount directory was removed: %v", err)
	}

	// Nothing to do for an experiment without mounts.
	unmountExperiment("never-mounted")
}

// TestUnmountExperimentDetachesOnlyVMMounts is a regression test: the cleanup
// of a stopped experiment lazily detached whatever was mounted at a VM mount
// directory, whatever its file system, such as a share someone bind-mounted
// there, while only 9p mounts are VM mounts.
func TestUnmountExperimentDetachesOnlyVMMounts(t *testing.T) {
	original := common.MountBase
	common.MountBase = t.TempDir() //nolint:reassign // test configuration

	t.Cleanup(func() { common.MountBase = original }) //nolint:reassign // restore

	expDir := filepath.Join(common.MountBase, "exp")

	for _, dir := range []string{"vm1", "notes"} {
		if err := os.MkdirAll(filepath.Join(expDir, dir), 0o750); err != nil {
			t.Fatal(err)
		}
	}

	var detached []string

	originalRead, originalDetach := readMountPoints, detachMount

	t.Cleanup(func() { readMountPoints, detachMount = originalRead, originalDetach })

	readMountPoints = func() map[string]string {
		return map[string]string{
			filepath.Join(expDir, "vm1"):   vmMountFSType,
			filepath.Join(expDir, "notes"): "ext4",
		}
	}

	detachMount = func(path string) error {
		detached = append(detached, path)

		return nil
	}

	unmountExperiment("exp")

	if want := []string{filepath.Join(expDir, "vm1")}; !slices.Equal(detached, want) {
		t.Errorf("unmountExperiment() detached %q, want only the 9p mount %q", detached, want)
	}

	if _, err := os.Stat(filepath.Join(expDir, "notes")); err != nil {
		t.Errorf("unmountExperiment() removed the non-VM mount point: %v", err)
	}
}

// TestMountPointsBehindSymlinks is a regression test: /proc/self/mountinfo
// lists mount points with symlinks resolved, and phenix looked the mount
// directory up as written, so with the mount directory behind a symlink a VM
// minimega had mounted read as mounted elsewhere, and the cleanup of a stopped
// experiment did not detach a VM mount minimega left behind.
func TestMountPointsBehindSymlinks(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "real")

	if err := os.MkdirAll(filepath.Join(target, "exp", "vm1"), 0o750); err != nil {
		t.Fatal(err)
	}

	if err := os.Symlink(target, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}

	canonical, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatal(err)
	}

	original, originalRead, originalDetach := common.MountBase, readMountPoints, detachMount

	t.Cleanup(func() {
		common.MountBase = original //nolint:reassign // restore
		readMountPoints, detachMount = originalRead, originalDetach
	})

	common.MountBase = filepath.Join(root, "link") //nolint:reassign // test configuration

	readMountPoints = func() map[string]string {
		return map[string]string{filepath.Join(canonical, "exp", "vm1"): vmMountFSType}
	}

	var detached []string

	detachMount = func(path string) error {
		detached = append(detached, path)

		return nil
	}

	if !Mounted(filepath.Join(common.MountBase, "exp", "vm1")) {
		t.Error("Mounted() = false for a VM mounted at a path behind a symlink")
	}

	unmountExperiment("exp")

	if want := []string{filepath.Join(common.MountBase, "exp", "vm1")}; !slices.Equal(detached, want) {
		t.Errorf("unmountExperiment() detached %q, want %q", detached, want)
	}
}
