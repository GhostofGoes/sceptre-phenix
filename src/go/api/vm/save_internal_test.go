package vm

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// scriptedPoll answers the polls of waitForVMSave with statuses in turn,
// repeating the last one, and counts them.
func scriptedPoll(statuses ...migration) (func() (migration, error), *int) {
	calls := 0

	return func() (migration, error) {
		i := calls
		calls++

		if i >= len(statuses) {
			return statuses[len(statuses)-1], nil
		}

		return statuses[i], nil
	}, &calls
}

func status(s string, transferred float64) migration {
	m := migration{Status: s}
	m.RAM.Total, m.RAM.Transferred = 1000, transferred

	return m
}

func TestWaitForVMSave(t *testing.T) {
	t.Run("progress until the save completes", func(t *testing.T) {
		poll, _ := scriptedPoll(status("setup", 0), status("active", 400), status("completed", 1000))

		var progress []string

		err := waitForVMSave("vm1", poll, func(p string) { progress = append(progress, p) }, time.Millisecond, time.Second, time.Second)
		if err != nil {
			t.Fatalf("waitForVMSave() = %v, want nil", err)
		}

		want := []string{"0.000000", "40.000000", statusCompleted}
		if strings.Join(progress, ",") != strings.Join(want, ",") {
			t.Fatalf("progress = %v, want %v", progress, want)
		}
	})

	t.Run("failed save is an error", func(t *testing.T) {
		poll, _ := scriptedPoll(status("active", 100), status("failed", 100))

		err := waitForVMSave("vm1", poll, nil, time.Millisecond, time.Second, time.Second)
		if err == nil || !strings.Contains(err.Error(), "failed") {
			t.Fatalf("waitForVMSave() = %v, want a failed error", err)
		}
	})

	t.Run("no migration at all times out", func(t *testing.T) {
		poll, calls := scriptedPoll(migration{})

		err := waitForVMSave("vm1", poll, nil, time.Millisecond, 5*time.Millisecond, time.Second)
		if err == nil || !strings.Contains(err.Error(), "no memory snapshot") {
			t.Fatalf("waitForVMSave() = %v, want a timeout error", err)
		}

		if *calls < 5 {
			t.Fatalf("polled %d times before timing out, want at least 5", *calls)
		}
	})

	// A guest that dirties its memory faster than QEMU copies it never
	// finishes saving, so the wait gives up after the timeout.
	t.Run("a save that never completes times out", func(t *testing.T) {
		poll, calls := scriptedPoll(status("active", 400))

		err := waitForVMSave("vm1", poll, nil, time.Millisecond, time.Second, 5*time.Millisecond)
		if err == nil || !strings.Contains(err.Error(), "did not complete within") {
			t.Fatalf("waitForVMSave() = %v, want a timeout error", err)
		}

		if *calls > 10 {
			t.Fatalf("polled %d times, want the save given up after about 5", *calls)
		}
	})

	// The time the polls take counts toward the timeout, not only the sleeps
	// between them: polls of 50 ms with a 1 ms interval must not stretch a
	// 100 ms limit.
	t.Run("the time polls take counts toward the timeout", func(t *testing.T) {
		poll, _ := scriptedPoll(status("active", 400))

		start := time.Now()

		err := waitForVMSave("vm1", func() (migration, error) {
			time.Sleep(50 * time.Millisecond)

			return poll()
		}, nil, time.Millisecond, time.Second, 100*time.Millisecond)
		if err == nil || !strings.Contains(err.Error(), "did not complete within") {
			t.Fatalf("waitForVMSave() = %v, want a timeout error", err)
		}

		if took := time.Since(start); took > time.Second {
			t.Fatalf("waitForVMSave() gave up after %v, want about 100ms", took)
		}
	})

	// A poll's error, such as minimega's, is returned at once; reading it as no
	// status would hide it behind a timeout.
	t.Run("a failed poll is returned at once", func(t *testing.T) {
		calls := 0
		want := errors.New("querying the memory snapshot of VM vm1: vm 5 is not running")

		err := waitForVMSave("vm1", func() (migration, error) {
			calls++

			return migration{}, want
		}, nil, time.Millisecond, time.Second, time.Second)

		if !errors.Is(err, want) || calls != 1 {
			t.Fatalf("waitForVMSave() = %v after %d polls, want %v after one", err, calls, want)
		}
	})
}

func TestMigrationRunning(t *testing.T) {
	for s, want := range map[string]bool{
		"":                false,
		"none":            false,
		"setup":           true,
		"active":          true,
		"device":          true,
		"pre-switchover":  true,
		"postcopy-active": true,
		"cancelling":      false,
		"cancelled":       false,
		"completed":       false,
		"failed":          false,
	} {
		if got := migrationRunning(migration{Status: s}); got != want {
			t.Errorf("migrationRunning(%q) = %v, want %v", s, got, want)
		}
	}
}
