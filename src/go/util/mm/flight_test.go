package mm

import (
	"sync"
	"sync/atomic"
	"testing"
)

// waiting returns how many callers have joined the unstarted run for key, or
// -1 if there is none.
func (g *flightGroup[T]) waiting(key string) int {
	g.mu.Lock()
	defer g.mu.Unlock()

	if f, ok := g.flights[key]; ok {
		return f.waiters
	}

	return -1
}

func TestFlightGroupMergesCallsBeforeSeal(t *testing.T) {
	t.Parallel()

	const callers = 5

	var (
		group   flightGroup[int]
		runs    atomic.Int32
		release = make(chan struct{})
		wg      sync.WaitGroup
		results = make([]int, callers)
	)

	fn := func(seal func()) int {
		runs.Add(1)
		<-release
		seal()

		return 42
	}

	wg.Add(1)

	go func() {
		defer wg.Done()

		results[0] = group.do("k", fn)
	}()

	waitFor(t, func() bool { return group.waiting("k") == 0 })

	for i := 1; i < callers; i++ {
		wg.Add(1)

		go func() {
			defer wg.Done()

			results[i] = group.do("k", fn)
		}()
	}

	waitFor(t, func() bool { return group.waiting("k") == callers-1 })

	close(release)
	wg.Wait()

	if got := runs.Load(); got != 1 {
		t.Fatalf("fn ran %d times, want 1", got)
	}

	for i, got := range results {
		if got != 42 {
			t.Fatalf("caller %d got %d, want 42", i, got)
		}
	}

	if got := group.waiting("k"); got != -1 {
		t.Fatalf("finished run is still joinable (%d waiting)", got)
	}
}

func TestFlightGroupDoesNotJoinStartedRun(t *testing.T) {
	t.Parallel()

	var (
		group   flightGroup[int]
		sealed  = make(chan struct{})
		release = make(chan struct{})
		first   = make(chan int, 1)
	)

	go func() {
		first <- group.do("k", func(seal func()) int {
			seal()
			close(sealed)
			<-release

			return 1
		})
	}()

	<-sealed

	// The first run has started, so this call must run on its own rather than
	// take a result that may predate it.
	if got := group.do("k", func(func()) int { return 2 }); got != 2 {
		t.Fatalf("second call got %d, want its own result 2", got)
	}

	close(release)

	if got := <-first; got != 1 {
		t.Fatalf("first call got %d, want 1", got)
	}
}

func TestFlightGroupKeysAreIndependent(t *testing.T) {
	t.Parallel()

	var group flightGroup[string]

	a := group.do("a", func(func()) string { return "a" })
	b := group.do("b", func(func()) string { return "b" })

	if a != "a" || b != "b" {
		t.Fatalf("got %q and %q", a, b)
	}
}

func TestFlightGroupReleasesWaitersOnPanic(t *testing.T) {
	t.Parallel()

	var (
		group   flightGroup[int]
		release = make(chan struct{})
		joined  = make(chan int, 1)
	)

	go func() {
		defer func() { _ = recover() }()

		group.do("k", func(func()) int {
			<-release
			panic("boom")
		})
	}()

	waitFor(t, func() bool { return group.waiting("k") == 0 })

	go func() { joined <- group.do("k", func(func()) int { return 7 }) }()

	waitFor(t, func() bool { return group.waiting("k") == 1 })
	close(release)

	if got := <-joined; got != 0 {
		t.Fatalf("waiter got %d, want the zero value", got)
	}
}
