package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gorilla/mux"

	v1 "phenix/types/version/v1"
	"phenix/util/mm"
	"phenix/web/middleware"
	"phenix/web/rbac"
)

// vncTestMM records whether the handler asked minimega for a VNC endpoint.
type vncTestMM struct {
	mm.MM

	endpointCalls int
}

func (m *vncTestMM) GetVNCEndpoint(...mm.Option) (string, error) {
	m.endpointCalls++

	return "", errNoVNC
}

var errNoVNC = errors.New("no vnc endpoint")

func vncWebSocketRequest(role rbac.Role) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/experiments/exp/vms/vm/vnc/ws", nil)
	req = mux.SetURLVars(req, map[string]string{"exp": "exp", "name": "vm"})

	ctx := context.WithValue(req.Context(), middleware.ContextKeyRole, role)
	ctx = context.WithValue(ctx, middleware.ContextKeyUser, "test-user")

	return req.WithContext(ctx)
}

func vncTestRole(resource string, names ...string) rbac.Role {
	return rbac.Role{
		Spec: &v1.RoleSpec{
			Policies: []*v1.PolicySpec{
				{Resources: []string{resource}, ResourceNames: names, Verbs: []string{"get"}},
			},
		},
	}
}

func installVNCTestMM(t *testing.T) *vncTestMM {
	t.Helper()

	fake := &vncTestMM{}
	original := mm.DefaultMM

	t.Cleanup(func() { mm.DefaultMM = original }) //nolint:reassign // restore test double

	mm.DefaultMM = fake //nolint:reassign // install test double

	return fake
}

func TestGetVNCWebSocketRequiresVNCPermission(t *testing.T) {
	tests := map[string]struct {
		role     rbac.Role
		wantCode int
		wantCall bool
	}{
		"no vnc policy": {
			role:     vncTestRole("experiments", "*"),
			wantCode: http.StatusForbidden,
		},
		"vnc on another vm": {
			role:     vncTestRole("vms/vnc", "exp/other"),
			wantCode: http.StatusForbidden,
		},
		"vnc on this vm": {
			role:     vncTestRole("vms/vnc", "exp/vm"),
			wantCode: http.StatusBadRequest, // fake minimega has no endpoint
			wantCall: true,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			fake := installVNCTestMM(t)

			rec := httptest.NewRecorder()
			GetVNCWebSocket(rec, vncWebSocketRequest(tc.role))

			if rec.Code != tc.wantCode {
				t.Fatalf("expected %d, got %d: %s", tc.wantCode, rec.Code, rec.Body.String())
			}

			if called := fake.endpointCalls > 0; called != tc.wantCall {
				t.Fatalf("expected minimega lookup %t, got %t", tc.wantCall, called)
			}
		})
	}
}
