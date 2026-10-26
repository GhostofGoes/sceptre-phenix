package vm

import (
	"testing"
	"time"
)

// TestFileTimestampKeepsTheSecond is a regression test: the layout kept only
// the hour, and then only the minute, so every subnet capture of an interface
// started in the same hour or minute got the same file name, and minimega
// truncated the earlier capture.
func TestFileTimestampKeepsTheSecond(t *testing.T) {
	stamp := time.Date(2026, time.October, 18, 18, 10, 55, 0, time.UTC)

	if got, want := fileTimestamp(stamp), "20261018_181055"; got != want {
		t.Fatalf("fileTimestamp(%s) = %q, want %q", stamp, got, want)
	}

	if a, b := fileTimestamp(stamp), fileTimestamp(stamp.Add(20*time.Second)); a == b {
		t.Fatalf("captures started 20 s apart both get stamp %q", a)
	}
}
