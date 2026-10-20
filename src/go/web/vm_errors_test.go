package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/golang/mock/gomock"
	"github.com/gorilla/mux"

	"phenix/store"
	v1 "phenix/types/version/v1"
	"phenix/util/mm"
	"phenix/web/middleware"
	"phenix/web/rbac"
)

// vmRouteRole allows every VM operation on every VM.
func vmRouteRole() rbac.Role {
	return rbac.Role{
		Spec: &v1.RoleSpec{
			Policies: []*v1.PolicySpec{{
				Resources: []string{
					"vms", "vms/start", "vms/stop", "vms/restart", "vms/shutdown", "vms/reset",
					"vms/redeploy", "vms/cdrom", "vms/captures", "vms/snapshots", "vms/vnc",
					"experiments/captures",
				},
				ResourceNames: []string{"*", "*/*"},
				Verbs:         []string{"update", "delete", "get", "list"},
			}},
		},
	}
}

func vmRouteRequest(method, path string, vars map[string]string) *http.Request {
	req := httptest.NewRequest(method, "/api/v1/experiments/exp1/vms/vm1"+path, strings.NewReader(""))
	req = mux.SetURLVars(req, vars)

	ctx := context.WithValue(req.Context(), middleware.ContextKeyRole, vmRouteRole())
	ctx = context.WithValue(ctx, middleware.ContextKeyUser, "test-user")

	return req.WithContext(ctx)
}

// TestVMOperationsRefuseStoppedExperimentsWith400 is a regression test: the
// restart, shutdown, reset, redeploy, disc, capture and restore routes
// answered 500, a server error, for the client-side precondition that the
// experiment is not running, which start, stop and VNC answer with 400; and
// redeploy did so only after its redeploying event and a 5 s sleep.
func TestVMOperationsRefuseStoppedExperimentsWith400(t *testing.T) {
	original := mm.DefaultMM

	t.Cleanup(func() { mm.DefaultMM = original }) //nolint:reassign // restore test double

	mm.DefaultMM = noVMMM{} //nolint:reassign // install test double

	vars := map[string]string{"exp": "exp1", "name": "vm1", "snapshot": "snap1"}

	handlers := []struct {
		name    string
		method  string
		path    string
		handler http.HandlerFunc
	}{
		{"restart", http.MethodGet, "/restart", RestartVM},
		{"shutdown", http.MethodGet, "/shutdown", ShutdownVM},
		{"reset", http.MethodGet, "/reset", ResetVM},
		{"redeploy", http.MethodPost, "/redeploy", RedeployVM},
		{"change disc", http.MethodPost, "/cdrom?isoPath=a.iso", ChangeOpticalDisc},
		{"eject disc", http.MethodDelete, "/cdrom", EjectOpticalDisc},
		{"stop captures", http.MethodDelete, "/captures", StopVMCaptures},
		{"restore", http.MethodPost, "/snapshots/snap1", RestoreVM},
	}

	for _, state := range []struct{ name, startTime string }{
		{"stopped", ""},
		{"dry run", "2026-10-18T00:00:00Z-DRYRUN"},
	} {
		t.Run(state.name, func(t *testing.T) {
			useVMTestStore(t, state.startTime)

			for _, h := range handlers {
				t.Run(h.name, func(t *testing.T) {
					defer func() {
						if r := recover(); r != nil {
							t.Errorf("the %s route reached minimega: %v", h.name, r)
						}
					}()

					rec := httptest.NewRecorder()
					h.handler(rec, vmRouteRequest(h.method, h.path, vars))

					if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "not running") {
						t.Errorf("the %s route = %d %q, want 400 saying the experiment is not running",
							h.name, rec.Code, rec.Body.String())
					}
				})
			}
		})
	}
}

// TestVMRoutesTellMissingExperimentsFromStoreFailures is a regression test:
// listing the captures of an experiment that does not exist answered 500, and
// a store failure on a VM start or stop answered 400, a client error.
func TestVMRoutesTellMissingExperimentsFromStoreFailures(t *testing.T) {
	original := mm.DefaultMM

	t.Cleanup(func() { mm.DefaultMM = original }) //nolint:reassign // restore test double

	mm.DefaultMM = noVMMM{} //nolint:reassign // install test double

	for _, tc := range []struct {
		name     string
		storeErr error
		want     int
	}{
		{"missing experiment", store.ErrNotExist, http.StatusNotFound},
		{"store failure", errors.New("bolt: database not open"), http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := store.NewMockStore(gomock.NewController(t))
			m.EXPECT().Get(gomock.Any()).Return(tc.storeErr).AnyTimes()

			originalStore := store.DefaultStore

			t.Cleanup(func() { store.DefaultStore = originalStore }) //nolint:reassign // restore test double

			store.DefaultStore = m //nolint:reassign // install test double

			for _, h := range []struct {
				name    string
				handler http.HandlerFunc
				vars    map[string]string
			}{
				{"experiment captures", GetExperimentCaptures, map[string]string{"name": "exp1"}},
				{"VM captures", GetVMCaptures, map[string]string{"exp": "exp1", "name": "vm1"}},
				{"start", StartVM, map[string]string{"exp": "exp1", "name": "vm1"}},
				{"stop", StopVM, map[string]string{"exp": "exp1", "name": "vm1"}},
			} {
				rec := httptest.NewRecorder()
				h.handler(rec, vmRouteRequest(http.MethodGet, "", h.vars))

				if rec.Code != tc.want {
					t.Errorf("the %s route = %d %q, want %d", h.name, rec.Code, rec.Body.String(), tc.want)
				}
			}
		})
	}
}
