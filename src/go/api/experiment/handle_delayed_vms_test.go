//nolint:testpackage // testing internals
package experiment

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/golang/mock/gomock"
	"github.com/hashicorp/go-multierror"

	"phenix/store"
	"phenix/util/common"
	"phenix/util/mm"
)

// countingMM is a test double for mm.MM that only tracks calls to
// ClearNamespace. Embedding the (nil) mm.MM interface means any other method
// call will panic, which is fine since these tests never exercise them.
type countingMM struct {
	mm.MM

	mu                  sync.Mutex
	clearNamespaceCalls int
}

func (m *countingMM) ClearNamespace(string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.clearNamespaceCalls++

	return nil
}

func (m *countingMM) calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.clearNamespaceCalls
}

// TestHandleDelayedVMsSkipsClearNamespaceOnContextCancellation verifies the
// fix for the race where stopping an experiment (which cancels the start
// context) while delayed VMs were still pending would cause
// handleDelayedVMs to call mm.ClearNamespace itself. That raced with the
// namespace teardown already being performed by the in-progress Stop call,
// which could delete resources (e.g. taps created by the tap app) out from
// under that cleanup. When the context is canceled, handleDelayedVMs should
// return the error without touching the namespace.
func TestHandleDelayedVMsSkipsClearNamespaceOnContextCancellation(t *testing.T) {
	fake := new(countingMM)

	original := mm.DefaultMM
	t.Cleanup(func() { mm.DefaultMM = original }) //nolint:reassign // restore test double

	mm.DefaultMM = fake //nolint:reassign // install test double

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// Use a delay long enough that, were the context-cancellation check not
	// hit first, the test would hang rather than silently pass.
	delays := map[string]time.Duration{"host01": time.Minute}

	err := handleDelayedVMs(ctx, "test-ns", "", delays, nil)
	if err == nil {
		t.Fatal("expected an error when the context is canceled")
	}

	merr, ok := err.(*multierror.Error) //nolint:errorlint // need access to wrapped Errors slice
	if !ok {
		t.Fatalf("expected *multierror.Error, got %T", err)
	}

	var foundCanceled bool

	for _, e := range merr.Errors {
		if errors.Is(e, context.Canceled) {
			foundCanceled = true

			break
		}
	}

	if !foundCanceled {
		t.Fatalf("expected context.Canceled among returned errors, got: %v", merr.Errors)
	}

	if calls := fake.calls(); calls != 0 {
		t.Fatalf("expected ClearNamespace not to be called on context cancellation, called %d time(s)", calls)
	}
}

// TestHandleDelayedVMsNoDelaysOrC2sReturnsNilWithoutClearingNamespace is a
// sanity check that the early-return path for experiments with no delayed
// VMs never touches the namespace either.
func TestHandleDelayedVMsNoDelaysOrC2sReturnsNilWithoutClearingNamespace(t *testing.T) {
	fake := new(countingMM)

	original := mm.DefaultMM
	t.Cleanup(func() { mm.DefaultMM = original }) //nolint:reassign // restore test double

	mm.DefaultMM = fake //nolint:reassign // install test double

	if err := handleDelayedVMs(context.Background(), "test-ns", "", nil, nil); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	if calls := fake.calls(); calls != 0 {
		t.Fatalf("expected ClearNamespace not to be called, called %d time(s)", calls)
	}
}

// c2GateMM is a test double for mm.MM that counts C2 checks and namespace
// clears; no C2 client ever registers.
type c2GateMM struct {
	countingMM

	c2Checks int
}

func (m *c2GateMM) IsC2ClientActive(...mm.C2Option) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.c2Checks++

	return mm.ErrC2ClientNotActive
}

// TestHandleDelayedVMsGivesUpOnceStoppedElsewhere is a regression test: a stop
// from another process (phenix experiment stop while phenix ui or the CLI waits
// for delayed VMs) cannot cancel the waits, so they kept running C2 checks and
// VM starts in the cleared namespace, and minimega created it again; a C2 wait
// did so every five seconds until the process exited. The waits stop once the
// stored start time is no longer theirs, and leave the namespace alone.
func TestHandleDelayedVMsGivesUpOnceStoppedElsewhere(t *testing.T) {
	const ns = "delay-stopped"

	original, originalStore, originalInterval := mm.DefaultMM, store.DefaultStore, c2CheckInterval
	originalBase := common.MinimegaBase

	t.Cleanup(func() {
		mm.DefaultMM = original            //nolint:reassign // restore test double
		store.DefaultStore = originalStore //nolint:reassign // restore test double
		c2CheckInterval = originalInterval
		common.MinimegaBase = originalBase //nolint:reassign // restore test fixture
	})

	c2CheckInterval = 10 * time.Millisecond

	// A VM start the waits must not send fails here instead of reaching a
	// minimega that runs on this host.
	common.MinimegaBase = t.TempDir() //nolint:reassign // test fixture

	tests := []struct {
		name   string
		stored string // the start time the store holds; "" once stopped
		absent bool   // the experiment was deleted
	}{
		{name: "stopped", stored: ""},
		{name: "stopped and started again", stored: "2026-10-18T12:05:00Z"},
		{name: "deleted", absent: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := store.NewMockStore(gomock.NewController(t))
			m.EXPECT().Get(gomock.Any()).DoAndReturn(func(c *store.Config) error {
				if tt.absent {
					return store.ErrNotExist
				}

				*c = store.Config{
					Version:  "phenix.sandia.gov/v1",
					Kind:     "Experiment",
					Metadata: store.ConfigMetadata{Name: ns},
					Spec: map[string]any{
						"experimentName": ns,
						"topology":       map[string]any{"nodes": []map[string]any{}},
					},
					Status: map[string]any{"startTime": tt.stored},
				}

				return nil
			}).AnyTimes()

			fake := new(c2GateMM)

			mm.DefaultMM = fake    //nolint:reassign // install test double
			store.DefaultStore = m //nolint:reassign // install test double

			done := make(chan error, 1)

			go func() {
				done <- handleDelayedVMs(
					context.Background(),
					ns,
					"2026-10-18T12:00:00Z",
					map[string]time.Duration{"timer": 10 * time.Millisecond},
					map[string]map[string]bool{"gated": {"gate": false}},
				)
			}()

			var err error

			select {
			case err = <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("the delayed VMs still wait although the experiment was stopped")
			}

			if !errors.Is(err, errStoppedWhileStarting) {
				t.Fatalf("handleDelayedVMs returned %v, want it to report the stop", err)
			}

			var delayErr DelayedVMError
			if errors.As(err, &delayErr) {
				t.Errorf("handleDelayedVMs reported VM %s failing to start (%v), want no VM start attempted", delayErr.VM, err)
			}

			if fake.c2Checks != 0 {
				t.Errorf("the C2 wait checked its gate %d times after the stop, want none", fake.c2Checks)
			}

			if calls := fake.calls(); calls != 0 {
				t.Errorf("ClearNamespace called %d time(s), want none: the stop already cleared it", calls)
			}
		})
	}
}
