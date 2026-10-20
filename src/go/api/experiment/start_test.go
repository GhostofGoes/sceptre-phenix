package experiment_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang/mock/gomock"

	"phenix/api/experiment"
	"phenix/app"
	"phenix/store"
	"phenix/types"
	"phenix/util/file"
	"phenix/util/mm"
	"phenix/util/notes"
)

const (
	startTestExp = "start-test"
	startTestApp = "start-test-app"
)

// startMM is a test double for the minimega calls a synchronous start makes.
// Embedding the (nil) mm.MM interface makes any other call panic.
type startMM struct {
	mm.MM

	ccErr    error
	onLaunch func()

	mu      sync.Mutex
	cleared int
}

func (m *startMM) ReadScriptFromFile(_, filename string) error {
	if strings.HasSuffix(filename, "-cc.mm") {
		return m.ccErr
	}

	return nil
}

func (m *startMM) ClearNamespace(string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.cleared++

	return nil
}

func (m *startMM) LaunchVMs(string, ...string) error {
	if m.onLaunch != nil {
		m.onLaunch()
	}

	return nil
}

func (*startMM) GetVMInfo(...mm.Option) mm.VMs {
	return mm.VMs{{Name: "idle", State: "RUNNING"}, {Name: "timer", State: "BUILDING"}}
}

func (*startMM) GetVLANs(...mm.Option) (map[string]int, error) {
	return map[string]int{}, nil
}

func (*startMM) Headnode() string {
	return "head"
}

type startFiles struct {
	file.ClusterFiles
}

func (startFiles) DeleteFile(string) error {
	return nil
}

// startApp is a scenario app that records the stages it ran for one subtest.
type startApp struct {
	rec *startRecord
}

type startRecord struct {
	postStartErr   error
	postStartSleep time.Duration // a post-start app's work that ignores cancellation
	onPostStart    func()        // runs as the post-start stage begins
	cleanupErr     error
	onCleanup      func() // runs as the cleanup stage begins

	mu                 sync.Mutex
	runningAtPostStart bool
	cleanups           int
}

func (a startApp) Name() string                                       { return startTestApp }
func (a startApp) Init(...app.Option) error                           { return nil }
func (a startApp) Configure(context.Context, *types.Experiment) error { return nil }
func (a startApp) PreStart(context.Context, *types.Experiment) error  { return nil }
func (a startApp) Running(context.Context, *types.Experiment) error   { return nil }

func (a startApp) PostStart(context.Context, *types.Experiment) error {
	if a.rec.onPostStart != nil {
		a.rec.onPostStart()
	}

	time.Sleep(a.rec.postStartSleep)

	a.rec.mu.Lock()
	defer a.rec.mu.Unlock()

	a.rec.runningAtPostStart = experiment.Running(startTestExp)

	return a.rec.postStartErr
}

func (a startApp) Cleanup(context.Context, *types.Experiment) error {
	if a.rec.onCleanup != nil {
		a.rec.onCleanup()
	}

	a.rec.mu.Lock()
	defer a.rec.mu.Unlock()

	a.rec.cleanups++

	return a.rec.cleanupErr
}

//nolint:gochecknoglobals // the app registry is global, so the app is registered once
var (
	startAppOnce sync.Once
	startAppRec  *startRecord
)

// startTestConfig returns an experiment with the nodes idle and timer, whose
// start is delayed by a minute when delayed is set.
func startTestConfig(t *testing.T, commands, delayed bool) store.Config {
	t.Helper()

	image := filepath.Join(t.TempDir(), "disk.qc2")
	if err := os.WriteFile(image, []byte("disk"), 0o600); err != nil {
		t.Fatal(err)
	}

	var delay map[string]any
	if delayed {
		delay = map[string]any{"timer": "1m"}
	}

	node := func(hostname string, delay map[string]any) map[string]any {
		n := map[string]any{
			"type":     "VirtualMachine",
			"general":  map[string]any{"hostname": hostname},
			"hardware": map[string]any{"os_type": "linux", "drives": []map[string]any{{"image": image}}},
			"network":  map[string]any{"interfaces": []map[string]any{}},
		}

		if delay != nil {
			n["delay"] = delay
		}

		if commands {
			n["commands"] = []string{"exec echo " + hostname}
		}

		return n
	}

	return store.Config{
		Version:  "phenix.sandia.gov/v1",
		Kind:     "Experiment",
		Metadata: store.ConfigMetadata{Name: startTestExp},
		Spec: map[string]any{
			"experimentName": startTestExp,
			"baseDir":        t.TempDir(),
			"topology": map[string]any{
				"nodes": []map[string]any{node("idle", nil), node("timer", delay)},
			},
			"scenario": map[string]any{"apps": []map[string]any{{"name": startTestApp}}},
		},
	}
}

// TestStartSyncPersistsStartTimeAndCleansUpOnFailure is a regression test: a
// synchronous start (the CLI) stored the start time only after the delayed
// VMs had started and the post-start apps had run, so the experiment was
// reported stopped while its VMs ran, and a failed cc script, a failed or
// interrupted delayed start cleared the namespace without running the
// cleanup apps for what pre-start had set up.
func TestStartSyncPersistsStartTimeAndCleansUpOnFailure(t *testing.T) {
	startAppOnce.Do(func() {
		err := app.RegisterUserApp(startTestApp, func() app.App { return startApp{rec: startAppRec} })
		if err != nil {
			t.Fatalf("registering test app: %v", err)
		}
	})

	tests := []struct {
		name      string
		interrupt bool  // cancel the start while the timer VM waits its delay
		ccErr     error // the cc script fails
		postErr   error // the post-start app fails
		wantErr   string
	}{
		{name: "success"},
		{name: "interrupted while a delayed VM waits", interrupt: true, wantErr: context.Canceled.Error()},
		{name: "cc script fails", ccErr: errors.New("bad cc script"), wantErr: "bad cc script"},
		{name: "post-start app fails", postErr: errors.New("post-start failed"), wantErr: "post-start failed"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(notes.Context(context.Background(), false))
			t.Cleanup(cancel)

			rec := &startRecord{postStartErr: tt.postErr}
			startAppRec = rec

			fake := &startMM{ccErr: tt.ccErr}
			if tt.interrupt {
				fake.onLaunch = cancel
			}

			originalMM, originalFiles, originalStore := mm.DefaultMM, file.DefaultClusterFiles, store.DefaultStore

			t.Cleanup(func() {
				mm.DefaultMM = originalMM                //nolint:reassign // restore test double
				file.DefaultClusterFiles = originalFiles //nolint:reassign // restore test double
				store.DefaultStore = originalStore       //nolint:reassign // restore test double
			})

			mm.DefaultMM = fake                     //nolint:reassign // install test double
			file.DefaultClusterFiles = startFiles{} //nolint:reassign // install test double

			var (
				ctrl    = gomock.NewController(t)
				mu      sync.Mutex
				current = startTestConfig(t, tt.ccErr != nil, tt.interrupt)
				starts  []string // the start time of every stored update
			)

			m := store.NewMockStore(ctrl)
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

				exp, err := types.DecodeExperimentFromConfig(current)
				if err != nil {
					t.Errorf("decoding stored experiment: %v", err)

					return err
				}

				starts = append(starts, exp.Status.StartTime())

				return nil
			}).AnyTimes()

			store.DefaultStore = m //nolint:reassign // install test double

			err := experiment.Start(ctx, experiment.StartWithName(startTestExp))

			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Start returned error: %v", err)
				}

				if !rec.runningAtPostStart {
					t.Error("the experiment was not stored as running when the post-start apps ran")
				}

				if last := starts[len(starts)-1]; last == "" {
					t.Errorf("stored start times %q, want the experiment stored running", starts)
				}

				if rec.cleanups != 0 {
					t.Errorf("cleanup apps ran %d times after a successful start", rec.cleanups)
				}

				return
			}

			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Start returned %v, want an error containing %q", err, tt.wantErr)
			}

			if rec.cleanups != 1 {
				t.Errorf("cleanup apps ran %d times, want once", rec.cleanups)
			}

			if fake.cleared == 0 {
				t.Error("the namespace was not cleared")
			}

			// The experiment was stored running before the failure, and stopped
			// after it.
			last := len(starts) - 1
			running := func(start string) bool { return start != "" }

			if last < 1 || !slices.ContainsFunc(starts[:last], running) || starts[last] != "" {
				t.Errorf("stored start times %q, want a start time, then none once the start failed", starts)
			}
		})
	}
}
