package forward

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/mux"

	"phenix/api/experiment"
	v1 "phenix/types/version/v1"
	"phenix/util/mm"
	ft "phenix/web/forward/forwardtypes"
	"phenix/web/middleware"
	"phenix/web/rbac"
)

// noForwardMM is a minimega double whose every method panics, so a call
// reaching it fails the test.
type noForwardMM struct {
	mm.MM
}

// TestForwardsLeaveMinimegaAloneWhenNotRunning is a regression test: creating
// and deleting a port forward asked minimega about the VM whatever the
// experiment's state, so either request for a stopped or dry-run experiment
// made minimega create its namespace, empty.
func TestForwardsLeaveMinimegaAloneWhenNotRunning(t *testing.T) {
	installExperimentStore(t, map[string]string{
		"stopped": "",
		"dry-run": "2026-10-18T00:00:00Z-DRYRUN",
	}, nil)

	original := mm.DefaultMM

	t.Cleanup(func() { mm.DefaultMM = original }) //nolint:reassign // restore test double

	mm.DefaultMM = noForwardMM{} //nolint:reassign // install test double

	role := rbac.Role{
		Spec: &v1.RoleSpec{
			Policies: []*v1.PolicySpec{{
				Resources:     []string{"vms/forwards"},
				ResourceNames: []string{"*/*"},
				Verbs:         []string{"delete"},
			}},
		},
	}

	for _, exp := range []string{"stopped", "dry-run"} {
		t.Run(exp, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("a forward request reached minimega: %v", r)
				}
			}()

			for _, dst := range []string{"22", "VNC"} {
				err := createPortForward(exp, "vm1", "8080", "", dst, "user")
				if !errors.Is(err, experiment.ErrExperimentNotRunning) {
					t.Errorf("creating a %s forward = %v, want ErrExperimentNotRunning", dst, err)
				}
			}

			// A forward phenix ui still holds from before a stop elsewhere.
			l := ft.Listener{
				Exp:     exp,
				VM:      "vm1",
				DstHost: "127.0.0.1",
				DstPort: 22,
				Owner:   "user",
			}

			forwardsMu.Lock()
			forwards[l.ToKey()] = l
			forwardsMu.Unlock()

			t.Cleanup(func() {
				forwardsMu.Lock()
				delete(forwards, l.ToKey())
				forwardsMu.Unlock()
			})

			req := httptest.NewRequest(http.MethodDelete, "/api/v1/experiments/"+exp+"/vms/vm1/forwards?dst=22", nil)
			req = mux.SetURLVars(req, map[string]string{"exp": exp, "name": "vm1"})

			ctx := context.WithValue(req.Context(), middleware.ContextKeyRole, role)
			ctx = context.WithValue(ctx, middleware.ContextKeyUser, "user")

			rec := httptest.NewRecorder()
			DeletePortForward(rec, req.WithContext(ctx))

			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "forward not found") {
				t.Errorf("deleting the forward = %d %q, want 400 forward not found: the reaper drops it", rec.Code, rec.Body.String())
			}

			forwardsMu.Lock()
			_, kept := forwards[l.ToKey()]
			forwardsMu.Unlock()

			if kept {
				t.Error("phenix ui still holds the forward of the experiment that is not running")
			}
		})
	}
}
