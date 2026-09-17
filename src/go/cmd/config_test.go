package cmd

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestConfigFiles(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "nested")

	if err := os.Mkdir(nested, 0o700); err != nil {
		t.Fatalf("creating nested directory: %v", err)
	}

	expected := []string{
		filepath.Join(root, "config.json"),
		filepath.Join(root, "config.yaml"),
		filepath.Join(nested, "config.yml"),
	}

	for _, path := range append(expected, filepath.Join(root, "ignored.txt")) {
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatalf("writing test file: %v", err)
		}
	}

	got, err := configFiles(root)
	if err != nil {
		t.Fatalf("discovering config files: %v", err)
	}

	if !reflect.DeepEqual(got, expected) {
		t.Fatalf("configFiles() = %#v, want %#v", got, expected)
	}
}

func TestConfigFilesMissingPath(t *testing.T) {
	if _, err := configFiles(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("expected missing path error")
	}
}

func TestConfigFilesRejectsUnsupportedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.txt")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("writing test file: %v", err)
	}

	if _, err := configFiles(path); err == nil {
		t.Fatal("expected unsupported file error")
	}
}

func TestConfigUpdateCommand(t *testing.T) {
	cmd := newConfigUpdateCmd()

	if cmd.Use != "update </path/to/filename> ..." {
		t.Fatalf("unexpected use string: %q", cmd.Use)
	}

	if !strings.Contains(cmd.Long, "kind and metadata.name") {
		t.Fatalf("long help does not explain update identity:\n%s", cmd.Long)
	}

	if err := cmd.RunE(cmd, nil); err == nil {
		t.Fatal("expected missing file argument error")
	}
}
