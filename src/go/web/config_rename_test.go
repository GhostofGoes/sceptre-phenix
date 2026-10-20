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
	"phenix/web/middleware"
	"phenix/web/rbac"
	"phenix/web/weberror"
)

// TestUpdateConfigAuthorizesTheBodysName is a regression test: PUT
// /configs/{kind}/{name} authorized only the URL's config, and config.Update
// stores the body under the body's own kind and name, so a role allowed to
// update only Experiment/lab-* renamed lab-1 to other-team-exp (creating it
// and deleting lab-1), overwrote other-team-exp, or overwrote a Role, its own
// included, with a body of kind Role.
func TestUpdateConfigAuthorizesTheBodysName(t *testing.T) {
	policy := func(verbs ...string) rbac.Role {
		return rbac.Role{Spec: &v1.RoleSpec{Policies: []*v1.PolicySpec{
			{Resources: []string{"configs"}, ResourceNames: []string{"Experiment/lab-*"}, Verbs: verbs},
		}}}
	}

	body := func(kind, name string) string {
		return `{"apiVersion":"phenix.sandia.gov/v1","kind":"` + kind + `","metadata":{"name":"` + name + `"},"spec":{}}`
	}

	for _, tc := range []struct {
		name   string
		role   rbac.Role
		body   string
		status int  // of the refusal, or 0 when the update reaches the store
		store  bool // whether the update reaches the store
	}{
		{"rename out of scope", policy("update"), body("Experiment", "other-team-exp"), http.StatusForbidden, false},
		{"rename in scope without create and delete", policy("update"), body("Experiment", "lab-2"), http.StatusForbidden, false},
		{"kind swap", policy("update"), body("Role", "lab-1"), http.StatusBadRequest, false},
		{"kind swap to a role of another name", policy("*"), body("Role", "global-admin"), http.StatusBadRequest, false},
		{"unknown kind", policy("*"), body("Nope", "lab-1"), http.StatusBadRequest, false},
		{"rename in scope", policy("create", "update", "delete"), body("Experiment", "lab-2"), 0, true},
		{"update in place", policy("update"), body("Experiment", "lab-1"), 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := installValidationTestStore(t)

			if tc.store {
				// The update gets as far as reading the stored config.
				m.EXPECT().Get(gomock.Any()).Return(store.ErrNotExist)
			}

			req := httptest.NewRequest(http.MethodPut, "/api/v1/configs/experiment/lab-1", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			req = mux.SetURLVars(req, map[string]string{"kind": "experiment", "name": "lab-1"})
			req = req.WithContext(context.WithValue(req.Context(), middleware.ContextKeyRole, tc.role))

			err := UpdateConfig(httptest.NewRecorder(), req)

			var werr *weberror.WebError
			if !errors.As(err, &werr) {
				t.Fatalf("UpdateConfig() = %v, want a web error", err)
			}

			if tc.status != 0 && werr.Status != tc.status {
				t.Errorf("UpdateConfig() = %d %q, want %d", werr.Status, werr.Message, tc.status)
			}

			if tc.store && !strings.Contains(werr.Message, "does not exist") {
				t.Errorf("UpdateConfig() = %d %q, want the update to reach the store", werr.Status, werr.Message)
			}
		})
	}
}
