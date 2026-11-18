package web

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gorilla/mux"
)

// TestRefuseUnsafeVMNames is a regression test: the VM routes put the VM name
// from the path into namespaced minimega commands as given, so a name such as
// a"b made minimega exit for every experiment, and a role allowed only web*
// killed every VM with DELETE .../vms/web1,all.
func TestRefuseUnsafeVMNames(t *testing.T) {
	var reached bool

	handler := func(w http.ResponseWriter, _ *http.Request) {
		reached = true

		w.WriteHeader(http.StatusNoContent)
	}

	router := mux.NewRouter()
	router.HandleFunc("/experiments/{exp}/vms/{name}/start", handler)
	router.HandleFunc("/experiments/{exp}/vms", handler)
	router.HandleFunc("/experiments/{name}", handler)
	router.Use(refuseUnsafeVMNames)

	for _, tc := range []struct {
		path string
		want int
	}{
		{"/experiments/exp/vms/h1/start", http.StatusNoContent},
		{"/experiments/exp/vms/web-01/start", http.StatusNoContent},
		{"/experiments/exp/vms", http.StatusNoContent},
		{"/experiments/exp", http.StatusNoContent},
		{"/experiments/exp/vms/a%22b/start", http.StatusBadRequest},
		{"/experiments/exp/vms/h1%5C/start", http.StatusBadRequest},
		{"/experiments/exp/vms/it%27s/start", http.StatusBadRequest},
		{"/experiments/exp/vms/a%23b/start", http.StatusBadRequest},
		{"/experiments/exp/vms/a%20b/start", http.StatusBadRequest},
		{"/experiments/exp/vms/web1,db1/start", http.StatusBadRequest},
		{"/experiments/exp/vms/web%5B1-3%5D/start", http.StatusBadRequest},
		{"/experiments/exp/vms/all/start", http.StatusBadRequest},
		{"/experiments/exp/vms/7/start", http.StatusBadRequest},
		{"/experiments/exp/vms/ALL/start", http.StatusNoContent},
		{"/experiments/exp/vms/web_1.lab/start", http.StatusNoContent},
	} {
		reached = false

		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, tc.path, nil))

		if rec.Code != tc.want || reached != (tc.want == http.StatusNoContent) {
			t.Errorf("POST %s = %d %q (handler reached: %v), want %d", tc.path, rec.Code, rec.Body.String(), reached, tc.want)
		}
	}
}
