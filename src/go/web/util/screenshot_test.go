package util_test

import (
	"bytes"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/golang/mock/gomock"

	"phenix/api/experiment"
	"phenix/store"
	"phenix/util/mm"
	"phenix/web/cache"
	"phenix/web/util"
)

// screenshotRuns numbers the experiments of the screenshot tests.
var screenshotRuns atomic.Int64 //nolint:gochecknoglobals // test counter

// screenshotTestExperiment returns an experiment name no other run of the test
// in this process used: the screenshot cache is the process's and keeps a
// screenshot for 10 s, so a second run, as with -count=2, got the first run's.
func screenshotTestExperiment(t *testing.T) string {
	t.Helper()

	return fmt.Sprintf("%s-%d", t.Name(), screenshotRuns.Add(1))
}

// screenshotMM answers every screenshot request with a new image, so a test
// can tell an image minimega took from one the cache returned.
type screenshotMM struct {
	mm.MM

	calls int
}

func (m *screenshotMM) GetVMScreenshot(...mm.Option) ([]byte, error) {
	m.calls++

	return fmt.Appendf(nil, "screenshot %d", m.calls), nil
}

// TestGetScreenshotCachesEachSize checks that a screenshot cached at one size
// is not returned for a request at another size, which got the 215 pixel
// image the VM list had cached when it asked for 100.
func TestGetScreenshotCachesEachSize(t *testing.T) {
	original := mm.DefaultMM
	t.Cleanup(func() { mm.DefaultMM = original }) //nolint:reassign // restore test double

	fake := new(screenshotMM)
	mm.DefaultMM = fake //nolint:reassign // install test double

	exp := screenshotTestExperiment(t)

	// Screenshots are taken of the VMs of a running experiment only.
	useRunningExperiment(t, exp)

	large, err := util.GetScreenshot(exp, "vm", "215")
	if err != nil {
		t.Fatalf("GetScreenshot(size 215) = %v", err)
	}

	small, err := util.GetScreenshot(exp, "vm", "100")
	if err != nil {
		t.Fatalf("GetScreenshot(size 100) = %v", err)
	}

	if bytes.Equal(small, large) {
		t.Fatalf("GetScreenshot(size 100) returned the cached size 215 image %q", large)
	}

	again, err := util.GetScreenshot(exp, "vm", "215")
	if err != nil {
		t.Fatalf("GetScreenshot(size 215) again = %v", err)
	}

	if !bytes.Equal(again, large) {
		t.Errorf("GetScreenshot(size 215) again = %q, want the cached %q", again, large)
	}

	if fake.calls != 2 {
		t.Errorf("minimega took %d screenshots, want 2: one for each size", fake.calls)
	}
}

// TestGetScreenshotChecksTheSize is a regression test: the cache was keyed by
// the size as the request spelled it, so 215, 0215 and +215 each took a
// screenshot and held a cache entry, and any string reached minimega, where a
// quote in it made minimega exit.
func TestGetScreenshotChecksTheSize(t *testing.T) {
	original := mm.DefaultMM
	t.Cleanup(func() { mm.DefaultMM = original }) //nolint:reassign // restore test double

	fake := new(screenshotMM)
	mm.DefaultMM = fake //nolint:reassign // install test double

	exp := screenshotTestExperiment(t)

	useRunningExperiment(t, exp)

	for _, size := range []string{"215", "0215", "+215"} {
		if _, err := util.GetScreenshot(exp, "vm", size); err != nil {
			t.Fatalf("GetScreenshot(size %s) = %v", size, err)
		}
	}

	if fake.calls != 1 {
		t.Errorf("minimega took %d screenshots for sizes 215, 0215 and +215, want 1", fake.calls)
	}

	for _, size := range []string{"", "abc", "-1", "4097", "1.5", `"1\"2"`} {
		if _, err := util.GetScreenshot(exp, "vm", size); !errors.Is(err, util.ErrInvalidScreenshotSize) {
			t.Errorf("GetScreenshot(size %q) = %v, want ErrInvalidScreenshotSize", size, err)
		}
	}

	if fake.calls != 1 {
		t.Errorf("minimega took %d screenshots, want only the one of size 215", fake.calls)
	}
}

// TestGetScreenshotRefusesStoppingExperiments is a regression test: the
// websocket screenshot ticker asked minimega for screenshots of an experiment
// that phenix ui was stopping, which the store still reports running after the
// stop cleared its namespace, and so created the namespace again.
func TestGetScreenshotRefusesStoppingExperiments(t *testing.T) {
	original := mm.DefaultMM
	t.Cleanup(func() { mm.DefaultMM = original }) //nolint:reassign // restore test double

	fake := new(screenshotMM)
	mm.DefaultMM = fake //nolint:reassign // install test double

	exp := screenshotTestExperiment(t)

	// The store reports the experiment running until the stop has ended.
	useRunningExperiment(t, exp)

	if err := cache.LockExperimentForStopping(exp); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { cache.UnlockExperiment(exp) })

	_, err := util.GetScreenshot(exp, "vm", "215")
	if !errors.Is(err, experiment.ErrExperimentNotRunning) || fake.calls != 0 {
		t.Errorf(
			"GetScreenshot of a stopping experiment = %v after %d minimega screenshots, want ErrExperimentNotRunning and none",
			err, fake.calls,
		)
	}
}

// useRunningExperiment makes the store hold exp as a running experiment.
func useRunningExperiment(t *testing.T, exp string) {
	t.Helper()

	m := store.NewMockStore(gomock.NewController(t))
	m.EXPECT().Get(gomock.Any()).DoAndReturn(func(c *store.Config) error {
		*c = store.Config{
			Version:  "phenix.sandia.gov/v1",
			Kind:     "Experiment",
			Metadata: store.ConfigMetadata{Name: exp},
			Spec:     map[string]any{"experimentName": exp},
			Status:   map[string]any{"startTime": "2026-10-18T00:00:00Z"},
		}

		return nil
	}).AnyTimes()

	originalStore := store.DefaultStore
	t.Cleanup(func() { store.DefaultStore = originalStore }) //nolint:reassign // restore test double

	store.DefaultStore = m //nolint:reassign // install test double
}
