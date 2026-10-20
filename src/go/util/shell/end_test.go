package shell_test

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"testing"
	"time"

	"phenix/util/shell"
)

// execUntilReady runs script with ExecCommand in the background, and returns
// once the script printed its first line, with a channel that gets
// ExecCommand's error.
func execUntilReady(t *testing.T, ctx context.Context, script string) <-chan error {
	t.Helper()

	stdout := make(chan []byte, 8)
	done := make(chan error, 1)

	go func() {
		_, _, err := shell.ExecCommand(ctx, shell.Command("sh"), shell.Args("-c", script), shell.StreamStdout(stdout))
		done <- err
	}()

	select {
	case _, ok := <-stdout:
		if !ok {
			t.Fatalf("the command ended before it printed a line: %v", <-done)
		}

		// Drain the rest, so the command never blocks on its output.
		go func() {
			for range stdout { //nolint:revive // draining
			}
		}()

		return done
	case <-time.After(10 * time.Second):
		t.Fatal("the command printed nothing")
	}

	return nil
}

// waitReturned fails the test unless done gets ExecCommand's error, which
// must report the ended command, within 5 seconds.
func waitReturned(t *testing.T, done <-chan error) {
	t.Helper()

	select {
	case err := <-done:
		if err == nil {
			t.Error("ExecCommand of an ended command returned no error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ExecCommand did not return once its process group was signaled")
	}
}

// TestEndGroupsPassesTheSignalOn is a regression test: once a command ran in a
// process group of its own, the signal of a terminal that ended phenix, such
// as a second Ctrl-C or a hangup, no longer reached it, so the command and the
// processes it started kept running after phenix ended.
func TestEndGroupsPassesTheSignalOn(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	done := execUntilReady(t, context.Background(), `echo ready; sleep 30`)

	shell.EndGroups(syscall.SIGTERM)

	waitReturned(t, done)
}

// TestEndGroupsKillsACanceledGroup checks that the group of a canceled
// command, which the cancel's SIGTERM did not end, is killed when phenix ends
// before the kill delay passes.
func TestEndGroupsKillsACanceledGroup(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	shell.SetKillDelay(t, time.Minute)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := execUntilReady(t, ctx, `trap '' TERM INT; echo ready; sleep 30`)

	cancel()
	shell.WaitCanceled(t)

	// SIGINT, which the command ignores, as the cancel's SIGTERM.
	shell.EndGroups(syscall.SIGINT)

	waitReturned(t, done)
}

// processGone reports whether the process pid has ended: it is gone, or it is
// a zombie that its new parent has not reaped yet.
func processGone(pid int) bool {
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return true
	}

	// The state follows the command name in parentheses.
	i := bytes.LastIndexByte(stat, ')')

	return i >= 0 && i+2 < len(stat) && stat[i+2] == 'Z'
}

// TestSignalEndingPhenixEndsCommands checks, in a helper process, that a
// signal that ends the process ends the commands ExecCommand runs too.
func TestSignalEndingPhenixEndsCommands(t *testing.T) {
	if os.Getenv("SHELL_END_TEST_HELPER") == "1" {
		stdout := make(chan []byte, 1)

		go func() {
			for line := range stdout {
				fmt.Println(string(line)) //nolint:forbidigo // read by the parent test
			}
		}()

		_, _, _ = shell.ExecCommand(
			context.Background(),
			shell.Command("sh"),
			shell.Args("-c", `echo $$; sleep 30`),
			shell.StreamStdout(stdout),
		)

		// Ending sh makes ExecCommand return before the signal handler sends
		// SIGTERM again to end the process, so exiting at once would race that
		// signal. Wait for it, as phenix does; a signal that never comes still
		// ends the helper, with a status the parent test reports.
		time.Sleep(30 * time.Second)
		os.Exit(0)
	}

	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skip("/proc not available")
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestSignalEndingPhenixEndsCommands$")
	cmd.Env = append(os.Environ(), "SHELL_END_TEST_HELPER=1")

	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}

	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = cmd.Process.Kill() })

	lines := bufio.NewScanner(out)
	if !lines.Scan() {
		t.Fatal("the helper process ended before its command printed its PID")
	}

	pid, err := strconv.Atoi(lines.Text())
	if err != nil {
		t.Fatalf("the helper's command printed %q, want its PID", lines.Text())
	}

	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}

	err = cmd.Wait()

	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("helper process after SIGTERM: %v, want it ended by the signal", err)
	}

	if status, ok := exit.Sys().(syscall.WaitStatus); !ok || !status.Signaled() || status.Signal() != syscall.SIGTERM {
		t.Errorf("helper process after SIGTERM: %v, want it ended by SIGTERM", err)
	}

	for deadline := time.Now().Add(5 * time.Second); !processGone(pid); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the helper's command, process %d, still runs after the helper ended", pid)
		}
	}
}
