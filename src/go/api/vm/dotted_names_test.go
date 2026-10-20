package vm_test

import (
	"path"
	"slices"
	"strings"
	"testing"

	"github.com/activeshadow/libminimega/minicli"

	"phenix/api/vm"
	"phenix/store"
	"phenix/util/file"
	"phenix/util/mm"
)

// TestSnapshotNamesKeepTheirDots is a regression test: snapshot and memory
// dump names may hold dots, but Snapshot, Restore and MemorySnapshot dropped
// the last dotted part of any name, so a snapshot v1.2.3 was saved as v1.2
// and could not be restored, and the dumps run.1 and run.2 both overwrote
// run.elf.
func TestSnapshotNamesKeepTheirDots(t *testing.T) {
	const files = "/srv/minimega/files"

	vm.SetMMFullPath(t, func(p string) string { return path.Join(files, p) })

	t.Run("snapshot", func(t *testing.T) {
		for name, want := range map[string]string{"v1.2.3": "test-vm__v1.2.3", "s1.state": "test-vm__s1"} {
			fake := installSnapshotTest(t, snapshotMinimega{status: qmpMigrate("completed")})

			if err := vm.Snapshot("test-experiment", "test-vm", name, nil); err != nil {
				t.Fatalf("Snapshot(%q) = %v", name, err)
			}

			move := "shell mv " + files + "/test-experiment_" + want + ".hdd " + files + "/test-experiment/files/" + want + ".hdd"
			if !slices.ContainsFunc(fake.sent(), func(cmd string) bool { return strings.TrimSpace(cmd) == move }) {
				t.Errorf("Snapshot(%q) sent %q, want %q", name, fake.sent(), move)
			}
		}
	})

	t.Run("restore", func(t *testing.T) {
		originalStore, originalMM, originalFiles := store.DefaultStore, mm.DefaultMM, file.DefaultClusterFiles

		t.Cleanup(func() {
			store.DefaultStore = originalStore       //nolint:reassign // restore test double
			mm.DefaultMM = originalMM                //nolint:reassign // restore test double
			file.DefaultClusterFiles = originalFiles //nolint:reassign // restore test double
		})

		getTestExperiment(t, "test-vm", nil, "2026-10-19T00:00:00Z")

		mm.DefaultMM = snapshotMM{}                                                      //nolint:reassign // install test double
		file.DefaultClusterFiles = snapshotFiles{snapshots: []string{"test-vm__v1.2.3"}} //nolint:reassign // install test double

		for _, snap := range []string{"test-vm__v1.2.3", "test-vm__v1.2.3.state"} {
			fake := useFakeMinimega(t, func(string) minicli.Responses { return minicli.Responses{{Host: "head"}} })

			if err := vm.Restore("test-experiment", "test-vm", snap); err != nil {
				t.Errorf("Restore(%q) = %v", snap, err)
			}

			want := "vm config state test-experiment/files/test-vm__v1.2.3.state"
			if !slices.ContainsFunc(fake.sent(), func(cmd string) bool { return strings.HasSuffix(cmd, want) }) {
				t.Errorf("Restore(%q) sent %q, want %q", snap, fake.sent(), want)
			}
		}
	})

	t.Run("memory snapshot", func(t *testing.T) {
		for name, want := range map[string]string{"run.1": "run.1.elf", "run.elf": "run.elf"} {
			fake := installQMPTest(t, func(cmd string) minicli.Responses {
				switch {
				case strings.HasSuffix(cmd, "vm info"):
					return mmTable([]string{"host", "name", "id", "state"}, []string{"head", "test-vm", "0", "RUNNING"})
				case strings.Contains(cmd, "query-dump"):
					done := `{"return": {"status": "completed", "completed": 512, "total": 512}}`

					return minicli.Responses{{Host: "head", Response: done}}
				default:
					return nil
				}
			}, "RUNNING")

			if _, err := vm.MemorySnapshot("test-experiment", "test-vm", name, nil); err != nil {
				t.Fatalf("MemorySnapshot(%q) = %v", name, err)
			}

			if !slices.ContainsFunc(fake.sent(), func(cmd string) bool { return strings.Contains(cmd, "/files/"+want) }) {
				t.Errorf("MemorySnapshot(%q) sent %q, want a dump to %s", name, fake.sent(), want)
			}
		}
	})
}
