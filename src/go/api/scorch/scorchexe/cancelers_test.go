package scorchexe

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func mustRegister(t *testing.T, exp string, run int) (context.Context, func()) {
	t.Helper()

	ctx, finished, err := Register(context.Background(), exp, run)
	if err != nil {
		t.Fatalf("Register(%s, %d) = %v", exp, run, err)
	}

	t.Cleanup(finished)

	return ctx, finished
}

// TestCancelExperimentWaitsForRuns checks that CancelExperiment cancels only
// the given experiment's run and returns once it has finished, or reports it
// if it did not finish in time.
func TestCancelExperimentWaitsForRuns(t *testing.T) {
	ctx, finished := mustRegister(t, "exp", 0)
	other, _ := mustRegister(t, "exp2", 0)

	tornDown := make(chan struct{})

	go func() {
		<-ctx.Done()
		time.Sleep(200 * time.Millisecond)
		close(tornDown)
		finished()
	}()

	if err := CancelExperiment("exp", 10*time.Second); err != nil {
		t.Fatalf("CancelExperiment() = %v", err)
	}

	Stopped("exp")

	select {
	case <-tornDown:
	default:
		t.Error("CancelExperiment returned before run 0 finished its teardown")
	}

	if other.Err() != nil {
		t.Error("CancelExperiment(exp) canceled a run of exp2")
	}

	// A run that never returns is reported once the wait times out.
	stuck, _ := mustRegister(t, "exp", 2)

	err := CancelExperiment("exp", 50*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "exp/2") {
		t.Errorf("CancelExperiment() with a run that never returns = %v, want an error naming exp/2", err)
	}

	Stopped("exp")

	if stuck.Err() == nil {
		t.Error("CancelExperiment did not cancel the run that never returns")
	}
}

// TestStaleRunLeavesLaterRunAlone is a regression test: runs were known only
// by experiment and run ID, so a run that outlived an experiment stop's wait
// canceled, when it returned, a later run of the same ID, and marked it
// finished, so the next stop did not wait for it.
func TestStaleRunLeavesLaterRunAlone(t *testing.T) {
	_, finishStale := mustRegister(t, "exp", 0)

	if err := CancelExperiment("exp", 10*time.Millisecond); err == nil {
		t.Fatal("CancelExperiment() of a run that does not return = nil, want an error")
	}

	Stopped("exp")

	// A run still tearing down is still executing.
	if _, _, err := Register(context.Background(), "exp", 0); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("Register() while the stale run tears down = %v, want %v", err, ErrAlreadyRunning)
	}

	finishStale()

	ctx, finished := mustRegister(t, "exp", 0)

	finishStale() // a second call of the stale run's finish changes nothing

	if ctx.Err() != nil {
		t.Fatal("the stale run's finish canceled the later run")
	}

	if Cancel("exp", 1) {
		t.Error("Cancel(exp, 1) canceled run 0")
	}

	go func() {
		<-ctx.Done()
		time.Sleep(100 * time.Millisecond)
		finished()
	}()

	start := time.Now()

	if err := CancelExperiment("exp", 10*time.Second); err != nil {
		t.Fatalf("CancelExperiment() = %v", err)
	}

	Stopped("exp")

	if waited := time.Since(start); waited < 100*time.Millisecond {
		t.Errorf("CancelExperiment() returned after %v, before the later run finished", waited)
	}
}

// TestRegisterRefusals is a regression test: a run started while an
// experiment stop waited for the old ones was registered after the stop
// looked, so the stop neither canceled nor awaited it, and two concurrent
// starts both passed the check that a run already executed.
func TestRegisterRefusals(t *testing.T) {
	if err := CancelExperiment("exp", time.Second); err != nil {
		t.Fatalf("CancelExperiment() without runs = %v", err)
	}

	if _, _, err := Register(context.Background(), "exp", 0); !errors.Is(err, ErrStopping) {
		t.Errorf("Register() during the stop = %v, want %v", err, ErrStopping)
	}

	Stopped("exp")

	var (
		wg         sync.WaitGroup
		registered atomic.Int32
		start      = make(chan struct{})
	)

	for run := range 50 {
		wg.Add(1)

		go func() {
			defer wg.Done()

			<-start

			if _, finished, err := Register(context.Background(), "exp", run%2); err == nil {
				registered.Add(1)
				t.Cleanup(finished)
			}
		}()
	}

	close(start)
	wg.Wait()

	if n := registered.Load(); n != 1 {
		t.Errorf("%d of 50 concurrent starts of the experiment's runs were registered, want 1", n)
	}
}
