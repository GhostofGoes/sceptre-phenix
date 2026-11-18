package vm_test

import (
	"errors"
	"strings"
	"testing"

	"phenix/api/vm"
	"phenix/util/file"
	"phenix/util/mm"
)

// subnetCaptureMM is a captureTestMM whose VMs run and whose VM listing
// reports listErr.
type subnetCaptureMM struct {
	captureTestMM

	listErr error
}

func (m *subnetCaptureMM) GetVMInfoErr(...mm.Option) (mm.VMs, error) {
	return m.vmInfo, m.listErr
}

func (*subnetCaptureMM) GetVMState(...mm.Option) (string, error) {
	return "RUNNING", nil
}

// TestCaptureSubnetReportsFailures is a regression test: a subnet capture
// replaced the error of the VM listing with one that did not say why, and a
// capture minimega failed to start read as success with nothing captured.
func TestCaptureSubnetReportsFailures(t *testing.T) {
	getCaptureTestExperiment(t, []string{"EXP_1"}, t.TempDir())

	running := mm.VMs{{Name: "test-vm", Running: true, Networks: []string{"EXP_1"}, IPv4: []string{"10.0.0.5"}}}

	down := errors.New("c2: timed out")

	fake := &subnetCaptureMM{captureTestMM: captureTestMM{vmInfo: running}, listErr: down}
	installCaptureTestMM(t, &fake.captureTestMM)
	mm.DefaultMM = fake //nolint:reassign // install test double

	// The experiment's files directory is empty, so every capture name is free.
	originalFiles := file.DefaultClusterFiles
	t.Cleanup(func() { file.DefaultClusterFiles = originalFiles }) //nolint:reassign // restore test double
	file.DefaultClusterFiles = subnetCaptureFiles{}                //nolint:reassign // install test double

	if _, err := vm.CaptureSubnet("test-experiment", "10.0.0.0/24", nil); !errors.Is(err, down) {
		t.Errorf("CaptureSubnet() with a failed VM listing = %v, want it to wrap %v", err, down)
	}

	failed := errors.New("capture failed")
	fake.listErr, fake.startErr = nil, failed

	if _, err := vm.CaptureSubnet("test-experiment", "10.0.0.0/24", nil); !errors.Is(err, failed) ||
		!strings.Contains(err.Error(), "VM test-vm") {
		t.Errorf("CaptureSubnet() with a failed capture = %v, want it to wrap %v naming test-vm", err, failed)
	}

	// A disconnected interface keeps its address, and is skipped.
	fake.startErr = nil
	fake.vmInfo = mm.VMs{{Name: "test-vm", Running: true, Networks: []string{"disconnected"}, IPv4: []string{"10.0.0.5"}}}

	if captures, err := vm.CaptureSubnet("test-experiment", "10.0.0.0/24", nil); err != nil || len(captures) != 0 {
		t.Errorf("CaptureSubnet() of a disconnected interface = %v, %v, want no captures and no error", captures, err)
	}

	if fake.startCalls != 1 {
		t.Errorf("StartVMCapture was called %d times, want once for the connected interface", fake.startCalls)
	}
}
