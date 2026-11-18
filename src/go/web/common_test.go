package web

import (
	"context"
	"errors"
	"net/http"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang/mock/gomock"

	"phenix/store"
	v1 "phenix/types/version/v1"
	"phenix/util/mm"
	"phenix/web/rbac"
	"phenix/web/weberror"
)

// noLaunchProgressMM reports no launch progress for the start progress loop.
type noLaunchProgressMM struct {
	mm.MM
}

func (noLaunchProgressMM) GetLaunchProgress(string, int) (float64, error) { return 0, nil }

// TestStartExperimentFailureEndsItsGoroutine is a regression test: after a
// failed start, the goroutine running it sent a second result on the
// unbuffered status channel, which nothing received any more, so every failed
// REST start leaked a goroutine.
func TestStartExperimentFailureEndsItsGoroutine(t *testing.T) {
	const name = "start-failure-test"

	ctrl := gomock.NewController(t)

	m := store.NewMockStore(ctrl)
	m.EXPECT().Get(gomock.Any()).DoAndReturn(func(c *store.Config) error {
		*c = store.Config{
			Version:  "phenix.sandia.gov/v1",
			Kind:     "Experiment",
			Metadata: store.ConfigMetadata{Name: name},
			Spec:     map[string]any{"experimentName": name},
			Status:   map[string]any{"startTime": "2024-01-01T00:00:00Z"}, // already running
		}

		return nil
	}).AnyTimes()

	originalStore, originalMM := store.DefaultStore, mm.DefaultMM
	t.Cleanup(func() {
		store.DefaultStore = originalStore //nolint:reassign // restore default
		mm.DefaultMM = originalMM          //nolint:reassign // restore default
	})

	store.DefaultStore = m              //nolint:reassign // install test double
	mm.DefaultMM = noLaunchProgressMM{} //nolint:reassign // install test double

	if _, err := startExperiment(name); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("expected starting a running experiment to fail with already running, got %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)

	for startGoroutineRunning() {
		if time.Now().After(deadline) {
			t.Fatal("the goroutine of the failed start never returned")
		}

		time.Sleep(10 * time.Millisecond)
	}
}

// startGoroutineRunning reports whether a goroutine started by startExperiment
// is still running.
func startGoroutineRunning() bool {
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)

	return strings.Contains(string(buf[:n]), "phenix/web.startExperiment.func")
}

// TestFailedStartKeepsTheRunningStartsCancelers is a regression test: a start
// of an experiment that is already running, such as a retried or double-clicked
// one, deleted every canceler of the experiment when it failed, so the next stop
// canceled nothing, waited forever for the periodic apps the first start ran,
// and left them running.
func TestFailedStartKeepsTheRunningStartsCancelers(t *testing.T) {
	const name = "start-twice-test"

	ctrl := gomock.NewController(t)

	m := store.NewMockStore(ctrl)
	m.EXPECT().Get(gomock.Any()).DoAndReturn(func(c *store.Config) error {
		*c = store.Config{
			Version:  "phenix.sandia.gov/v1",
			Kind:     "Experiment",
			Metadata: store.ConfigMetadata{Name: name},
			Spec:     map[string]any{"experimentName": name},
			Status:   map[string]any{"startTime": "2024-01-01T00:00:00Z"}, // already running
		}

		return nil
	}).AnyTimes()

	originalStore, originalMM := store.DefaultStore, mm.DefaultMM
	t.Cleanup(func() {
		store.DefaultStore = originalStore //nolint:reassign // restore default
		mm.DefaultMM = originalMM          //nolint:reassign // restore default
	})

	store.DefaultStore = m              //nolint:reassign // install test double
	mm.DefaultMM = noLaunchProgressMM{} //nolint:reassign // install test double

	// What the first, successful start registered for the periodic apps.
	periodic, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup

	addCanceler(name, cancel)

	commonMu.Lock()
	waiters[name] = &wg
	commonMu.Unlock()

	t.Cleanup(func() {
		commonMu.Lock()
		delete(cancelers, name)
		delete(waiters, name)
		commonMu.Unlock()
	})

	if _, err := startExperiment(name); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("starting the running experiment again = %v, want already running", err)
	}

	commonMu.Lock()
	cancels := takeCancelers(name)
	kept := waiters[name] == &wg
	commonMu.Unlock()

	if len(cancels) != 1 || !kept {
		t.Fatalf("after the failed start: %d cancelers, waiter kept %v; want the first start's canceler and waiter", len(cancels), kept)
	}

	cancels[0]()

	if periodic.Err() == nil {
		t.Error("the kept canceler does not cancel the periodic apps")
	}
}

// TestAddCancelerRemovesOnlyItsOwn checks that removing one registration
// leaves the others of the same key.
func TestAddCancelerRemovesOnlyItsOwn(t *testing.T) {
	const key = "exp/app"

	var first, second bool

	addCanceler(key, func() { first = true })
	removeSecond := addCanceler(key, func() { second = true })

	removeSecond()

	commonMu.Lock()
	cancels := takeCancelers(key)
	_, left := cancelers[key]
	commonMu.Unlock()

	for _, cancel := range cancels {
		cancel()
	}

	if !first || second || left {
		t.Errorf("first canceled %v, second canceled %v, key left %v; want only the first canceled and the key gone", first, second, left)
	}
}

// stopMM answers the minimega calls of an experiment stop.
type stopMM struct {
	mm.MM
}

func (stopMM) Headnode() string              { return "head" }
func (stopMM) ClearNamespace(string) error   { return nil }
func (stopMM) GetVMInfo(...mm.Option) mm.VMs { return nil }

// TestStopExperimentReportsAnUnreadableStore is a regression test: when the
// store could not be read after the stop, stopExperiment only logged it and
// then dereferenced the nil experiment, so the handler panicked.
func TestStopExperimentReportsAnUnreadableStore(t *testing.T) {
	const name = "stop-unreadable-test"

	var updated bool

	m := store.NewMockStore(gomock.NewController(t))
	m.EXPECT().Get(gomock.Any()).DoAndReturn(func(c *store.Config) error {
		if updated {
			return errors.New("store read failed")
		}

		*c = store.Config{
			Version:  "phenix.sandia.gov/v1",
			Kind:     "Experiment",
			Metadata: store.ConfigMetadata{Name: name},
			Spec:     map[string]any{"experimentName": name, "topology": map[string]any{"nodes": []any{}}},
			Status:   map[string]any{"startTime": "2024-01-01T00:00:00Z"},
		}

		return nil
	}).AnyTimes()
	m.EXPECT().Update(gomock.Any()).DoAndReturn(func(*store.Config) error {
		updated = true

		return nil
	}).AnyTimes()

	originalStore, originalMM := store.DefaultStore, mm.DefaultMM
	t.Cleanup(func() {
		store.DefaultStore = originalStore //nolint:reassign // restore default
		mm.DefaultMM = originalMM          //nolint:reassign // restore default
	})

	store.DefaultStore = m  //nolint:reassign // install test double
	mm.DefaultMM = stopMM{} //nolint:reassign // install test double

	// Clients showed the experiment stopping until a reload: they heard
	// neither stop nor errorStopping.
	events := recordBroadcasts(t, rbac.Role{Spec: &v1.RoleSpec{Policies: []*v1.PolicySpec{{
		Resources: []string{"experiments/stop"}, ResourceNames: []string{"*"}, Verbs: []string{"update"},
	}}}})

	_, err := stopExperiment(name)

	var webErr *weberror.WebError
	if !errors.As(err, &webErr) || webErr.Status != http.StatusInternalServerError || !strings.Contains(webErr.Cause, "store read failed") {
		t.Fatalf("stopExperiment = %v, want a 500 naming the store failure", err)
	}

	var actions []string

	for timeout := time.After(5 * time.Second); len(actions) < 2; {
		select {
		case ev := <-events:
			if ev.Type == "experiment" && ev.Name == name {
				actions = append(actions, ev.Action)
			}
		case <-timeout:
			t.Fatalf("clients heard %v of the stop, want stopping and errorStopping", actions)
		}
	}

	if !slices.Equal(actions, []string{"stopping", "errorStopping"}) {
		t.Errorf("clients heard %v of the stop, want stopping and errorStopping", actions)
	}
}
