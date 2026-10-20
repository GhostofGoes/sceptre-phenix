package scorch

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"phenix/api/scorch/scorchmd"
	"phenix/store"
	"phenix/types"
)

// TestBackgroundedConcurrentRuns is a regression test: the table of
// backgrounded components was written without a lock, so the runs of two
// experiments that backgrounded components at the same time in phenix ui
// could crash it with "concurrent map writes". Run it with -race.
func TestBackgroundedConcurrentRuns(t *testing.T) {
	var wg sync.WaitGroup

	for i := range 8 {
		wg.Add(1)

		go func() {
			defer wg.Done()

			name := fmt.Sprintf("exp-%d", i)
			exp := types.NewExperiment(store.ConfigMetadata{Name: name})
			exp.Spec.SetExperimentName(name)

			options := NewOptions(Experiment(*exp), Name("bg"), Background())

			for range 50 {
				ended := make(chan struct{})

				runInBackground(context.Background(), ActionStart, options, func(ctx context.Context) error {
					<-ctx.Done()
					close(ended)

					return nil
				})

				if handled, err := handleBackgrounded(context.Background(), ActionStop, options); !handled || err != nil {
					t.Errorf("%s: the stop stage found a backgrounded start stage %t (%v), want true", name, handled, err)

					return
				}

				select {
				case <-ended:
				default:
					t.Errorf("%s: the stop stage returned before the backgrounded start stage ended", name)

					return
				}
			}
		}()
	}

	wg.Wait()
}

// TestStopWaitsForBackgroundedStage is a regression test: a stop stage
// canceled the component's backgrounded start stage and went on at once, so
// the run archived its files, and a later component read them, before the
// start stage had finished acting on its SIGTERM.
func TestStopWaitsForBackgroundedStage(t *testing.T) {
	dir := t.TempDir()
	record := filepath.Join(dir, "stages")

	// A user component whose start stage runs until SIGTERM and takes a
	// second to act on it.
	script := "#!/bin/sh\n" +
		"cat >/dev/null\n" +
		"if [ \"$1\" = start ]; then\n" +
		"  trap 'sleep 1; echo terminated >> " + record + "; exit 0' TERM\n" +
		"  echo started >> " + record + "\n" +
		"  while :; do sleep 0.05; done\n" +
		"fi\n" +
		"echo \"$1\" >> " + record + "\n"

	//nolint:gosec // test script must be executable
	if err := os.WriteFile(filepath.Join(dir, "phenix-scorch-component-sleeper"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	exp := types.NewExperiment(store.ConfigMetadata{Name: "test-exp"})
	exp.Spec.SetExperimentName("test-exp")

	components := scorchmd.ComponentSpecMap{
		"bg":    {Name: "bg", Type: "sleeper", Background: true},
		"wait1": {Name: "wait1", Type: "pause", Metadata: scorchmd.ComponentMetadata{"duration": "1s"}},
	}

	loop := &scorchmd.Loop{
		Start: []string{"bg", "wait1"},
		Stop:  []string{"bg"},
	}

	if err := executor(context.Background(), components, loop, Experiment(*exp), RunID(0)); err != nil {
		t.Fatalf("executor() = %v", err)
	}

	data, err := os.ReadFile(record) //nolint:gosec // test file
	if err != nil {
		t.Fatal(err)
	}

	if got := strings.Fields(string(data)); strings.Join(got, ",") != "started,terminated,stop" {
		t.Fatalf("component recorded %v when the run ended, want started, terminated, stop", got)
	}
}

// TestBackgroundedStageFailure is a regression test: the error of a
// backgrounded stage was dropped, so a component whose start stage failed in
// the background left the run successful. A stage that exits with an error
// once it is stopped still counts as stopped.
func TestBackgroundedStageFailure(t *testing.T) {
	dir := t.TempDir()

	// "fails" exits 3 at once; "dies" runs until SIGTERM kills it.
	scripts := map[string]string{
		"fails": "#!/bin/sh\ncat >/dev/null\n[ \"$1\" = start ] && exit 3\nexit 0\n",
		"dies":  "#!/bin/sh\ncat >/dev/null\nif [ \"$1\" = start ]; then while :; do sleep 0.05; done; fi\n",
	}

	for typ, script := range scripts {
		//nolint:gosec // test script must be executable
		if err := os.WriteFile(filepath.Join(dir, "phenix-scorch-component-"+typ), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	exp := types.NewExperiment(store.ConfigMetadata{Name: "test-exp"})
	exp.Spec.SetExperimentName("test-exp")

	tests := []struct {
		typ     string
		wantErr string // empty: the run succeeds
	}{
		{"fails", "backgrounded start stage of component bg failed"},
		{"dies", ""},
	}

	for _, tt := range tests {
		t.Run(tt.typ, func(t *testing.T) {
			components := scorchmd.ComponentSpecMap{
				"bg":    {Name: "bg", Type: tt.typ, Background: true},
				"wait1": {Name: "wait1", Type: "pause", Metadata: scorchmd.ComponentMetadata{"duration": "1s"}},
			}

			loop := &scorchmd.Loop{
				Start: []string{"bg", "wait1"},
				Stop:  []string{"bg"},
			}

			err := executor(context.Background(), components, loop, Experiment(*exp), RunID(0))

			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("executor() = %v, want nil", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Fatalf("executor() = %v, want an error containing %q", err, tt.wantErr)
			}
		})
	}
}

// TestLoopEndsItsBackgroundedStages checks that a loop cancels the
// backgrounded stages that no stop or cleanup stage named once its own stages
// are done, and leaves what an enclosing loop backgrounded to that loop.
func TestLoopEndsItsBackgroundedStages(t *testing.T) {
	dir := t.TempDir()
	record := filepath.Join(dir, "calls")

	// A user component whose start stage, when backgrounded, runs until
	// SIGTERM; it records each stage it runs and when it is terminated.
	script := "#!/bin/sh\n" +
		"cat >/dev/null\n" +
		"[ \"$1\" = start ] && [ \"$2\" != echo ] && background=1\n" +
		"[ -n \"$background\" ] && trap 'echo \"terminated $2\" >> " + record + "; exit 0' TERM\n" +
		"echo \"$1 $2 loop=$4\" >> " + record + "\n" +
		"if [ -n \"$background\" ]; then while :; do sleep 0.05; done; fi\n"

	//nolint:gosec // test script must be executable
	if err := os.WriteFile(filepath.Join(dir, "phenix-scorch-component-recorder"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	exp := types.NewExperiment(store.ConfigMetadata{Name: "test-exp"})
	exp.Spec.SetExperimentName("test-exp")

	components := scorchmd.ComponentSpecMap{
		"outer": {Name: "outer", Type: "recorder", Background: true},
		"inner": {Name: "inner", Type: "recorder", Background: true},
		"echo":  {Name: "echo", Type: "recorder"},
		"wait1": {Name: "wait1", Type: "pause", Metadata: scorchmd.ComponentMetadata{"duration": "1s"}},
	}

	// Neither outer nor inner is named by a stop or cleanup stage. The pause
	// gives both time to start.
	loop := &scorchmd.Loop{
		Start: []string{"outer"},
		Loop:  &scorchmd.Loop{Count: 1, Start: []string{"inner", "echo", "wait1"}},
		Stop:  []string{"echo"},
	}

	if err := executor(context.Background(), components, loop, Experiment(*exp), RunID(0)); err != nil {
		t.Fatalf("executor() = %v", err)
	}

	data, err := os.ReadFile(record) //nolint:gosec // test file
	if err != nil {
		t.Fatal(err)
	}

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")

	at := func(line string) int {
		i := slices.Index(lines, line)
		if i < 0 {
			t.Fatalf("component never recorded %q:\n%s", line, data)
		}

		return i
	}

	// The backgrounded stages start concurrently with the stages after them,
	// so only these orders are fixed.
	for _, order := range [][2]string{
		{"start inner loop=1", "terminated inner"},
		{"start echo loop=1", "terminated inner"},
		{"terminated inner", "stop echo loop=0"},
		{"start outer loop=0", "terminated outer"},
		{"stop echo loop=0", "terminated outer"},
	} {
		if at(order[0]) > at(order[1]) {
			t.Errorf("component recorded %q after %q:\n%s", order[0], order[1], data)
		}
	}

	backgroundedMu.Lock()
	defer backgroundedMu.Unlock()

	for stage, tasks := range backgrounded {
		if len(tasks) != 0 {
			t.Errorf("after the run, %d %s stages are still backgrounded", len(tasks), stage)
		}
	}
}

// TestBackgroundedInNestedLoops is a regression test: backgrounded stages
// were known by experiment, stage and component only, so a component
// backgrounded in an outer loop and again in an inner one lost the outer
// stage, which no stop stage then ended, and an inner loop's stop stage ended
// the outer loop's backgrounded stage.
func TestBackgroundedInNestedLoops(t *testing.T) {
	dir := t.TempDir()
	record := filepath.Join(dir, "stages")

	// A user component whose start stage runs until SIGTERM. It records each
	// stage with its loop.
	script := "#!/bin/sh\n" +
		"cat >/dev/null\n" +
		"if [ \"$1\" = start ]; then\n" +
		"  trap 'echo start-loop$4-terminated >> " + record + "; exit 0' TERM\n" +
		"  echo start-loop$4 >> " + record + "\n" +
		"  i=0; while [ $i -lt 400 ]; do sleep 0.05; i=$((i+1)); done\n" +
		"  exit 0\n" +
		"fi\n" +
		"echo $1-loop$4 >> " + record + "\n"

	//nolint:gosec // test script must be executable
	if err := os.WriteFile(filepath.Join(dir, "phenix-scorch-component-looprec"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	exp := types.NewExperiment(store.ConfigMetadata{Name: "test-exp"})
	exp.Spec.SetExperimentName("test-exp")

	// wait1 gives each backgrounded start stage time to begin.
	components := scorchmd.ComponentSpecMap{
		"bg":    {Name: "bg", Type: "looprec", Background: true},
		"wait1": {Name: "wait1", Type: "pause", Metadata: scorchmd.ComponentMetadata{"duration": "1s"}},
	}

	tests := []struct {
		name  string
		inner *scorchmd.Loop
		want  []string
	}{
		{
			"backgrounded in both loops",
			&scorchmd.Loop{Count: 1, Start: []string{"bg", "wait1"}, Stop: []string{"bg"}},
			[]string{
				"start-loop0", "start-loop1", "start-loop1-terminated", "stop-loop1",
				"start-loop0-terminated", "stop-loop0",
			},
		},
		{
			"stopped in the inner loop only",
			&scorchmd.Loop{Count: 1, Stop: []string{"bg"}},
			[]string{"start-loop0", "stop-loop1", "start-loop0-terminated", "stop-loop0"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_ = os.Remove(record)

			loop := &scorchmd.Loop{Start: []string{"bg", "wait1"}, Stop: []string{"bg"}, Loop: tt.inner}

			if err := executor(context.Background(), components, loop, Experiment(*exp), RunID(0)); err != nil {
				t.Fatalf("executor() = %v", err)
			}

			data, err := os.ReadFile(record) //nolint:gosec // test file
			if err != nil {
				t.Fatal(err)
			}

			if got := strings.Fields(string(data)); !slices.Equal(got, tt.want) {
				t.Errorf("component recorded %v, want %v", got, tt.want)
			}
		})
	}
}

// TestDuplicateBackgroundedComponent is a regression test: a stage that
// listed a backgrounded component twice replaced the first backgrounded
// stage with the second in the table that stop stages and the end of the
// loop end them from, so the first one outlived the run.
func TestDuplicateBackgroundedComponent(t *testing.T) {
	dir := t.TempDir()
	record := filepath.Join(dir, "calls")

	// A user component whose start stage runs until SIGTERM.
	script := "#!/bin/sh\n" +
		"cat >/dev/null\n" +
		"[ \"$1\" = start ] || exit 0\n" +
		"trap 'echo terminated >> " + record + "; exit 0' TERM\n" +
		"echo started >> " + record + "\n" +
		"while :; do sleep 0.05; done\n"

	//nolint:gosec // test script must be executable
	if err := os.WriteFile(filepath.Join(dir, "phenix-scorch-component-forever"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	// Whatever outlives the run.
	t.Cleanup(func() { _ = exec.Command("pkill", "-f", dir).Run() })

	exp := types.NewExperiment(store.ConfigMetadata{Name: "test-exp"})
	exp.Spec.SetExperimentName("test-exp")

	components := scorchmd.ComponentSpecMap{
		"bg":    {Name: "bg", Type: "forever", Background: true},
		"wait1": {Name: "wait1", Type: "pause", Metadata: scorchmd.ComponentMetadata{"duration": "500ms"}},
	}

	loop := &scorchmd.Loop{Start: []string{"bg", "wait1", "bg", "wait1"}, Stop: []string{"bg"}}

	if err := executor(context.Background(), components, loop, Experiment(*exp), RunID(0)); err != nil {
		t.Fatalf("executor() = %v", err)
	}

	data, err := os.ReadFile(record) //nolint:gosec // test file
	if err != nil {
		t.Fatal(err)
	}

	got := strings.Fields(string(data))
	slices.Sort(got)

	if want := []string{"started", "started", "terminated", "terminated"}; !slices.Equal(got, want) {
		t.Errorf("the backgrounded component recorded %v by the end of the run, want %v", got, want)
	}
}
