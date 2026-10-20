package scorch

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"phenix/store"
	"phenix/types"
)

// TestExecuteComponentConcurrentRuns is a regression test: every execution
// of a component type initialized one shared instance, so when the runs of
// two experiments executed a component at the same time, one of them could
// execute it with the other's experiment, name and metadata.
func TestExecuteComponentConcurrentRuns(t *testing.T) {
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")

	// A component that records the name it is executed as.
	script := "#!/bin/sh\ncat >/dev/null\necho \"$2\" >> " + calls + "\n"

	//nolint:gosec // test script must be executable
	if err := os.WriteFile(filepath.Join(dir, "phenix-scorch-component-recorder"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	const runs = 16

	var (
		wg   sync.WaitGroup
		want = make([]string, 0, runs)
	)

	for i := range runs {
		name := fmt.Sprintf("cmp-%d", i)
		want = append(want, name)

		wg.Add(1)

		go func() {
			defer wg.Done()

			exp := types.NewExperiment(store.ConfigMetadata{Name: "exp-" + name})
			exp.Spec.SetExperimentName("exp-" + name)

			err := ExecuteComponent(
				context.Background(),
				Experiment(*exp),
				Name(name),
				Type("recorder"),
				Stage(ActionStart),
			)
			if err != nil {
				t.Errorf("executing %s: %v", name, err)
			}
		}()
	}

	wg.Wait()

	data, err := os.ReadFile(calls) //nolint:gosec // test file
	if err != nil {
		t.Fatal(err)
	}

	got := strings.Fields(string(data))
	slices.Sort(got)
	slices.Sort(want)

	if !slices.Equal(got, want) {
		t.Errorf("components executed as %v, want each of %v once", got, want)
	}
}
