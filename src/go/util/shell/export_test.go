package shell

import (
	"os"
	"testing"
	"time"
)

// SetKillDelay shortens how long a canceled command may run after SIGTERM
// until the test ends.
func SetKillDelay(t *testing.T, delay time.Duration) {
	t.Helper()

	original := killDelay

	t.Cleanup(func() { killDelay = original })

	killDelay = delay
}

// SetCloseDelay shortens how long a canceled command's output is still read
// once its process group was killed, until the test ends.
func SetCloseDelay(t *testing.T, delay time.Duration) {
	t.Helper()

	original := closeDelay

	t.Cleanup(func() { closeDelay = original })

	closeDelay = delay
}

// EndGroups passes sig on to the process groups of the commands ExecCommand
// runs, as a signal that ends phenix does.
func EndGroups(sig os.Signal) { endGroups(sig) }

// WaitCanceled waits until a command ExecCommand runs was canceled.
func WaitCanceled(t *testing.T) {
	t.Helper()

	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		groupsMu.Lock()

		for _, canceled := range groups {
			if canceled.Load() {
				groupsMu.Unlock()

				return
			}
		}

		groupsMu.Unlock()
	}

	t.Fatal("no command was canceled")
}
