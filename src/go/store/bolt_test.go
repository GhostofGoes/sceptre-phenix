package store

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.etcd.io/bbolt"
)

func TestBoltDBOpenFailure(t *testing.T) {
	cfg := &Config{Kind: "Topology", Metadata: ConfigMetadata{Name: "foo"}}

	tests := []struct {
		name string
		call func(*BoltDB) error
	}{
		{name: "get", call: func(b *BoltDB) error { return b.Get(cfg) }},
		{name: "update", call: func(b *BoltDB) error { return b.Update(cfg) }},
		{name: "delete", call: func(b *BoltDB) error { return b.Delete(cfg) }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// The parent directory does not exist, so every open fails.
			b := &BoltDB{path: filepath.Join(t.TempDir(), "missing", "store.bdb")}

			done := make(chan error, 2)

			go func() {
				done <- tt.call(b)
				done <- tt.call(b)
			}()

			for range 2 {
				select {
				case err := <-done:
					if !errors.Is(err, fs.ErrNotExist) {
						t.Fatalf("expected the BoltDB open error, got %v", err)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("call blocked after a failed open")
				}
			}
		})
	}
}

func TestBoltDBOpenTimeout(t *testing.T) {
	orig := boltOpenTimeout
	boltOpenTimeout = 100 * time.Millisecond

	t.Cleanup(func() { boltOpenTimeout = orig })

	path := filepath.Join(t.TempDir(), "store.bdb")

	// Holding the file open takes its exclusive lock, as another phenix process would.
	holder, err := bbolt.Open(path, boltFileMode, nil)
	if err != nil {
		t.Fatalf("opening BoltDB file: %v", err)
	}

	t.Cleanup(func() { _ = holder.Close() })

	b := &BoltDB{path: path}
	cfg := &Config{Kind: "Topology", Metadata: ConfigMetadata{Name: "foo"}}
	done := make(chan error, 1)

	go func() { done <- b.Get(cfg) }()

	select {
	case err := <-done:
		if !errors.Is(err, bbolt.ErrTimeout) {
			t.Fatalf("expected bbolt.ErrTimeout, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("open waited for the file lock without a timeout")
	}
}

func TestBoltDBInitCreatesMissingDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "nested", "store.bdb")

	db := NewBoltDB()

	if err := db.Init(Endpoint("bolt://" + path)); err != nil {
		t.Fatalf("Init() with a missing directory: %v", err)
	}

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("store file was not created: %v", err)
	}
}

// TestBoltDBOpenFailureReleasesLock is a regression test: when the database
// could not be opened, open kept the store locked, so every later call, in a
// long-running phenix ui every request, blocked forever.
func TestBoltDBOpenFailureReleasesLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.bdb")

	db := NewBoltDB()

	if err := db.Init(Endpoint("bolt://" + path)); err != nil {
		t.Fatalf("Init(): %v", err)
	}

	if err := os.WriteFile(path, []byte("not a bolt database"), 0o600); err != nil {
		t.Fatal(err)
	}

	c, _ := NewConfig("experiment/test")

	calls := []struct {
		name string
		call func() error
	}{
		{"List", func() error { _, err := db.List("Experiment"); return err }},
		{"Get", func() error { return db.Get(c) }},
		{"Update", func() error { return db.Update(c) }},
		{"Delete", func() error { return db.Delete(c) }},
		{"List again", func() error { _, err := db.List("Experiment"); return err }},
	}

	for _, call := range calls {
		done := make(chan error, 1)

		go func() { done <- call.call() }()

		select {
		case err := <-done:
			if err == nil {
				t.Errorf("%s on a corrupted database returned no error", call.name)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s blocked after a failed open: the store stayed locked", call.name)
		}
	}
}

// TestBoltDBInitCreatesNoRelativeDirectory is a regression test: Init created
// the directory of any endpoint, so a malformed one such as
// bolt://etc/phenix/store.bdb (two slashes) or bolt://localhost/var/x.bdb,
// whose path is relative, silently created a fresh empty store under the
// working directory.
func TestBoltDBInitCreatesNoRelativeDirectory(t *testing.T) {
	for _, endpoint := range []string{"bolt://etc/phenix/store.bdb", "bolt://localhost/var/lib/x.bdb", "bolt://~/.phenix.bdb"} {
		t.Run(endpoint, func(t *testing.T) {
			dir := t.TempDir()
			t.Chdir(dir)

			if err := NewBoltDB().Init(Endpoint(endpoint)); err == nil {
				t.Errorf("Init(%q) succeeded, want an error for a missing relative directory", endpoint)
			}

			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}

			if len(entries) != 0 {
				t.Errorf("Init(%q) created %v under the working directory", endpoint, entries)
			}
		})
	}
}

// TestBoltDBReportsBucketFailures is a regression test: Delete returned nil
// when it could not create the config's bucket, as on a full or read-only
// volume, so a delete that kept the config reported success, and Update
// reported any failed read as a missing config. A config without a kind has
// no bucket, which Delete and Update must both report.
func TestBoltDBReportsBucketFailures(t *testing.T) {
	db := NewBoltDB()

	if err := db.Init(Endpoint("bolt://" + filepath.Join(t.TempDir(), "store.bdb"))); err != nil {
		t.Fatalf("Init(): %v", err)
	}

	c := &Config{Metadata: ConfigMetadata{Name: "test"}}

	if err := db.Delete(c); err == nil {
		t.Error("Delete() with a failing bucket creation = nil, want the error")
	}

	if err := db.Update(c); err == nil || errors.Is(err, ErrNotExist) {
		t.Errorf("Update() with a failing bucket creation = %v, want the error, not ErrNotExist", err)
	}
}
