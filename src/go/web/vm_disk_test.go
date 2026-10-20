package web

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/golang/mock/gomock"
	"github.com/gorilla/mux"

	"phenix/api/vm"
	"phenix/store"
	v1 "phenix/types/version/v1"
	"phenix/util/mm"
	"phenix/web/middleware"
	"phenix/web/rbac"
)

// TestUpdateVMsRefuseHostFileDisks checks that PATCH
// /experiments/{exp}/vms/{name} and PATCH /experiments/{exp}/vms answer 400
// naming the VM for a disk outside minimega's files directory, which the
// next start would give the guest, and store nothing.
func TestUpdateVMsRefuseHostFileDisks(t *testing.T) {
	original := store.DefaultStore
	t.Cleanup(func() { store.DefaultStore = original }) //nolint:reassign // restore test double

	c := store.Config{
		Version:  "phenix.sandia.gov/v1",
		Kind:     "Experiment",
		Metadata: store.ConfigMetadata{Name: "test-experiment"},
		Spec: map[string]any{
			"experimentName": "test-experiment",
			"topology": map[string]any{"nodes": []map[string]any{{
				"type":     "VirtualMachine",
				"general":  map[string]any{"hostname": "test-vm"},
				"hardware": map[string]any{"os_type": "linux", "drives": []map[string]any{{"image": "test.qc2"}}},
			}}},
		},
	}

	// The mock fails the test on any Update, so nothing may be stored.
	m := store.NewMockStore(gomock.NewController(t))
	m.EXPECT().Get(gomock.Any()).DoAndReturn(func(cfg *store.Config) error {
		*cfg = c

		return nil
	}).AnyTimes()

	store.DefaultStore = m //nolint:reassign // install test double

	role := rbac.Role{
		Spec: &v1.RoleSpec{
			Policies: []*v1.PolicySpec{
				{Resources: []string{"vms"}, ResourceNames: []string{"*/*"}, Verbs: []string{"patch"}},
			},
		},
	}

	for _, tc := range []struct {
		handler http.HandlerFunc
		path    string
		vars    map[string]string
		body    string
	}{
		{
			handler: UpdateVM,
			path:    "/api/v1/experiments/test-experiment/vms/test-vm",
			vars:    map[string]string{"exp": "test-experiment", "name": "test-vm"},
			body:    `{"disk":"../../../../etc/shadow"}`,
		},
		{
			handler: UpdateVMs,
			path:    "/api/v1/experiments/test-experiment/vms",
			vars:    map[string]string{"exp": "test-experiment"},
			body:    `{"total":1,"vms":[{"name":"test-vm","disk":"/etc/shadow"}]}`,
		},
	} {
		req := httptest.NewRequest(http.MethodPatch, tc.path, strings.NewReader(tc.body))
		req = mux.SetURLVars(req, tc.vars)
		req = req.WithContext(context.WithValue(req.Context(), middleware.ContextKeyRole, role))

		rec := httptest.NewRecorder()
		tc.handler(rec, req)

		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "VM test-vm") {
			t.Errorf("PATCH %s %s = %d %q, want 400 naming test-vm", tc.path, tc.body, rec.Code, rec.Body.String())
		}
	}
}

// TestChangeOpticalDiscRefusesHostFiles checks that POST
// /experiments/{exp}/vms/{name}/cdrom answers 400 naming the VM for an
// optical disc outside minimega's files directory, which QEMU would attach.
func TestChangeOpticalDiscRefusesHostFiles(t *testing.T) {
	original := store.DefaultStore
	t.Cleanup(func() { store.DefaultStore = original }) //nolint:reassign // restore test double

	installCaptureHandlerTestExperiment(t)

	req := vmRouteRequest(http.MethodPost, "/cdrom?isoPath=%2Fetc%2Fshadow",
		map[string]string{"exp": "test-experiment", "name": "test-vm"})

	rec := httptest.NewRecorder()
	ChangeOpticalDisc(rec, req)

	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "VM test-vm") {
		t.Errorf("POST cdrom?isoPath=/etc/shadow = %d %q, want 400 naming test-vm", rec.Code, rec.Body.String())
	}
}

// TestCommitVMRefusesHostPaths checks that POST
// /experiments/{exp}/vms/{name}/commit answers 400 for a file name that is a
// host path, which phenix truncated with a copy of the VM's base image.
func TestCommitVMRefusesHostPaths(t *testing.T) {
	original := store.DefaultStore
	t.Cleanup(func() { store.DefaultStore = original }) //nolint:reassign // restore test double

	installCaptureHandlerTestExperiment(t)

	role := rbac.Role{
		Spec: &v1.RoleSpec{
			Policies: []*v1.PolicySpec{
				{Resources: []string{"vms/commit"}, ResourceNames: []string{"*/*"}, Verbs: []string{"create"}},
			},
		},
	}

	for _, filename := range []string{"/etc/passwd", "../../etc/passwd"} {
		req := httptest.NewRequest(
			http.MethodPost,
			"/api/v1/experiments/test-experiment/vms/test-vm/commit",
			strings.NewReader(`{"filename":"`+filename+`"}`),
		)
		req = mux.SetURLVars(req, map[string]string{"exp": "test-experiment", "name": "test-vm"})
		req = req.WithContext(context.WithValue(req.Context(), middleware.ContextKeyRole, role))

		rec := httptest.NewRecorder()
		CommitVM(rec, req)

		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid file name") {
			t.Errorf("POST commit {filename: %q} = %d %q, want 400 for an invalid file name", filename, rec.Code, rec.Body.String())
		}
	}
}

// TestVMErrorStatusRefusals checks that the refusals of a disk, a file name
// or an image name phenix will not use answer 400, not 500.
func TestVMErrorStatusRefusals(t *testing.T) {
	for _, err := range []error{vm.ErrInvalidDisk, mm.ErrInvalidFileName, vm.ErrImageExists} {
		if got := vmErrorStatus(fmt.Errorf("refusing: %w", err)); got != http.StatusBadRequest {
			t.Errorf("vmErrorStatus(%v) = %d, want 400", err, got)
		}
	}
}
