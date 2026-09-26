package disk

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// fakeQemuImg reports every image as a standalone qcow2 and counts calls.
func fakeQemuImg(t *testing.T) *atomic.Int32 {
	t.Helper()

	var calls atomic.Int32

	orig := qemuImgInfo
	qemuImgInfo = func(_ context.Context, path string, _ bool) ([]byte, error) {
		calls.Add(1)

		return json.Marshal([]qemuImage{{Filename: path, Format: "qcow2", VirtualSize: 1 << 30, ActualSize: 2048}})
	}

	t.Cleanup(func() { qemuImgInfo = orig })
	ClearCache()
	t.Cleanup(ClearCache)

	return &calls
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCachedChainInspectsOnlyChangedImages(t *testing.T) {
	calls := fakeQemuImg(t)
	path := filepath.Join(t.TempDir(), "a.qc2")
	writeFile(t, path, "one")

	for range 3 {
		if _, err := cachedChain(path); err != nil {
			t.Fatal(err)
		}
	}

	if got := calls.Load(); got != 1 {
		t.Fatalf("unchanged image inspected %d times, want 1", got)
	}

	writeFile(t, path, "changed")

	if _, err := cachedChain(path); err != nil {
		t.Fatal(err)
	}

	if got := calls.Load(); got != 2 {
		t.Fatalf("changed image not inspected again (%d calls)", got)
	}

	ClearCache()

	if _, err := cachedChain(path); err != nil {
		t.Fatal(err)
	}

	if got := calls.Load(); got != 3 {
		t.Fatalf("ClearCache did not force inspection (%d calls)", got)
	}
}

func TestResolveLocalSkipsMissingAndKeepsFirstName(t *testing.T) {
	fakeQemuImg(t)

	dir := t.TempDir()
	present := filepath.Join(dir, "a.qc2")
	writeFile(t, present, "x")

	details := map[string]Details{}
	resolveLocal([]string{present, filepath.Join(dir, "missing.qc2")}, details)

	if len(details) != 1 {
		t.Fatalf("got %d images, want 1: %v", len(details), details)
	}

	got := details["a.qc2"]
	if got.FullPath != present || got.Kind != VMImage || got.Size != "2.0 KiB" || got.VirtualSize != "1.0 GiB" {
		t.Fatalf("unexpected details: %+v", got)
	}
}

func TestLockedInodes(t *testing.T) {
	locks := filepath.Join(t.TempDir(), "locks")
	writeFile(t, locks, "1: POSIX  ADVISORY  WRITE 1234 08:02:5678 0 EOF\n"+
		"2: FLOCK  ADVISORY  WRITE 99 00:1f:42 0 EOF\n")

	orig := procLocks
	procLocks = locks

	t.Cleanup(func() { procLocks = orig })

	locked, err := lockedInodes()
	if err != nil {
		t.Fatal(err)
	}

	if !locked[5678] || !locked[42] || locked[567] || len(locked) != 2 {
		t.Fatalf("unexpected locked inodes: %v", locked)
	}
}

func TestChainDetailsWithRealQemuImg(t *testing.T) {
	if _, err := exec.LookPath("qemu-img"); err != nil {
		t.Skip("qemu-img not installed")
	}

	ClearCache()
	t.Cleanup(ClearCache)

	dir := t.TempDir()
	base := filepath.Join(dir, "base.qc2")
	snap := filepath.Join(dir, "snap.qcow2")
	iso := filepath.Join(dir, "tools.iso")

	for _, args := range [][]string{
		{"create", "-f", "qcow2", base, "1G"},
		{"create", "-f", "qcow2", "-b", base, "-F", "qcow2", snap},
		{"create", "-f", "raw", iso, "1M"},
	} {
		if out, err := exec.Command("qemu-img", args...).CombinedOutput(); err != nil {
			t.Fatalf("qemu-img %v: %v: %s", args, err, out)
		}
	}

	paths, err := imageFiles(dir)
	if err != nil {
		t.Fatal(err)
	}

	details := map[string]Details{}
	resolveLocal(paths, details)

	if len(details) != 3 {
		t.Fatalf("got %d images, want 3: %v", len(details), details)
	}

	if got := details["snap.qcow2"].BackingImages; len(got) != 1 || got[0] != "base.qc2" {
		t.Fatalf("snapshot backing chain = %v, want [base.qc2]", got)
	}

	if details["tools.iso"].Kind != ISOImage || details["base.qc2"].VirtualSize != "1.0 GiB" {
		t.Fatalf("unexpected details: %+v", details)
	}
}

func TestWatchDebouncesImageChanges(t *testing.T) {
	dir := t.TempDir()
	changes := make(chan struct{}, 10)

	if err := Watch(t.Context(), dir, 100*time.Millisecond, func() { changes <- struct{}{} }); err != nil {
		t.Fatal(err)
	}

	writeFile(t, filepath.Join(dir, "notes.txt"), "ignored")

	for i := range 5 {
		writeFile(t, filepath.Join(dir, "upload.qc2"), string(rune('a'+i)))
	}

	select {
	case <-changes:
	case <-time.After(5 * time.Second):
		t.Fatal("no change reported")
	}

	select {
	case <-changes:
		t.Fatal("burst of writes reported more than once")
	case <-time.After(300 * time.Millisecond):
	}
}
