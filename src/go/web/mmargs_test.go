package web

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gorilla/mux"
)

func TestRejectUnsafeMinimegaVars(t *testing.T) {
	tests := map[string]struct {
		path     string
		vars     map[string]string
		wantNext bool
	}{
		"plain vm": {
			path:     "/api/v1/experiments/exp/vms/vm1/start",
			vars:     map[string]string{"exp": "exp", "name": "vm1"},
			wantNext: true,
		},
		"quote in vm": {path: "/api/v1/experiments/exp/vms/a/start", vars: map[string]string{"exp": "exp", "name": `a"b`}},
		"hash in vm":  {path: "/api/v1/experiments/exp/vms/a/stop", vars: map[string]string{"exp": "exp", "name": "admin#x"}},
		"env in disk": {path: "/api/v1/disks/rename", vars: map[string]string{"disk": "$HOME/x.qc2", "new": "y"}},
		"url in snapshot": {
			path: "/api/v1/experiments/exp/vms/vm/snapshots/s",
			vars: map[string]string{"exp": "exp", "name": "vm", "snapshot": "http://h/s"},
		},
		"control in exp":   {path: "/api/v1/experiments/e/vms", vars: map[string]string{"exp": "e\x01"}},
		"other routes":     {path: "/api/v1/configs/topology/a", vars: map[string]string{"name": `a"b`}, wantNext: true},
		"user names alone": {path: "/api/v1/users/a", vars: map[string]string{"username": `a"b`}, wantNext: true},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			var reached bool

			handler := rejectUnsafeMinimegaVars(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				reached = true
			}))

			req := mux.SetURLVars(httptest.NewRequest(http.MethodPost, tc.path, nil), tc.vars)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if reached != tc.wantNext {
				t.Fatalf("handler reached = %t, want %t (status %d)", reached, tc.wantNext, rec.Code)
			}

			if !tc.wantNext && rec.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d", rec.Code)
			}
		})
	}
}

func TestBadMinimegaFilename(t *testing.T) {
	for name, bad := range map[string]bool{
		"snap":            false,
		"snap.qc2":        false,
		"dir/snap":        false,
		"":                false,
		"/etc/cron.d/x":   true,
		"../x":            true,
		"a/../../x":       true,
		"..":              true,
		"has space#x":     true,
		"$HOME":           true,
		"http://host/x":   true,
		"quote\"in\"name": true,
	} {
		if got := badMinimegaFilename(httptest.NewRecorder(), name); got != bad {
			t.Errorf("badMinimegaFilename(%q) = %t, want %t", name, got, bad)
		}
	}
}
