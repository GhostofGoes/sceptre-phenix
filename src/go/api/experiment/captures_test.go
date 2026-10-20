package experiment_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/golang/mock/gomock"

	"phenix/api/experiment"
	"phenix/store"
	"phenix/util/file"
	"phenix/util/mm"
)

type captureCountingMM struct {
	mm.MM

	calls int
}

func (m *captureCountingMM) GetExperimentCaptures(...mm.Option) []mm.Capture {
	m.calls++

	return []mm.Capture{{VM: "h1", Filepath: "/phenix/images/capture-test/files/h1.pcap"}}
}

type noClusterFiles struct {
	file.ClusterFiles
}

func (noClusterFiles) GetExperimentFiles(string, string) (file.Files, error) {
	return nil, nil
}

// TestCapturesAskMinimegaOnlyForRunningExperiments is a regression test:
// listing captures or downloading a file of a stopped or dry-run experiment
// asked minimega for its captures, which re-created the cleared namespace.
func TestCapturesAskMinimegaOnlyForRunningExperiments(t *testing.T) {
	tests := []struct {
		name      string
		startTime string
		running   bool
	}{
		{name: "running", startTime: "2024-01-01T00:00:00Z", running: true},
		{name: "stopped", startTime: ""},
		{name: "dry-run", startTime: "2024-01-01T00:00:00Z-DRYRUN"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)

			cfg := triggerTestConfig("capture-test")
			cfg.Status["startTime"] = tt.startTime

			m := store.NewMockStore(ctrl)
			m.EXPECT().Get(gomock.Any()).DoAndReturn(func(c *store.Config) error {
				*c = cfg
				return nil
			}).AnyTimes()

			fake := new(captureCountingMM)

			var (
				originalStore = store.DefaultStore
				originalMM    = mm.DefaultMM
				originalFiles = file.DefaultClusterFiles
			)

			t.Cleanup(func() {
				store.DefaultStore = originalStore       //nolint:reassign // restore default
				mm.DefaultMM = originalMM                //nolint:reassign // restore default
				file.DefaultClusterFiles = originalFiles //nolint:reassign // restore default
			})

			store.DefaultStore = m                      //nolint:reassign // install test double
			mm.DefaultMM = fake                         //nolint:reassign // install test double
			file.DefaultClusterFiles = noClusterFiles{} //nolint:reassign // install test double

			captures, err := experiment.Captures("capture-test")
			if err != nil {
				t.Fatalf("listing captures: %v", err)
			}

			if got := len(captures) == 1; got != tt.running {
				t.Fatalf("expected captures only for a running experiment, got %v", captures)
			}

			_, err = experiment.File("capture-test", "h1.pcap")
			if tt.running {
				if !errors.Is(err, mm.ErrCaptureExists) {
					t.Fatalf("expected the running capture to block the download, got %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "file not found") {
				t.Fatalf("expected file not found, got %v", err)
			}

			want := 0
			if tt.running {
				want = 2
			}

			if fake.calls != want {
				t.Fatalf("expected %d capture queries, got %d", want, fake.calls)
			}
		})
	}
}
