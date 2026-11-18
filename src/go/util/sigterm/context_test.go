package sigterm

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"syscall"
	"testing"
	"time"
)

// TestSecondSignalEndsProcess is a regression test: CancelContext kept
// trapping SIGINT and SIGTERM after the first one canceled its context, so a
// command whose cleanup then hung, such as phenix experiment scorch with a
// long stop stage, could not be interrupted short of SIGKILL.
func TestSecondSignalEndsProcess(t *testing.T) {
	if os.Getenv("SIGTERM_TEST_HELPER") == "1" {
		ctx := CancelContext(context.Background())

		fmt.Println("ready") //nolint:forbidigo // read by the parent test

		<-ctx.Done()

		fmt.Println("canceled") //nolint:forbidigo // read by the parent test

		// A cleanup that hangs.
		time.Sleep(30 * time.Second)
		os.Exit(0)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestSecondSignalEndsProcess$")
	cmd.Env = append(os.Environ(), "SIGTERM_TEST_HELPER=1")

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}

	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = cmd.Process.Kill() })

	lines := bufio.NewScanner(stdout)

	expect := func(want string) {
		t.Helper()

		for lines.Scan() {
			if lines.Text() == want {
				return
			}
		}

		t.Fatalf("the helper process ended before it printed %q", want)
	}

	expect("ready")

	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}

	expect("canceled")

	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)

	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		var exit *exec.ExitError

		if !errors.As(err, &exit) {
			t.Fatalf("helper process after a second SIGINT: %v, want it killed by the signal", err)
		}

		if status, ok := exit.Sys().(syscall.WaitStatus); !ok || !status.Signaled() || status.Signal() != syscall.SIGINT {
			t.Errorf("helper process after a second SIGINT: %v, want it killed by SIGINT", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a second SIGINT did not end the process")
	}
}

// TestEndingSignalRunsOnEndHooks checks that a signal that ends the process
// runs the OnEnd hooks first, with the signal, and then still ends the
// process by that signal: a second SIGINT after the first canceled the
// context, a SIGHUP, which cancels nothing, and a SIGTERM with no context to
// cancel.
func TestEndingSignalRunsOnEndHooks(t *testing.T) {
	if mode := os.Getenv("SIGTERM_ONEND_TEST_HELPER"); mode != "" {
		OnEnd(func(sig os.Signal) {
			fmt.Println("ending", sig) //nolint:forbidigo // read by the parent test
		})

		ctx := context.Background()
		if mode == "cancel" {
			ctx = CancelContext(ctx)
		}

		fmt.Println("ready") //nolint:forbidigo // read by the parent test

		if mode == "cancel" {
			<-ctx.Done()

			fmt.Println("canceled") //nolint:forbidigo // read by the parent test
		}

		time.Sleep(30 * time.Second)
		os.Exit(0)
	}

	type step struct {
		sig  syscall.Signal
		want string // the line the helper prints at the signal
	}

	for _, tc := range []struct {
		name  string
		mode  string
		steps []step
	}{
		{
			name:  "second SIGINT",
			mode:  "cancel",
			steps: []step{{syscall.SIGINT, "canceled"}, {syscall.SIGINT, "ending interrupt"}},
		},
		{name: "SIGHUP", mode: "cancel", steps: []step{{syscall.SIGHUP, "ending hangup"}}},
		{name: "SIGTERM without a context", mode: "plain", steps: []step{{syscall.SIGTERM, "ending terminated"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestEndingSignalRunsOnEndHooks$")
			cmd.Env = append(os.Environ(), "SIGTERM_ONEND_TEST_HELPER="+tc.mode)

			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}

			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}

			t.Cleanup(func() { _ = cmd.Process.Kill() })

			var (
				lines = bufio.NewScanner(stdout)
				seen  []string
			)

			expect := func(want string) {
				t.Helper()

				for lines.Scan() {
					seen = append(seen, lines.Text())

					if lines.Text() == want {
						return
					}
				}

				t.Fatalf("the helper process ended before it printed %q; it printed %q", want, seen)
			}

			expect("ready")

			for _, s := range tc.steps {
				if err := cmd.Process.Signal(s.sig); err != nil {
					t.Fatal(err)
				}

				expect(s.want)
			}

			for lines.Scan() {
				seen = append(seen, lines.Text())
			}

			last := tc.steps[len(tc.steps)-1].sig

			var exit *exec.ExitError

			if err := cmd.Wait(); !errors.As(err, &exit) {
				t.Fatalf("helper process after %v: %v, want it killed by the signal", last, err)
			}

			if status, ok := exit.Sys().(syscall.WaitStatus); !ok || !status.Signaled() || status.Signal() != last {
				t.Errorf("helper process after %v: %v, want it killed by %v", last, exit, last)
			}

			if tc.mode == "cancel" && len(tc.steps) == 1 && slices.Contains(seen, "canceled") {
				t.Errorf("a %v canceled the context; it printed %q", last, seen)
			}
		})
	}
}
