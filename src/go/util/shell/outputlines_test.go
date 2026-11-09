package shell

import (
	"bufio"
	"context"
	"strings"
	"testing"
	"time"
)

// longLine is a shell command that prints a 300000-byte line of c.
func longLine(c string) string {
	return "head -c 300000 /dev/zero | tr '\\0' " + c + "; echo"
}

// execWithin runs script with sh and fails the test when ExecCommand has
// not returned a minute later: a command blocked on a full pipe never ends.
func execWithin(t *testing.T, script string) ([]byte, []byte, error) {
	t.Helper()

	type result struct {
		stdout, stderr []byte
		err            error
	}

	done := make(chan result, 1)

	go func() {
		stdout, stderr, err := ExecCommand(context.Background(), Command("sh"), Args("-c", script), Stdin(nil))
		done <- result{stdout, stderr, err}
	}()

	select {
	case r := <-done:
		return r.stdout, r.stderr, r.err
	case <-time.After(time.Minute):
		t.Fatal("ExecCommand() has not returned after a minute")
	}

	return nil, nil, nil
}

// TestExecCommandReadsLongLines is a regression test: the output scanners
// kept bufio's 64 KiB line limit, so a scheduler that printed a large
// experiment as one line failed with "token too long", or, once the line
// filled the pipe, blocked writing and ExecCommand never returned.
func TestExecCommandReadsLongLines(t *testing.T) {
	for _, stream := range []string{"stdout", "stderr"} {
		t.Run(stream, func(t *testing.T) {
			script := longLine("a")
			if stream == "stderr" {
				script = "{ " + script + "; } >&2"
			}

			stdout, stderr, err := execWithin(t, script)
			if err != nil {
				t.Fatalf("ExecCommand() = %v", err)
			}

			got := stdout
			if stream == "stderr" {
				got = stderr
			}

			if len(got) != 300000 || strings.Trim(string(got), "a") != "" {
				t.Errorf("ExecCommand() read %d bytes of %s, want the 300000-byte line", len(got), stream)
			}
		})
	}
}

// TestExecCommandReportsOverlongLines checks that a line beyond
// maxOutputLine fails the command instead of blocking it on a full pipe.
func TestExecCommandReportsOverlongLines(t *testing.T) {
	original := maxOutputLine
	t.Cleanup(func() { maxOutputLine = original })

	maxOutputLine = 1024

	_, _, err := execWithin(t, longLine("a")+"; "+longLine("b"))
	if err == nil || !strings.Contains(err.Error(), bufio.ErrTooLong.Error()) {
		t.Fatalf("ExecCommand() = %v, want %q", err, bufio.ErrTooLong)
	}
}
