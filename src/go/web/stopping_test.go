package web

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gorilla/mux"

	"phenix/web/cache"
)

// TestRefuseStoppingExperiments is a regression test: during a REST stop, the
// VM routes passed experiment.RequireRunning until the stop cleared the
// experiment's start time, after it had cleared the minimega namespace, so a
// VM operation in between created the namespace again.
func TestRefuseStoppingExperiments(t *testing.T) {
	exp := "stopping-" + t.Name()

	if err := cache.LockExperimentForStopping(exp); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { cache.UnlockExperiment(exp) })

	var reached bool

	handler := func(w http.ResponseWriter, _ *http.Request) {
		reached = true

		w.WriteHeader(http.StatusNoContent)
	}

	router := mux.NewRouter()
	router.HandleFunc("/experiments/{exp}/vms/{name}/start", handler)
	router.HandleFunc("/experiments/{exp}/vms", handler)
	router.HandleFunc("/experiments/{name}", handler)
	router.Use(refuseStoppingExperiments)

	for _, tc := range []struct {
		path string
		want int
	}{
		{"/experiments/" + exp + "/vms/h1/start", http.StatusConflict},
		{"/experiments/" + exp + "/vms", http.StatusConflict},
		{"/experiments/other/vms/h1/start", http.StatusNoContent},
		// The experiment routes themselves, such as its stop, stay open.
		{"/experiments/" + exp, http.StatusNoContent},
	} {
		reached = false

		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, tc.path, nil))

		if rec.Code != tc.want || reached != (tc.want == http.StatusNoContent) {
			t.Errorf("POST %s = %d %q (handler reached: %v), want %d", tc.path, rec.Code, rec.Body.String(), reached, tc.want)
		}
	}
}
