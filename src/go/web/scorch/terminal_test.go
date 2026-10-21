package scorch

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestKillTerminalOnce is a regression test: KillTerminal closed the
// terminal's Done channel unguarded, so the watcher of the terminal's context
// and an ExitTerminal request, or two ExitTerminal requests, both killing it
// panicked phenix ui with "close of closed channel".
func TestKillTerminalOnce(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if _, err := CreateWebTerminal(ctx, "term-exp", 0, 0, "stop", "brk", t.TempDir(), "/bin/sleep", []string{"30"}); err != nil {
		t.Fatal(err)
	}

	terms, _ := GetExperimentTerminals("term-exp", -1)
	if len(terms) != 1 {
		t.Fatalf("terminals of term-exp = %v, want one", terms)
	}

	// Two ExitTerminal requests that both found the terminal.
	first, err := GetTerminalByPID(terms[0].Pid)
	if err != nil {
		t.Fatal(err)
	}

	second := first

	if err := KillTerminal(first); err != nil {
		t.Fatalf("KillTerminal() = %v", err)
	}

	if err := KillTerminal(second); err != nil {
		t.Errorf("a second KillTerminal() = %v, want nil", err)
	}

	// The context watcher too.
	cancel()
	time.Sleep(100 * time.Millisecond)

	if _, err := GetTerminalByPID(first.Pid); !errors.Is(err, ErrTerminalNotFound) {
		t.Errorf("GetTerminalByPID() of the killed terminal = %v, want %v", err, ErrTerminalNotFound)
	}
}
