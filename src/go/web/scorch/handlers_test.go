package scorch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gorilla/mux"

	v1 "phenix/types/version/v1"
	"phenix/web/middleware"
	"phenix/web/rbac"
)

// componentOutputRequest asks for a component's output in experiment "exp"
// as a user whose role may get only the named experiments.
func componentOutputRequest(allowed ...string) *http.Request {
	req := httptest.NewRequest(
		http.MethodGet,
		"/api/v1/experiments/exp/scorch/components/0/0/start/cc",
		nil,
	)
	req = mux.SetURLVars(req, map[string]string{
		"name": "exp", "run": "0", "loop": "0", "stage": "start", "cmp": "cc",
	})

	role := rbac.Role{
		Spec: &v1.RoleSpec{
			Policies: []*v1.PolicySpec{
				{
					Resources:     []string{"experiments"},
					ResourceNames: allowed,
					Verbs:         []string{"get"},
				},
			},
		},
	}

	ctx := context.WithValue(req.Context(), middleware.ContextKeyRole, role)
	ctx = context.WithValue(ctx, middleware.ContextKeyUser, "test-user")

	return req.WithContext(ctx)
}

func TestCanViewForbidsOtherExperiments(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	if canView(rec, componentOutputRequest("other"), "exp") {
		t.Fatal("canView allowed an experiment outside the role")
	}

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rec.Code)
	}
}

func TestCanViewAllowsRoleExperiments(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	if !canView(rec, componentOutputRequest("exp"), "exp") {
		t.Fatalf("canView refused an allowed experiment: %d", rec.Code)
	}
}

func TestCanViewWithoutRole(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()

	if canView(rec, req, "exp") {
		t.Fatal("canView allowed a request without a role")
	}
}
