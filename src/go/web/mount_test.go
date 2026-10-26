package web

import (
	"bytes"
	"context"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/gorilla/mux"

	v1 "phenix/types/version/v1"
	"phenix/util/common"
	"phenix/web/middleware"
	"phenix/web/rbac"
)

// installMountTestFiles builds an active mount for exp/vm whose guest file
// system holds a regular file and a symlink to a host directory outside the
// mount, plus a second active mount for exp/vm2. It returns the host directory.
func installMountTestFiles(t *testing.T) string {
	t.Helper()

	base := t.TempDir()
	mounts := filepath.Join(base, "mounts")
	host := filepath.Join(base, "host")

	for path, body := range map[string]string{
		filepath.Join(mounts, "exp", "vm", "guest.txt"):    "guest",
		filepath.Join(mounts, "exp", "vm2", "other.txt"):   "other vm",
		filepath.Join(host, "secret"):                      "host secret",
		filepath.Join(mounts, "exp", "vm", "dir", "a.txt"): "a",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if err := os.Symlink(host, filepath.Join(mounts, "exp", "vm", "escape")); err != nil {
		t.Fatal(err)
	}

	original := common.MountBase

	t.Cleanup(func() { common.MountBase = original }) //nolint:reassign // restore test fixture

	common.MountBase = mounts //nolint:reassign // test fixture

	activeMountsMu.Lock()
	for _, vm := range []string{"vm", "vm2"} {
		activeMounts[filepath.Join(mounts, "exp", vm)] = &MountInfo{1, &sync.RWMutex{}}
	}
	activeMountsMu.Unlock()

	t.Cleanup(func() {
		activeMountsMu.Lock()
		for _, vm := range []string{"vm", "vm2"} {
			delete(activeMounts, filepath.Join(mounts, "exp", vm))
		}
		activeMountsMu.Unlock()
	})

	return host
}

func mountRequest(method, path string, body *bytes.Buffer, contentType string) *http.Request {
	if body == nil {
		body = &bytes.Buffer{}
	}

	req := httptest.NewRequest(method, "/api/v1/experiments/exp/vms/vm/files?path="+url.QueryEscape(path), body)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	req = mux.SetURLVars(req, map[string]string{"exp": "exp", "name": "vm", "path": path})

	role := rbac.Role{
		Spec: &v1.RoleSpec{
			Policies: []*v1.PolicySpec{
				{Resources: []string{"vms/mount"}, ResourceNames: []string{"exp/vm"}, Verbs: []string{"*"}},
			},
		},
	}

	ctx := context.WithValue(req.Context(), middleware.ContextKeyRole, role)
	ctx = context.WithValue(ctx, middleware.ContextKeyUser, "test-user")

	return req.WithContext(ctx)
}

func TestDownloadMountFileStaysInMount(t *testing.T) {
	installMountTestFiles(t)

	tests := map[string]struct {
		path     string
		wantCode int
		wantBody string
	}{
		"guest file":            {path: "/guest.txt", wantCode: http.StatusOK, wantBody: "guest"},
		"guest file in dir":     {path: "/dir/a.txt", wantCode: http.StatusOK, wantBody: "a"},
		"symlink out of mount":  {path: "/escape/secret", wantCode: http.StatusBadRequest},
		"sibling mount":         {path: "/../vm2/other.txt", wantCode: http.StatusBadRequest},
		"relative sibling":      {path: "../vm2/other.txt", wantCode: http.StatusBadRequest},
		"parent of all mounts":  {path: "../../../host/secret", wantCode: http.StatusBadRequest},
		"directory not allowed": {path: "/dir", wantCode: http.StatusBadRequest},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			DownloadMountFile(rec, mountRequest(http.MethodGet, tc.path, nil, ""))

			if rec.Code != tc.wantCode {
				t.Fatalf("expected %d, got %d: %s", tc.wantCode, rec.Code, rec.Body.String())
			}

			if strings.Contains(rec.Body.String(), "host secret") && tc.wantCode != http.StatusOK {
				t.Fatalf("response leaked host file contents: %q", rec.Body.String())
			}

			if tc.wantBody != "" && rec.Body.String() != tc.wantBody {
				t.Fatalf("expected body %q, got %q", tc.wantBody, rec.Body.String())
			}
		})
	}
}

func TestGetMountFilesDoesNotFollowSymlinkOutOfMount(t *testing.T) {
	installMountTestFiles(t)

	rec := httptest.NewRecorder()
	GetMountFiles(rec, mountRequest(http.MethodGet, "/escape", nil, ""))

	if strings.Contains(rec.Body.String(), "\"secret\"") {
		t.Fatalf("listing followed a symlink out of the mount: %s", rec.Body.String())
	}

	rec = httptest.NewRecorder()
	GetMountFiles(rec, mountRequest(http.MethodGet, "/", nil, ""))

	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "guest.txt") {
		t.Fatalf("expected the mount root to list guest.txt, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestUploadMountFileStaysInMount(t *testing.T) {
	host := installMountTestFiles(t)

	upload := func(dir string) *httptest.ResponseRecorder {
		var body bytes.Buffer

		mw := multipart.NewWriter(&body)

		fw, err := mw.CreateFormFile("file", "planted.txt")
		if err != nil {
			t.Fatal(err)
		}

		_, _ = fw.Write([]byte("planted"))
		_ = mw.Close()

		rec := httptest.NewRecorder()
		UploadMountFile(rec, mountRequest(http.MethodPut, dir, &body, mw.FormDataContentType()))

		return rec
	}

	if rec := upload("/escape"); rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 uploading through a symlink, got %d: %s", rec.Code, rec.Body.String())
	}

	if _, err := os.Stat(filepath.Join(host, "planted.txt")); err == nil {
		t.Fatal("upload wrote a file outside the mount")
	}

	if rec := upload("/dir"); rec.Code != http.StatusOK {
		t.Fatalf("expected 200 uploading into the mount, got %d: %s", rec.Code, rec.Body.String())
	}
}
