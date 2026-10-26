//nolint:testpackage // testing internals
package scorch

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"phenix/api/scorch/scorchmd"
)

// TestStartFilebeatHonorsRunLevelSetting is a regression test: startFilebeat
// checked only the global filebeat.enabled flag, so a run that enabled
// Filebeat in its own filebeat setting got a Filebeat config written but
// Filebeat was never started.
func TestStartFilebeatHonorsRunLevelSetting(t *testing.T) {
	dir := t.TempDir()
	record := filepath.Join(dir, "filebeat.args")

	// A stand-in for filebeat that records its arguments and keeps running.
	script := "#!/bin/sh\necho \"$@\" > " + record + "\nexec sleep 60\n"

	//nolint:gosec // test script must be executable
	if err := os.WriteFile(filepath.Join(dir, "filebeat"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	enabled := &scorchmd.FilebeatSpec{Enabled: true}
	disabled := &scorchmd.FilebeatSpec{Enabled: false}

	tests := []struct {
		name   string
		global bool
		run    *scorchmd.FilebeatSpec
		start  bool
	}{
		{name: "run-level enabled", global: false, run: enabled, start: true},
		{name: "global enabled", global: true, run: nil, start: true},
		{name: "run-level disabled", global: true, run: disabled, start: false},
		{name: "disabled", global: false, run: nil, start: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_ = os.Remove(record)

			s := Scorch{
				md: scorchmd.ScorchMetadata{
					Filebeat: scorchmd.FilebeatSpec{Enabled: tc.global},
					Runs:     []*scorchmd.Loop{{Filebeat: tc.run}},
				},
			}

			runDir := t.TempDir()

			cmd, _, err := s.startFilebeat(context.Background(), 0, runDir)
			if err != nil {
				t.Fatalf("starting Filebeat: %v", err)
			}

			if cmd != nil {
				t.Cleanup(func() {
					_ = cmd.Process.Kill()
					_ = cmd.Wait()
				})
			}

			if started := cmd != nil; started != tc.start {
				t.Fatalf("Filebeat started = %t, want %t", started, tc.start)
			}

			if !tc.start {
				return
			}

			args, err := os.ReadFile(record) //nolint:gosec // test file
			if err != nil {
				t.Fatalf("reading Filebeat arguments: %v", err)
			}

			want := filepath.Join(runDir, "filebeat", "filebeat.yml")
			if !strings.Contains(string(args), "-c "+want) {
				t.Fatalf("Filebeat arguments = %q, want the config %s", args, want)
			}
		})
	}
}

// TestFilebeatOutlivesCancel checks that Filebeat keeps running after the
// run's context is canceled, so the stop and cleanup stages, which run after a
// cancel, can still have their output shipped. The end of the run's teardown
// budget interrupts it, and stopFilebeat reaps it.
func TestFilebeatOutlivesCancel(t *testing.T) {
	original := teardownTimeout
	t.Cleanup(func() { teardownTimeout = original })

	teardownTimeout = time.Second

	dir := t.TempDir()
	record := filepath.Join(dir, "filebeat.signals")

	script := "#!/bin/sh\n" +
		"trap 'echo INT >> " + record + "; exit 0' INT\n" +
		"trap 'echo TERM >> " + record + "; exit 0' TERM\n" +
		"i=0; while [ $i -lt 400 ]; do sleep 0.05; i=$((i+1)); done\n"

	//nolint:gosec // test script must be executable
	if err := os.WriteFile(filepath.Join(dir, "filebeat"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	s := Scorch{md: scorchmd.ScorchMetadata{
		Filebeat: scorchmd.FilebeatSpec{Enabled: true},
		Runs:     []*scorchmd.Loop{{}},
	}}

	pipeline, cancel := context.WithCancel(context.Background())
	ctx, release := withTeardown(pipeline)

	t.Cleanup(release)

	cmd, port, err := s.startFilebeat(teardownContext(ctx), 0, t.TempDir())
	if err != nil || cmd == nil {
		t.Fatalf("startFilebeat() = %v, %v", cmd, err)
	}

	t.Cleanup(func() { _ = cmd.Process.Kill() })

	state := func() string {
		stat, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(cmd.Process.Pid), "stat"))
		if err != nil {
			return "gone"
		}

		// The state follows the parenthesized command name.
		if fields := strings.Fields(string(stat[bytes.LastIndexByte(stat, ')')+1:])); len(fields) > 0 {
			return fields[0]
		}

		return "unknown"
	}

	cancel()
	time.Sleep(300 * time.Millisecond)

	if got, _ := os.ReadFile(record); len(got) != 0 || state() == "Z" { //nolint:gosec // test file
		t.Fatalf("right after the cancel, Filebeat got %q and is in state %s, want it still running", got, state())
	}

	start := time.Now()
	s.stopFilebeat(teardownContext(ctx), cmd, port)

	if waited := time.Since(start); waited > 5*time.Second {
		t.Errorf("stopFilebeat() took %v once the teardown budget had run out", waited)
	}

	if got, _ := os.ReadFile(record); strings.TrimSpace(string(got)) != "INT" { //nolint:gosec // test file
		t.Errorf("Filebeat got the signals %q, want an interrupt", got)
	}

	if st := state(); st != "gone" {
		t.Errorf("after stopFilebeat(), Filebeat is in state %s, want it reaped", st)
	}
}
