package vm_test

import (
	"errors"
	"testing"

	"github.com/activeshadow/libminimega/minicli"
	"github.com/golang/mock/gomock"

	"phenix/api/experiment"
	"phenix/api/vm"
	"phenix/store"
	"phenix/util/mm"
)

// noMM is a minimega double whose every method panics, so a call reaching it
// fails the test.
type noMM struct {
	mm.MM
}

// TestVMOperationsLeaveMinimegaAloneWhenNotRunning checks that VM operations
// on a stopped or dry-run experiment return ErrExperimentNotRunning without
// sending minimega any command: minimega creates a namespace for any command
// sent to it, so the operation would create the experiment's namespace, empty,
// and then fail with minimega's error.
func TestVMOperationsLeaveMinimegaAloneWhenNotRunning(t *testing.T) {
	const (
		exp  = "test-experiment"
		name = "test-vm"
	)

	ops := []struct {
		name string
		call func() error
	}{
		{"Screenshot", func() error { _, err := vm.Screenshot(exp, name, "215"); return err }},
		{"Pause", func() error { return vm.Pause(exp, name) }},
		{"Resume", func() error { return vm.Resume(exp, name) }},
		{"Restart", func() error { return vm.Restart(exp, name) }},
		{"Shutdown", func() error { return vm.Shutdown(exp, name) }},
		{"ResetDiskState", func() error { return vm.ResetDiskState(exp, name) }},
		{"Redeploy", func() error { return vm.Redeploy(exp, name) }},
		{"Kill", func() error { return vm.Kill(exp, name) }},
		{"Restore", func() error { return vm.Restore(exp, name, "snap") }},
		{"CommitToDisk", func() error { _, err := vm.CommitToDisk(exp, name, "", nil); return err }},
		{"MemorySnapshot", func() error { _, err := vm.MemorySnapshot(exp, name, "", nil); return err }},
		{"ChangeOpticalDisc", func() error { return vm.ChangeOpticalDisc(exp, name, "/disc.iso") }},
		{"EjectOpticalDisc", func() error { return vm.EjectOpticalDisc(exp, name) }},
		{"StopCaptures", func() error { return vm.StopCaptures(exp, name) }},
		{"StopCapture", func() error { return vm.StopCapture(exp, name, 0) }},
		{"Connect", func() error { return vm.Connect(exp, name, 0, "EXP_1") }},
		{"Disconnect", func() error { return vm.Disconnect(exp, name, 0) }},
		{"MountFilesystem", func() error { return vm.MountFilesystem(exp, name) }},
		{"UnmountFilesystem", func() error { return vm.UnmountFilesystem(exp, name) }},
	}

	original := mm.DefaultMM

	t.Cleanup(func() { mm.DefaultMM = original }) //nolint:reassign // restore test double

	mm.DefaultMM = noMM{} //nolint:reassign // install test double

	for _, state := range []struct{ name, startTime string }{
		{"stopped", ""},
		{"dry run", "2026-10-18T00:00:00Z-DRYRUN"},
	} {
		t.Run(state.name, func(t *testing.T) {
			getTestExperiment(t, name, nil, state.startTime)

			fake := useFakeMinimega(t, func(string) minicli.Responses { return mmError("unexpected command") })

			for _, op := range ops {
				t.Run(op.name, func(t *testing.T) {
					defer func() {
						if r := recover(); r != nil {
							t.Errorf("%s reached minimega: %v", op.name, r)
						}
					}()

					if err := op.call(); !errors.Is(err, experiment.ErrExperimentNotRunning) {
						t.Errorf("%s = %v, want ErrExperimentNotRunning", op.name, err)
					}
				})
			}

			if sent := fake.sent(); len(sent) != 0 {
				t.Errorf("commands sent to minimega: %q", sent)
			}
		})
	}
}

// TestDryRunVMChangesLeaveMinimegaAlone is a regression test: a dry run counts
// as running, so new tags and subnet captures for its VMs went to minimega,
// which created the namespace the dry run never had.
func TestDryRunVMChangesLeaveMinimegaAlone(t *testing.T) {
	const (
		exp  = "test-experiment"
		name = "test-vm"
	)

	original := mm.DefaultMM

	t.Cleanup(func() { mm.DefaultMM = original }) //nolint:reassign // restore test double

	mm.DefaultMM = noMM{} //nolint:reassign // install test double

	getTestExperiment(t, name, nil, "2026-10-18T00:00:00Z-DRYRUN")

	// Let experiment.Save store the new labels.
	get := store.DefaultStore

	m := store.NewMockStore(gomock.NewController(t))
	m.EXPECT().Get(gomock.Any()).DoAndReturn(get.Get).AnyTimes()
	m.EXPECT().Update(gomock.Any()).Return(nil).AnyTimes()

	store.DefaultStore = m //nolint:reassign // monkey patching for test

	fake := useFakeMinimega(t, func(string) minicli.Responses { return mmError("unexpected command") })

	defer func() {
		if r := recover(); r != nil {
			t.Errorf("a dry-run VM change reached minimega: %v", r)
		}
	}()

	if err := vm.Update(vm.UpdateExperiment(exp), vm.UpdateVM(name), vm.UpdateWithTags(map[string]string{"k": "v"}, false)); err != nil {
		t.Errorf("updating the tags of a dry-run VM: %v", err)
	}

	if _, err := vm.CaptureSubnet(exp, "10.0.0.0/24", nil); err == nil {
		t.Error("CaptureSubnet started captures in a dry run")
	}

	if _, err := vm.StopCaptureSubnet(exp, "10.0.0.0/24", nil); err == nil {
		t.Error("StopCaptureSubnet stopped captures in a dry run")
	}

	if sent := fake.sent(); len(sent) != 0 {
		t.Errorf("commands sent to minimega: %q", sent)
	}
}
