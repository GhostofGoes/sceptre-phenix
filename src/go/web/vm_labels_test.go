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

// labelsMM accepts every tag change, so only phenix's own check can refuse
// a label.
type labelsMM struct {
	mm.MM
}

func (labelsMM) SetVMTags(...mm.Option) error { return nil }

// TestUpdateVMRefusesLabelMinimegaCannotStore checks that PATCH
// /experiments/{exp}/vms/{name} answers 400 naming the VM and the label for a
// label minimega cannot store as a tag of a running VM, instead of sending it
// to minimega, which exited on such a label.
func TestUpdateVMRefusesLabelMinimegaCannotStore(t *testing.T) {
	originalStore, originalMM := store.DefaultStore, mm.DefaultMM
	t.Cleanup(func() {
		store.DefaultStore = originalStore //nolint:reassign // restore test double
		mm.DefaultMM = originalMM          //nolint:reassign // restore test double
	})

	// A running experiment whose store fails the test on any update.
	installCaptureHandlerTestExperiment(t)

	mm.DefaultMM = labelsMM{} //nolint:reassign // install test double

	role := rbac.Role{
		Spec: &v1.RoleSpec{
			Policies: []*v1.PolicySpec{
				{Resources: []string{"vms"}, ResourceNames: []string{"*/*"}, Verbs: []string{"patch"}},
			},
		},
	}

	req := httptest.NewRequest(
		http.MethodPatch,
		"/api/v1/experiments/test-experiment/vms/test-vm",
		strings.NewReader(`{"tag_update_mode":"SET","tags":{"role":"web","note":"it's"}}`),
	)
	req = mux.SetURLVars(req, map[string]string{"exp": "test-experiment", "name": "test-vm"})
	req = req.WithContext(context.WithValue(req.Context(), middleware.ContextKeyRole, role))

	rec := httptest.NewRecorder()
	UpdateVM(rec, req)

	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `label "note" of VM test-vm`) {
		t.Errorf("UpdateVM() = %d %q, want 400 naming test-vm and the label note", rec.Code, rec.Body.String())
	}
}

// connectCheckMM runs the real minimega client's checks of a `vm net connect`
// command; a command they accept fails the test.
type connectCheckMM struct {
	mm.MM

	t *testing.T
}

func (m connectCheckMM) ConnectVMInterface(opts ...mm.Option) error {
	err := mm.Minimega{}.ConnectVMInterface(opts...)
	if !errors.Is(err, mm.ErrUnsafeArgument) {
		m.t.Errorf("ConnectVMInterface() = %v, want ErrUnsafeArgument before minimega", err)
	}

	return err
}

// TestUpdateVMRefusesVLANMinimegaCannotCarry is a regression test: PATCH
// /experiments/{exp}/vms/{name} and PATCH /experiments/{exp}/vms sent the
// VLAN of an interface unchecked in `vm net connect`, and a VLAN such as a\"b
// made minimega exit for every experiment. Both answer 400.
func TestUpdateVMRefusesVLANMinimegaCannotCarry(t *testing.T) {
	originalStore, originalMM := store.DefaultStore, mm.DefaultMM
	t.Cleanup(func() {
		store.DefaultStore = originalStore //nolint:reassign // restore test double
		mm.DefaultMM = originalMM          //nolint:reassign // restore test double
	})

	installCaptureHandlerTestExperiment(t)

	mm.DefaultMM = connectCheckMM{t: t} //nolint:reassign // install test double

	role := rbac.Role{
		Spec: &v1.RoleSpec{
			Policies: []*v1.PolicySpec{
				{Resources: []string{"vms"}, ResourceNames: []string{"*/*"}, Verbs: []string{"patch"}},
			},
		},
	}

	for _, tc := range []struct {
		path, body string
		handler    http.HandlerFunc
	}{
		{"/vms/test-vm", `{"interface":{"index":0,"vlan":"a\\\"b"}}`, UpdateVM},
		{"/vms", `{"total":1,"vms":[{"name":"test-vm","interface":{"index":0,"vlan":"a\\\"b"}}]}`, UpdateVMs},
	} {
		req := httptest.NewRequest(
			http.MethodPatch,
			"/api/v1/experiments/test-experiment"+tc.path,
			strings.NewReader(tc.body),
		)
		req = mux.SetURLVars(req, map[string]string{"exp": "test-experiment", "name": "test-vm"})
		req = req.WithContext(context.WithValue(req.Context(), middleware.ContextKeyRole, role))

		rec := httptest.NewRecorder()
		tc.handler(rec, req)

		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "VLAN") {
			t.Errorf("PATCH %s = %d %q, want 400 naming the VLAN", tc.path, rec.Code, rec.Body.String())
		}
	}
}
