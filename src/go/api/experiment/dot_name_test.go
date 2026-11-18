package experiment_test

import (
	"errors"
	"strings"
	"testing"

	"phenix/api/config"
	"phenix/store"
)

// TestCreateRefusesDotNames is a regression test: the config name check
// accepts "." and "..", and an experiment so named got the experiments
// directory or phenix's base directory as its default base directory, which
// deleting the experiment removed with everything in it. config.Create also
// accepted the reserved name all in another letter case.
func TestCreateRefusesDotNames(t *testing.T) {
	useBoltStore(t, startTestConfig(t, false, false))

	for _, name := range []string{".", "..", "all", "ALL"} {
		t.Run(name, func(t *testing.T) {
			_, err := config.Create(config.CreateFromConfig(&store.Config{
				Version:  "phenix.sandia.gov/v1",
				Kind:     "Experiment",
				Metadata: store.ConfigMetadata{Name: name},
				Spec:     map[string]any{"experimentName": name},
			}))
			if err == nil || !strings.Contains(err.Error(), "for experiment name") {
				t.Errorf("creating experiment %q returned %v, want it refused", name, err)
			}

			c, _ := store.NewConfig("experiment/" + name)
			if err := store.Get(c); !errors.Is(err, store.ErrNotExist) {
				t.Errorf("experiment %q was stored (%v)", name, err)
			}
		})
	}
}

// TestRenameRefusesDotNames is a regression test: only the create hook
// refused "." and "..", so a config update that renamed an experiment to ".."
// (phenix config edit, or PUT /configs/experiment/{name}) made one whose base
// directory was the experiments directory's parent, and deleting it removed
// phenix's images and every other experiment.
func TestRenameRefusesDotNames(t *testing.T) {
	useBoltStore(t, startTestConfig(t, false, false))

	for _, name := range []string{".", "..", "all", "ALL"} {
		t.Run(name, func(t *testing.T) {
			c, _ := store.NewConfig("experiment/" + startTestExp)
			if err := store.Get(c); err != nil {
				t.Fatalf("getting experiment: %v", err)
			}

			c.Metadata.Name = name

			err := config.Update("experiment/"+startTestExp, c)
			if err == nil || !strings.Contains(err.Error(), "for experiment name") {
				t.Errorf("renaming experiment to %q returned %v, want it refused", name, err)
			}

			renamed, _ := store.NewConfig("experiment/" + name)
			if err := store.Get(renamed); !errors.Is(err, store.ErrNotExist) {
				t.Errorf("experiment %q was stored (%v)", name, err)
			}

			kept, _ := store.NewConfig("experiment/" + startTestExp)
			if err := store.Get(kept); err != nil {
				t.Errorf("experiment %s is gone after the refused rename: %v", startTestExp, err)
			}
		})
	}
}
