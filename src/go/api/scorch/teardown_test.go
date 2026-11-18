package scorch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"phenix/api/scorch/scorchexe"
	"phenix/api/scorch/scorchmd"
	"phenix/app"
	"phenix/store"
	"phenix/types"
	"phenix/web/scorch"
)

// TestCanceledPipelineStillRunsStopAndCleanup is a regression test: when a
// pipeline was canceled during its start stage, every user component's stop
// and cleanup returned the context's error without running, so components
// left captures, QoS rules and guest processes behind.
func TestCanceledPipelineStillRunsStopAndCleanup(t *testing.T) {
	dir := t.TempDir()
	record := filepath.Join(dir, "stages")

	// A user component that records every stage it runs and hangs in start
	// until it is terminated.
	script := "#!/bin/sh\n" +
		"cat >/dev/null\n" +
		"echo \"$1\" >> " + record + "\n" +
		"if [ \"$1\" = start ]; then trap 'exit 0' TERM; while :; do sleep 0.05; done; fi\n"

	if err := os.WriteFile(
		filepath.Join(dir, "phenix-scorch-component-recorder"),
		[]byte(script),
		0o700,
	); err != nil { //nolint:gosec // test script
		t.Fatal(err)
	}

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	exp := types.NewExperiment(store.ConfigMetadata{Name: "test-exp"})
	exp.Spec.SetExperimentName("test-exp")

	components := scorchmd.ComponentSpecMap{
		"rec": {Name: "rec", Type: "recorder"},
	}

	loop := &scorchmd.Loop{
		Start:   []string{"rec"},
		Stop:    []string{"rec"},
		Cleanup: []string{"rec"},
	}

	ctx, cancel := context.WithCancel(context.Background())

	cancelOnRecord(t, record, "start", cancel)

	_ = executor(ctx, components, loop, Experiment(*exp), RunID(0))

	data, err := os.ReadFile(record) //nolint:gosec // test file
	if err != nil {
		t.Fatal(err)
	}

	if got := strings.Fields(string(data)); strings.Join(got, ",") != "start,stop,cleanup" {
		t.Fatalf("stages run = %v, want start, stop and cleanup", got)
	}
}

// TestCanceledPipelineErrors checks that scorchexe.Canceled is true for a run
// canceled in its start stage, whatever the component reports, and false when
// a stop stage then failed.
func TestCanceledPipelineErrors(t *testing.T) {
	dir := t.TempDir()
	started := filepath.Join(dir, "started")

	// A user component whose start stage runs until SIGTERM kills it, and
	// whose stop stage fails when its name says so.
	script := "#!/bin/sh\n" +
		"cat >/dev/null\n" +
		"if [ \"$1\" = start ]; then touch " + started + "; while :; do sleep 0.05; done; fi\n" +
		"if [ \"$1\" = stop ] && [ \"$2\" = badstop ]; then exit 4; fi\n"

	if err := os.WriteFile(
		filepath.Join(dir, "phenix-scorch-component-killed"),
		[]byte(script),
		0o700,
	); err != nil { //nolint:gosec // test script
		t.Fatal(err)
	}

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	exp := types.NewExperiment(store.ConfigMetadata{Name: "test-exp"})
	exp.Spec.SetExperimentName("test-exp")

	for name, wantCanceled := range map[string]bool{"goodstop": true, "badstop": false} {
		t.Run(name, func(t *testing.T) {
			_ = os.Remove(started)

			components := scorchmd.ComponentSpecMap{name: {Name: name, Type: "killed"}}
			loop := &scorchmd.Loop{Start: []string{name}, Stop: []string{name}}

			ctx, cancel := context.WithCancel(context.Background())

			go func() {
				for range 500 {
					if _, err := os.Stat(started); err == nil {
						break
					}

					time.Sleep(20 * time.Millisecond)
				}

				cancel()
			}()

			err := executor(ctx, components, loop, Experiment(*exp), RunID(0))
			if err == nil {
				t.Fatal("executor() of a canceled pipeline = nil, want the start stage's cancel")
			}

			if got := scorchexe.Canceled(err); got != wantCanceled {
				t.Errorf("scorchexe.Canceled(%v) = %t, want %t", err, got, wantCanceled)
			}
		})
	}
}

// installTeardownComponent puts the user component type "td" on PATH and
// returns the file it records its stages in. Its components named "slow*"
// take a second in their stop stage. Those named "hang*" run until SIGTERM,
// for 20 seconds at most, in every stage.
func installTeardownComponent(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	record := filepath.Join(dir, "stages")

	script := "#!/bin/sh\n" +
		"cat >/dev/null\n" +
		"rec() { echo \"$2-$1-$3\" >> " + record + "; }\n" +
		"rec \"$1\" \"$2\" begin\n" +
		"case \"$2\" in\n" +
		"slow*) if [ \"$1\" = stop ]; then sleep 1; fi ;;\n" +
		"hang*) trap 'rec \"$1\" \"$2\" terminated; exit 0' TERM\n" +
		"  i=0; while [ $i -lt 400 ]; do sleep 0.05; i=$((i+1)); done ;;\n" +
		"esac\n" +
		"rec \"$1\" \"$2\" done\n"

	//nolint:gosec // test script must be executable
	if err := os.WriteFile(filepath.Join(dir, "phenix-scorch-component-td"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	return record
}

// recorded returns the lines of the record file.
func recorded(t *testing.T, record string) []string {
	t.Helper()

	data, err := os.ReadFile(record) //nolint:gosec // test file
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}

	return strings.Fields(string(data))
}

// cancelOnRecord cancels once the record file holds line.
func cancelOnRecord(t *testing.T, record, line string, cancel context.CancelFunc) {
	t.Helper()

	go func() {
		defer cancel()

		for range 500 {
			if data, _ := os.ReadFile(record); slices.Contains(strings.Fields(string(data)), line) { //nolint:gosec // test file
				return
			}

			time.Sleep(20 * time.Millisecond)
		}
	}()
}

// runExecutor runs executor, failing the test if it has not returned within
// 15 seconds.
func runExecutor(t *testing.T, ctx context.Context, components scorchmd.ComponentSpecMap, loop *scorchmd.Loop) error {
	t.Helper()

	exp := types.NewExperiment(store.ConfigMetadata{Name: "test-exp"})
	exp.Spec.SetExperimentName("test-exp")

	done := make(chan error, 1)

	go func() { done <- executor(ctx, components, loop, Experiment(*exp), RunID(0)) }()

	select {
	case err := <-done:
		return err
	case <-time.After(15 * time.Second):
		t.Fatal("executor() did not return within 15 seconds of its start")

		return nil
	}
}

// TestCancelDuringTeardown checks that a cancel during a stop or cleanup stage
// is reported, that a hung stage is ended when the teardown budget
// (teardownTimeout) runs out, and that one budget covers every loop level.
func TestCancelDuringTeardown(t *testing.T) {
	original := teardownTimeout
	t.Cleanup(func() { teardownTimeout = original })

	t.Run("a stage that finishes reports the cancel", func(t *testing.T) {
		record := installTeardownComponent(t)

		components := scorchmd.ComponentSpecMap{"slow1": {Name: "slow1", Type: "td"}}
		loop := &scorchmd.Loop{Stop: []string{"slow1"}}

		ctx, cancel := context.WithCancel(context.Background())
		cancelOnRecord(t, record, "slow1-stop-begin", cancel)

		err := runExecutor(t, ctx, components, loop)
		if !scorchexe.Canceled(err) {
			t.Errorf("executor() of a run canceled during its stop stage = %v, want the cancel", err)
		}

		if got := recorded(t, record); !slices.Contains(got, "slow1-stop-done") {
			t.Errorf("component recorded %v, want its stop stage to finish after the cancel", got)
		}
	})

	t.Run("a hung stage is ended", func(t *testing.T) {
		record := installTeardownComponent(t)
		teardownTimeout = 300 * time.Millisecond

		components := scorchmd.ComponentSpecMap{"hang1": {Name: "hang1", Type: "td"}}
		loop := &scorchmd.Loop{Stop: []string{"hang1"}}

		ctx, cancel := context.WithCancel(context.Background())
		cancelOnRecord(t, record, "hang1-stop-begin", cancel)

		err := runExecutor(t, ctx, components, loop)
		if err == nil || scorchexe.Canceled(err) || !errors.Is(err, errTeardownExpired) {
			t.Errorf("executor() of a run whose stop stage outlived the teardown budget = %v, want %v", err, errTeardownExpired)
		}

		if got := recorded(t, record); !slices.Contains(got, "hang1-stop-terminated") {
			t.Errorf("component recorded %v, want its hung stop stage terminated", got)
		}
	})

	t.Run("one budget covers every loop level", func(t *testing.T) {
		record := installTeardownComponent(t)
		teardownTimeout = time.Second

		components := scorchmd.ComponentSpecMap{
			"hangstart": {Name: "hangstart", Type: "td"},
			"hanginner": {Name: "hanginner", Type: "td"},
			"hangouter": {Name: "hangouter", Type: "td"},
		}

		loop := &scorchmd.Loop{
			Stop: []string{"hangouter"},
			Loop: &scorchmd.Loop{Count: 1, Start: []string{"hangstart"}, Stop: []string{"hanginner"}},
		}

		ctx, cancel := context.WithCancel(context.Background())
		cancelOnRecord(t, record, "hangstart-start-begin", cancel)

		if err := runExecutor(t, ctx, components, loop); !errors.Is(err, errTeardownExpired) {
			t.Errorf("executor() = %v, want %v", err, errTeardownExpired)
		}

		got := recorded(t, record)

		if !slices.Contains(got, "hanginner-stop-terminated") {
			t.Errorf("component recorded %v, want the inner stop stage ended by the budget", got)
		}

		// The inner stop stage used up the budget, so the outer one gets none.
		if slices.Contains(got, "hangouter-stop-begin") {
			t.Errorf("component recorded %v, want no outer stop stage after the budget ran out", got)
		}
	})
}

// TestPauseAndBreakEndOnCancel checks that a cancel ends a pause and a break
// in every stage, stop and cleanup included, although those stages run under
// a context that the cancel does not end. A pause or break releases nothing,
// so nothing is lost by ending it, and a canceled run opens no web terminal.
func TestPauseAndBreakEndOnCancel(t *testing.T) {
	original := teardownTimeout
	t.Cleanup(func() { teardownTimeout = original })

	teardownTimeout = 3 * time.Second

	pause := scorchmd.ComponentMetadata{"duration": "5s"}

	tests := []struct {
		name       string
		components scorchmd.ComponentSpecMap
		stop       []string
		cleanup    []string
	}{
		{
			"pause",
			scorchmd.ComponentSpecMap{
				"p1": {Name: "p1", Type: "pause", Metadata: pause},
				"p2": {Name: "p2", Type: "pause", Metadata: pause},
			},
			[]string{"p1"},
			[]string{"p2"},
		},
		{
			"break",
			scorchmd.ComponentSpecMap{"brk": {Name: "brk", Type: "break"}},
			[]string{"brk"},
			nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			record := installTeardownComponent(t)

			tt.components["hangstart"] = scorchmd.ComponentSpec{Name: "hangstart", Type: "td"}
			loop := &scorchmd.Loop{Start: []string{"hangstart"}, Stop: tt.stop, Cleanup: tt.cleanup}

			ctx, cancel := context.WithCancel(context.Background())
			cancelOnRecord(t, record, "hangstart-start-begin", cancel)

			// As phenix ui starts a run, so a break opens a web terminal.
			err := runExecutor(t, app.SetContextTriggerUI(ctx), tt.components, loop)
			if !scorchexe.Canceled(err) {
				t.Errorf("executor() of a run canceled during its start stage = %v, want only the cancel", err)
			}

			if terms, _ := scorch.GetExperimentTerminals("test-exp", -1); len(terms) != 0 {
				t.Errorf("the canceled run opened web terminals %v, want none", terms)
			}
		})
	}
}
