package forward

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gorilla/mux"

	v1 "phenix/types/version/v1"
	"phenix/util/mm"
	ft "phenix/web/forward/forwardtypes"
	"phenix/web/middleware"
	"phenix/web/rbac"
)

// forwardRole allows the port forward verbs on every VM, and the VNC console
// too when vnc is set.
func forwardRole(vnc bool) rbac.Role {
	policies := []*v1.PolicySpec{{
		Resources:     []string{"vms/forwards"},
		ResourceNames: []string{"*/*"},
		Verbs:         []string{"create", "get"},
	}}

	if vnc {
		policies = append(policies, &v1.PolicySpec{
			Resources:     []string{"vms/vnc"},
			ResourceNames: []string{"*/*"},
			Verbs:         []string{"get"},
		})
	}

	return rbac.Role{Spec: &v1.RoleSpec{Policies: policies}}
}

func forwardRequest(method, target string, vars map[string]string, role rbac.Role) *http.Request {
	req := httptest.NewRequest(method, target, nil)
	req = mux.SetURLVars(req, vars)

	ctx := context.WithValue(req.Context(), middleware.ContextKeyRole, role)
	ctx = context.WithValue(ctx, middleware.ContextKeyUser, "user")

	return req.WithContext(ctx)
}

// TestVNCForwardsNeedTheVNCPermission is a regression test: the VNC websocket
// needs vms/vnc, but a port forward to a VM's VNC endpoint, created by the user
// or by anyone else such as the startup app's autotunnels, carried the same
// console with only vms/forwards.
func TestVNCForwardsNeedTheVNCPermission(t *testing.T) {
	installExperimentStore(t, map[string]string{"stopped": "", "running": "2026-10-18T00:00:00Z"}, nil)

	original := mm.DefaultMM

	t.Cleanup(func() { mm.DefaultMM = original }) //nolint:reassign // restore test double

	mm.DefaultMM = noForwardMM{} //nolint:reassign // install test double

	t.Run("create", func(t *testing.T) {
		vars := map[string]string{"exp": "stopped", "name": "vm1"}

		for _, tc := range []struct {
			vnc  bool
			want int
		}{
			{vnc: false, want: http.StatusForbidden},
			// Past the permission check, the stopped experiment refuses it.
			{vnc: true, want: http.StatusBadRequest},
		} {
			rec := httptest.NewRecorder()
			CreatePortForward(rec, forwardRequest(http.MethodPost, "/forwards?src=5901&dst=VNC", vars, forwardRole(tc.vnc)))

			if rec.Code != tc.want {
				t.Errorf("creating a VNC forward with vms/vnc %v = %d %q, want %d", tc.vnc, rec.Code, rec.Body.String(), tc.want)
			}
		}
	})

	t.Run("connect", func(t *testing.T) {
		for _, owner := range []string{"user", "bot"} {
			l := ft.Listener{
				Exp: "running", VM: "vm1", DstHost: "127.0.0.1", DstPort: 5900, Owner: owner,
				ClusterHost: "127.0.0.1", ClusterPort: 1, QEMU: true,
			}

			forwardsMu.Lock()
			forwards[l.ToKey()] = l
			forwardsMu.Unlock()

			vars := map[string]string{"exp": "running", "name": "vm1", "host": "127.0.0.1", "port": "5900"}

			rec := httptest.NewRecorder()
			GetPortForwardWebSocket(rec, forwardRequest(http.MethodGet, "/forwards/127.0.0.1/5900/ws", vars, forwardRole(false)))

			if rec.Code != http.StatusForbidden {
				t.Errorf("connecting to %s's VNC forward without vms/vnc = %d %q, want 403", owner, rec.Code, rec.Body.String())
			}

			forwardsMu.Lock()
			delete(forwards, l.ToKey())
			forwardsMu.Unlock()
		}
	})
}
