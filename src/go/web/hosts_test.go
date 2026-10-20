package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/activeshadow/structs"
	"github.com/golang/mock/gomock"
	"github.com/gorilla/mux"

	"phenix/store"
	"phenix/types"
	v1 "phenix/types/version/v1"
	"phenix/util/common"
	"phenix/util/mm"
	"phenix/web/middleware"
	"phenix/web/rbac"
)

type hostsTestMM struct {
	mm.MM

	hosts mm.Hosts
	calls *int // counts GetClusterHosts calls when set
}

func (m hostsTestMM) GetClusterHosts(bool) (mm.Hosts, error) {
	if m.calls != nil {
		*m.calls++
	}

	return m.hosts, nil
}

// TestGetClusterHostsDisplaysTrimmedNames checks that GET /hosts lists each host
// by its name without the configured suffixes, authorizes it by that name, and
// gives the name minimega reports as minimega_name.
func TestGetClusterHostsDisplaysTrimmedNames(t *testing.T) {
	originalMM := mm.DefaultMM
	originalSuffixes := common.HostnameSuffixes

	t.Cleanup(func() {
		mm.DefaultMM = originalMM                  //nolint:reassign // restore test double
		common.HostnameSuffixes = originalSuffixes //nolint:reassign // restore configuration
	})

	mm.DefaultMM = hostsTestMM{hosts: mm.Hosts{ //nolint:reassign // install test double
		{Name: "compute1-minimega", Schedulable: true},
		{Name: "e2e-head-minimega", Schedulable: true, Headnode: true},
	}}
	common.HostnameSuffixes = "-minimega,-phenix" //nolint:reassign // configure suffixes

	role := rbac.Role{
		Spec: &v1.RoleSpec{
			Policies: []*v1.PolicySpec{
				{
					Resources:     []string{"hosts"},
					ResourceNames: []string{"e2e-head"},
					Verbs:         []string{"list"},
				},
			},
		},
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/hosts", nil)
	req = req.WithContext(context.WithValue(req.Context(), middleware.ContextKeyRole, role))

	rec := httptest.NewRecorder()
	GetClusterHosts(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /hosts = %d %s", rec.Code, rec.Body.String())
	}

	var got struct {
		Hosts []struct {
			Name         string `json:"name"`
			MinimegaName string `json:"minimega_name"`
			Headnode     bool   `json:"headnode"`
		} `json:"hosts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding GET /hosts: %v", err)
	}

	// The UI's host select of a scheduled VM holds minimega's name, so the
	// response carries it beside the display name.
	if len(got.Hosts) != 1 || got.Hosts[0].Name != "e2e-head" || got.Hosts[0].MinimegaName != "e2e-head-minimega" ||
		!got.Hosts[0].Headnode {
		t.Errorf("GET /hosts listed %+v, want only the headnode named e2e-head, e2e-head-minimega to minimega", got.Hosts)
	}
}

// TestUpdateVMSchedulesOnMinimegasHostName is a regression test: GET /hosts
// lists display names without the -minimega suffix, the UI schedules a VM on
// the name it lists, and phenix stored that name, so the start failed with
// minimega's `VM scheduled on unknown host`. It is also one for a host that
// names no cluster host: phenix stored it as given, and the minimega script
// writes it raw, so a host holding a line break ran any minimega command, such
// as shell, as root when the experiment started.
func TestUpdateVMSchedulesOnMinimegasHostName(t *testing.T) {
	originalStore, originalMM := store.DefaultStore, mm.DefaultMM
	originalSuffixes := common.HostnameSuffixes

	t.Cleanup(func() {
		store.DefaultStore = originalStore         //nolint:reassign // restore test double
		mm.DefaultMM = originalMM                  //nolint:reassign // restore test double
		common.HostnameSuffixes = originalSuffixes //nolint:reassign // restore configuration
	})

	mm.DefaultMM = hostsTestMM{hosts: mm.Hosts{ //nolint:reassign // install test double
		{Name: "compute1-minimega", Schedulable: true},
		{Name: "compute2", Schedulable: true},
	}}
	common.HostnameSuffixes = "-minimega,-phenix" //nolint:reassign // configure suffixes

	exp := types.NewExperiment(store.ConfigMetadata{Name: "exp1"})
	exp.Spec.SetExperimentName("exp1")

	spec := structs.MapDefaultCase(exp.Spec, structs.CASESNAKE)
	spec["topology"] = map[string]any{"nodes": []map[string]any{{
		"type":     "VirtualMachine",
		"general":  map[string]any{"hostname": "vm1"},
		"hardware": map[string]any{"os_type": "linux"},
		"network":  map[string]any{"interfaces": []map[string]any{}},
	}}}

	stored := store.Config{
		Version:  "phenix.sandia.gov/v1",
		Kind:     "Experiment",
		Metadata: store.ConfigMetadata{Name: "exp1"},
		Spec:     spec,
		Status:   structs.MapDefaultCase(exp.Status, structs.CASESNAKE),
	}

	m := store.NewMockStore(gomock.NewController(t))
	m.EXPECT().Get(gomock.Any()).DoAndReturn(func(c *store.Config) error {
		*c = stored

		return nil
	}).AnyTimes()
	m.EXPECT().Update(gomock.Any()).DoAndReturn(func(c *store.Config) error {
		stored = *c

		return nil
	}).AnyTimes()

	store.DefaultStore = m //nolint:reassign // install test double

	role := rbac.Role{
		Spec: &v1.RoleSpec{
			Policies: []*v1.PolicySpec{
				{Resources: []string{"vms"}, ResourceNames: []string{"*/*"}, Verbs: []string{"patch"}},
			},
		},
	}

	for _, tc := range []struct {
		host, want string
		code       int
	}{
		{"compute1", "compute1-minimega", http.StatusOK},
		{"compute1-minimega", "compute1-minimega", http.StatusOK},
		{"compute2", "compute2", http.StatusOK},
		{"unknown", "compute2", http.StatusBadRequest},
		{`compute2\nshell touch /tmp/pwned`, "compute2", http.StatusBadRequest},
		{"compute", "compute2", http.StatusBadRequest},
	} {
		req := httptest.NewRequest(
			http.MethodPatch,
			"/api/v1/experiments/exp1/vms/vm1",
			strings.NewReader(`{"host":"`+tc.host+`"}`),
		)
		req = mux.SetURLVars(req, map[string]string{"exp": "exp1", "name": "vm1"})
		req = req.WithContext(context.WithValue(req.Context(), middleware.ContextKeyRole, role))

		rec := httptest.NewRecorder()
		UpdateVM(rec, req)

		updated, err := types.DecodeExperimentFromConfig(stored)
		if err != nil {
			t.Fatalf("decoding the stored experiment: %v", err)
		}

		if got := updated.Spec.Schedules()["vm1"]; rec.Code != tc.code || got != tc.want {
			t.Errorf("PATCH host %s = %d %q, schedule %q; want %d and %q", tc.host, rec.Code, rec.Body.String(), got, tc.code, tc.want)
		}
	}

	// The bulk update lists the cluster hosts once, and refuses an unknown
	// host before it changes any VM.
	var calls int

	mm.DefaultMM = hostsTestMM{hosts: mm.Hosts{ //nolint:reassign // install test double
		{Name: "compute1-minimega", Schedulable: true},
		{Name: "compute2", Schedulable: true},
	}, calls: &calls}

	for _, tc := range []struct {
		body, want  string
		code, calls int
	}{
		{`{"total":2,"vms":[{"name":"vm1","host":"compute1"},{"name":"vm1","host":"compute2"}]}`, "compute2", http.StatusOK, 1},
		{`{"total":2,"vms":[{"name":"vm1","host":"compute1"},{"name":"vm1","host":"unknown"}]}`, "compute2", http.StatusBadRequest, 1},
	} {
		calls = 0

		req := httptest.NewRequest(http.MethodPatch, "/api/v1/experiments/exp1/vms", strings.NewReader(tc.body))
		req = mux.SetURLVars(req, map[string]string{"exp": "exp1"})
		req = req.WithContext(context.WithValue(req.Context(), middleware.ContextKeyRole, role))

		rec := httptest.NewRecorder()
		UpdateVMs(rec, req)

		updated, err := types.DecodeExperimentFromConfig(stored)
		if err != nil {
			t.Fatalf("decoding the stored experiment: %v", err)
		}

		if got := updated.Spec.Schedules()["vm1"]; rec.Code != tc.code || got != tc.want || calls != tc.calls {
			t.Errorf(
				"PATCH %s = %d %q, schedule %q, %d host listings; want %d, %q and %d",
				tc.body, rec.Code, rec.Body.String(), got, calls, tc.code, tc.want, tc.calls,
			)
		}
	}
}
