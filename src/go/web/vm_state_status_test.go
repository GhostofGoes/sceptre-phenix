package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/mux"

	v1 "phenix/types/version/v1"
	"phenix/util/mm"
	"phenix/web/middleware"
	"phenix/web/rbac"
)

// vmStateMM lists vm1, with one interface, as running or not. The interface
// is on EXP_1 unless vlan names another.
type vmStateMM struct {
	mm.MM

	running bool
	vlan    string
}

func (m vmStateMM) GetVMInfo(...mm.Option) mm.VMs {
	state := "PAUSED"
	if m.running {
		state = "RUNNING"
	}

	vlan := "EXP_1"
	if m.vlan != "" {
		vlan = m.vlan
	}

	return mm.VMs{{Name: "vm1", Host: "head", State: state, Running: m.running, Networks: []string{vlan}}}
}

func (m vmStateMM) GetVMInfoErr(opts ...mm.Option) (mm.VMs, error) {
	return m.GetVMInfo(opts...), nil
}

// TestSnapshotAndCaptureRefusalsOfRunningExperiments is a regression test: in
// a running experiment, a snapshot or capture of a VM that is not running,
// such as a paused one, and a capture on an interface the VM does not have or
// one that is disconnected answered 500, as if phenix or minimega had failed.
// They answer 409 and 400, as the other VM routes do.
func TestSnapshotAndCaptureRefusalsOfRunningExperiments(t *testing.T) {
	useVMTestStore(t, "2026-10-20T00:00:00Z")

	original := mm.DefaultMM

	t.Cleanup(func() { mm.DefaultMM = original }) //nolint:reassign // restore test double

	role := rbac.Role{
		Spec: &v1.RoleSpec{
			Policies: []*v1.PolicySpec{{
				Resources:     []string{"vms/snapshots", "vms/captures"},
				ResourceNames: []string{"*/*"},
				Verbs:         []string{"create"},
			}},
		},
	}

	for _, tc := range []struct {
		name, route, body string
		handler           http.HandlerFunc
		running           bool
		vlan              string
		want              int
	}{
		{"snapshot of a paused VM", "snapshots", `{"filename":"s1"}`, SnapshotVM, false, "", http.StatusConflict},
		{"capture on a paused VM", "captures", `{"interface":0,"filename":"c1.pcap"}`, StartVMCapture, false, "", http.StatusConflict},
		{"capture on no interface", "captures", `{"interface":5,"filename":"c1.pcap"}`, StartVMCapture, true, "", http.StatusBadRequest},
		{"capture on a disconnected interface", "captures", `{"interface":0,"filename":"c1.pcap"}`, StartVMCapture, true, "disconnected", http.StatusBadRequest},
	} {
		mm.DefaultMM = vmStateMM{running: tc.running, vlan: tc.vlan} //nolint:reassign // install test double

		req := httptest.NewRequest(http.MethodPost, "/api/v1/experiments/exp1/vms/vm1/"+tc.route, strings.NewReader(tc.body))
		req = mux.SetURLVars(req, map[string]string{"exp": "exp1", "name": "vm1"})
		req = req.WithContext(context.WithValue(req.Context(), middleware.ContextKeyRole, role))

		rec := httptest.NewRecorder()
		tc.handler(rec, req)

		if rec.Code != tc.want {
			t.Errorf("%s = %d %q, want %d", tc.name, rec.Code, rec.Body.String(), tc.want)
		}
	}
}
