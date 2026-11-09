package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"testing"

	"phenix/api/disk"
	"phenix/store"
	v1 "phenix/types/version/v1"
	"phenix/util/mm"
	"phenix/web/middleware"
	"phenix/web/rbac"
)

// assignableDiskFiles lists images as GET /disks finds them.
type assignableDiskFiles struct {
	disk.DiskFiles

	images []disk.Details
}

func (f assignableDiskFiles) GetImages(string) ([]disk.Details, error) {
	return slices.Clone(f.images), nil
}

// TestGetDisksListsOnlyAssignableDisks is a regression test: the disk lists
// the UI offers for a VM's disk, a redeploy and an optical disc held the
// backing images of the listed images, which may be in subdirectories or
// outside minimega's files directory, so the UI offered disks that a disk
// change, a redeploy and an optical disc change refuse with 400.
func TestGetDisksListsOnlyAssignableDisks(t *testing.T) {
	originalStore, originalDisks := store.DefaultStore, disk.DefaultDiskFiles

	t.Cleanup(func() {
		store.DefaultStore = originalStore    //nolint:reassign // restore test double
		disk.DefaultDiskFiles = originalDisks //nolint:reassign // restore test double
	})

	installCaptureHandlerTestExperiment(t)

	files := mm.GetMMFullPath("")
	image := func(path string) disk.Details {
		return disk.Details{Kind: disk.VMImage, Name: filepath.Base(path), FullPath: path}
	}

	disk.DefaultDiskFiles = assignableDiskFiles{images: []disk.Details{ //nolint:reassign // install test double
		image(filepath.Join(files, "test.qc2")),
		image(filepath.Join(files, "top.qc2")),
		image(filepath.Join(files, "base", "os.qc2")),
		image(filepath.Join(files, "test-experiment", "files", "test-vm__s1.hdd")),
		image(filepath.Join(files, "head_other_vm_snapshot")),
		image("/data/elsewhere.qc2"),
	}}

	role := rbac.Role{
		Spec: &v1.RoleSpec{
			Policies: []*v1.PolicySpec{{Resources: []string{"disks"}, ResourceNames: []string{"*"}, Verbs: []string{"list"}}},
		},
	}

	get := func(query string) (int, []string) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/disks?"+query, nil)
		req = req.WithContext(context.WithValue(req.Context(), middleware.ContextKeyRole, role))

		rec := httptest.NewRecorder()
		GetDisks(rec, req)

		var body struct {
			Disks []disk.Details `json:"disks"`
		}

		_ = json.Unmarshal(rec.Body.Bytes(), &body)

		names := make([]string, 0, len(body.Disks))
		for _, d := range body.Disks {
			names = append(names, d.Name)
		}

		return rec.Code, names
	}

	if code, names := get("expName=test-experiment&assignable=true"); code != http.StatusOK ||
		!slices.Equal(names, []string{"test.qc2", "top.qc2"}) {
		t.Errorf("GET disks?assignable=true = %d %q, want 200 with test.qc2 and top.qc2", code, names)
	}

	if code, names := get("expName=test-experiment"); code != http.StatusOK || len(names) != 6 {
		t.Errorf("GET disks = %d %q, want 200 with all 6 images", code, names)
	}

	if code, _ := get("assignable=true"); code != http.StatusBadRequest {
		t.Errorf("GET disks?assignable=true without expName = %d, want 400", code)
	}
}
