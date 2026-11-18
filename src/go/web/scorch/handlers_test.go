package scorch

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang/mock/gomock"
	"github.com/gorilla/mux"

	"phenix/api/scorch/scorchexe"
	"phenix/store"
	"phenix/web/middleware"
	"phenix/web/weberror"
)

// installPipelineTestExperiment backs the store with the running experiment
// "test-exp", whose scorch app has one run, with the given status.
func installPipelineTestExperiment(t *testing.T, status map[string]any) {
	t.Helper()

	installPipelineTestRun(t, status, []any{"wait1"})
}

// installPipelineTestRun backs the store with the experiment "test-exp",
// whose scorch app defines the component wait1 and has one run, which starts
// the given components, with the given status.
func installPipelineTestRun(t *testing.T, status map[string]any, start []any) {
	t.Helper()

	c := store.Config{
		Version:  "phenix.sandia.gov/v1",
		Kind:     "Experiment",
		Metadata: store.ConfigMetadata{Name: "test-exp"},
		Spec: map[string]any{
			"experimentName": "test-exp",
			"scenario": map[string]any{
				"apps": []any{map[string]any{
					"name": "scorch",
					"metadata": map[string]any{
						"components": []any{map[string]any{"name": "wait1", "type": "pause"}},
						"runs":       []any{map[string]any{"start": start}},
					},
				}},
			},
		},
		Status: status,
	}

	ctrl := gomock.NewController(t)
	m := store.NewMockStore(ctrl)

	m.EXPECT().Get(gomock.Any()).DoAndReturn(func(cfg *store.Config) error {
		*cfg = c

		return nil
	}).AnyTimes()

	original := store.DefaultStore
	store.DefaultStore = m //nolint:reassign // install test double

	t.Cleanup(func() { store.DefaultStore = original }) //nolint:reassign // restore test double
}

func startPipelineRequest(run string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/experiments/test-exp/scorch/pipelines/"+run, nil)
	req = mux.SetURLVars(req, map[string]string{"name": "test-exp", "run": run})

	role := testRole([2]string{"experiments/trigger", "create"}, [2]string{"experiments", "get"})

	ctx := context.WithValue(req.Context(), middleware.ContextKeyRole, role)
	ctx = context.WithValue(ctx, middleware.ContextKeyUser, "test-user")

	return req.WithContext(ctx)
}

// TestStartPipelineRefusals is a regression test: StartPipeline answered 204
// and only then had scorchexe.Execute refuse the run, reporting the refusal
// to the log and pubsub alone, so a client starting a run while another ran,
// a run the scenario does not have, or one of a stopped experiment was told
// it had started.
func TestStartPipelineRefusals(t *testing.T) {
	running := map[string]any{"startTime": "2024-01-01T00:00:00Z"}

	tests := []struct {
		name   string
		status map[string]any
		run    string
		code   int
		want   string
	}{
		{
			"already running",
			map[string]any{"startTime": "2024-01-01T00:00:00Z", "appRunningStageStatus": map[string]any{"scorch": true}},
			"0",
			http.StatusConflict,
			scorchexe.ErrAlreadyRunning.Error(),
		},
		{"no such run", running, "3", http.StatusBadRequest, "invalid Scorch run ID"},
		{"stopped", map[string]any{}, "0", http.StatusBadRequest, "is not running"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			installPipelineTestExperiment(t, tt.status)

			rec := httptest.NewRecorder()
			weberror.ErrorHandler(StartPipeline).ServeHTTP(rec, startPipelineRequest(tt.run))

			if rec.Code != tt.code {
				t.Fatalf("POST pipelines/%s: status %d %s, want %d", tt.run, rec.Code, rec.Body, tt.code)
			}

			var body weberror.WebError

			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decoding the response %q: %v", rec.Body, err)
			}

			if !strings.Contains(body.Cause, tt.want) {
				t.Errorf("POST pipelines/%s: cause %q, want it to contain %q", tt.run, body.Cause, tt.want)
			}

			_, finished, err := scorchexe.Register(context.Background(), "test-exp", 0)
			if err != nil {
				t.Errorf("the refused run left a registration behind: %v", err)
			} else {
				finished()
			}
		})
	}
}

// TestStartPipelineConflicts is a regression test: a start of a run that
// already executed in phenix ui was answered 400, where one started from the
// CLI was answered 409, and a start while an experiment stop waited for its
// runs was accepted, so the stop neither canceled nor awaited it.
func TestStartPipelineConflicts(t *testing.T) {
	running := map[string]any{"startTime": "2024-01-01T00:00:00Z"}

	tests := []struct {
		name string
		hold func(t *testing.T)
		want error
	}{
		{
			"run executing here",
			func(t *testing.T) {
				t.Helper()

				_, finished, err := scorchexe.Register(context.Background(), "test-exp", 0)
				if err != nil {
					t.Fatal(err)
				}

				t.Cleanup(finished)
			},
			scorchexe.ErrAlreadyRunning,
		},
		{
			"experiment stopping",
			func(t *testing.T) {
				t.Helper()

				if err := scorchexe.CancelExperiment("test-exp", time.Second); err != nil {
					t.Fatal(err)
				}

				t.Cleanup(func() { scorchexe.Stopped("test-exp") })
			},
			scorchexe.ErrStopping,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			installPipelineTestExperiment(t, running)
			tt.hold(t)

			rec := httptest.NewRecorder()
			weberror.ErrorHandler(StartPipeline).ServeHTTP(rec, startPipelineRequest("0"))

			if rec.Code != http.StatusConflict {
				t.Fatalf("POST pipelines/0: status %d %s, want %d", rec.Code, rec.Body, http.StatusConflict)
			}

			var body weberror.WebError

			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decoding the response %q: %v", rec.Body, err)
			}

			if !strings.Contains(body.Cause, tt.want.Error()) {
				t.Errorf("POST pipelines/0: cause %q, want it to contain %q", body.Cause, tt.want)
			}
		})
	}
}

// TestStartPipelineUndefinedComponent is a regression test: StartPipeline
// answered 204 for a run that names a component the scorch app does not
// define, which an experiment created before runs were validated can hold,
// and the run then failed in the background.
func TestStartPipelineUndefinedComponent(t *testing.T) {
	installPipelineTestRun(t, map[string]any{"startTime": "2024-01-01T00:00:00Z"}, []any{"wait1", "nosuchcmp"})

	rec := httptest.NewRecorder()
	weberror.ErrorHandler(StartPipeline).ServeHTTP(rec, startPipelineRequest("0"))

	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "nosuchcmp") {
		t.Errorf("POST pipelines/0 of a run naming an undefined component: status %d %s, want 400 naming it", rec.Code, rec.Body)
	}
}
