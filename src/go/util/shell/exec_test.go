package shell_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"phenix/util/shell"
	"phenix/util/testenv"
)

func TestExecCommandCopiesStreamedBytes(t *testing.T) {
	testenv.RequireCommands(t, "sh")

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

// Canceling the context sends SIGTERM to the command, so that it can stop
// gracefully, and ExecCommand returns the context error.
func TestExecCommandCancelSendsSIGTERM(t *testing.T) {
	testenv.RequireCommands(t, "sh", "sleep")

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	type result struct {
		stdout []byte
		err    error
	}

	stream := make(chan []byte, 4)
	done := make(chan result, 1)

	go func() {
		stdout, _, err := shell.ExecCommand(
			ctx,
			shell.Command("sh"),
			shell.Args("-c", "trap 'echo stopped; exit 0' TERM; echo ready; while :; do sleep 0.1; done"),
			shell.StreamStdout(stream),
		)

		done <- result{stdout: stdout, err: err}
	}()

	select {
	case line := <-stream:
		if string(line) != "ready" {
			t.Fatalf("first streamed line = %q, want ready", line)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("command streamed nothing for 10s, want ready")
	}

	cancel()

	select {
	case got := <-done:
		if string(got.stdout) != "readystopped" {
			t.Errorf("ExecCommand() stdout = %q, want readystopped from the SIGTERM trap", got.stdout)
		}

		if !errors.Is(got.err, context.Canceled) {
			t.Errorf("ExecCommand() error = %v, want one that wraps context.Canceled", got.err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("ExecCommand() did not return 8s after cancel, want it to return after SIGTERM")
	}
}
