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
	"testing"

	"github.com/golang/mock/gomock"

	"phenix/store"
	v1 "phenix/types/version/v1"
	"phenix/util/common"
	"phenix/web/middleware"
	"phenix/web/rbac"
)

// uploadRequest is a file server upload of files, by name and content, to
// the experiment test-experiment.
func uploadRequest(t *testing.T, files map[string]string) *http.Request {
	t.Helper()

	var (
		body bytes.Buffer
		mw   = multipart.NewWriter(&body)
	)

	if err := mw.WriteField("experiment", "test-experiment"); err != nil {
		t.Fatal(err)
	}

	for name, content := range files {
		part, err := mw.CreateFormFile("files", name)
		if err != nil {
			t.Fatal(err)
		}

		if _, err := part.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}

	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}

	role := rbac.Role{
		Spec: &v1.RoleSpec{
			Policies: []*v1.PolicySpec{
				{Resources: []string{"experiments/files"}, ResourceNames: []string{"*"}, Verbs: []string{"create"}},
			},
		},
	}

	req := httptest.NewRequest(http.MethodPost, "/upload", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())

	return req.WithContext(context.WithValue(req.Context(), middleware.ContextKeyRole, role))
}

// TestUploadExperimentFilesRefusesSnapshots is a regression test: the file
// server stored uploads named <vm>__<name>.hdd and .state in the
// experiment's files, which a restore then gave QEMU, running as root, as
// the VM's disk, a qcow2 file that may back onto any host file, and as its
// migration stream.
func TestUploadExperimentFilesRefusesSnapshots(t *testing.T) {
	originalStore, originalBase := store.DefaultStore, common.PhenixBase

	t.Cleanup(func() {
		store.DefaultStore = originalStore //nolint:reassign // restore test double
		common.PhenixBase = originalBase   //nolint:reassign // restore test configuration
	})

	common.PhenixBase = t.TempDir() //nolint:reassign // test configuration

	c := store.Config{
		Version:  "phenix.sandia.gov/v1",
		Kind:     "Experiment",
		Metadata: store.ConfigMetadata{Name: "test-experiment"},
		Spec:     map[string]any{"experimentName": "test-experiment"},
	}

	m := store.NewMockStore(gomock.NewController(t))
	m.EXPECT().Get(gomock.Any()).DoAndReturn(func(cfg *store.Config) error {
		*cfg = c

		return nil
	}).AnyTimes()
	m.EXPECT().List(gomock.Any()).Return(store.Configs{c}, nil).AnyTimes()

	store.DefaultStore = m //nolint:reassign // install test double

	dir := filepath.Join(common.PhenixBase, "images", "test-experiment", "files")

	for _, name := range []string{"test-vm__s1.hdd", "test-vm__s1.state"} {
		rec := httptest.NewRecorder()
		uploadExperimentFiles(false)(rec, uploadRequest(t, map[string]string{"notes.txt": "notes", name: "QFI\xfb"}))

		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), name) {
			t.Errorf("uploading %s = %d %q, want 400 naming it", name, rec.Code, rec.Body.String())
		}

		if entries, _ := os.ReadDir(dir); len(entries) != 0 {
			t.Errorf("uploading %s with notes.txt wrote %v, want nothing", name, entries)
		}
	}

	rec := httptest.NewRecorder()
	uploadExperimentFiles(false)(rec, uploadRequest(t, map[string]string{"notes.txt": "notes"}))

	if got, err := os.ReadFile(filepath.Join(dir, "notes.txt")); rec.Code != http.StatusOK || string(got) != "notes" {
		t.Errorf("uploading notes.txt = %d, file %q (%v), want 200 and the file", rec.Code, got, err)
	}
}
