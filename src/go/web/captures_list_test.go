package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/golang/mock/gomock"
	"github.com/gorilla/mux"

	"phenix/store"
	v1 "phenix/types/version/v1"
	"phenix/util/mm"
	"phenix/web/middleware"
	"phenix/web/rbac"
)

type captureListTestMM struct {
	mm.MM

	calls int
}

func (m *captureListTestMM) GetExperimentCaptures(...mm.Option) []mm.Capture {
	m.calls++

	return []mm.Capture{{VM: "test-vm", Filepath: "/phenix/images/test-experiment/files/a.pcap"}}
}

func (m *captureListTestMM) GetVMCaptures(...mm.Option) []mm.Capture {
	m.calls++

	return []mm.Capture{{VM: "test-vm", Filepath: "/phenix/images/test-experiment/files/a.pcap"}}
}

func captureListTestRequest(url string, vars map[string]string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, url, nil)
	req = mux.SetURLVars(req, vars)

	role := rbac.Role{
		Spec: &v1.RoleSpec{
			Policies: []*v1.PolicySpec{
				{
					Resources:     []string{"experiments/captures", "vms/captures"},
					ResourceNames: []string{"*", "*/*"},
					Verbs:         []string{"list"},
				},
			},
		},
	}

	ctx := context.WithValue(req.Context(), middleware.ContextKeyRole, role)
	ctx = context.WithValue(ctx, middleware.ContextKeyUser, "test-user")

	return req.WithContext(ctx)
}

// TestCaptureListsAskMinimegaOnlyForRunningExperiments is a regression test:
// listing the captures of a stopped or dry-run experiment, or of one of its
// VMs, asked minimega, which re-created the experiment's cleared namespace.
func TestCaptureListsAskMinimegaOnlyForRunningExperiments(t *testing.T) {
	handlers := []struct {
		name    string
		handler http.HandlerFunc
		request *http.Request
	}{
		{
			name:    "experiment",
			handler: GetExperimentCaptures,
			request: captureListTestRequest(
				"/api/v1/experiments/test-experiment/captures",
				map[string]string{"name": "test-experiment"},
			),
		},
		{
			name:    "vm",
			handler: GetVMCaptures,
			request: captureListTestRequest(
				"/api/v1/experiments/test-experiment/vms/test-vm/captures",
				map[string]string{"exp": "test-experiment", "name": "test-vm"},
			),
		},
	}

	states := []struct {
		name      string
		startTime string
		want      int
	}{
		{name: "running", startTime: "2024-01-01T00:00:00Z", want: 1},
		{name: "stopped", startTime: "", want: 0},
		{name: "dry-run", startTime: "2024-01-01T00:00:00Z-DRYRUN", want: 0},
	}

	for _, h := range handlers {
		for _, state := range states {
			t.Run(h.name+"/"+state.name, func(t *testing.T) {
				ctrl := gomock.NewController(t)

				c := store.Config{
					Version:  "phenix.sandia.gov/v1",
					Kind:     "Experiment",
					Metadata: store.ConfigMetadata{Name: "test-experiment"},
					Spec:     map[string]any{"experimentName": "test-experiment"},
					Status:   map[string]any{"startTime": state.startTime},
				}

				m := store.NewMockStore(ctrl)
				m.EXPECT().Get(gomock.Any()).DoAndReturn(func(cfg *store.Config) error {
					*cfg = c

					return nil
				}).AnyTimes()

				originalStore := store.DefaultStore
				t.Cleanup(func() { store.DefaultStore = originalStore }) //nolint:reassign // restore test double

				store.DefaultStore = m //nolint:reassign // monkey patching for test

				fake := new(captureListTestMM)
				original := mm.DefaultMM
				t.Cleanup(func() { mm.DefaultMM = original }) //nolint:reassign // restore test double

				mm.DefaultMM = fake //nolint:reassign // install test double

				rec := httptest.NewRecorder()
				h.handler(rec, h.request)

				if rec.Code != http.StatusOK {
					t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
				}

				var body struct {
					Captures []json.RawMessage `json:"captures"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
					t.Fatalf("decoding response %q: %v", rec.Body.String(), err)
				}

				if fake.calls != state.want || len(body.Captures) != state.want {
					t.Fatalf(
						"expected %d minimega queries and captures, got %d queries and %s",
						state.want, fake.calls, rec.Body.String(),
					)
				}
			})
		}
	}
}
