package vm_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/activeshadow/libminimega/minicli"

	"phenix/api/vm"
	"phenix/util/common"
	"phenix/util/mm"
)

// TestMemorySnapshotRefusesUnsafeName checks that a memory snapshot name is a
// plain file name: an absolute path made QEMU, as root, write the dump to any
// *.elf file on the VM's host, and the name also reached `shell mkdir -p`.
func TestMemorySnapshotRefusesUnsafeName(t *testing.T) {
	for _, name := range []string{
		"/root/.ssh/authorized_keys",
		"../../x",
		"a b",
		`a"b`,
		"-p",
	} {
		t.Run(name, func(t *testing.T) {
			fake := installQMPTest(t, nil, "RUNNING")

			_, err := vm.MemorySnapshot("test-experiment", "test-vm", name, func(string) {})
			if !errors.Is(err, mm.ErrInvalidFileName) {
				t.Fatalf("MemorySnapshot(%q) = %v, want mm.ErrInvalidFileName", name, err)
			}

			if sent := fake.sent(); len(sent) != 0 {
				t.Errorf("MemorySnapshot(%q) sent %q to minimega", name, sent)
			}
		})
	}
}

// TestMemorySnapshotRemovesFailedDump is a regression test: a dump that
// failed left its partial ELF in the experiment's files, and an answer
// without a status, such as a QMP error after an active one, left the
// previous status in place, so the dump was polled on forever.
func TestMemorySnapshotRemovesFailedDump(t *testing.T) {
	const active = `{"return": {"status": "active", "completed": 128, "total": 512}}`

	elf := common.PhenixBase + "/images/test-experiment/files/mem1.elf"

	for _, tc := range []struct {
		name    string
		answers []string // the answers to query-dump in turn
		result  string
		wantErr string
	}{
		{"the dump fails", []string{active, `{"return": {"status": "failed", "completed": 128, "total": 512}}`}, "failed", "failed to create memory snapshot"},
		{"QEMU answers without a status", []string{active, `{"error": {"class": "GenericError"}}`}, "", "no status available"},
		{"QEMU answers garbage", []string{"not json"}, "", `QEMU answered "not json"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			answers := tc.answers

			fake := installQMPTest(t, func(cmd string) minicli.Responses {
				switch {
				case strings.HasSuffix(cmd, "vm info"):
					return mmTable([]string{"host", "name", "id", "state"}, []string{"head", "test-vm", "0", "RUNNING"})
				case strings.Contains(cmd, "query-dump"):
					answer := answers[0]
					if len(answers) > 1 {
						answers = answers[1:]
					}

					return minicli.Responses{{Host: "head", Response: answer}}
				default:
					return nil
				}
			}, "RUNNING")

			var progress []string

			result, err := vm.MemorySnapshot("test-experiment", "test-vm", "mem1", func(p string) { progress = append(progress, p) })
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) || result != tc.result {
				t.Fatalf("MemorySnapshot() = %q, %v, want %q and an error saying %q", result, err, tc.result, tc.wantErr)
			}

			if sent := fake.sent(); !slices.Contains(sent, " shell rm -f "+elf) {
				t.Errorf("MemorySnapshot() did not remove %s; minimega got %q", elf, sent)
			}

			if len(progress) == 0 || progress[len(progress)-1] != "failed" {
				t.Errorf("MemorySnapshot() reported %q, want it to end with failed", progress)
			}
		})
	}
}
