package experiment

import (
	"context"
	"sync"
	"time"

	"phenix/util/plog"
)

// asyncStartStopWait bounds how long Stop waits for an asynchronous start it
// canceled. A post-start app that ignores the cancellation must not keep the
// experiment from stopping.
var asyncStartStopWait = time.Minute //nolint:gochecknoglobals // shortened by tests

// asyncStart is the goroutine an asynchronous start leaves running in this
// process for the experiment's delayed VMs and post-start apps. It sends
// commands to the experiment's minimega namespace, and minimega creates a
// namespace for any command sent to it, so Stop must end it before it clears
// the namespace.
type asyncStart struct {
	cancel context.CancelFunc
	done   chan struct{}
}

//nolint:gochecknoglobals // the asynchronous starts running in this process
var (
	asyncStartsMu sync.Mutex
	asyncStarts   = make(map[string]*asyncStart)
)

// trackAsyncStart records the goroutine an asynchronous start of the named
// experiment is about to run. cancel must cancel the context it runs with.
func trackAsyncStart(name string, cancel context.CancelFunc) *asyncStart {
	run := &asyncStart{cancel: cancel, done: make(chan struct{})}

	asyncStartsMu.Lock()
	defer asyncStartsMu.Unlock()

	asyncStarts[name] = run

	return run
}

// finish forgets the start and releases a Stop waiting for it. The start's
// goroutine calls it when it returns.
func (r *asyncStart) finish(name string) {
	asyncStartsMu.Lock()
	if asyncStarts[name] == r {
		delete(asyncStarts, name)
	}
	asyncStartsMu.Unlock()

	r.cancel()
	close(r.done)
}

// endAsyncStart cancels the asynchronous start of the named experiment still
// running in this process, if any, and waits for its goroutine to return. The
// goroutine stopping its own failed start passes itself as self: it neither
// waits for itself nor ends another start.
func endAsyncStart(name string, self *asyncStart) {
	if self != nil {
		return
	}

	asyncStartsMu.Lock()
	run := asyncStarts[name]
	asyncStartsMu.Unlock()

	if run == nil {
		return
	}

	run.cancel()

	select {
	case <-run.done:
	case <-time.After(asyncStartStopWait):
		plog.Warn(
			plog.TypeSystem,
			"stopping experiment whose canceled start has not returned",
			"exp", name,
			"waited", asyncStartStopWait,
		)
	}
}

//nolint:gochecknoglobals // the stops running in this process
var (
	stopsMu sync.Mutex
	stops   = make(map[string]*sync.Mutex)
)

// lockStop serializes the stops of the named experiment in this process, such
// as a failed asynchronous start's own and a user's, which would otherwise
// both run its teardown. It returns the unlock.
func lockStop(name string) func() {
	stopsMu.Lock()

	mu, ok := stops[name]
	if !ok {
		mu = new(sync.Mutex)
		stops[name] = mu
	}

	stopsMu.Unlock()

	mu.Lock()

	return mu.Unlock
}
