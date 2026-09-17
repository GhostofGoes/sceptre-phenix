package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gorilla/mux"

	"phenix/store"
	v1 "phenix/types/version/v1"
	"phenix/web/middleware"
	"phenix/web/rbac"
)

func adminRoleForTest() rbac.Role {
	return rbac.Role{Spec: &v1.RoleSpec{
		Name: "Test Admin",
		Policies: []*v1.PolicySpec{{
			Resources:     []string{"*", "*/*"},
			ResourceNames: []string{"*", "*/*"},
			Verbs:         []string{"*"},
		}},
	}}
}

func requestWithRole(req *http.Request, role rbac.Role) *http.Request {
	ctx := context.WithValue(req.Context(), middleware.ContextKeyRole, role)
	ctx = context.WithValue(ctx, middleware.ContextKeyUser, "tester")

	return req.WithContext(ctx)
}

func setupRoleStore(t *testing.T) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "store.bdb")
	if err := store.Init(store.Endpoint("bolt://" + path)); err != nil {
		t.Fatalf("initializing store: %v", err)
	}

	t.Cleanup(func() {
		store.DefaultStore = store.NewBoltDB()
	})
}

func TestMetadataNameForRole(t *testing.T) {
	got := metadataNameForRole("My Custom Role")
	if got != "my-custom-role" {
		t.Fatalf("metadataNameForRole = %q, want %q", got, "my-custom-role")
	}
}

func TestConfigFromRoleRequestValidatesPatterns(t *testing.T) {
	_, err := configFromRoleRequest(RoleRequest{
		Name: "Bad Role",
		Policies: []Policy{{
			Resources: []string{"["},
			Verbs:     []string{"list"},
		}},
	})
	if err == nil {
		t.Fatal("expected invalid resource pattern error")
	}
}

func TestGetRoleResourcesRequiresPermission(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/roles/resources", nil)
	req = requestWithRole(req, rbac.Role{Spec: &v1.RoleSpec{Name: "No Access"}})
	rec := httptest.NewRecorder()

	GetRoleResources(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
}

func TestGetRoleResourcesReturnsKnownResources(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/roles/resources", nil)
	req = requestWithRole(req, adminRoleForTest())
	rec := httptest.NewRecorder()

	GetRoleResources(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var body struct {
		Resources []KnownPermission `json:"resources"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(body.Resources) == 0 {
		t.Fatal("expected known resources")
	}
}

func TestUpdateRoleRejectsMetadataRename(t *testing.T) {
	req := httptest.NewRequest(
		http.MethodPut,
		"/roles/original",
		strings.NewReader(`{"metadata_name":"renamed","name":"Renamed","policies":[{"resources":["roles"],"verbs":["list"]}]}`),
	)
	req = mux.SetURLVars(requestWithRole(req, adminRoleForTest()), map[string]string{"name": "original"})
	rec := httptest.NewRecorder()

	UpdateRole(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestRoleCRUDHandlers(t *testing.T) {
	setupRoleStore(t)

	createReq := httptest.NewRequest(
		http.MethodPost,
		"/roles",
		strings.NewReader(`{"name":"Test Role","policies":[{"resources":["roles"],"resourceNames":["*"],"verbs":["list"]}]}`),
	)
	createReq = requestWithRole(createReq, adminRoleForTest())
	createRec := httptest.NewRecorder()

	CreateRole(createRec, createReq)

	if createRec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want %d: %s", createRec.Code, http.StatusCreated, createRec.Body.String())
	}

	role, err := rbac.RoleFromConfig("test-role")
	if err != nil {
		t.Fatalf("getting created role: %v", err)
	}
	if role.Spec.Name != "Test Role" {
		t.Fatalf("role name = %q, want Test Role", role.Spec.Name)
	}

	updateReq := httptest.NewRequest(
		http.MethodPut,
		"/roles/test-role",
		strings.NewReader(`{"metadata_name":"test-role","name":"Updated Role","policies":[{"resources":["roles"],"resourceNames":["*"],"verbs":["list","update"]}]}`),
	)
	updateReq = mux.SetURLVars(requestWithRole(updateReq, adminRoleForTest()), map[string]string{"name": "test-role"})
	updateRec := httptest.NewRecorder()

	UpdateRole(updateRec, updateReq)

	if updateRec.Code != http.StatusNoContent {
		t.Fatalf("update status = %d, want %d: %s", updateRec.Code, http.StatusNoContent, updateRec.Body.String())
	}

	role, err = rbac.RoleFromConfig("test-role")
	if err != nil {
		t.Fatalf("getting updated role: %v", err)
	}
	if role.Spec.Name != "Updated Role" {
		t.Fatalf("role name = %q, want Updated Role", role.Spec.Name)
	}

	deleteReq := httptest.NewRequest(http.MethodDelete, "/roles/test-role", nil)
	deleteReq = mux.SetURLVars(requestWithRole(deleteReq, adminRoleForTest()), map[string]string{"name": "test-role"})
	deleteRec := httptest.NewRecorder()

	DeleteRole(deleteRec, deleteReq)

	if deleteRec.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, want %d: %s", deleteRec.Code, http.StatusNoContent, deleteRec.Body.String())
	}
	if _, err = rbac.RoleFromConfig("test-role"); err == nil {
		t.Fatal("expected deleted role lookup to fail")
	}
}

func TestConfigFromRoleRequestCreatesRoleConfig(t *testing.T) {
	c, err := configFromRoleRequest(RoleRequest{
		Name: "Test Role",
		Policies: []Policy{{
			Resources:     []string{"roles"},
			ResourceNames: []string{"*"},
			Verbs:         []string{"list"},
		}},
	})
	if err != nil {
		t.Fatalf("configFromRoleRequest returned error: %v", err)
	}

	if c.Kind != "Role" {
		t.Fatalf("kind = %q, want Role", c.Kind)
	}
	if c.Metadata.Name != "test-role" {
		t.Fatalf("metadata name = %q, want test-role", c.Metadata.Name)
	}
	if c.Spec["roleName"] != "Test Role" {
		t.Fatalf("roleName = %q, want Test Role", c.Spec["roleName"])
	}
	if _, err := store.NewConfig("role/" + c.Metadata.Name); err != nil {
		t.Fatalf("role config name is invalid: %v", err)
	}
}
