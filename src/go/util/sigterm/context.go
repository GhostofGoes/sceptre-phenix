package sigterm

import (
	"context"
	"os"
	"os/signal"
	"slices"
	"sync"
	"syscall"
)

// phenix receives every signal it watches on one channel, so that each
// signal either cancels the contexts of CancelContext or ends the process,
// never both.
//
//nolint:gochecknoglobals // signal handling is process-wide
var (
	mu      sync.Mutex
	signals chan os.Signal                     // nil until the first watch
	pending = make(map[int]context.CancelFunc) // contexts no signal canceled yet
	nextID  int
	onEnd   []func(os.Signal)
)

// CancelContext returns a context that wraps the given context with a cancel
// function that's called when a SIGTERM or SIGINT is trapped. Only the first
// signal is trapped: a second one ends the process as its default handling
// would, once the functions registered with [OnEnd] have run, so a command
// whose cleanup after the first one hangs can still be interrupted.
func CancelContext(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}

	ctxWithCancel, cancel := context.WithCancel(ctx)

	mu.Lock()

	id := nextID
	nextID++
	pending[id] = cancel

	// Before CancelContext returns, so a signal sent right after the call
	// cancels the context.
	watch(syscall.SIGTERM, syscall.SIGINT)

	mu.Unlock()

	go func() {
		<-ctxWithCancel.Done()

		mu.Lock()
		defer mu.Unlock()

		delete(pending, id)
	}()

	return ctxWithCancel
}

// OnEnd registers f to run, with the signal, before a SIGINT, SIGTERM or
// SIGHUP ends the process, and makes phenix watch those of the three it does
// not ignore. A SIGINT or SIGTERM that cancels the contexts of CancelContext
// does not end the process; a SIGHUP always does.
func OnEnd(f func(os.Signal)) {
	mu.Lock()
	defer mu.Unlock()

	onEnd = append(onEnd, f)

	for _, sig := range []os.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP} {
		if !signal.Ignored(sig) {
			watch(sig)
		}
	}
}

// watch adds sigs to the signals phenix watches. The caller holds mu.
func watch(sigs ...os.Signal) {
	if signals == nil {
		signals = make(chan os.Signal, 1)

		go handle(signals)
	}

	signal.Notify(signals, sigs...)
}

// handle cancels the pending contexts of CancelContext at a SIGINT or SIGTERM,
// and ends the process at a signal that finds none pending, or a SIGHUP.
func handle(ch chan os.Signal) {
	for sig := range ch {
		mu.Lock()

		if cancels := pending; sig != syscall.SIGHUP && len(cancels) > 0 {
			pending = make(map[int]context.CancelFunc)

			mu.Unlock()

			for _, cancel := range cancels {
				cancel()
			}

			continue
		}

		hooks := slices.Clone(onEnd)

		mu.Unlock()

		end(ch, sig, hooks)

		return
	}
}

// end runs hooks, then stops watching signals and sends sig to the process
// again, so that it gets its default handling and ends the process as if
// phenix had never watched it.
func end(ch chan os.Signal, sig os.Signal, hooks []func(os.Signal)) {
	for _, hook := range hooks {
		hook(sig)
	}

	mu.Lock()

	signal.Stop(ch)
	signals = nil

	mu.Unlock()

	if s, ok := sig.(syscall.Signal); ok {
		_ = syscall.Kill(syscall.Getpid(), s)
	}
}
