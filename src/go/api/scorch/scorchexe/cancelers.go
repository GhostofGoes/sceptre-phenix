package scorchexe

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// ErrStopping is the refusal of a run while its experiment is being stopped.
var ErrStopping = errors.New("the experiment is being stopped")

// execution is a run that Register registered and that has not returned.
type execution struct {
	run    int
	cancel context.CancelFunc
	done   chan struct{} // closed once the run has returned
}

var (
	executionsMu sync.Mutex //nolint:gochecknoglobals // global lock

	// executions holds, for each experiment, the run of it that executes in
	// this process, if any.
	executions = make(map[string]*execution) //nolint:gochecknoglobals // global state

	// stopping counts, for each experiment, the stops that CancelExperiment
	// began and Stopped has not ended.
	stopping = make(map[string]int) //nolint:gochecknoglobals // global state
)

// Register records that the given run of the experiment is about to execute
// in this process. It returns the run's context, which Cancel and
// CancelExperiment cancel, and the function to call once the run has
// returned. It refuses with ErrAlreadyRunning while a run of the experiment
// registered here has not returned, even one an experiment stop gave up
// waiting for, and with ErrStopping while the experiment is being stopped.
// The check and the registration are one step, so of two concurrent starts
// only one is registered.
func Register(ctx context.Context, exp string, run int) (context.Context, func(), error) {
	executionsMu.Lock()
	defer executionsMu.Unlock()

	if stopping[exp] > 0 {
		return nil, nil, ErrStopping
	}

	if _, ok := executions[exp]; ok {
		return nil, nil, ErrAlreadyRunning
	}

	ctx, cancel := context.WithCancel(ctx)
	e := &execution{run: run, cancel: cancel, done: make(chan struct{})}
	executions[exp] = e

	var once sync.Once

	finished := func() {
		once.Do(func() {
			cancel()

			executionsMu.Lock()
			defer executionsMu.Unlock()

			// Only this run's own registration goes, whatever runs later.
			if executions[exp] == e {
				delete(executions, exp)
			}

			close(e.done)
		})
	}

	return ctx, finished, nil
}

// Cancel cancels the given run of the experiment if it executes in this
// process, and reports whether it did.
func Cancel(exp string, run int) bool {
	executionsMu.Lock()
	defer executionsMu.Unlock()

	e, ok := executions[exp]
	if !ok || e.run != run {
		return false
	}

	e.cancel()

	return true
}

// CancelExperiment cancels the run of the experiment that executes in this
// process, if any, and waits up to timeout for it to return, so its stop and
// cleanup stages run while the experiment's VMs still exist. Until Stopped
// is called for the experiment, Register refuses its runs. It returns an
// error naming the run if it had not returned by then.
func CancelExperiment(exp string, timeout time.Duration) error {
	executionsMu.Lock()
	stopping[exp]++
	e := executions[exp]
	executionsMu.Unlock()

	if e == nil {
		return nil
	}

	e.cancel()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-e.done:
		return nil
	case <-timer.C:
		return fmt.Errorf("run %s/%d still executing %v after it was canceled", exp, e.run, timeout)
	}
}

// Stopped ends the refusal of the experiment's runs that a CancelExperiment
// for its stop began.
func Stopped(exp string) {
	executionsMu.Lock()
	defer executionsMu.Unlock()

	if stopping[exp]--; stopping[exp] <= 0 {
		delete(stopping, exp)
	}
}
