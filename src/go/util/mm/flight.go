package mm

import "sync"

// flightGroup merges concurrent identical reads into one run, so several
// callers asking minimega the same question at once (the VMs page for a few
// users, the websocket VM list, state of health) cost one set of commands.
//
// Unlike golang.org/x/sync/singleflight, a call only joins a run that has not
// yet sent its first minimega command. A run that has already started may have
// read minimega before the joining caller's own previous action (starting a VM,
// say) took effect, so joining it could hand back an answer older than the
// call. Nothing is cached once a run finishes.
type flightGroup[T any] struct {
	mu      sync.Mutex
	flights map[string]*flight[T]
}

type flight[T any] struct {
	done    chan struct{}
	val     T
	waiters int // callers that joined; guarded by flightGroup.mu
}

// do runs fn, or waits for and returns the result of an identical run that
// has not started yet. fn must call seal just before it sends its first
// minimega command (see mmcli.RunStarted); seal may be called more than once.
//
// Every caller receives the same value, so callers must not modify it and
// should hand their own callers a copy.
func (g *flightGroup[T]) do(key string, fn func(seal func()) T) T { //nolint:ireturn // T is concrete at each use
	g.mu.Lock()

	if g.flights == nil {
		g.flights = make(map[string]*flight[T])
	}

	if f, ok := g.flights[key]; ok {
		f.waiters++

		g.mu.Unlock()

		<-f.done

		return f.val
	}

	f := &flight[T]{done: make(chan struct{})} //nolint:exhaustruct // val is set by fn below

	g.flights[key] = f

	g.mu.Unlock()

	var once sync.Once

	seal := func() {
		once.Do(func() {
			g.mu.Lock()
			defer g.mu.Unlock()

			if g.flights[key] == f {
				delete(g.flights, key)
			}
		})
	}

	// Release anyone waiting even if fn panics; they get the zero value.
	defer func() {
		seal()
		close(f.done)
	}()

	f.val = fn(seal)

	return f.val
}
