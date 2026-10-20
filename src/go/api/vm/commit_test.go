package vm_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/activeshadow/libminimega/minicli"
	"github.com/golang/mock/gomock"

	"phenix/api/vm"
	"phenix/store"
	"phenix/util/common"
	"phenix/util/mm"
)

// commitMM answers the backing chain of the image a commit copies.
type commitMM struct {
	mm.MM

	chain string
}

func (m commitMM) MeshShellResponse(string, string) (string, error) { return m.chain, nil }

// GetVMState fails, as when minimega cannot be asked for the VM's state.
func (commitMM) GetVMState(...mm.Option) (string, error) {
	return "", errors.New("minimega is down")
}

// useCommitExperiment makes the store hold the running test-experiment, whose
// test-vm boots from a base image in a temporary phenix images directory, and
// returns that directory.
func useCommitExperiment(t *testing.T) string {
	t.Helper()

	originalBase, originalMM := common.PhenixBase, mm.DefaultMM
	t.Cleanup(func() {
		common.PhenixBase = originalBase //nolint:reassign // restore test configuration
		mm.DefaultMM = originalMM        //nolint:reassign // restore test double
	})

	common.PhenixBase = t.TempDir() //nolint:reassign // test configuration
	images := filepath.Join(common.PhenixBase, "images")

	base := filepath.Join(images, "base.qc2")
	if err := os.MkdirAll(images, 0o750); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(base, []byte("BASE IMAGE"), 0o600); err != nil {
		t.Fatal(err)
	}

	chain, _ := json.Marshal([]map[string]string{{"filename": base}})
	mm.DefaultMM = commitMM{chain: string(chain)} //nolint:reassign // install test double

	c := store.Config{
		Version:  "phenix.sandia.gov/v1",
		Kind:     "Experiment",
		Metadata: store.ConfigMetadata{Name: "test-experiment"},
		Spec: map[string]any{
			"experimentName": "test-experiment",
			"topology": map[string]any{"nodes": []map[string]any{{
				"type":     "VirtualMachine",
				"general":  map[string]any{"hostname": "test-vm"},
				"hardware": map[string]any{"os_type": "linux", "drives": []map[string]any{{"image": base}}},
			}}},
		},
		Status: map[string]any{"startTime": "2026-10-19T00:00:00Z"},
	}

	m := store.NewMockStore(gomock.NewController(t))
	m.EXPECT().Get(gomock.Any()).DoAndReturn(func(cfg *store.Config) error {
		*cfg = c

		return nil
	}).AnyTimes()

	original := store.DefaultStore
	t.Cleanup(func() { store.DefaultStore = original }) //nolint:reassign // restore test double

	store.DefaultStore = m //nolint:reassign // install test double

	return images
}

// TestCommitToDiskRefusesHostPaths is a regression test: the file name of a
// REST commit was used as a host path, absolute or with "..", and phenix
// truncated it with a copy of the VM's base image before stopping the VM, and
// removed it when the copy failed, so a user allowed to commit could destroy
// any file phenix may write.
func TestCommitToDiskRefusesHostPaths(t *testing.T) {
	images := useCommitExperiment(t)
	victim := filepath.Join(filepath.Dir(images), "victim.conf")

	if err := os.WriteFile(victim, []byte("HOST FILE"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, out := range []string{victim, "../victim.conf", "sub/new.qc2", "a;b.qc2", ".hidden.qc2"} {
		fake := useFakeMinimega(t, nil)

		if _, err := vm.CommitToDisk("test-experiment", "test-vm", out, nil); !errors.Is(err, mm.ErrInvalidFileName) {
			t.Errorf("CommitToDisk(%q) = %v, want mm.ErrInvalidFileName", out, err)
		}

		if sent := fake.sent(); len(sent) != 0 {
			t.Errorf("CommitToDisk(%q) sent %q to minimega", out, sent)
		}
	}

	if got, _ := os.ReadFile(victim); string(got) != "HOST FILE" {
		t.Errorf("victim holds %q after the refused commits, want it unchanged", got)
	}
}

// TestCommitToDiskRefusesExistingImages is a regression test: a commit to
// the name of an existing image, such as the base image it copies or one
// other experiments boot from, truncated it.
func TestCommitToDiskRefusesExistingImages(t *testing.T) {
	images := useCommitExperiment(t)

	if err := os.WriteFile(filepath.Join(images, "shared.qc2"), []byte("SHARED IMAGE"), 0o600); err != nil {
		t.Fatal(err)
	}

	fake := useFakeMinimega(t, func(string) minicli.Responses {
		return mmTable([]string{"host", "name", "id", "state"}, []string{"head", "test-vm", "1", "RUNNING"})
	})

	for _, out := range []string{"shared.qc2", "base.qc2"} {
		_, err := vm.CommitToDisk("test-experiment", "test-vm", out, nil)
		if !errors.Is(err, vm.ErrImageExists) || !strings.Contains(err.Error(), "VM test-vm") {
			t.Errorf("CommitToDisk(%q) = %v, want vm.ErrImageExists naming test-vm", out, err)
		}
	}

	for name, want := range map[string]string{"shared.qc2": "SHARED IMAGE", "base.qc2": "BASE IMAGE"} {
		if got, _ := os.ReadFile(filepath.Join(images, name)); string(got) != want {
			t.Errorf("%s holds %q after the refused commits, want %q", name, got, want)
		}
	}

	for _, cmd := range fake.sent() {
		if !strings.HasSuffix(cmd, "vm info") {
			t.Errorf("CommitToDisk sent %q to minimega before refusing the existing image", cmd)
		}
	}
}

// TestCommitToDiskFreesNameOfFailedCommit is a regression test: a commit
// reserves its new image's name, and a failure after the reservation, such
// as one to stop the VM, left a copy of the base image under the name, so a
// retry was refused because the image existed.
func TestCommitToDiskFreesNameOfFailedCommit(t *testing.T) {
	images := useCommitExperiment(t)

	useFakeMinimega(t, func(string) minicli.Responses {
		return mmTable([]string{"host", "name", "id", "state"}, []string{"head", "test-vm", "1", "RUNNING"})
	})

	for range 2 {
		_, err := vm.CommitToDisk("test-experiment", "test-vm", "new.qc2", nil)
		if err == nil || errors.Is(err, vm.ErrImageExists) || !strings.Contains(err.Error(), "minimega is down") {
			t.Errorf("CommitToDisk(new.qc2) = %v, want the failure to stop the VM", err)
		}

		if _, err := os.Stat(filepath.Join(images, "new.qc2")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("new.qc2 after the failed commit: %v, want it removed", err)
		}
	}
}
