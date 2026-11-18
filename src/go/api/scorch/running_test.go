package scorch

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/golang/mock/gomock"
	"github.com/hashicorp/go-multierror"

	"phenix/app"
	"phenix/store"
	"phenix/types"
	"phenix/util/common"
	"phenix/util/mm"
)

// experimentStore backs store.DefaultStore with a single experiment config.
type experimentStore struct {
	mu      sync.Mutex
	current []byte // JSON of the stored config
}

func (s *experimentStore) load() store.Config {
	s.mu.Lock()
	defer s.mu.Unlock()

	var c store.Config

	if err := json.Unmarshal(s.current, &c); err != nil {
		panic(err)
	}

	return c
}

func (s *experimentStore) save(c store.Config) {
	body, err := json.Marshal(c)
	if err != nil {
		panic(err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.current = body
}

func installExperimentStore(t *testing.T, c store.Config) *experimentStore {
	t.Helper()

	s := new(experimentStore)
	s.save(c)

	ctrl := gomock.NewController(t)
	m := store.NewMockStore(ctrl)

	m.EXPECT().Get(gomock.Any()).DoAndReturn(func(c *store.Config) error {
		*c = s.load()

		return nil
	}).AnyTimes()

	m.EXPECT().Update(gomock.Any()).DoAndReturn(func(c *store.Config) error {
		s.save(*c)

		return nil
	}).AnyTimes()

	m.EXPECT().List(gomock.Any()).DoAndReturn(func(...string) (store.Configs, error) {
		return store.Configs{s.load()}, nil
	}).AnyTimes()

	original := store.DefaultStore
	store.DefaultStore = m //nolint:reassign // install test double

	t.Cleanup(func() { store.DefaultStore = original }) //nolint:reassign // restore test double

	return s
}

// tapCountingMM is a test double for mm.MM that counts host tap creates and
// deletes. Any other method panics.
type tapCountingMM struct {
	mm.MM

	mu   sync.Mutex
	taps int
}

func (*tapCountingMM) Headnode() string { return "head" }

func (m *tapCountingMM) TapVLAN(...mm.TapOption) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.taps++

	return nil
}

func (m *tapCountingMM) calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.taps
}

// TestTriggerRunningRecordsScorchStatus is a regression test: the running
// stage of the app framework (`phenix experiment trigger running <exp>
// scorch`, and runPeriodically) called the scorch app outside
// scorchexe.Execute, which alone recorded the run in status.apps.scorch. A
// tap component then created its tap, failed with "missing status for app
// scorch" and left the tap behind.
func TestTriggerRunningRecordsScorchStatus(t *testing.T) {
	bin := t.TempDir()

	// A component that saves the experiment it gets on stdin.
	script := "#!/bin/sh\nmkdir -p \"$PHENIX_FILES_DIR\"\ncat > \"$PHENIX_FILES_DIR/$1-$2.json\"\n"

	//nolint:gosec // test script must be executable
	if err := os.WriteFile(filepath.Join(bin, "phenix-scorch-component-stdinsaver"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	phenixBase, mmBase := common.PhenixBase, common.MinimegaBase
	common.PhenixBase = t.TempDir()                              //nolint:reassign // test files directory
	common.MinimegaBase = filepath.Join(t.TempDir(), "minimega") //nolint:reassign // no minimega to reach

	t.Cleanup(func() {
		common.PhenixBase, common.MinimegaBase = phenixBase, mmBase //nolint:reassign // restore globals
	})

	fake := new(tapCountingMM)
	originalMM := mm.DefaultMM
	mm.DefaultMM = fake //nolint:reassign // install test double

	t.Cleanup(func() { mm.DefaultMM = originalMM }) //nolint:reassign // restore test double

	const name = "trigger-test"

	s := installExperimentStore(t, store.Config{
		Version:  "phenix.sandia.gov/v1",
		Kind:     "Experiment",
		Metadata: store.ConfigMetadata{Name: name},
		Spec: map[string]any{
			"experimentName": name,
			"defaultBridge":  "phenix",
			"scenario": map[string]any{
				"apps": []any{map[string]any{
					"name": "scorch",
					"metadata": map[string]any{
						"components": []any{
							map[string]any{
								"name":     "tap1",
								"type":     "tap",
								"metadata": map[string]any{"bridge": "phenix", "vlan": "LAN", "ip": "10.10.0.210/24"},
							},
							map[string]any{"name": "saver", "type": "stdinsaver"},
						},
						"runs": []any{map[string]any{
							"start": []any{"tap1", "saver"},
							"stop":  []any{"tap1"},
						}},
					},
				}},
			},
		},
	})

	exp, err := types.DecodeExperimentFromConfig(s.load())
	if err != nil {
		t.Fatal(err)
	}

	// What `phenix experiment trigger running <exp> scorch` runs.
	err = app.ApplyApps(
		context.Background(),
		exp,
		app.Stage(app.ActionRunning),
		app.FilterApp("scorch"),
		app.Trigger(),
	)

	// The run's info file needs the minimega version, which this test cannot
	// provide; that must be the run's only error.
	var merr *multierror.Error
	if errors.As(err, &merr) && len(merr.Errors) == 1 &&
		strings.Contains(merr.Errors[0].Error(), "getting minimega version") {
		err = nil
	}

	if err != nil {
		t.Fatalf("running stage of the scorch app: %v", err)
	}

	if got := fake.calls(); got != 2 {
		t.Errorf("tap component created and deleted %d host taps in all, want 2 (one create, one delete)", got)
	}

	body, err := os.ReadFile(filepath.Join(exp.FilesDir(), "start-saver.json"))
	if err != nil {
		t.Fatalf("the saver component's start stage did not run: %v", err)
	}

	var seen struct {
		Status struct {
			Apps map[string]struct {
				RunID *int           `json:"runID"`
				Taps  map[string]any `json:"taps"`
			} `json:"apps"`
		} `json:"status"`
	}

	if err := json.Unmarshal(body, &seen); err != nil {
		t.Fatal(err)
	}

	scorch, ok := seen.Status.Apps["scorch"]
	if !ok || scorch.RunID == nil || *scorch.RunID != 0 || scorch.Taps["tap1"] == nil {
		t.Errorf("during the run, status.apps.scorch = %+v (present %t), want run 0 with tap1's tap", scorch, ok)
	}

	stored := s.load()
	apps, _ := stored.Status["apps"].(map[string]any)
	running, _ := stored.Status["appRunningStageStatus"].(map[string]any)

	if _, ok := apps["scorch"]; ok || running["scorch"] == true {
		t.Errorf("after the run, status.apps = %v and appRunningStageStatus = %v, want no scorch entry and not running", apps, running)
	}
}

// TestTapComponentReadsStatusBeforeCreatingTap checks that a tap component
// without status.apps.scorch fails before it creates a tap it could not
// record.
func TestTapComponentReadsStatusBeforeCreatingTap(t *testing.T) {
	fake := new(tapCountingMM)
	originalMM := mm.DefaultMM
	mm.DefaultMM = fake //nolint:reassign // install test double

	t.Cleanup(func() { mm.DefaultMM = originalMM }) //nolint:reassign // restore test double

	exp := types.NewExperiment(store.ConfigMetadata{Name: "test-exp"})
	exp.Spec.SetExperimentName("test-exp")

	var cmp Tap

	_ = cmp.Init(
		Experiment(*exp),
		Name("tap1"),
		Type("tap"),
		Metadata(map[string]any{"bridge": "phenix", "vlan": "LAN", "ip": "10.10.0.210/24"}),
	)

	err := cmp.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "missing status for app scorch") {
		t.Fatalf("Start() = %v, want the missing status error", err)
	}

	if got := fake.calls(); got != 0 {
		t.Errorf("Start() created %d host taps before failing, want 0", got)
	}
}
