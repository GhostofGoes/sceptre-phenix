package lumberjack

import (
	"os"
	"path/filepath"
	"testing"
)

// TestNewFileKeepsLinesOfOtherWriters checks that a Logger that created its
// log file appends to it, so the lines another process appended to the same
// file in the meantime are not overwritten.
func TestNewFileKeepsLinesOfOtherWriters(t *testing.T) {
	name := filepath.Join(t.TempDir(), "phenix.log")

	l := &Logger{Filename: name}
	t.Cleanup(func() { _ = l.Close() })

	if _, err := l.Write([]byte("ui 1\n")); err != nil {
		t.Fatalf("writing the first line: %v", err)
	}

	other, err := os.OpenFile(name, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("opening the log file as another writer: %v", err)
	}

	if _, err := other.WriteString("cli 1\n"); err != nil {
		t.Fatalf("appending as another writer: %v", err)
	}

	if err := other.Close(); err != nil {
		t.Fatalf("closing the other writer: %v", err)
	}

	if _, err := l.Write([]byte("ui 2\n")); err != nil {
		t.Fatalf("writing the second line: %v", err)
	}

	got, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("reading the log file: %v", err)
	}

	if want := "ui 1\ncli 1\nui 2\n"; string(got) != want {
		t.Errorf("log file holds %q, want %q", got, want)
	}
}
