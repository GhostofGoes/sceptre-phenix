package web

import (
	"context"
	"errors"
	"fmt"
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

// screenshotHandlerTestMM is a test double for mm.MM whose screenshots fail
// with the given error.
type screenshotHandlerTestMM struct {
	mm.MM

	err error
}

func (m screenshotHandlerTestMM) GetVMScreenshot(...mm.Option) ([]byte, error) {
	return nil, m.err
}

// TestGetScreenshotStatus checks the status of a refused screenshot: 409 for a
// VM that is not running, such as a paused one, 400 for an unknown VM or a
// missing screenshot, and 500 for a minimega failure.
func TestGetScreenshotStatus(t *testing.T) {
	original := mm.DefaultMM
	t.Cleanup(func() { mm.DefaultMM = original }) //nolint:reassign // restore test double

	useVMTestStore(t, "2026-10-18T00:00:00Z")

	role := rbac.Role{
		Spec: &v1.RoleSpec{
			Policies: []*v1.PolicySpec{
				{
					Resources:     []string{"vms/screenshot"},
					ResourceNames: []string{"*/*"},
					Verbs:         []string{"get"},
				},
			},
		},
	}

	for _, tc := range []struct {
		err        error
		wantStatus int
		wantBody   string
	}{
		{err: mm.ErrVMNotRunning, wantStatus: http.StatusConflict, wantBody: "VM is not running"},
		{err: mm.ErrVMNotFound, wantStatus: http.StatusBadRequest, wantBody: "VM not found"},
		{err: mm.ErrScreenshotNotFound, wantStatus: http.StatusBadRequest, wantBody: "screenshot not found"},
		// A minimega failure is not the client's fault.
		{err: errors.New("screenshotting VM vm1: qmp timeout"), wantStatus: http.StatusInternalServerError, wantBody: "qmp timeout"},
	} {
		t.Run(tc.err.Error(), func(t *testing.T) {
			mm.DefaultMM = screenshotHandlerTestMM{err: tc.err} //nolint:reassign // install test double

			// A name of its own keeps a screenshot cached by another test out.
			exp := strings.ReplaceAll(t.Name(), "/", "_")

			req := httptest.NewRequest(
				http.MethodGet,
				fmt.Sprintf("/api/v1/experiments/%s/vms/vm1/screenshot.png", exp),
				nil,
			)
			req = mux.SetURLVars(req, map[string]string{"exp": exp, "name": "vm1"})
			req = req.WithContext(context.WithValue(req.Context(), middleware.ContextKeyRole, role))

			rec := httptest.NewRecorder()
			GetScreenshot(rec, req)

			if rec.Code != tc.wantStatus || !strings.Contains(rec.Body.String(), tc.wantBody) {
				t.Errorf(
					"GetScreenshot() = %d %q, want %d containing %q",
					rec.Code, rec.Body.String(), tc.wantStatus, tc.wantBody,
				)
			}
		})
	}
}
