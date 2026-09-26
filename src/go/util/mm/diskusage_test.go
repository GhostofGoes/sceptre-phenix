package mm

import (
	"testing"
	"time"
)

func TestGetDiskUsageReusesRecentMeasurement(t *testing.T) { //nolint:paralleltest // shares the package cache
	key := "compute1\x00/phenix"

	diskUsageMu.Lock()
	diskUsageCache[key] = diskUsageEntry{value: 42, at: time.Now()}
	diskUsageMu.Unlock()

	t.Cleanup(func() {
		diskUsageMu.Lock()
		delete(diskUsageCache, key)
		diskUsageMu.Unlock()
	})

	// a fresh entry is returned without asking minimega, which isn't running
	if got := (Minimega{}).getDiskUsage("compute1", "/phenix"); got != 42 {
		t.Fatalf("getDiskUsage() = %v, want 42", got)
	}
}
