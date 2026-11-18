package experiment_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"phenix/api/config"
	"phenix/api/experiment"
	"phenix/store"
	"phenix/util/common"
)

// usePhenixBase makes a temporary directory phenix's base directory, with a
// stand-in disk image in its images directory, until the test ends. It
// returns the base directory and the image.
func usePhenixBase(t *testing.T) (string, string) {
	t.Helper()

	original := common.PhenixBase
	t.Cleanup(func() { common.PhenixBase = original }) //nolint:reassign // restore default

	base := filepath.Join(t.TempDir(), "phenix")
	common.PhenixBase = base //nolint:reassign // test base directory

	image := filepath.Join(base, "images", "keep.qc2")
	if err := os.MkdirAll(filepath.Dir(image), 0o750); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(image, []byte("disk"), 0o600); err != nil {
		t.Fatal(err)
	}

	return base, image
}

// TestUpdateRefusesUnsafeBaseDir is a regression test: a config update
// accepted any spec.baseDir, and deleting the experiment removed that
// directory with everything in it.
func TestUpdateRefusesUnsafeBaseDir(t *testing.T) {
	base, _ := usePhenixBase(t)
	useBoltStore(t, startTestConfig(t, false, false))

	for _, dir := range []string{
		"/",
		base,
		filepath.Dir(base),
		filepath.Join(base, "experiments"),
		filepath.Join(base, "images"),
		filepath.Join(base, "configs"),
		filepath.Join(base, "injects"),
		filepath.Join(base, "topologies"),
		filepath.Join(base, "mounts"),
		common.MinimegaBase,
		filepath.Dir(common.MinimegaBase),
		base + "/experiments/" + startTestExp + "/..",
		base + "/experiments/" + startTestExp + "/.",
	} {
		t.Run(dir, func(t *testing.T) {
			c, _ := store.NewConfig("experiment/" + startTestExp)
			if err := store.Get(c); err != nil {
				t.Fatalf("getting experiment: %v", err)
			}

			c.Spec["baseDir"] = dir

			err := config.Update("experiment/"+startTestExp, c)
			if err == nil || !strings.Contains(err.Error(), "spec.baseDir") {
				t.Errorf("updating spec.baseDir to %s returned %v, want it refused", dir, err)
			}
		})
	}
}

// TestDeleteKeepsUnsafeBaseDir is a regression test: experiment delete ran
// [os.RemoveAll] on the stored base directory, whatever it was, so an
// experiment whose base directory was phenix's base directory, or a parent of
// it, removed phenix's images and the other experiments.
func TestDeleteKeepsUnsafeBaseDir(t *testing.T) {
	base, image := usePhenixBase(t)

	for _, dir := range []string{
		base,
		filepath.Dir(base),
		filepath.Join(base, "experiments"),
		filepath.Join(base, "images"),
		base + "/experiments/../images",
	} {
		t.Run(dir, func(t *testing.T) {
			// Stored as an older phenix that checked nothing would have
			// stored it.
			cfg := startTestConfig(t, false, false)
			cfg.Spec["baseDir"] = dir

			installStartDoubles(t, new(startMM), cfg)
			useBoltStore(t, cfg)

			err := experiment.Delete(startTestExp)
			if err == nil || !strings.Contains(err.Error(), "not deleting experiment base directory") {
				t.Errorf("deleting experiment with base directory %s returned %v, want the removal refused", dir, err)
			}

			if _, err := os.Stat(image); err != nil {
				t.Fatalf("deleting experiment with base directory %s removed %s: %v", dir, image, err)
			}
		})
	}
}

// TestDeleteRemovesBaseDir checks that the base directory checks leave an
// ordinary base directory to be removed.
func TestDeleteRemovesBaseDir(t *testing.T) {
	base, image := usePhenixBase(t)

	dir := filepath.Join(base, "experiments", startTestExp)
	if err := os.MkdirAll(filepath.Join(dir, "files"), 0o750); err != nil {
		t.Fatal(err)
	}

	cfg := startTestConfig(t, false, false)
	cfg.Spec["baseDir"] = dir

	installStartDoubles(t, new(startMM), cfg)
	useBoltStore(t, cfg)

	if err := experiment.Delete(startTestExp); err != nil {
		t.Fatalf("deleting experiment: %v", err)
	}

	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("base directory %s is still there (%v)", dir, err)
	}

	if _, err := os.Stat(image); err != nil {
		t.Errorf("deleting experiment removed %s: %v", image, err)
	}
}

// TestRenameKeepsBaseDirParents is a regression test: renaming an experiment
// replaced its old name everywhere in the base directory, so renaming the
// experiment exp to lab moved its base directory from
// <base>/experiments/exp to <base>/laberiments/lab.
func TestRenameKeepsBaseDirParents(t *testing.T) {
	base, _ := usePhenixBase(t)

	cfg := startTestConfig(t, false, false)
	cfg.Metadata.Name = "exp"
	cfg.Spec["experimentName"] = "exp"
	cfg.Spec["baseDir"] = filepath.Join(base, "experiments", "exp")

	useBoltStore(t, cfg)

	c, _ := store.NewConfig("experiment/exp")
	if err := store.Get(c); err != nil {
		t.Fatalf("getting experiment: %v", err)
	}

	c.Metadata.Name = "lab"

	if err := config.Update("experiment/exp", c); err != nil {
		t.Fatalf("renaming experiment: %v", err)
	}

	exp, err := experiment.Get("lab")
	if err != nil {
		t.Fatalf("getting renamed experiment: %v", err)
	}

	if got, want := exp.Spec.BaseDir(), filepath.Join(base, "experiments", "lab"); got != want {
		t.Errorf("base directory after the rename = %s, want %s", got, want)
	}
}

// TestUpdateChecksRelativeBaseDirOnceAbsolute checks that a config update
// makes a relative spec.baseDir absolute against the working directory, as
// phenix experiment create --base-dir takes one, and refuses it when the
// absolute directory is one that deleting the experiment must not remove.
func TestUpdateChecksRelativeBaseDirOnceAbsolute(t *testing.T) {
	base, _ := usePhenixBase(t)
	useBoltStore(t, startTestConfig(t, false, false))

	update := func(dir string) error {
		t.Helper()

		c, _ := store.NewConfig("experiment/" + startTestExp)
		if err := store.Get(c); err != nil {
			t.Fatalf("getting experiment: %v", err)
		}

		c.Spec["baseDir"] = dir

		return config.Update("experiment/"+startTestExp, c)
	}

	t.Chdir(filepath.Dir(base))

	if err := update(filepath.Base(base)); err == nil || !strings.Contains(err.Error(), "spec.baseDir") {
		t.Errorf("updating spec.baseDir to phenix's base directory, relative, returned %v, want it refused", err)
	}

	experiments := filepath.Join(base, "experiments")
	if err := os.MkdirAll(experiments, 0o750); err != nil {
		t.Fatal(err)
	}

	t.Chdir(experiments)

	if err := update("lab"); err != nil {
		t.Fatalf("updating spec.baseDir to a relative experiment directory: %v", err)
	}

	exp, err := experiment.Get(startTestExp)
	if err != nil {
		t.Fatalf("getting experiment: %v", err)
	}

	if got, want := exp.Spec.BaseDir(), filepath.Join(experiments, "lab"); got != want {
		t.Errorf("stored base directory = %s, want %s", got, want)
	}
}
