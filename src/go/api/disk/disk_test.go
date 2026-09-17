package disk

import (
	"path/filepath"
	"testing"

	"phenix/util/mm"
	"phenix/util/mm/mmcli"
)

func TestGetAllFilesRecursively(t *testing.T) {
	runTabular := func(cmd *mmcli.Command) []map[string]string {
		if cmd.Command != "file list / recursive" {
			t.Fatalf("command = %q, want %q", cmd.Command, "file list / recursive")
		}

		return []map[string]string{
			{"dir": "", "name": "root.qcow2"},
			{"dir": "", "name": "linux/server.qcow2"},
			{"dir": "", "name": "linux/releases/server.qcow2"},
			{"dir": "<dir>", "name": "linux"},
		}
	}

	resolve := func(path string) []Details {
		return []Details{{
			Name:     filepath.Base(path),
			FullPath: path,
		}}
	}

	details := make(map[string]Details)
	getAllFilesWith(details, runTabular, resolve)

	expected := []string{
		mm.GetMMFullPath("root.qcow2"),
		mm.GetMMFullPath("linux/server.qcow2"),
		mm.GetMMFullPath("linux/releases/server.qcow2"),
	}

	if len(details) != len(expected) {
		t.Fatalf("got %d images, want %d", len(details), len(expected))
	}

	for _, path := range expected {
		if _, ok := details[path]; !ok {
			t.Errorf("missing image %q", path)
		}
	}
}

func TestAddImagePathUsesFullPathIdentity(t *testing.T) {
	resolve := func(path string) []Details {
		return []Details{{
			Name:     filepath.Base(path),
			FullPath: path,
		}}
	}

	details := make(map[string]Details)
	addImagePathWith(details, "linux/base.qcow2", resolve)
	addImagePathWith(details, "windows/base.qcow2", resolve)
	addImagePathWith(details, "linux/base.qcow2", resolve)
	addImagePathWith(details, "/external/base.qcow2", resolve)

	if len(details) != 3 {
		t.Fatalf("got %d images, want 3", len(details))
	}

	for _, path := range []string{
		mm.GetMMFullPath("linux/base.qcow2"),
		mm.GetMMFullPath("windows/base.qcow2"),
		"/external/base.qcow2",
	} {
		if _, ok := details[path]; !ok {
			t.Errorf("missing image %q", path)
		}
	}
}

func TestDisplayImagePath(t *testing.T) {
	tests := map[string]struct {
		path string
		want string
	}{
		"root image": {
			path: mm.GetMMFullPath("base.qcow2"),
			want: "base.qcow2",
		},
		"nested image": {
			path: mm.GetMMFullPath("linux/releases/base.qcow2"),
			want: filepath.Join("linux", "releases", "base.qcow2"),
		},
		"external image": {
			path: "/external/base.qcow2",
			want: "/external/base.qcow2",
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			if got := displayImagePath(test.path); got != test.want {
				t.Errorf("displayImagePath(%q) = %q, want %q", test.path, got, test.want)
			}
		})
	}
}
