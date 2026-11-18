package shell_test

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"phenix/util/shell"
)

func TestExecCommandCopiesStreamedBytes(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	tests := []struct {
		name      string
		script    string
		streamOpt func(chan []byte) shell.Option
		want      []string
	}{
		{
			name:      "stdout",
			script:    "printf 'alpha\nbravo\ncharl\n'",
			streamOpt: shell.StreamStdout,
			want:      []string{"alpha", "bravo", "charl"},
		},
		{
			name:      "stderr",
			script:    "printf 'alpha\nbravo\ncharl\n' >&2",
			streamOpt: shell.StreamStderr,
			want:      []string{"alpha", "bravo", "charl"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stream := make(chan []byte, len(tt.want))

			_, _, err := shell.ExecCommand(
				context.Background(),
				shell.Command("sh"),
				shell.Args("-c", tt.script),
				tt.streamOpt(stream),
			)
			if err != nil {
				t.Fatalf("ExecCommand returned error: %v", err)
			}

			var got [][]byte
			for line := range stream {
				got = append(got, line)
			}

			if len(got) != len(tt.want) {
				t.Fatalf("expected %d lines, got %d", len(tt.want), len(got))
			}

			for i, want := range tt.want {
				if string(got[i]) != want {
					t.Fatalf("line %d: expected %q, got %q", i, want, string(got[i]))
				}
			}
		})
	}
}

// TestExecCommandTerminatesGracefullyOnCancel is a regression test: canceling
// the context SIGKILLed the child at once, because [exec.CommandContext] kills
// on cancel and raced the SIGTERM meant to let it clean up.
func TestExecCommandTerminatesGracefullyOnCancel(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	ctx, cancel := context.WithCancel(context.Background())

	stdout := make(chan []byte, 8)

	go func() {
		// Cancel once the child reports it installed its trap.
		<-stdout
		cancel()
	}()

	out, _, _ := shell.ExecCommand(
		ctx,
		shell.Command("sh"),
		shell.Args("-c", `trap 'echo terminated; exit 0' TERM; echo ready; while :; do sleep 0.05; done`),
		shell.StreamStdout(stdout),
	)

	if got := string(out); got != "readyterminated" {
		t.Fatalf("stdout = %q, want the child's TERM handler to run (readyterminated)", got)
	}
}

// TestExecCommandKilledAfterDelayReportsBothStreams is a regression test,
// meant for the race detector: when a canceled child ignored SIGTERM and did
// not read its stdin, os/exec closed both output pipes together once the kill
// delay passed, and both scanner goroutines appended their error to the same
// variable unsynchronized, a data race that could also lose one error.
func TestExecCommandKilledAfterDelayReportsBothStreams(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	shell.SetKillDelay(t, 100*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, _, err := shell.ExecCommand(
		ctx,
		shell.Command("sh"),
		shell.Args("-c", `trap '' TERM; sleep 5`),
		shell.Stdin(make([]byte, 1<<20)), // more than the pipe holds, never read
	)
	if err == nil {
		t.Fatal("ExecCommand of a killed command returned no error")
	}
}

// TestExecCommandCancelEndsTheChildsProcesses is a regression test: a cancel
// signaled only the command's own process, so a process it started, such as
// the foreground command of a shell wrapper, outlived it, held its output
// pipes open and kept ExecCommand, and so a SCORCH run's teardown, from
// returning until that process ended on its own.
func TestExecCommandCancelEndsTheChildsProcesses(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	shell.SetKillDelay(t, 200*time.Millisecond)
	shell.SetCloseDelay(t, 200*time.Millisecond)

	tests := []struct {
		name   string
		script string
	}{
		// SIGTERM ends both the wrapper and its sleep.
		{name: "wrapper", script: `sleep 30; echo done`},
		// The ignored SIGTERM is inherited, so only the SIGKILL of the
		// group ends them.
		{name: "wrapper ignoring SIGTERM", script: `trap '' TERM; sleep 30; echo done`},
	}

	if _, err := exec.LookPath("setsid"); err == nil {
		// A process that left the group still holds the pipes, which
		// ExecCommand closes.
		tests = append(tests, struct {
			name   string
			script string
		}{name: "process outside the group", script: `setsid sleep 5 & sleep 30`})
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()

			began := time.Now()

			_, _, err := shell.ExecCommand(ctx, shell.Command("sh"), shell.Args("-c", tt.script))
			if err == nil {
				t.Error("ExecCommand of a canceled command returned no error")
			}

			if took := time.Since(began); took > 3*time.Second {
				t.Errorf("ExecCommand returned %v after the cancel, want it bounded by the kill and close delays", took)
			}
		})
	}
}

// TestExecCommandFailedStartClosesStreams is a regression test: a command
// that failed to start left the stream channels open, so the goroutines that
// read them until they were closed, as a SCORCH user component starts, leaked.
func TestExecCommandFailedStartClosesStreams(t *testing.T) {
	stdout, stderr := make(chan []byte), make(chan []byte)

	_, _, err := shell.ExecCommand(
		context.Background(),
		shell.Command(filepath.Join(t.TempDir(), "missing")),
		shell.StreamStdout(stdout),
		shell.StreamStderr(stderr),
	)
	if err == nil {
		t.Fatal("ExecCommand of a missing command returned no error")
	}

	for name, stream := range map[string]chan []byte{"stdout": stdout, "stderr": stderr} {
		select {
		case _, ok := <-stream:
			if ok {
				t.Errorf("the %s stream got output from a command that never started", name)
			}
		case <-time.After(time.Second):
			t.Errorf("the %s stream is still open after the failed start", name)
		}
	}
}
