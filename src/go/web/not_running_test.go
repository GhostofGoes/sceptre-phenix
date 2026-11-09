package web

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/activeshadow/structs"
	"github.com/golang/mock/gomock"
	"github.com/gorilla/mux"

	"phenix/store"
	"phenix/types"
	v1 "phenix/types/version/v1"
	"phenix/util/mm"
	"phenix/web/middleware"
	"phenix/web/rbac"
)

// noVMMM is a minimega double whose every method panics, so a call reaching
// it fails the test.
type noVMMM struct {
	mm.MM
}

// useVMTestStore makes the store hold, under any name, an experiment with the
// VM vm1 and the given start time until the test ends.
func useVMTestStore(t *testing.T, startTime string) {
	t.Helper()

	m := store.NewMockStore(gomock.NewController(t))
	m.EXPECT().Get(gomock.Any()).DoAndReturn(func(c *store.Config) error {
		name := c.Metadata.Name

		exp := types.NewExperiment(store.ConfigMetadata{Name: name})
		exp.Spec.SetExperimentName(name)
		exp.Status.SetStartTime(startTime)

		spec := structs.MapDefaultCase(exp.Spec, structs.CASESNAKE)
		spec["topology"] = map[string]any{"nodes": []map[string]any{{
			"type":     "VirtualMachine",
			"general":  map[string]any{"hostname": "vm1"},
			"hardware": map[string]any{"os_type": "linux"},
			"network":  map[string]any{"interfaces": []map[string]any{}},
		}}}

		*c = store.Config{
			Version:  "phenix.sandia.gov/v1",
			Kind:     "Experiment",
			Metadata: store.ConfigMetadata{Name: name},
			Spec:     spec,
			Status:   structs.MapDefaultCase(exp.Status, structs.CASESNAKE),
		}

		return nil
	}).AnyTimes()

	original := store.DefaultStore

	t.Cleanup(func() { store.DefaultStore = original }) //nolint:reassign // restore test double

	store.DefaultStore = m //nolint:reassign // install test double
}

// TestVMHandlersLeaveMinimegaAloneWhenNotRunning is a regression test: the VM
// start, stop and VNC routes sent their commands to minimega whatever the
// experiment's state, and the VM delete route did so for a dry run, so using
// them on such an experiment made minimega create its namespace, empty. They
// answer 400 without asking minimega.
func TestVMHandlersLeaveMinimegaAloneWhenNotRunning(t *testing.T) {
	original := mm.DefaultMM

	t.Cleanup(func() { mm.DefaultMM = original }) //nolint:reassign // restore test double

	mm.DefaultMM = noVMMM{} //nolint:reassign // install test double

	role := rbac.Role{
		Spec: &v1.RoleSpec{
			Policies: []*v1.PolicySpec{{
				Resources:     []string{"vms", "vms/start", "vms/stop", "vms/vnc"},
				ResourceNames: []string{"*/*"},
				Verbs:         []string{"update", "delete", "get"},
			}},
		},
	}

	handlers := []struct {
		name    string
		method  string
		path    string
		handler http.HandlerFunc
	}{
		{"start", http.MethodPost, "/start", StartVM},
		{"stop", http.MethodPost, "/stop", StopVM},
		{"VNC websocket", http.MethodGet, "/vnc/ws", GetVNCWebSocket},
		{"delete", http.MethodDelete, "", DeleteVM},
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

					req := httptest.NewRequest(h.method, "/api/v1/experiments/exp1/vms/vm1"+h.path, nil)
					req = mux.SetURLVars(req, map[string]string{"exp": "exp1", "name": "vm1"})

					ctx := context.WithValue(req.Context(), middleware.ContextKeyRole, role)
					ctx = context.WithValue(ctx, middleware.ContextKeyUser, "test-user")

					rec := httptest.NewRecorder()
					h.handler(rec, req.WithContext(ctx))

					if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "not running") {
						t.Errorf("the %s route = %d %q, want 400 saying the experiment is not running",
							h.name, rec.Code, rec.Body.String())
					}
				})
			}
		})
	}
}

// TestRefusalsOfStoppedExperimentsAnswer400 is a regression test: on a
// stopped experiment the snapshot, capture start and subnet capture routes
// answered 500, where the other VM routes answer 400.
func TestRefusalsOfStoppedExperimentsAnswer400(t *testing.T) {
	original := mm.DefaultMM

	t.Cleanup(func() { mm.DefaultMM = original }) //nolint:reassign // restore test double

	mm.DefaultMM = noVMMM{} //nolint:reassign // install test double

	useVMTestStore(t, "")

	role := rbac.Role{Spec: &v1.RoleSpec{Policies: []*v1.PolicySpec{{
		Resources:     []string{"vms/snapshots", "vms/captures", "exp/captureSubnet"},
		ResourceNames: []string{"*", "*/*"},
		Verbs:         []string{"create"},
	}}}}

	for _, h := range []struct {
		path, body string
		handler    http.HandlerFunc
	}{
		{"/vms/vm1/snapshots", `{"filename":"s1"}`, SnapshotVM},
		{"/vms/vm1/captures", `{"interface":0,"filename":"c1.pcap"}`, StartVMCapture},
		{"/captureSubnet", `{"subnet":"10.0.0.0/24"}`, StartCaptureSubnet},
		{"/stopCaptureSubnet", `{"subnet":"10.0.0.0/24"}`, StopCaptureSubnet},
	} {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/experiments/exp1"+h.path, strings.NewReader(h.body))
		req = mux.SetURLVars(req, map[string]string{"exp": "exp1", "name": "vm1"})
		req = req.WithContext(context.WithValue(req.Context(), middleware.ContextKeyRole, role))

		rec := httptest.NewRecorder()
		h.handler(rec, req)

		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "not running") {
			t.Errorf("POST %s = %d %q, want 400 saying the experiment is not running", h.path, rec.Code, rec.Body.String())
		}
	}
}

// vmNotFoundMM answers every VM command as minimega does for a name it does
// not know.
type vmNotFoundMM struct {
	mm.MM
}

func (vmNotFoundMM) StartVM(...mm.Option) error { return fmt.Errorf("starting: %w", mm.ErrVMNotFound) }
func (vmNotFoundMM) StopVM(...mm.Option) error  { return fmt.Errorf("stopping: %w", mm.ErrVMNotFound) }
func (vmNotFoundMM) KillVM(...mm.Option) error  { return fmt.Errorf("killing: %w", mm.ErrVMNotFound) }

// TestVMRoutesAnswer400ForVMsMinimegaDoesNotKnow is a regression test: the VM
// start, stop and delete routes answered minimega's `vm not found` with 500.
func TestVMRoutesAnswer400ForVMsMinimegaDoesNotKnow(t *testing.T) {
	original := mm.DefaultMM

	t.Cleanup(func() { mm.DefaultMM = original }) //nolint:reassign // restore test double

	mm.DefaultMM = vmNotFoundMM{} //nolint:reassign // install test double

	useVMTestStore(t, "2026-10-18T00:00:00Z") // running

	role := rbac.Role{Spec: &v1.RoleSpec{Policies: []*v1.PolicySpec{{
		Resources:     []string{"vms", "vms/start", "vms/stop"},
		ResourceNames: []string{"*/*"},
		Verbs:         []string{"update", "delete"},
	}}}}

	for _, h := range []struct {
		method, path string
		handler      http.HandlerFunc
	}{
		{http.MethodPost, "/start", StartVM},
		{http.MethodPost, "/stop", StopVM},
		{http.MethodDelete, "", DeleteVM},
	} {
		req := httptest.NewRequest(h.method, "/api/v1/experiments/exp1/vms/vm9"+h.path, nil)
		req = mux.SetURLVars(req, map[string]string{"exp": "exp1", "name": "vm9"})
		req = req.WithContext(context.WithValue(req.Context(), middleware.ContextKeyRole, role))

		rec := httptest.NewRecorder()
		h.handler(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s %s = %d %q, want 400", h.method, h.path, rec.Code, rec.Body.String())
		}
	}
}
