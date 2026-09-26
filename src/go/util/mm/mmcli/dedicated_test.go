package mmcli

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/activeshadow/libminimega/miniclient"
)

// TestRunDedicatedDoesNotWaitForSharedConnection covers long-running commands
// (an experiment's script, launching its VMs) no longer holding the shared
// connection, and so every other command, for their whole duration.
func TestRunDedicatedDoesNotWaitForSharedConnection(t *testing.T) {
	release := make(chan struct{})

	dir := newFakeMinimega(t, func(cmd string) (*miniclient.Response, fakeAction) {
		if strings.Contains(cmd, "hold") {
			<-release
		}

		return reply("ok"), actionReply
	})

	useFakeMinimega(t, dir)

	held := make(chan error, 1)

	go func() {
		cmd := NewCommand()
		cmd.Command = "hold"

		held <- ErrorResponse(Run(cmd))
	}()

	// Let the holding command take the shared connection first.
	time.Sleep(testTimeout / 2)

	done := make(chan string, 1)

	go func() {
		cmd := NewCommand()
		cmd.Command = "vm launch"

		got, _ := SingleResponse(RunDedicated(cmd))

		done <- got
	}()

	select {
	case got := <-done:
		if got != "ok" {
			t.Fatalf("dedicated command got %q, want %q", got, "ok")
		}
	case <-time.After(maxWait):
		t.Fatal("dedicated command waited for the shared connection")
	}

	close(release)

	if err := <-held; err != nil {
		t.Fatalf("holding command failed: %v", err)
	}
}

// TestRunStartedNotifiesBeforeSending covers the hook that lets a caller know
// its command is no longer queued behind others, on both connection types.
func TestRunStartedNotifiesBeforeSending(t *testing.T) {
	var (
		mu   sync.Mutex
		sent []string
	)

	started := make(chan struct{}, 2)

	dir := newFakeMinimega(t, func(cmd string) (*miniclient.Response, fakeAction) {
		select {
		case <-started:
			mu.Lock()
			sent = append(sent, cmd)
			mu.Unlock()
		default:
			t.Errorf("command %q sent before started was called", cmd)
		}

		return reply("ok"), actionReply
	})

	useFakeMinimega(t, dir)

	notify := func() { started <- struct{}{} }

	shared := NewCommand()
	shared.Command = "shared"

	if rows := RunTabularStarted(shared, notify); len(rows) != 0 {
		t.Fatalf("unexpected rows %v", rows)
	}

	private := NewCommand()
	private.Command = "private"
	private.Timeout = maxWait

	if err := ErrorResponse(RunStarted(private, notify)); err != nil {
		t.Fatalf("private command failed: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	if len(sent) != 2 {
		t.Fatalf("sent %v, want both commands", sent)
	}
}
