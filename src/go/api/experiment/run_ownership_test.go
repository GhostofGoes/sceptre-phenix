package experiment_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"phenix/api/experiment"
	"phenix/app"
	"phenix/store"
	"phenix/util/notes"
)

const otherRun = "OTHER-RUN" // the start time of a run another process started

// useBoltStore makes a BoltDB in a temporary directory, holding cfg, the store
// until the test ends.
func useBoltStore(t *testing.T, cfg store.Config) {
	t.Helper()

	original := store.DefaultStore
	t.Cleanup(func() { store.DefaultStore = original }) //nolint:reassign // restore default

	db := store.NewBoltDB()

	if err := db.Init(store.Endpoint("bolt://" + filepath.Join(t.TempDir(), "store.bdb"))); err != nil {
		t.Fatalf("initializing BoltDB: %v", err)
	}

	store.DefaultStore = db //nolint:reassign // install test store

	if err := store.Create(&cfg); err != nil {
		t.Fatalf("storing experiment: %v", err)
	}
}

func storedStart(t *testing.T) string {
	t.Helper()

	exp, err := experiment.Get(startTestExp)
	if err != nil {
		t.Errorf("getting experiment: %v", err)

		return ""
	}

	return exp.Status.StartTime()
}

// setStoredStart stores the start test experiment with the given start time,
// as a stop ("") or a start in another process does.
func setStoredStart(t *testing.T, start string) {
	t.Helper()

	c, _ := store.NewConfig("experiment/" + startTestExp)

	err := store.Modify(c, func(c *store.Config) error {
		if c.Status == nil {
			c.Status = make(map[string]any)
		}

		c.Status["startTime"] = start

		return nil
	})
	if err != nil {
		t.Errorf("storing start time %q: %v", start, err)
	}
}

func registerStartApp(t *testing.T, rec *startRecord) {
	t.Helper()

	startAppOnce.Do(func() {
		err := app.RegisterUserApp(startTestApp, func() app.App { return startApp{rec: startAppRec} })
		if err != nil {
			t.Fatalf("registering test app: %v", err)
		}
	})

	startAppRec = rec
}

// TestStartLeavesARunStoppedElsewhereAlone is a regression test: the status
// writes around a synchronous start's post-start apps wrote its start time
// back over a stop from another process, so the start then stored the stopped
// experiment as running again, with no VMs, and reported success; once the
// other process had started it again, the start overwrote the new run, and a
// failed post-start app made the start stop that new run.
func TestStartLeavesARunStoppedElsewhereAlone(t *testing.T) {
	tests := []struct {
		name         string
		async        bool
		restart      bool  // the other process starts the experiment again
		postStartErr error // the post-start app then fails
		wantErr      []string
	}{
		{name: "stopped", wantErr: []string{"was stopped while it was starting"}},
		{name: "stopped and started again", restart: true, wantErr: []string{"was stopped while it was starting"}},
		{
			name:         "stopped and started again, then post-start fails",
			restart:      true,
			postStartErr: errors.New("post-start failed"),
			wantErr:      []string{"post-start failed", "was stopped while it was starting"},
		},
		{
			name:         "async: stopped and started again, then post-start fails",
			async:        true,
			restart:      true,
			postStartErr: errors.New("post-start failed"),
			wantErr:      []string{"post-start failed", "was stopped while it was starting"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := new(startMM)
			installStartDoubles(t, fake, startTestConfig(t, false, false))
			useBoltStore(t, startTestConfig(t, false, false))

			// The other process's stop.
			stopsElsewhere := 0
			rec := &startRecord{postStartErr: tt.postStartErr}
			rec.onPostStart = func() {
				if tt.restart || tt.async {
					// The async start's goroutine runs this app, so a Stop
					// in this process would wait for it: store what the
					// other process leaves instead.
					setStoredStart(t, "")
				} else if err := experiment.Stop(startTestExp); err != nil {
					t.Errorf("stopping the experiment from elsewhere: %v", err)
				}

				stopsElsewhere = rec.cleanups

				if tt.restart {
					setStoredStart(t, otherRun)
				}
			}

			registerStartApp(t, rec)

			opts := []experiment.StartOption{experiment.StartWithName(startTestExp)}

			errs := newStartErrors()
			if tt.async {
				opts = append(opts, experiment.StartWithErrorChannel(errs.ch))
			}

			err := experiment.Start(notes.Context(context.Background(), false), opts...)

			if tt.async {
				if err != nil {
					t.Fatalf("Start returned %v", err)
				}

				err = errors.Join(errs.wait(t)...)
			}

			for _, want := range tt.wantErr {
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Errorf("Start reported %v, want an error containing %q", err, want)
				}
			}

			want := ""
			if tt.restart {
				want = otherRun
			}

			if got := storedStart(t); got != want {
				t.Errorf("stored start time %q, want %q: the start wrote its ended run back", got, want)
			}

			if rec.cleanups != stopsElsewhere || fake.cleared != stopsElsewhere {
				t.Errorf(
					"cleanup apps ran %d times and the namespace was cleared %d times, want %d: the start tore down a run it no longer owned",
					rec.cleanups,
					fake.cleared,
					stopsElsewhere,
				)
			}
		})
	}
}

// TestStopWaitsForAFailedAsyncStartsOwnStop is a regression test: a failed
// asynchronous start (phenix ui) forgot its run before it stopped the
// experiment itself, so a user's stop during that stop did not wait for it,
// and both ran the cleanup apps and cleared the namespace.
func TestStopWaitsForAFailedAsyncStartsOwnStop(t *testing.T) {
	var (
		inCleanup = make(chan struct{})
		release   = make(chan struct{})
		once      sync.Once
	)

	rec := &startRecord{postStartErr: errors.New("post-start failed")}
	rec.onCleanup = func() {
		once.Do(func() {
			close(inCleanup)
			<-release
		})
	}

	registerStartApp(t, rec)

	fake := new(startMM)
	installStartDoubles(t, fake, startTestConfig(t, false, false))
	useBoltStore(t, startTestConfig(t, false, false))

	errs := newStartErrors()

	err := experiment.Start(
		notes.Context(context.Background(), false),
		experiment.StartWithName(startTestExp),
		experiment.StartWithErrorChannel(errs.ch),
	)
	if err != nil {
		t.Fatalf("Start returned %v", err)
	}

	select {
	case <-inCleanup:
	case <-time.After(10 * time.Second):
		t.Fatal("the failed start did not stop the experiment")
	}

	stopped := make(chan error, 1)

	go func() { stopped <- experiment.Stop(startTestExp) }()

	select {
	case err := <-stopped:
		t.Fatalf("the user's Stop returned %v while the start's own stop still ran", err)
	case <-time.After(300 * time.Millisecond):
	}

	close(release)

	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("the user's Stop did not return")
	}

	errs.wait(t)

	if rec.cleanups != 1 || fake.cleared != 1 {
		t.Errorf("cleanup apps ran %d times and the namespace was cleared %d times, want once", rec.cleanups, fake.cleared)
	}

	if got := storedStart(t); got != "" {
		t.Errorf("stored start time %q, want the experiment stopped", got)
	}
}

// TestStopTimeoutLeavesTheExperimentStopped is a regression test: when an
// asynchronous start's post-start app ignored the cancellation for longer than
// Stop waits, the status write after the app returned stored the stopped
// experiment as running again.
func TestStopTimeoutLeavesTheExperimentStopped(t *testing.T) {
	experiment.SetAsyncStartStopWait(t, 100*time.Millisecond)

	var (
		inPostStart = make(chan struct{})
		once        sync.Once
	)

	rec := &startRecord{postStartSleep: time.Second}
	rec.onPostStart = func() { once.Do(func() { close(inPostStart) }) }

	registerStartApp(t, rec)

	fake := new(startMM)
	installStartDoubles(t, fake, startTestConfig(t, false, false))
	useBoltStore(t, startTestConfig(t, false, false))

	errs := newStartErrors()

	err := experiment.Start(
		notes.Context(context.Background(), false),
		experiment.StartWithName(startTestExp),
		experiment.StartWithErrorChannel(errs.ch),
	)
	if err != nil {
		t.Fatalf("Start returned %v", err)
	}

	select {
	case <-inPostStart:
	case <-time.After(10 * time.Second):
		t.Fatal("the start did not run its post-start app")
	}

	if err := experiment.Stop(startTestExp); err != nil {
		t.Fatalf("Stop returned %v", err)
	}

	if errs.returned() {
		t.Fatal("the post-start app returned before Stop gave up waiting for it")
	}

	if got := storedStart(t); got != "" {
		t.Fatalf("stored start time %q after Stop, want the experiment stopped", got)
	}

	errs.wait(t)

	if got := storedStart(t); got != "" {
		t.Errorf("stored start time %q once the post-start app returned, want the experiment stopped", got)
	}
}
