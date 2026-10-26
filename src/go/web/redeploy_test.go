package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/mux"

	"phenix/store"
	v1 "phenix/types/version/v1"
	"phenix/web/middleware"
	"phenix/web/rbac"
)

// TestRedeployRequest is a regression test: a redeploy with an empty body sent
// the disk vm.Get reports, which for a VM on a disk snapshot is the snapshot's
// backing image, so the VM was relaunched on the image without phenix's
// injections.
func TestRedeployRequest(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		inject  bool
		cpus    uint32
		ram     uint32
		disk    string
		injects bool
	}{
		{name: "empty body keeps the VM's settings"},
		{name: "empty body with replicate-injects", inject: true, injects: true},
		{name: "empty object", body: "{}"},
		{
			name:    "new settings",
			body:    `{"cpus":2,"ram":1024,"disk":"other.qc2","injects":true}`,
			cpus:    2,
			ram:     1024,
			disk:    "other.qc2",
			injects: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req, err := redeployRequest([]byte(test.body), test.inject)
			if err != nil {
				t.Fatalf("redeployRequest returned error: %v", err)
			}

			if req.GetCpus() != test.cpus || req.GetRam() != test.ram || req.GetDisk() != test.disk ||
				req.GetInjects() != test.injects {
				t.Errorf("redeployRequest = cpus %d, ram %d, disk %q, injects %v, want %d, %d, %q, %v",
					req.GetCpus(), req.GetRam(), req.GetDisk(), req.GetInjects(),
					test.cpus, test.ram, test.disk, test.injects)
			}
		})
	}
}

func TestRedeployRequestRejectsAnInvalidBody(t *testing.T) {
	if _, err := redeployRequest([]byte("{"), false); err == nil {
		t.Error("redeployRequest accepted an invalid body")
	}
}

// TestRedeployVMRefusesAVMOfNoNode is a regression test: the redeploy route
// looked the VM up first and answered any failure with 500, so a VM that is
// no node of the topology got 500 instead of the 400 the route documents.
func TestRedeployVMRefusesAVMOfNoNode(t *testing.T) {
	originalStore := store.DefaultStore
	t.Cleanup(func() { store.DefaultStore = originalStore }) //nolint:reassign // restore test double

	installCaptureHandlerTestExperiment(t) // running, with the node test-vm

	role := rbac.Role{Spec: &v1.RoleSpec{Policies: []*v1.PolicySpec{{
		Resources: []string{"vms/redeploy"}, ResourceNames: []string{"*/*"}, Verbs: []string{"update"},
	}}}}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/experiments/test-experiment/vms/nosuchvm/redeploy", nil)
	req = mux.SetURLVars(req, map[string]string{"exp": "test-experiment", "name": "nosuchvm"})
	req = req.WithContext(context.WithValue(req.Context(), middleware.ContextKeyRole, role))

	rec := httptest.NewRecorder()
	RedeployVM(rec, req)

	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "nosuchvm is not a node") {
		t.Errorf("RedeployVM(nosuchvm) = %d %q, want 400 saying it is not a node", rec.Code, rec.Body.String())
	}
}
