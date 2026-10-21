package vm_test

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/activeshadow/libminimega/minicli"

	"phenix/api/vm"
	"phenix/store"
	"phenix/util/mm"
)

// qmpMM is the mm.MM side of test-vm for the operations that send `vm qmp`;
// state answers each GetVMState in turn.
type qmpMM struct {
	snapshotMM

	state func() string
}

func (m qmpMM) GetVMState(...mm.Option) (string, error) {
	return m.state(), nil
}

func (qmpMM) GetVMCaptures(...mm.Option) []mm.Capture {
	return nil
}

// installQMPTest stores a running experiment holding test-vm, whose state
// GetVMState reports from states in turn, repeating the last one, and points
// mmcli at a fake minimega that answers with answer, or with an empty success.
func installQMPTest(t *testing.T, answer func(cmd string) minicli.Responses, states ...string) *fakeMinimega {
	t.Helper()

	originalStore, originalMM := store.DefaultStore, mm.DefaultMM

	t.Cleanup(func() {
		store.DefaultStore = originalStore //nolint:reassign // restore test double
		mm.DefaultMM = originalMM          //nolint:reassign // restore test double
	})

	getTestExperiment(t, "test-vm", nil, "2026-10-18T00:00:00Z")

	mm.DefaultMM = qmpMM{state: func() string { //nolint:reassign // install test double
		state := states[0]
		if len(states) > 1 {
			states = states[1:]
		}

		return state
	}}

	return useFakeMinimega(t, func(cmd string) minicli.Responses {
		if answer != nil {
			if resps := answer(cmd); resps != nil {
				return resps
			}
		}

		return minicli.Responses{{Host: "head"}}
	})
}

// registerQMPPatterns registers the patterns of mmcli's `.record` prefix
// (minicli builtins.go) and of minimega's `namespace` and `vm qmp`
// (cmd/minimega/namespace_cli.go, vm_cli.go) with minicli for the rest of the
// test.
func registerQMPPatterns(t *testing.T) {
	t.Helper()

	minicli.Reset()
	t.Cleanup(minicli.Reset)

	noop := func(*minicli.Command, chan<- minicli.Responses) {}

	for _, pattern := range []string{
		".record <true,false> (command)",
		"namespace <name> (command)",
		"vm qmp <vm name> <qmp command>",
	} {
		minicli.MustRegister(&minicli.Handler{Patterns: []string{pattern}, Call: noop})
	}
}

// minimegaQMP returns the QMP argument of raw, a `vm qmp` command as mmcli
// sent it, as phenix wrote it and as minimega's vm qmp handler reads it.
// minimega compiles raw, then compiles the innermost command again from the
// Original that minicli rebuilt from its parsed arguments, which quotes only
// the arguments holding whitespace, before it runs the command on each host of
// the namespace (namespaceCommands in cmd/minimega/cli.go).
func minimegaQMP(raw string) (string, string, error) {
	cmd, err := minicli.Compile(raw)
	if err != nil {
		return "", "", fmt.Errorf("compiling %q: %w", raw, err)
	}

	for cmd.Subcommand != nil {
		cmd = cmd.Subcommand
	}

	again, err := minicli.Compile(cmd.Original)
	if err != nil {
		return "", "", fmt.Errorf("compiling the rebuilt %q: %w", cmd.Original, err)
	}

	return cmd.StringArgs["qmp"], again.StringArgs["qmp"], nil
}

// TestVMQMPCommandsSurviveMinimegaRebuild is a regression test: phenix sent
// `vm qmp <vm> '{"execute":"migrate_cancel"}'`, which minimega rebuilt as
// `vm qmp <vm> {"execute":"migrate_cancel"}` and parsed again into the invalid
// JSON `{execute:migrate_cancel}`, so a VM paused by a failed snapshot was
// never resumed. Every `vm qmp` phenix sends must reach QEMU as valid JSON
// through both of minimega's compiles.
func TestVMQMPCommandsSurviveMinimegaRebuild(t *testing.T) {
	for _, tc := range []struct {
		name string
		want []string // the QMP commands the operation sends, in order
		run  func(t *testing.T) *fakeMinimega
	}{
		{
			name: "restart",
			want: []string{"system_reset"},
			run: func(t *testing.T) *fakeMinimega {
				t.Helper()

				fake := installQMPTest(t, nil, "RUNNING")

				if err := vm.Restart("test-experiment", "test-vm"); err != nil {
					t.Errorf("Restart() = %v", err)
				}

				return fake
			},
		},
		{
			name: "shutdown",
			want: []string{"system_powerdown"},
			run: func(t *testing.T) *fakeMinimega {
				t.Helper()

				fake := installQMPTest(t, nil, "RUNNING", "QUIT")

				if err := vm.Shutdown("test-experiment", "test-vm"); err != nil {
					t.Errorf("Shutdown() = %v", err)
				}

				return fake
			},
		},
		{
			name: "snapshot whose save does not complete in time",
			want: []string{"query-migrate", "migrate_cancel"},
			run: func(t *testing.T) *fakeMinimega {
				t.Helper()

				vm.SetVMSaveTimeout(t, 0)

				fake := installSnapshotTest(t, snapshotMinimega{status: qmpMigrate("active"), state: "PAUSED"})

				if err := vm.Snapshot("test-experiment", "test-vm", "s1", nil); err == nil {
					t.Error("Snapshot() = nil, want the error of the save that timed out")
				}

				return fake
			},
		},
		{
			name: "memory snapshot",
			want: []string{"dump-guest-memory", "query-dump"},
			run: func(t *testing.T) *fakeMinimega {
				t.Helper()

				fake := installQMPTest(t, func(cmd string) minicli.Responses {
					switch {
					case strings.HasSuffix(cmd, "vm info"):
						return mmTable([]string{"host", "name", "id", "state"}, []string{"head", "test-vm", "0", "RUNNING"})
					case strings.Contains(cmd, "query-dump"):
						return minicli.Responses{{
							Host:     "head",
							Response: `{"return": {"status": "completed", "completed": 512, "total": 512}}`,
						}}
					default:
						return nil
					}
				}, "RUNNING")

				if _, err := vm.MemorySnapshot("test-experiment", "test-vm", "", func(string) {}); err != nil {
					t.Errorf("MemorySnapshot() = %v", err)
				}

				return fake
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := tc.run(t)

			registerQMPPatterns(t)

			var executes []string

			for _, raw := range fake.sentRaw() {
				if !strings.Contains(raw, " vm qmp ") {
					continue
				}

				sent, got, err := minimegaQMP(raw)
				if err != nil {
					t.Errorf("minimega cannot run %q: %v", raw, err)

					continue
				}

				var qmp struct {
					Execute string `json:"execute"`
				}

				if err := json.Unmarshal([]byte(got), &qmp); err != nil || got != sent {
					t.Errorf("minimega read the QMP command of %q as %q (%v), want %q", raw, got, err, sent)

					continue
				}

				executes = append(executes, qmp.Execute)
			}

			if executes = slices.Compact(executes); !slices.Equal(executes, tc.want) {
				t.Errorf("QEMU got the QMP commands %q, want %q; minimega got %q", executes, tc.want, fake.sentRaw())
			}
		})
	}
}
