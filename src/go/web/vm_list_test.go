package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/mux"

	"phenix/store"
	v1 "phenix/types/version/v1"
	"phenix/util/mm"
	"phenix/web/middleware"
	"phenix/web/rbac"
)

// partialListMM lists the VMs of the hosts that answered, and the error of
// one that did not.
type partialListMM struct {
	mm.MM
}

func (partialListMM) GetVMInfoErr(...mm.Option) (mm.VMs, error) {
	return mm.VMs{{Name: "test-vm", Host: "c1", State: "RUNNING", Running: true}}, errors.New("c2: timed out")
}

// TestGetVMsListsTheVMsOfHostsThatAnswered is a regression test: GET
// /experiments/{exp}/vms answered 500 and listed nothing when one host of a
// running experiment did not answer minimega's vm info in time, where it had
// listed the VMs of the other hosts.
func TestGetVMsListsTheVMsOfHostsThatAnswered(t *testing.T) {
	originalStore, originalMM := store.DefaultStore, mm.DefaultMM

	t.Cleanup(func() {
		store.DefaultStore = originalStore //nolint:reassign // restore test double
		mm.DefaultMM = originalMM          //nolint:reassign // restore test double
	})

	installCaptureHandlerTestExperiment(t)

	mm.DefaultMM = partialListMM{} //nolint:reassign // install test double

	role := rbac.Role{
		Spec: &v1.RoleSpec{
			Policies: []*v1.PolicySpec{
				{Resources: []string{"vms"}, ResourceNames: []string{"*", "*/*"}, Verbs: []string{"list"}},
			},
		},
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/experiments/test-experiment/vms", nil)
	req = mux.SetURLVars(req, map[string]string{"exp": "test-experiment"})
	req = req.WithContext(context.WithValue(req.Context(), middleware.ContextKeyRole, role))

	rec := httptest.NewRecorder()
	GetVMs(rec, req)

	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"test-vm"`) {
		t.Errorf("GET vms = %d %q, want 200 listing test-vm", rec.Code, rec.Body.String())
	}
}
