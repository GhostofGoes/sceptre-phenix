package vm_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/golang/mock/gomock"

	"phenix/api/vm"
	"phenix/store"
	"phenix/util/mm"
)

// useExternalNodeExperiment makes the store hold test-experiment, running
// when startTime is set, with the external node plc, which has no drives,
// until the test ends.
func useExternalNodeExperiment(t *testing.T, startTime string) {
	t.Helper()

	ctrl := gomock.NewController(t)
	t.Cleanup(ctrl.Finish)

	c := store.Config{
		Version:  "phenix.sandia.gov/v1",
		Kind:     "Experiment",
		Metadata: store.ConfigMetadata{Name: "test-experiment"},
		Spec: map[string]any{
			"experimentName": "test-experiment",
			"topology": map[string]any{
				"nodes": []map[string]any{
					{
						"type":     "HIL",
						"external": true,
						"general":  map[string]any{"hostname": "plc"},
						"network": map[string]any{
							"interfaces": []map[string]any{{"name": "eth0", "vlan": "EXP_1"}},
						},
					},
				},
			},
		},
		Status: map[string]any{"startTime": startTime},
	}

	m := store.NewMockStore(ctrl)
	m.EXPECT().Get(gomock.Any()).DoAndReturn(func(cfg *store.Config) error {
		*cfg = c

		return nil
	}).AnyTimes()

	original := store.DefaultStore
	t.Cleanup(func() { store.DefaultStore = original }) //nolint:reassign // restore test double

	store.DefaultStore = m //nolint:reassign // install test double
}

// TestRedeployExternalNode is a regression test: a redeploy that replicates
// injections indexed the first drive of the named node, so for an external
// node, which has none, it panicked, and through REST the panic in the
// redeploy goroutine killed phenix ui.
func TestRedeployExternalNode(t *testing.T) {
	useExternalNodeExperiment(t, "2026-10-18T00:00:00Z")

	original := mm.DefaultMM
	t.Cleanup(func() { mm.DefaultMM = original }) //nolint:reassign // restore test double

	mm.DefaultMM = new(mm.Minimega) //nolint:reassign // the implementation under test

	for _, opts := range [][]vm.RedeployOption{{vm.Inject(true)}, nil} {
		fake := useFakeMinimega(t, nil)

		err := vm.Redeploy("test-experiment", "plc", opts...)
		if !errors.Is(err, vm.ErrNotRedeployable) || !strings.Contains(err.Error(), "VM plc") {
			t.Errorf("Redeploy(plc) = %v, want vm.ErrNotRedeployable naming plc", err)
		}

		if sent := fake.sent(); len(sent) != 0 {
			t.Errorf("Redeploy(plc) sent %q to minimega", sent)
		}
	}
}

// TestUpdateDiskOfExternalNode is a regression test: changing the disk or
// the inject partition of a node without drives indexed its first drive and
// panicked.
func TestUpdateDiskOfExternalNode(t *testing.T) {
	useExternalNodeExperiment(t, "")

	for _, opt := range []vm.UpdateOption{vm.UpdateWithDisk("other.qc2"), vm.UpdateWithPartition(2)} {
		err := vm.Update(vm.UpdateExperiment("test-experiment"), vm.UpdateVM("plc"), opt)
		if err == nil || !strings.Contains(err.Error(), "VM plc") || !strings.Contains(err.Error(), "no disk") {
			t.Errorf("Update() = %v, want an error saying plc has no disk", err)
		}
	}
}

// TestNewDiskNameOfExternalNode is a regression test: the name of a new disk
// for a node without drives, which a commit asks for, indexed its first drive
// and panicked.
func TestNewDiskNameOfExternalNode(t *testing.T) {
	useExternalNodeExperiment(t, "2026-10-18T00:00:00Z")

	if name, err := vm.GetNewDiskName("test-experiment", "plc"); err == nil || !strings.Contains(err.Error(), "no disk") {
		t.Errorf("GetNewDiskName(plc) = %q, %v, want an error saying plc has no disk", name, err)
	}
}
