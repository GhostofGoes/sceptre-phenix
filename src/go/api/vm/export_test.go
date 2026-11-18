package vm

import (
	"testing"
	"time"
)

// SetMMFullPath makes this package resolve paths in minimega's files
// directory with f until the test ends.
func SetMMFullPath(t *testing.T, f func(string) string) {
	t.Helper()

	original := mmFullPath

	t.Cleanup(func() { mmFullPath = original })

	mmFullPath = f
}

// SetVMSaveStatusTimeout makes Snapshot give up on a `vm save` without status
// after d until the test ends.
func SetVMSaveStatusTimeout(t *testing.T, d time.Duration) {
	t.Helper()

	original := vmSaveStatusTimeout

	t.Cleanup(func() { vmSaveStatusTimeout = original })

	vmSaveStatusTimeout = d
}

// SetVMSaveTimeout makes Snapshot give up on a `vm save` that has not
// completed after d until the test ends.
func SetVMSaveTimeout(t *testing.T, d time.Duration) {
	t.Helper()

	original := vmSaveTimeout

	t.Cleanup(func() { vmSaveTimeout = original })

	vmSaveTimeout = d
}

// SetSaveCommandTimeout makes Snapshot give up on each command of a save
// that minimega has not answered after d until the test ends.
func SetSaveCommandTimeout(t *testing.T, d time.Duration) {
	t.Helper()

	original := saveCommandTimeout

	t.Cleanup(func() { saveCommandTimeout = original })

	saveCommandTimeout = d
}

// SetMounted makes this package decide whether a VM filesystem is mounted at
// a path with f until the test ends.
func SetMounted(t *testing.T, f func(string) bool) {
	t.Helper()

	original := vmMounted

	t.Cleanup(func() { vmMounted = original })

	vmMounted = f
}
