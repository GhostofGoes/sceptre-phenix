package web

import (
	"bytes"
	"context"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/gorilla/mux"

	v1 "phenix/types/version/v1"
	"phenix/util/common"
	"phenix/util/mm"
	"phenix/web/middleware"
	"phenix/web/rbac"
)

// mountTestRequest builds a request for a mount handler of VM vm1 in
// experiment "exp", with a role that allows every mount verb.
func mountTestRequest(method, target string, body *bytes.Buffer, contentType string) *http.Request {
	if body == nil {
		body = &bytes.Buffer{}
	}

	req := httptest.NewRequest(method, target, body)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	req = mux.SetURLVars(req, map[string]string{"exp": "exp", "name": "vm1", "path": req.URL.Query().Get("path")})

	role := rbac.Role{
		Spec: &v1.RoleSpec{
			Policies: []*v1.PolicySpec{
				{
					Resources:     []string{"vms/mount"},
					ResourceNames: []string{"*/*"},
					Verbs:         []string{"list", "get", "patch", "create", "update"},
				},
			},
		},
	}

	ctx := context.WithValue(req.Context(), middleware.ContextKeyRole, role)
	ctx = context.WithValue(ctx, middleware.ContextKeyUser, "test-user")

	return req.WithContext(ctx)
}

// TestMountHandlersRejectSiblingMounts is a regression test: the mount
// handlers checked that the requested path had the VM's mount path as a
// string prefix, so "../vm1b" from VM vm1 reached the mount of VM vm1b.
func TestMountHandlersRejectSiblingMounts(t *testing.T) {
	original, originalMounted := common.MountBase, vmMounted
	common.MountBase = t.TempDir() //nolint:reassign // test configuration

	t.Cleanup(func() {
		common.MountBase = original //nolint:reassign // restore
		vmMounted = originalMounted
	})

	own := mm.GetLocalMountPath("exp", "vm1")
	sibling := mm.GetLocalMountPath("exp", "vm1b")

	// Both VMs are mounted, through this process.
	vmMounted = func(path string) bool { return path == own || path == sibling }

	for _, dir := range []string{own, sibling} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
	}

	if err := os.WriteFile(filepath.Join(sibling, "secret"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}

	activeMountsMu.Lock()
	activeMounts[own] = &MountInfo{users: 1, lock: &sync.RWMutex{}}
	activeMounts[sibling] = &MountInfo{users: 1, lock: &sync.RWMutex{}}
	activeMountsMu.Unlock()

	t.Cleanup(func() {
		activeMountsMu.Lock()
		delete(activeMounts, own)
		delete(activeMounts, sibling)
		activeMountsMu.Unlock()
	})

	t.Run("list", func(t *testing.T) {
		rec := httptest.NewRecorder()
		GetMountFiles(rec, mountTestRequest(http.MethodGet, "/files?path=../vm1b", nil, ""))

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("listing ../vm1b from vm1: status %d (%s), want 400", rec.Code, rec.Body.String())
		}
	})

	t.Run("download", func(t *testing.T) {
		rec := httptest.NewRecorder()
		DownloadMountFile(rec, mountTestRequest(http.MethodGet, "/files/download?path=../vm1b/secret", nil, ""))

		if rec.Code != http.StatusBadRequest || bytes.Contains(rec.Body.Bytes(), []byte("secret\n")) {
			t.Fatalf("downloading ../vm1b/secret from vm1: status %d (%s), want 400", rec.Code, rec.Body.String())
		}
	})

	t.Run("upload", func(t *testing.T) {
		var body bytes.Buffer

		w := multipart.NewWriter(&body)

		part, err := w.CreateFormFile("file", "planted")
		if err != nil {
			t.Fatal(err)
		}

		_, _ = part.Write([]byte("planted"))
		_ = w.Close()

		rec := httptest.NewRecorder()
		UploadMountFile(rec, mountTestRequest(http.MethodPut, "/files/upload?path=../vm1b", &body, w.FormDataContentType()))

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("uploading into ../vm1b from vm1: status %d (%s), want 400", rec.Code, rec.Body.String())
		}

		if _, err := os.Stat(filepath.Join(sibling, "planted")); !os.IsNotExist(err) {
			t.Fatalf("upload landed in the sibling mount (stat: %v)", err)
		}
	})

	t.Run("own mount still works", func(t *testing.T) {
		rec := httptest.NewRecorder()
		GetMountFiles(rec, mountTestRequest(http.MethodGet, "/files?path=/", nil, ""))

		if rec.Code != http.StatusOK {
			t.Fatalf("listing vm1's own mount: status %d (%s), want 200", rec.Code, rec.Body.String())
		}
	})
}

// TestMountHandlersServeMountsOfOtherProcesses is a regression test: the file
// handlers knew only the mounts made through this process, so a VM mounted
// with `phenix vm mount` was refused with "No existing mount".
func TestMountHandlersServeMountsOfOtherProcesses(t *testing.T) {
	original, originalMounted := common.MountBase, vmMounted
	common.MountBase = t.TempDir() //nolint:reassign // test configuration

	t.Cleanup(func() {
		common.MountBase = original //nolint:reassign // restore
		vmMounted = originalMounted
	})

	own := mm.GetLocalMountPath("exp", "vm1")

	if err := os.MkdirAll(filepath.Join(own, "etc"), 0o750); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(own, "etc", "hostname"), []byte("vm1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	mounted := true
	vmMounted = func(path string) bool { return mounted && path == own }

	t.Run("list", func(t *testing.T) {
		rec := httptest.NewRecorder()
		GetMountFiles(rec, mountTestRequest(http.MethodGet, "/files?path=/", nil, ""))

		if rec.Code != http.StatusOK || !bytes.Contains(rec.Body.Bytes(), []byte(`"etc"`)) {
			t.Fatalf("listing vm1's mount: status %d (%s), want 200 listing etc", rec.Code, rec.Body.String())
		}
	})

	t.Run("download", func(t *testing.T) {
		rec := httptest.NewRecorder()
		DownloadMountFile(rec, mountTestRequest(http.MethodGet, "/files/download?path=/etc/hostname", nil, ""))

		if rec.Code != http.StatusOK || rec.Body.String() != "vm1\n" {
			t.Fatalf("downloading /etc/hostname: status %d (%q), want 200 with vm1", rec.Code, rec.Body.String())
		}
	})

	t.Run("not mounted", func(t *testing.T) {
		mounted = false

		rec := httptest.NewRecorder()
		GetMountFiles(rec, mountTestRequest(http.MethodGet, "/files?path=/", nil, ""))

		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "No existing mount") {
			t.Fatalf("listing an unmounted VM: status %d (%s), want 400 No existing mount", rec.Code, rec.Body.String())
		}
	})
}

// TestUploadMountFileReplacesTheFile is a regression test: an upload over an
// existing file kept the old file's tail when the new one was shorter, and a
// request without a file part panicked the handler.
func TestUploadMountFileReplacesTheFile(t *testing.T) {
	original, originalMounted := common.MountBase, vmMounted
	common.MountBase = t.TempDir() //nolint:reassign // test configuration

	t.Cleanup(func() {
		common.MountBase = original //nolint:reassign // restore
		vmMounted = originalMounted
	})

	own := mm.GetLocalMountPath("exp", "vm1")
	vmMounted = func(path string) bool { return path == own }

	if err := os.MkdirAll(filepath.Join(own, "etc"), 0o750); err != nil {
		t.Fatal(err)
	}

	target := filepath.Join(own, "etc", "app.conf")
	if err := os.WriteFile(target, []byte("OLD-CONTENT-THAT-IS-LONG\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	activeMountsMu.Lock()
	activeMounts[own] = &MountInfo{users: 1, lock: &sync.RWMutex{}}
	activeMountsMu.Unlock()

	t.Cleanup(func() {
		activeMountsMu.Lock()
		delete(activeMounts, own)
		activeMountsMu.Unlock()
	})

	t.Run("shorter file", func(t *testing.T) {
		var body bytes.Buffer

		w := multipart.NewWriter(&body)

		part, err := w.CreateFormFile("file", "app.conf")
		if err != nil {
			t.Fatal(err)
		}

		_, _ = part.Write([]byte("new\n"))
		_ = w.Close()

		rec := httptest.NewRecorder()
		UploadMountFile(rec, mountTestRequest(http.MethodPut, "/files/upload?path=/etc", &body, w.FormDataContentType()))

		got, err := os.ReadFile(target)
		if rec.Code != http.StatusOK || err != nil || string(got) != "new\n" {
			t.Errorf("upload = %d %q, file now %q (%v); want 200 and exactly the upload", rec.Code, rec.Body.String(), got, err)
		}
	})

	t.Run("no file part", func(t *testing.T) {
		var body bytes.Buffer

		w := multipart.NewWriter(&body)
		_ = w.WriteField("other", "x")
		_ = w.Close()

		rec := httptest.NewRecorder()
		UploadMountFile(rec, mountTestRequest(http.MethodPut, "/files/upload?path=/etc", &body, w.FormDataContentType()))

		if rec.Code != http.StatusBadRequest {
			t.Errorf("upload without a file part = %d %q, want 400", rec.Code, rec.Body.String())
		}
	})
}

// TestMountVMLeavesMountsOfOtherProcesses is a regression test: MountVM on a
// VM that `phenix vm mount` had mounted recorded the mount as its own, so the
// UnmountVM the UI sends when its file browser closes unmounted the CLI
// user's mount.
func TestMountVMLeavesMountsOfOtherProcesses(t *testing.T) {
	original, originalMounted := common.MountBase, vmMounted
	common.MountBase = t.TempDir() //nolint:reassign // test configuration

	t.Cleanup(func() {
		common.MountBase = original //nolint:reassign // restore
		vmMounted = originalMounted
	})

	own := mm.GetLocalMountPath("exp", "vm1")
	vmMounted = func(path string) bool { return path == own }

	role := rbac.Role{
		Spec: &v1.RoleSpec{
			Policies: []*v1.PolicySpec{{
				Resources:     []string{"vms/mount"},
				ResourceNames: []string{"*/*"},
				Verbs:         []string{"post", "delete"},
			}},
		},
	}

	request := func(method, target string) *http.Request {
		req := httptest.NewRequest(method, target, nil)
		req = mux.SetURLVars(req, map[string]string{"exp": "exp", "name": "vm1"})

		return req.WithContext(context.WithValue(req.Context(), middleware.ContextKeyRole, role))
	}

	// Mounting or unmounting for real would need the experiment and minimega,
	// so either answers 500 here.
	rec := httptest.NewRecorder()
	MountVM(rec, request(http.MethodPost, "/mount"))

	if rec.Code != http.StatusOK {
		t.Fatalf("mounting the VM the CLI mounted = %d %q, want 200", rec.Code, rec.Body.String())
	}

	activeMountsMu.RLock()
	_, recorded := activeMounts[own]
	activeMountsMu.RUnlock()

	if recorded {
		t.Fatal("MountVM recorded the CLI's mount as its own")
	}

	rec = httptest.NewRecorder()
	UnmountVM(rec, request(http.MethodDelete, "/unmount"))

	if rec.Code != http.StatusOK {
		t.Errorf("unmounting the VM the CLI mounted = %d %q, want 200, leaving the mount", rec.Code, rec.Body.String())
	}
}

// TestMountHandlersForgetMountsThatAreGone is a regression test: an entry of
// activeMounts outlived its mount when an experiment stop or `phenix vm
// unmount` detached it, so MountVM answered 200 without mounting, the file
// routes failed with 500 on the empty directory, and UnmountVM, failing on the
// stopped experiment, still counted the user off, leaving a count that never
// reached 0 again.
func TestMountHandlersForgetMountsThatAreGone(t *testing.T) {
	original, originalMounted := common.MountBase, vmMounted
	common.MountBase = t.TempDir() //nolint:reassign // test configuration

	t.Cleanup(func() {
		common.MountBase = original //nolint:reassign // restore
		vmMounted = originalMounted

		activeMountsMu.Lock()
		clear(activeMounts)
		activeMountsMu.Unlock()
	})

	useVMTestStore(t, "") // stopped: mounting and unmounting fail at once

	own := mm.GetLocalMountPath("exp", "vm1")

	mounted := false
	vmMounted = func(path string) bool { return mounted && path == own }

	role := rbac.Role{Spec: &v1.RoleSpec{Policies: []*v1.PolicySpec{{
		Resources: []string{"vms/mount"}, ResourceNames: []string{"*/*"}, Verbs: []string{"post", "delete", "list"},
	}}}}

	request := func(method, target string) *http.Request {
		req := httptest.NewRequest(method, target, nil)
		req = mux.SetURLVars(req, map[string]string{"exp": "exp", "name": "vm1", "path": "/"})

		return req.WithContext(context.WithValue(req.Context(), middleware.ContextKeyRole, role))
	}

	entry := func() *MountInfo {
		activeMountsMu.RLock()
		defer activeMountsMu.RUnlock()

		return activeMounts[own]
	}

	stale := func() {
		activeMountsMu.Lock()
		activeMounts[own] = &MountInfo{users: 1, lock: &sync.RWMutex{}}
		activeMountsMu.Unlock()
	}

	t.Run("mount", func(t *testing.T) {
		stale()

		rec := httptest.NewRecorder()
		MountVM(rec, request(http.MethodPost, "/mount"))

		if rec.Code == http.StatusOK || entry() != nil {
			t.Errorf(
				"MountVM over a mount that is gone = %d %q with entry %v, want it to mount again",
				rec.Code, rec.Body.String(), entry(),
			)
		}
	})

	t.Run("files", func(t *testing.T) {
		stale()

		rec := httptest.NewRecorder()
		GetMountFiles(rec, request(http.MethodGet, "/files?path=/"))

		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "No existing mount") {
			t.Errorf("listing a mount that is gone = %d %q, want 400 No existing mount", rec.Code, rec.Body.String())
		}
	})

	t.Run("unmount", func(t *testing.T) {
		stale()

		rec := httptest.NewRecorder()
		UnmountVM(rec, request(http.MethodPost, "/unmount"))

		if rec.Code != http.StatusOK || entry() != nil {
			t.Errorf(
				"unmounting a mount that is gone = %d %q with entry %v, want 200 and no entry",
				rec.Code, rec.Body.String(), entry(),
			)
		}
	})

	t.Run("failed unmount keeps the count", func(t *testing.T) {
		stale()

		mounted = true

		rec := httptest.NewRecorder()
		UnmountVM(rec, request(http.MethodPost, "/unmount"))

		if rec.Code != http.StatusBadRequest || entry() == nil || entry().users != 1 {
			t.Errorf(
				"a failed unmount = %d %q, entry %+v; want 400 and the entry with 1 user",
				rec.Code, rec.Body.String(), entry(),
			)
		}
	})
}
