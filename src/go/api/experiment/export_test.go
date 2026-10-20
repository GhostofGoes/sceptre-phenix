package experiment

import (
	"testing"
	"time"
)

// SetAsyncStartStopWait shortens how long Stop waits for a canceled
// asynchronous start until the test ends.
func SetAsyncStartStopWait(t *testing.T, wait time.Duration) {
	t.Helper()

	original := asyncStartStopWait

	t.Cleanup(func() { asyncStartStopWait = original })

	asyncStartStopWait = wait
}
