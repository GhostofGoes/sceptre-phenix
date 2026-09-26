package mm

import (
	"testing"
	"time"

	"phenix/util/common"
)

func TestGetHostDiskUsageReusesRecentMeasurement(t *testing.T) { //nolint:paralleltest // shares the package cache
	keys := map[string]float64{
		"compute1\x00" + common.PhenixBase:   42,
		"compute1\x00" + common.MinimegaBase: 17,
	}

	diskUsageMu.Lock()

	for key, value := range keys {
		diskUsageCache[key] = diskUsageEntry{value: value, at: time.Now()}
	}

	diskUsageMu.Unlock()

	t.Cleanup(func() {
		diskUsageMu.Lock()

		for key := range keys {
			delete(diskUsageCache, key)
		}

		diskUsageMu.Unlock()
	})

	// fresh entries are returned without asking minimega, which isn't running
	want := DiskUsage{Phenix: 42, Minimega: 17}
	if got := (Minimega{}).getHostDiskUsage("compute1"); got != want {
		t.Fatalf("getHostDiskUsage() = %#v, want %#v", got, want)
	}
}
