package app_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang/mock/gomock"

	"phenix/app"
	"phenix/store"
	"phenix/types"
)

const periodicTestApp = "periodic-test-app"

// periodicRecord records the running stages a periodic app ran.
type periodicRecord struct {
	mu     sync.Mutex
	runs   int
	dryRun []bool // per run, whether the app was initialized for a dry run
}

func (r *periodicRecord) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.runs
}

type periodicApp struct {
	rec    *periodicRecord
	dryRun bool
}

func (a *periodicApp) Init(opts ...app.Option) error {
	a.dryRun = app.NewOptions(opts...).DryRun

	return nil
}

func (a *periodicApp) Name() string                                       { return periodicTestApp }
func (a *periodicApp) Configure(context.Context, *types.Experiment) error { return nil }
func (a *periodicApp) PreStart(context.Context, *types.Experiment) error  { return nil }
func (a *periodicApp) PostStart(context.Context, *types.Experiment) error { return nil }
func (a *periodicApp) Cleanup(context.Context, *types.Experiment) error   { return nil }

func (a *periodicApp) Running(context.Context, *types.Experiment) error {
	a.rec.mu.Lock()
	defer a.rec.mu.Unlock()

	a.rec.runs++
	a.rec.dryRun = append(a.rec.dryRun, a.dryRun)

	return nil
}

//nolint:gochecknoglobals // the app registry is global, so the app is registered once
var (
	periodicAppOnce sync.Once
	periodicAppRec  *periodicRecord
)

// registerPeriodicApp registers the periodic test app once; each test sets
// periodicAppRec to the record its runs go to.
func registerPeriodicApp(t *testing.T) {
	t.Helper()

	periodicAppOnce.Do(func() {
		err := app.RegisterUserApp(periodicTestApp, func() app.App { return &periodicApp{rec: periodicAppRec} })
		if err != nil {
			t.Fatalf("registering test app: %v", err)
		}
	})
}

// TestPeriodicAppsFollowTheStoredRun is a regression test: the goroutine that
// runs an app's running stage periodically kept the experiment it was given,
// so after a stop from another process (phenix experiment stop while phenix ui
// ran the app) it wrote that copy's start time back, storing the stopped
// experiment as running again, and kept running the app; it also ran the app
// of a dry run without the dry-run option.
func TestPeriodicAppsFollowTheStoredRun(t *testing.T) {
	t.Cleanup(app.SetMinPeriod(time.Millisecond))

	registerPeriodicApp(t)

	for _, start := range []string{"2026-10-19T00:00:00Z", "2026-10-19T00:00:00Z-DRYRUN"} {
		t.Run(start, func(t *testing.T) {
			rec := new(periodicRecord)
			periodicAppRec = rec

			const name = "periodic-test"

			useBoltStore(t, store.Config{
				Version:  "phenix.sandia.gov/v1",
				Kind:     "Experiment",
				Metadata: store.ConfigMetadata{Name: name},
				Spec: map[string]any{
					"experimentName": name,
					"scenario": map[string]any{
						"apps": []map[string]any{{"name": periodicTestApp, "runPeriodically": "10ms"}},
					},
				},
				Status: map[string]any{"startTime": start},
			})

			exp := readExperiment(t, name)

			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)

			var wg sync.WaitGroup

			if err := app.PeriodicallyRunApps(ctx, &wg, exp); err != nil {
				t.Fatalf("PeriodicallyRunApps: %v", err)
			}

			waitFor(t, "the app's first periodic run", func() bool { return rec.count() > 0 })

			// Another process stops the experiment.
			modifyStatus(t, name, func(status map[string]any) { status["startTime"] = "" })

			done := make(chan struct{})

			go func() {
				wg.Wait()
				close(done)
			}()

			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("the periodic app kept running after the experiment was stopped")
			}

			if got := readExperiment(t, name).Status.StartTime(); got != "" {
				t.Errorf("stored start time %q after the stop, want the experiment stopped", got)
			}

			rec.mu.Lock()
			defer rec.mu.Unlock()

			for i, dryRun := range rec.dryRun {
				if want := exp.DryRun(); dryRun != want {
					t.Errorf("run %d initialized the app with dry run %t, want %t", i, dryRun, want)
				}
			}
		})
	}
}

// TestPeriodicAppRunsAgainAfterASkippedTick is a regression test: a tick that
// found the app already executing its running stage, for example triggered by
// hand, skipped it without rearming the timer, so the app never ran
// periodically again, and ending the periodic runs then drained the fired
// timer, which blocks forever since Go 1.23.
func TestPeriodicAppRunsAgainAfterASkippedTick(t *testing.T) {
	t.Cleanup(app.SetMinPeriod(time.Millisecond))

	registerPeriodicApp(t)

	rec := new(periodicRecord)
	periodicAppRec = rec

	const (
		name      = "periodic-skip-test"
		frequency = "200ms"
	)

	useBoltStore(t, store.Config{
		Version:  "phenix.sandia.gov/v1",
		Kind:     "Experiment",
		Metadata: store.ConfigMetadata{Name: name},
		Spec: map[string]any{
			"experimentName": name,
			"scenario": map[string]any{
				"apps": []map[string]any{{"name": periodicTestApp, "runPeriodically": frequency}},
			},
		},
		Status: map[string]any{"startTime": "2026-10-19T00:00:00Z"},
	})

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	var wg sync.WaitGroup

	if err := app.PeriodicallyRunApps(ctx, &wg, readExperiment(t, name)); err != nil {
		t.Fatalf("PeriodicallyRunApps: %v", err)
	}

	setRunning := func(running bool) {
		t.Helper()

		modifyStatus(t, name, func(status map[string]any) {
			status["appRunningStageStatus"] = map[string]any{periodicTestApp: running}
		})
	}

	// Once scheduled, the app is triggered by hand and still runs.
	waitFor(t, "the app to be scheduled", func() bool {
		return readExperiment(t, name).Status.AppFrequency()[periodicTestApp] == frequency
	})
	setRunning(true)

	time.Sleep(500 * time.Millisecond)

	if n := rec.count(); n != 0 {
		t.Fatalf("the periodic app ran %d times while it was already running", n)
	}

	setRunning(false)

	waitFor(t, "a periodic run after the skipped ticks", func() bool { return rec.count() > 0 })

	waitForPeriodicRuns(t, cancel, &wg)
}

// TestPeriodicAppSurvivesAFailedFirstRead is a regression test: a store read
// that failed as the periodic app was scheduled ended its periodic runs for
// good, although later reads succeed.
func TestPeriodicAppSurvivesAFailedFirstRead(t *testing.T) {
	t.Cleanup(app.SetMinPeriod(time.Millisecond))

	registerPeriodicApp(t)

	rec := new(periodicRecord)
	periodicAppRec = rec

	const name = "periodic-read-test"

	cfg := store.Config{
		Version:  "phenix.sandia.gov/v1",
		Kind:     "Experiment",
		Metadata: store.ConfigMetadata{Name: name},
		Spec: map[string]any{
			"experimentName": name,
			"scenario": map[string]any{
				"apps": []map[string]any{{"name": periodicTestApp, "runPeriodically": "10ms"}},
			},
		},
		Status: map[string]any{"startTime": "2026-10-19T00:00:00Z"},
	}

	exp, err := types.DecodeExperimentFromConfig(cfg)
	if err != nil {
		t.Fatalf("decoding experiment: %v", err)
	}

	var (
		mu    sync.Mutex
		reads atomic.Int32
	)

	ctrl := gomock.NewController(t)
	m := store.NewMockStore(ctrl)
	m.EXPECT().Get(gomock.Any()).DoAndReturn(func(c *store.Config) error {
		if reads.Add(1) == 1 {
			return errors.New("store unavailable")
		}

		mu.Lock()
		defer mu.Unlock()

		*c = cfg

		return nil
	}).AnyTimes()
	m.EXPECT().Update(gomock.Any()).DoAndReturn(func(c *store.Config) error {
		mu.Lock()
		defer mu.Unlock()

		cfg = *c

		return nil
	}).AnyTimes()

	original := store.DefaultStore
	t.Cleanup(func() { store.DefaultStore = original }) //nolint:reassign // restore default

	store.DefaultStore = m //nolint:reassign // install test double

	ctx, cancel := context.WithCancel(context.Background())

	var wg sync.WaitGroup

	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})

	if err := app.PeriodicallyRunApps(ctx, &wg, exp); err != nil {
		t.Fatalf("PeriodicallyRunApps: %v", err)
	}

	waitFor(t, "a periodic run after the failed first read", func() bool { return rec.count() > 0 })
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)

	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}

		time.Sleep(5 * time.Millisecond)
	}
}
