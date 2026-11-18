package web

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gorilla/mux"

	"phenix/api/experiment"
	"phenix/store"
	v1 "phenix/types/version/v1"
	"phenix/web/middleware"
	"phenix/web/rbac"
	"phenix/web/weberror"
)

// vlanAliasTestExp is the stopped experiment the VLAN alias tests patch: its
// VLAN range is 100-200 and its VLAN EXP is unpinned.
const vlanAliasTestExp = "vlan-alias-test"

func useVLANAliasTestStore(t *testing.T) {
	t.Helper()

	original := store.DefaultStore
	t.Cleanup(func() { store.DefaultStore = original }) //nolint:reassign // restore default

	db := store.NewBoltDB()
	if err := db.Init(store.Endpoint("bolt://" + filepath.Join(t.TempDir(), "store.bdb"))); err != nil {
		t.Fatalf("initializing BoltDB: %v", err)
	}

	store.DefaultStore = db //nolint:reassign // install test store

	c := store.Config{
		Version:  "phenix.sandia.gov/v1",
		Kind:     "Experiment",
		Metadata: store.ConfigMetadata{Name: vlanAliasTestExp},
		Spec: map[string]any{
			"experimentName": vlanAliasTestExp,
			"baseDir":        t.TempDir(),
			"vlans": map[string]any{
				"aliases": map[string]any{"EXP": 0, "MGMT": 150},
				"min":     100,
				"max":     200,
			},
		},
	}

	if err := store.Create(&c); err != nil {
		t.Fatalf("storing experiment: %v", err)
	}
}

func patchVLANAliases(body string) error {
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/experiments/"+vlanAliasTestExp, bytes.NewBufferString(body))
	req = mux.SetURLVars(req, map[string]string{"name": vlanAliasTestExp})

	role := rbac.Role{
		Spec: &v1.RoleSpec{
			Policies: []*v1.PolicySpec{
				{Resources: []string{"experiments"}, ResourceNames: []string{"*"}, Verbs: []string{"patch"}},
			},
		},
	}

	ctx := context.WithValue(req.Context(), middleware.ContextKeyRole, role)
	ctx = context.WithValue(ctx, middleware.ContextKeyUser, "test-user")

	return UpdateExperiment(httptest.NewRecorder(), req.WithContext(ctx))
}

func storedVLANAliases(t *testing.T) map[string]int {
	t.Helper()

	exp, err := experiment.Get(vlanAliasTestExp)
	if err != nil {
		t.Fatalf("getting experiment: %v", err)
	}

	return exp.Spec.VLANs().Aliases()
}

// TestUpdateExperimentRefusesVLANIDsOutOfRange is a regression test: PATCH
// /experiments/{name} stored any VLAN ID for an alias, one outside the
// experiment's VLAN range, negative or above 4095 included, which phenix vlan
// set refuses and minimega refuses at start.
func TestUpdateExperimentRefusesVLANIDsOutOfRange(t *testing.T) {
	for _, body := range []string{
		`{"EXP": 99}`,
		`{"EXP": 200}`,
		`{"EXP": -1}`,
		`{"EXP": 5000}`,
		`{"EXP": 120, "MGMT": 300}`,
	} {
		t.Run(body, func(t *testing.T) {
			useVLANAliasTestStore(t)

			err := patchVLANAliases(body)

			var webErr *weberror.WebError
			if !errors.As(err, &webErr) || webErr.Status != http.StatusBadRequest || !strings.Contains(err.Error(), "unable to set VLAN") {
				t.Fatalf("PATCH %s returned %v, want 400 refusing the VLAN ID", body, err)
			}

			if got := storedVLANAliases(t); got["EXP"] != 0 || got["MGMT"] != 150 {
				t.Errorf("stored aliases after the refused PATCH = %v, want them unchanged", got)
			}
		})
	}
}

// TestUpdateExperimentPinsAndUnpinsVLANs checks that a PATCH still pins an ID
// in the range, unpins an alias set to 0 and ignores unknown aliases.
func TestUpdateExperimentPinsAndUnpinsVLANs(t *testing.T) {
	useVLANAliasTestStore(t)

	if err := patchVLANAliases(`{"EXP": 199, "MGMT": 0, "OTHER": 120}`); err != nil {
		t.Fatalf("PATCH returned %v", err)
	}

	got := storedVLANAliases(t)

	if got["EXP"] != 199 || got["MGMT"] != 0 {
		t.Errorf("stored aliases = %v, want EXP 199 and MGMT 0", got)
	}

	if _, ok := got["OTHER"]; ok {
		t.Errorf("stored aliases = %v, want no alias OTHER", got)
	}
}
