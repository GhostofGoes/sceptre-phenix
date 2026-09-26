package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/golang/mock/gomock"
	"github.com/gorilla/mux"
	"google.golang.org/protobuf/encoding/protojson"

	"phenix/store"
	v1 "phenix/types/version/v1"
	"phenix/util/file"
	"phenix/util/mm"
	"phenix/web/middleware"
	"phenix/web/proto"
	"phenix/web/rbac"
)

// experimentCapturesTestMM stubs the experiment capture listing; any other
// mm.MM method panics through the nil embedded interface.
type experimentCapturesTestMM struct {
	mm.MM

	captures []mm.Capture
}

func (m *experimentCapturesTestMM) GetExperimentCaptures(...mm.Option) []mm.Capture {
	return m.captures
}

// experimentFilesTestFiles stubs the experiment file listing; any other
// file.ClusterFiles method panics through the nil embedded interface.
type experimentFilesTestFiles struct {
	file.ClusterFiles

	files file.Files
}

func (f *experimentFilesTestFiles) GetExperimentFiles(string, string) (file.Files, error) {
	return f.files, nil
}

// testRole returns a role with one policy.
func testRole(resource, verb string, names ...string) rbac.Role {
	return rbac.Role{
		Spec: &v1.RoleSpec{
			Policies: []*v1.PolicySpec{
				{
					Resources:     []string{resource},
					ResourceNames: names,
					Verbs:         []string{verb},
				},
			},
		},
	}
}

func requestAs(req *http.Request, role rbac.Role, vars map[string]string) *http.Request {
	req = mux.SetURLVars(req, vars)

	ctx := context.WithValue(req.Context(), middleware.ContextKeyRole, role)
	ctx = context.WithValue(ctx, middleware.ContextKeyUser, "test-user")

	return req.WithContext(ctx)
}

func TestGetExperimentCapturesFiltersByExperimentVM(t *testing.T) {
	fake := &experimentCapturesTestMM{
		captures: []mm.Capture{
			{VM: "vm-a", Interface: 0, Filepath: "/tmp/a.pcap"},
			{VM: "vm-b", Interface: 0, Filepath: "/tmp/b.pcap"},
		},
	}

	original := mm.DefaultMM
	t.Cleanup(func() { mm.DefaultMM = original }) //nolint:reassign // restore test double

	mm.DefaultMM = fake //nolint:reassign // install test double

	// a role scoped to one VM of the experiment, named exp/vm like every other
	// VM resource name
	role := testRole("experiments/captures", "list", "test-experiment", "test-experiment/vm-a")

	req := requestAs(
		httptest.NewRequest(http.MethodGet, "/api/v1/experiments/test-experiment/captures", nil),
		role,
		map[string]string{"name": "test-experiment"},
	)

	rec := httptest.NewRecorder()
	GetExperimentCaptures(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var list proto.CaptureList
	if err := protojson.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("unmarshaling captures: %v", err)
	}

	if len(list.GetCaptures()) != 1 || list.GetCaptures()[0].GetVm() != "vm-a" {
		t.Fatalf("expected only vm-a's capture, got %v", list.GetCaptures())
	}
}

func TestGetExperimentFilesReportsTotalBeforePaging(t *testing.T) {
	original := file.DefaultClusterFiles
	t.Cleanup(func() { file.DefaultClusterFiles = original }) //nolint:reassign // restore test double

	file.DefaultClusterFiles = &experimentFilesTestFiles{ //nolint:reassign // test double
		files: file.Files{
			{Name: "a", Path: "a"},
			{Name: "b", Path: "b"},
			{Name: "c", Path: "c"},
		},
	}

	role := testRole("experiments/files", "list", "test-experiment")

	tests := map[string]struct {
		query     string
		wantCode  int
		wantFiles int
	}{
		"paged":         {query: "?pageNum=1&perPage=2", wantCode: http.StatusOK, wantFiles: 2},
		"last page":     {query: "?pageNum=2&perPage=2", wantCode: http.StatusOK, wantFiles: 1},
		"not paged":     {query: "", wantCode: http.StatusOK, wantFiles: 3},
		"zero page":     {query: "?pageNum=0&perPage=2", wantCode: http.StatusBadRequest},
		"negative size": {query: "?pageNum=1&perPage=-2", wantCode: http.StatusBadRequest},
		"not a number":  {query: "?pageNum=one&perPage=2", wantCode: http.StatusBadRequest},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			req := requestAs(
				httptest.NewRequest(
					http.MethodGet, "/api/v1/experiments/test-experiment/files"+tc.query, nil,
				),
				role,
				map[string]string{"name": "test-experiment"},
			)

			rec := httptest.NewRecorder()
			GetExperimentFiles(rec, req)

			if rec.Code != tc.wantCode {
				t.Fatalf("expected %d, got %d: %s", tc.wantCode, rec.Code, rec.Body.String())
			}

			if tc.wantCode != http.StatusOK {
				return
			}

			var body struct {
				Files []file.File `json:"files"`
				Total int         `json:"total"`
			}

			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("unmarshaling response: %v", err)
			}

			if body.Total != 3 {
				t.Fatalf("expected total 3, got %d", body.Total)
			}

			if len(body.Files) != tc.wantFiles {
				t.Fatalf("expected %d files, got %d", tc.wantFiles, len(body.Files))
			}
		})
	}
}

func TestStopCaptureSubnetNeedsDelete(t *testing.T) {
	tests := map[string]struct {
		role     rbac.Role
		wantCode int
	}{
		// the handler reads the (invalid) body only once the role check passes
		"delete allowed": {
			role:     testRole("exp/captureSubnet", "delete", "test-experiment"),
			wantCode: http.StatusBadRequest,
		},
		"create only": {
			role:     testRole("exp/captureSubnet", "create", "test-experiment"),
			wantCode: http.StatusForbidden,
		},
		"other experiment": {
			role:     testRole("exp/captureSubnet", "delete", "other-experiment"),
			wantCode: http.StatusForbidden,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			req := requestAs(
				httptest.NewRequest(
					http.MethodPost,
					"/api/v1/experiments/test-experiment/stopCaptureSubnet",
					strings.NewReader("not json"),
				),
				tc.role,
				map[string]string{"exp": "test-experiment"},
			)

			rec := httptest.NewRecorder()
			StopCaptureSubnet(rec, req)

			if rec.Code != tc.wantCode {
				t.Fatalf("expected %d, got %d: %s", tc.wantCode, rec.Code, rec.Body.String())
			}
		})
	}
}

func TestGetExperimentSoHNeedsExperimentGet(t *testing.T) {
	ctrl := gomock.NewController(t)
	t.Cleanup(ctrl.Finish)

	original := store.DefaultStore
	t.Cleanup(func() { store.DefaultStore = original }) //nolint:reassign // restore test double

	m := store.NewMockStore(ctrl)
	m.EXPECT().Get(gomock.Any()).Return(errors.New("no such experiment")).AnyTimes()

	store.DefaultStore = m //nolint:reassign // install test double

	tests := map[string]struct {
		role     rbac.Role
		wantCode int
	}{
		// allowed through to the lookup, which the mock store fails
		"experiment get": {
			role:     testRole("experiments", "get", "test-experiment"),
			wantCode: http.StatusInternalServerError,
		},
		"other experiment": {
			role:     testRole("experiments", "get", "other-experiment"),
			wantCode: http.StatusForbidden,
		},
		"vms list only": {
			role:     testRole("vms", "list", "*"),
			wantCode: http.StatusForbidden,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			req := requestAs(
				httptest.NewRequest(http.MethodGet, "/api/v1/experiments/test-experiment/soh", nil),
				tc.role,
				map[string]string{"name": "test-experiment"},
			)

			rec := httptest.NewRecorder()
			GetExperimentSoH(rec, req)

			if rec.Code != tc.wantCode {
				t.Fatalf("expected %d, got %d: %s", tc.wantCode, rec.Code, rec.Body.String())
			}
		})
	}
}
