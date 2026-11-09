package experiment_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/go-multierror"

	"phenix/api/experiment"
	"phenix/app"
	"phenix/util/common"
	"phenix/util/notes"
)

// orderMM is the minimega double of a start; it records, for each clear of
// the namespace, how many times the cleanup apps had run before it.
type orderMM struct {
	*startMM

	rec *startRecord

	mu    sync.Mutex
	clear []int // cleanups that ran before each clear
}

func (m *orderMM) ClearNamespace(ns string) error {
	m.rec.mu.Lock()
	cleanups := m.rec.cleanups
	m.rec.mu.Unlock()

	m.mu.Lock()
	m.clear = append(m.clear, cleanups)
	m.mu.Unlock()

	return m.startMM.ClearNamespace(ns)
}

// TestFailedDelayedStartIsTornDownOnce is a regression test: when a delayed VM
// failed to start, the delayed start cleared the namespace itself and then
// stopped the experiment, so the cleanup apps ran against a cleared namespace,
// any that asked minimega created it again, and Stop cleared it a second time.
// Stop tears the failed start down alone: cleanup apps first, then one clear.
func TestFailedDelayedStartIsTornDownOnce(t *testing.T) {
	startAppOnce.Do(func() {
		err := app.RegisterUserApp(startTestApp, func() app.App { return startApp{rec: startAppRec} })
		if err != nil {
			t.Fatalf("registering test app: %v", err)
		}
	})

	// The timer VM's start fails: there is no minimega to send it to.
	originalBase := common.MinimegaBase

	t.Cleanup(func() { common.MinimegaBase = originalBase }) //nolint:reassign // restore test fixture

	common.MinimegaBase = t.TempDir() //nolint:reassign // test fixture

	for _, async := range []bool{false, true} {
		name := "phenix experiment start"
		if async {
			name = "phenix ui"
		}

		t.Run(name, func(t *testing.T) {
			rec := new(startRecord)
			startAppRec = rec

			fake := &orderMM{startMM: new(startMM), rec: rec}

			cfg := startTestConfig(t, false, true)

			topo, _ := cfg.Spec["topology"].(map[string]any)
			nodes, _ := topo["nodes"].([]map[string]any)
			nodes[1]["delay"] = map[string]any{"timer": "1s"}

			installStartDoubles(t, fake, cfg)

			opts := []experiment.StartOption{experiment.StartWithName(startTestExp)}

			var errs chan error
			if async {
				errs = make(chan error)
				opts = append(opts, experiment.StartWithErrorChannel(errs))
			}

			err := experiment.Start(notes.Context(context.Background(), false), opts...)

			if async {
				if err != nil {
					t.Fatalf("Start returned error: %v", err)
				}

				var got error

				for {
					select {
					case e, ok := <-errs:
						if ok {
							got = multierror.Append(got, e)

							continue
						}
					case <-time.After(30 * time.Second):
						t.Fatal("the start's goroutine did not return")
					}

					break
				}

				err = got
			}

			if err == nil || !strings.Contains(err.Error(), "starting VM timer") {
				t.Fatalf("the start reported %v, want the timer VM's failed start", err)
			}

			if rec.cleanups != 1 {
				t.Errorf("cleanup apps ran %d times, want once", rec.cleanups)
			}

			if len(fake.clear) != 1 || fake.clear[0] != 1 {
				t.Errorf("the namespace was cleared %d times, after %v cleanups each; want once, after the cleanup apps ran",
					len(fake.clear), fake.clear)
			}
		})
	}
}
