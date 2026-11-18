package vm

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestCopierReportsProgressOfSmallCopies is a regression test: the progress
// goroutine of a copy read the source's size only once it ran, by which time
// a small copy had ended and closed the source, so Stat failed and the nil
// file info panicked; a report still being sent when the copy closed its
// subscriptions panicked too. Either killed phenix ui, so committing a VM
// with a small base image crashed the server.
func TestCopierReportsProgressOfSmallCopies(t *testing.T) {
	dir := t.TempDir()

	src := filepath.Join(dir, "base.qc2")
	if err := os.WriteFile(src, []byte("eighteen bytes...."), 0o600); err != nil {
		t.Fatal(err)
	}

	empty := filepath.Join(dir, "empty.qc2")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	for i := range 200 {
		for _, from := range []string{src, empty} {
			c := newCopier()
			s := c.subscribe()

			var last float64

			drained := make(chan struct{})

			go func() {
				defer close(drained)

				for p := range s {
					last = p
				}
			}()

			dst := filepath.Join(dir, "copy.qc2")
			if err := c.copy(context.Background(), from, dst); err != nil {
				t.Fatalf("copy %d of %s: %v", i, from, err)
			}

			<-drained

			if last < 0 || last > 1 {
				t.Fatalf("copy %d of %s reported progress %v, want 0 to 1", i, from, last)
			}

			want, _ := os.ReadFile(from)
			if got, _ := os.ReadFile(dst); string(got) != string(want) {
				t.Fatalf("copy %d of %s holds %q, want %q", i, from, got, want)
			}
		}
	}
}
