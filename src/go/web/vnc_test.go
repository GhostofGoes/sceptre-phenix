package web

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

// TestGetVNCWebSocketRequiresVNCPermission is a regression test: the VNC
// websocket, which carries the VM's console, did not check the vms/vnc
// permission that the VNC page checks.
func TestGetVNCWebSocketRequiresVNCPermission(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/experiments/exp/vms/vm1/vnc/ws", nil)
	req = mux.SetURLVars(req, map[string]string{"exp": "exp", "name": "vm1"})

	// Allowed to see the VM, but not its console.
	role := rbac.Role{
		Spec: &v1.RoleSpec{
			Policies: []*v1.PolicySpec{
				{Resources: []string{"vms"}, ResourceNames: []string{"*/*"}, Verbs: []string{"get"}},
			},
		},
	}

	ctx := context.WithValue(req.Context(), middleware.ContextKeyRole, role)
	ctx = context.WithValue(ctx, middleware.ContextKeyUser, "test-user")

	rec := httptest.NewRecorder()
	GetVNCWebSocket(rec, req.WithContext(ctx))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}
