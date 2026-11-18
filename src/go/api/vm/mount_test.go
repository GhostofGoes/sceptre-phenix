package vm_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"phenix/api/vm"
	"phenix/store"
	"phenix/util/mm"
)

// mountMM is the mm.MM side of mounting the running test-vm, whose C2 client
// is active and which minimega reports mounted already.
type mountMM struct {
	snapshotMM

	calls *int
}

func (mountMM) IsC2ClientActive(...mm.C2Option) error {
	return nil
}

func (m mountMM) ExecC2Command(...mm.C2Option) (string, error) {
	*m.calls++

	// minimega's answer to a `cc mount` of a VM that has a mount, wherever.
	return "", errors.New("already connected to 127.0.0.1:40125")
}

func installMountTest(t *testing.T) *int {
	t.Helper()

	originalStore, originalMM := store.DefaultStore, mm.DefaultMM

	t.Cleanup(func() {
		store.DefaultStore = originalStore //nolint:reassign // restore test double
		mm.DefaultMM = originalMM          //nolint:reassign // restore test double
	})

	getTestExperiment(t, "test-vm", nil, "2026-10-18T00:00:00Z")

	calls := 0
	mm.DefaultMM = mountMM{calls: &calls} //nolint:reassign // install test double

	return &calls
}

// TestMountAtPathOfVMMountedElsewhere is a regression test: minimega answers
// `already connected` to a `cc mount` of a VM that has a mount wherever it is,
// and phenix took that for success, so `phenix vm mount` printed a custom path
// with nothing mounted at it.
func TestMountAtPathOfVMMountedElsewhere(t *testing.T) {
	installMountTest(t)

	dir := filepath.Join(t.TempDir(), "vmfs")

	vm.SetMounted(t, func(string) bool { return false })

	if got, err := vm.Mount("test-experiment", "test-vm", dir); err == nil || !strings.Contains(err.Error(), "already mounted elsewhere") {
		t.Errorf("Mount() = %q, %v, want an error saying test-vm is mounted elsewhere", got, err)
	}

	vm.SetMounted(t, func(path string) bool { return path == dir })

	if got, err := vm.Mount("test-experiment", "test-vm", dir); err != nil || got != dir {
		t.Errorf("Mount() of a VM mounted at %s = %q, %v, want that path", dir, got, err)
	}
}

// TestMountRefusesPathMinimegaWouldRewrite checks that a mount path minimega
// would split at a space or expand at a $ is refused before anything is
// created or sent.
func TestMountRefusesPathMinimegaWouldRewrite(t *testing.T) {
	calls := installMountTest(t)

	for _, name := range []string{"my vm", "$HOME", `a"b`, "a#b"} {
		dir := filepath.Join(t.TempDir(), name)

		if got, err := vm.Mount("test-experiment", "test-vm", dir); err == nil || !strings.Contains(err.Error(), "mount path") {
			t.Errorf("Mount(%q) = %q, %v, want an error about the mount path", dir, got, err)
		}

		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("Mount(%q) created the directory (stat: %v)", dir, err)
		}
	}

	if *calls != 0 {
		t.Errorf("Mount() sent %d C2 commands for refused paths", *calls)
	}
}
