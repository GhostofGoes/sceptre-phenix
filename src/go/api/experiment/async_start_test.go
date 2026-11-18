package experiment_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/golang/mock/gomock"

	"phenix/api/experiment"
	"phenix/app"
	"phenix/store"
	"phenix/util/file"
	"phenix/util/mm"
	"phenix/util/notes"
)

// asyncStartMM is the minimega double of a start; it records whether the
// start's goroutine had returned each time the namespace was cleared.
type asyncStartMM struct {
	*startMM

	returned func() bool

	mu          sync.Mutex
	clearedLive int // clears while the start's goroutine still ran
}

func (m *asyncStartMM) ClearNamespace(ns string) error {
	m.mu.Lock()
	if !m.returned() {
		m.clearedLive++
	}
	m.mu.Unlock()

	return m.startMM.ClearNamespace(ns)
}

// TestStopEndsAsyncStartBeforeClearingNamespace is a regression test: a stop
// in the process that started an experiment asynchronously (phenix ui) cleared
// the namespace while the start's goroutine still waited for a delayed VM or
// ran post-start apps, so a C2 check, VM start or app command in flight then
// made minimega create the namespace again. Stop cancels that goroutine and
// waits for it first.
func TestStopEndsAsyncStartBeforeClearingNamespace(t *testing.T) {
	startAppOnce.Do(func() {
		err := app.RegisterUserApp(startTestApp, func() app.App { return startApp{rec: startAppRec} })
		if err != nil {
			t.Fatalf("registering test app: %v", err)
		}
	})

	tests := []struct {
		name        string
		delayed     bool          // the timer VM waits a minute
		postStart   time.Duration // the post-start app's work, which ignores cancellation
		cancelFirst bool          // the caller cancels the start before it stops (phenix ui)
		wantErrs    int           // errors the start reports, each its cancellation
	}{
		{name: "stop while a delayed VM waits", delayed: true, wantErrs: 1},
		{name: "cancel and stop while a delayed VM waits", delayed: true, cancelFirst: true, wantErrs: 1},
		{name: "cancel and stop while a post-start app runs", postStart: 500 * time.Millisecond, cancelFirst: true, wantErrs: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(notes.Context(context.Background(), false))
			t.Cleanup(cancel)

			rec := &startRecord{postStartSleep: tt.postStart}
			startAppRec = rec

			errs := newStartErrors()

			fake := &asyncStartMM{startMM: new(startMM), returned: errs.returned}

			installStartDoubles(t, fake, startTestConfig(t, false, tt.delayed))

			err := experiment.Start(
				ctx,
				experiment.StartWithName(startTestExp),
				experiment.StartWithErrorChannel(errs.ch),
			)
			if err != nil {
				t.Fatalf("Start returned error: %v", err)
			}

			if errs.returned() {
				t.Fatal("the start's goroutine returned before the test stopped the experiment")
			}

			if tt.cancelFirst {
				cancel()
			}

			if err := experiment.Stop(startTestExp); err != nil {
				t.Fatalf("Stop returned error: %v", err)
			}

			if fake.cleared == 0 {
				t.Fatal("Stop did not clear the namespace")
			}

			if fake.clearedLive != 0 {
				t.Error("Stop cleared the namespace while the start's goroutine still ran")
			}

			got := errs.wait(t)

			if len(got) != tt.wantErrs || !all(got, func(err error) bool { return errors.Is(err, context.Canceled) }) {
				t.Errorf("the start reported %v, want %d cancellation errors", got, tt.wantErrs)
			}

			if rec.cleanups != 1 {
				t.Errorf("cleanup apps ran %d times, want once: the canceled start must not stop the experiment again", rec.cleanups)
			}
		})
	}
}

// startErrors collects the errors an asynchronous start reports. The start
// closes its error channel before it releases a Stop waiting for it, so
// returned is true by the time such a Stop goes on.
type startErrors struct {
	ch chan error

	mu     sync.Mutex
	got    []error
	closed bool
}

func newStartErrors() *startErrors {
	return &startErrors{ch: make(chan error, 10)}
}

// returned reports whether the start's goroutine closed its error channel.
func (e *startErrors) returned() bool {
	e.mu.Lock()
	defer e.mu.Unlock()

	for !e.closed {
		select {
		case err, ok := <-e.ch:
			if ok {
				e.got = append(e.got, err)
			} else {
				e.closed = true
			}
		default:
			return false
		}
	}

	return true
}

// wait waits for the start's goroutine to return and returns its errors.
func (e *startErrors) wait(t *testing.T) []error {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)

	for !e.returned() {
		if time.Now().After(deadline) {
			t.Fatal("the start's goroutine did not return")
		}

		time.Sleep(10 * time.Millisecond)
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	return e.got
}

// installStartDoubles installs fake as minimega, a cluster file store that
// deletes nothing, and a store that holds the single experiment config cfg,
// and restores the originals when the test ends.
func installStartDoubles(t *testing.T, fake mm.MM, cfg store.Config) {
	t.Helper()

	originalMM, originalFiles, originalStore := mm.DefaultMM, file.DefaultClusterFiles, store.DefaultStore

	t.Cleanup(func() {
		mm.DefaultMM = originalMM                //nolint:reassign // restore test double
		file.DefaultClusterFiles = originalFiles //nolint:reassign // restore test double
		store.DefaultStore = originalStore       //nolint:reassign // restore test double
	})

	var (
		mu      sync.Mutex
		current = cfg
		m       = store.NewMockStore(gomock.NewController(t))
	)

	m.EXPECT().Get(gomock.Any()).DoAndReturn(func(c *store.Config) error {
		mu.Lock()
		defer mu.Unlock()

		*c = current

		return nil
	}).AnyTimes()
	m.EXPECT().Update(gomock.Any()).DoAndReturn(func(c *store.Config) error {
		mu.Lock()
		defer mu.Unlock()

		current = *c

		return nil
	}).AnyTimes()

	mm.DefaultMM = fake                     //nolint:reassign // install test double
	file.DefaultClusterFiles = startFiles{} //nolint:reassign // install test double
	store.DefaultStore = m                  //nolint:reassign // install test double
}

func all[T any](s []T, f func(T) bool) bool {
	for _, v := range s {
		if !f(v) {
			return false
		}
	}

	return true
}
