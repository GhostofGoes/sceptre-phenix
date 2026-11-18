package cache

import (
	"testing"
	"time"
)

// lockRecorder is a WebCache that records the expiration of each lock.
type lockRecorder struct {
	WebCache

	exp map[string]time.Duration
}

func (r *lockRecorder) Lock(key string, _ Status, exp time.Duration) Status {
	r.exp[key] = exp

	return ""
}

// TestStopLockOutlastsTheStop is a regression test: the stopping lock expired
// after 1 minute, while the SCORCH stopping hook alone may wait 5 minutes for
// canceled runs, so a second stop request took the lock and ran the stop again
// while the first one was still running.
func TestStopLockOutlastsTheStop(t *testing.T) {
	original := DefaultWebCache
	t.Cleanup(func() { DefaultWebCache = original })

	rec := &lockRecorder{exp: make(map[string]time.Duration)}
	DefaultWebCache = rec

	if err := LockExperimentForStopping("exp"); err != nil {
		t.Fatal(err)
	}

	if got := rec.exp["experiment|exp"]; got < 10*time.Minute {
		t.Errorf("the stopping lock expires after %v, want well over the 5 minute SCORCH stopping wait", got)
	}
}
