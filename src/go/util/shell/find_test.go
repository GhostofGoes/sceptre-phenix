package shell_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"phenix/util/shell"
)

func writeFile(t *testing.T, path string, mode os.FileMode) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("creating directory for %s: %v", path, err)
	}

	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), mode); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

func TestFindCommandsWithPrefix(t *testing.T) {
	first := t.TempDir()
	second := t.TempDir()
	missing := filepath.Join(t.TempDir(), "does-not-exist")

	writeFile(t, filepath.Join(first, "phenix-app-alpha"), 0o755)
	writeFile(t, filepath.Join(first, "phenix-app-noexec"), 0o644)
	writeFile(t, filepath.Join(first, "other-tool"), 0o755)
	writeFile(t, filepath.Join(first, "phenix-app-"), 0o755)
	writeFile(t, filepath.Join(first, "nested", "phenix-app-nested"), 0o755)

	if err := os.Mkdir(filepath.Join(first, "phenix-app-dir"), 0o750); err != nil {
		t.Fatal(err)
	}

	// A command installed as a symbolic link (for example into a virtualenv)
	// must be found, and a dangling link must not be.
	writeFile(t, filepath.Join(second, "venv", "bin", "phenix-app-bravo"), 0o755)

	if err := os.Symlink(filepath.Join(second, "venv", "bin", "phenix-app-bravo"), filepath.Join(second, "phenix-app-bravo")); err != nil {
		t.Fatal(err)
	}

	if err := os.Symlink(filepath.Join(second, "gone"), filepath.Join(second, "phenix-app-dangling")); err != nil {
		t.Fatal(err)
	}

	// The same name later in PATH is shadowed, as it is when the command runs.
	writeFile(t, filepath.Join(second, "phenix-app-alpha"), 0o755)

	// A PATH entry that does not exist (sudo's secure_path lists /snap/bin on
	// hosts without snapd) must not hide the commands in the other entries.
	t.Setenv("PATH", strings.Join([]string{missing, first, "", second}, string(os.PathListSeparator)))

	got := shell.FindCommandsWithPrefix("phenix-app-")
	slices.Sort(got)

	want := []string{"alpha", "bravo"}
	if !slices.Equal(got, want) {
		t.Fatalf("FindCommandsWithPrefix() = %v, want %v", got, want)
	}
}

func TestFindCommandsWithPrefixEmptyPath(t *testing.T) {
	t.Setenv("PATH", "")

	if got := shell.FindCommandsWithPrefix("phenix-app-"); len(got) != 0 {
		t.Fatalf("FindCommandsWithPrefix() with empty PATH = %v, want none", got)
	}
}
