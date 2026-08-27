package vm_test

import (
	"errors"
	"testing"

	"github.com/golang/mock/gomock"

	"phenix/api/vm"
	"phenix/store"
	"phenix/util/mm"
)

// captureTestMM is a test double for mm.MM. Embedding the (nil) mm.MM
// interface means any method call this test doesn't stub will panic, which
// is fine since these tests only ever exercise the capture-related methods.
type captureTestMM struct {
	mm.MM

	vmInfo   mm.VMs
	captures []mm.Capture

	startErr error
	stopErr  error

	startCalls int
	stopCalls  int
}

func (m *captureTestMM) GetVMInfo(...mm.Option) mm.VMs {
	return m.vmInfo
}

func (m *captureTestMM) GetVMCaptures(...mm.Option) []mm.Capture {
	return m.captures
}

func (m *captureTestMM) StartVMCapture(...mm.Option) error {
	m.startCalls++

	return m.startErr
}

func (m *captureTestMM) StopVMCapture(...mm.Option) error {
	m.stopCalls++

	return m.stopErr
}

// installCaptureTestMM installs the given fake as mm.DefaultMM for the
// duration of the test, restoring the original afterward.
func installCaptureTestMM(t *testing.T, fake *captureTestMM) {
	t.Helper()

	original := mm.DefaultMM
	t.Cleanup(func() { mm.DefaultMM = original }) //nolint:reassign // restore test double

	mm.DefaultMM = fake //nolint:reassign // install test double
}

// getCaptureTestExperiment builds a store mock backing a running experiment
// (i.e. one with a non-empty status start time) named "test-experiment" with
// a single VM named "test-vm" that has one network interface per entry in
// networks. baseDir is used as the experiment's base directory so that any
// directories phenix creates during the test (e.g. for captures) land in a
// temporary location instead of the default "/phenix/..." path.
func getCaptureTestExperiment(t *testing.T, networks []string, baseDir string) {
	t.Helper()

	ctrl := gomock.NewController(t)
	t.Cleanup(ctrl.Finish)

	interfaces := make([]map[string]any, len(networks))
	for i, vlan := range networks {
		interfaces[i] = map[string]any{
			"name": vlan,
			"vlan": vlan,
		}
	}

	c := store.Config{
		Version: "phenix.sandia.gov/v1",
		Kind:    "Experiment",
		Metadata: store.ConfigMetadata{
			Name: "test-experiment",
		},
		Spec: map[string]any{
			"experimentName": "test-experiment",
			"baseDir":        baseDir,
			"topology": map[string]any{
				"nodes": []map[string]any{
					{
						"type": "VirtualMachine",
						"general": map[string]any{
							"hostname":    "test-vm",
							"do_not_boot": false,
							"snapshot":    false,
						},
						"hardware": map[string]any{
							"vcpus":   2,
							"memory":  512,
							"os_type": "linux",
							"drives": []map[string]any{
								{
									"image":            "test.qc2",
									"inject_partition": 1,
								},
							},
						},
						"network": map[string]any{
							"interfaces": interfaces,
						},
					},
				},
			},
		},
		Status: map[string]any{
			"startTime": "2024-01-01T00:00:00Z",
		},
	}

	m := store.NewMockStore(ctrl)
	m.EXPECT().Get(gomock.Any()).DoAndReturn(func(cfg *store.Config) error {
		*cfg = c

		return nil
	}).AnyTimes()

	store.DefaultStore = m //nolint:reassign // monkey patching for test
}

// ---------------------------------------------------------------------------
// StartCapture
// ---------------------------------------------------------------------------

func TestStartCaptureMissingExperimentName(t *testing.T) {
	if err := vm.StartCapture("", "test-vm", 0, "out.pcap"); err == nil {
		t.Fatal("expected error for missing experiment name")
	}
}

func TestStartCaptureMissingVMName(t *testing.T) {
	if err := vm.StartCapture("test-experiment", "", 0, "out.pcap"); err == nil {
		t.Fatal("expected error for missing VM name")
	}
}

func TestStartCaptureMissingOutputFile(t *testing.T) {
	if err := vm.StartCapture("test-experiment", "test-vm", 0, ""); err == nil {
		t.Fatal("expected error for missing output file")
	}
}

func TestStartCaptureVMNotRunning(t *testing.T) {
	getCaptureTestExperiment(t, []string{"EXP_1"}, t.TempDir())

	fake := &captureTestMM{
		vmInfo: mm.VMs{{Name: "test-vm", Running: false, Networks: []string{"EXP_1"}}},
	}
	installCaptureTestMM(t, fake)

	err := vm.StartCapture("test-experiment", "test-vm", 0, "out.pcap")
	if err == nil {
		t.Fatal("expected error when VM is not running")
	}

	if fake.startCalls != 0 {
		t.Fatalf("expected StartVMCapture to not be called, got %d calls", fake.startCalls)
	}
}

func TestStartCaptureInvalidInterfaceIndex(t *testing.T) {
	getCaptureTestExperiment(t, []string{"EXP_1"}, t.TempDir())

	fake := &captureTestMM{vmInfo: mm.VMs{{Name: "test-vm", Running: true, Networks: []string{"EXP_1"}}}}
	installCaptureTestMM(t, fake)

	for _, iface := range []int{-1, 1, 5} {
		err := vm.StartCapture("test-experiment", "test-vm", iface, "out.pcap")
		if err == nil {
			t.Fatalf("expected error for out-of-range interface index %d", iface)
		}
	}

	if fake.startCalls != 0 {
		t.Fatalf("expected StartVMCapture to not be called, got %d calls", fake.startCalls)
	}
}

func TestStartCaptureDisconnectedInterface(t *testing.T) {
	getCaptureTestExperiment(t, []string{"disconnected"}, t.TempDir())

	fake := &captureTestMM{
		vmInfo: mm.VMs{{Name: "test-vm", Running: true, Networks: []string{"disconnected"}}},
	}
	installCaptureTestMM(t, fake)

	err := vm.StartCapture("test-experiment", "test-vm", 0, "out.pcap")
	if err == nil {
		t.Fatal("expected error when capturing on a disconnected interface")
	}

	if fake.startCalls != 0 {
		t.Fatalf("expected StartVMCapture to not be called, got %d calls", fake.startCalls)
	}
}

func TestStartCaptureSuccess(t *testing.T) {
	getCaptureTestExperiment(t, []string{"EXP_1", "EXP_2"}, t.TempDir())

	fake := &captureTestMM{
		vmInfo: mm.VMs{{Name: "test-vm", Running: true, Networks: []string{"EXP_1", "EXP_2"}}},
	}
	installCaptureTestMM(t, fake)

	if err := vm.StartCapture("test-experiment", "test-vm", 1, "out"); err != nil {
		t.Fatalf("unexpected error starting capture: %v", err)
	}

	if fake.startCalls != 1 {
		t.Fatalf("expected StartVMCapture to be called once, got %d calls", fake.startCalls)
	}
}

func TestStartCapturePropagatesMinimegaError(t *testing.T) {
	getCaptureTestExperiment(t, []string{"EXP_1"}, t.TempDir())

	wantErr := errors.New("boom")

	fake := &captureTestMM{
		vmInfo:   mm.VMs{{Name: "test-vm", Running: true, Networks: []string{"EXP_1"}}},
		startErr: wantErr,
	}
	installCaptureTestMM(t, fake)

	err := vm.StartCapture("test-experiment", "test-vm", 0, "out.pcap")
	if err == nil {
		t.Fatal("expected error to be propagated")
	}

	if !errors.Is(err, wantErr) {
		t.Fatalf("expected wrapped error %v, got %v", wantErr, err)
	}
}

// ---------------------------------------------------------------------------
// StopCaptures (all interfaces)
// ---------------------------------------------------------------------------

func TestStopCapturesMissingExperimentName(t *testing.T) {
	if err := vm.StopCaptures("", "test-vm"); err == nil {
		t.Fatal("expected error for missing experiment name")
	}
}

func TestStopCapturesMissingVMName(t *testing.T) {
	if err := vm.StopCaptures("test-experiment", ""); err == nil {
		t.Fatal("expected error for missing VM name")
	}
}

func TestStopCapturesNoCapturesRunning(t *testing.T) {
	fake := new(captureTestMM)
	installCaptureTestMM(t, fake)

	err := vm.StopCaptures("test-experiment", "test-vm")
	if !errors.Is(err, vm.ErrNoCaptures) {
		t.Fatalf("expected ErrNoCaptures, got %v", err)
	}

	if fake.stopCalls != 0 {
		t.Fatalf("expected StopVMCapture to not be called, got %d calls", fake.stopCalls)
	}
}

func TestStopCapturesStopsAllInterfaces(t *testing.T) {
	getCaptureTestExperiment(t, []string{"EXP_1", "EXP_2"}, t.TempDir())

	fake := &captureTestMM{
		captures: []mm.Capture{
			{VM: "test-vm", Interface: 0, Filepath: "/tmp/0.pcap"},
			{VM: "test-vm", Interface: 1, Filepath: "/tmp/1.pcap"},
		},
	}
	installCaptureTestMM(t, fake)

	if err := vm.StopCaptures("test-experiment", "test-vm"); err != nil {
		t.Fatalf("unexpected error stopping captures: %v", err)
	}

	if fake.stopCalls != 1 {
		t.Fatalf("expected StopVMCapture to be called once, got %d calls", fake.stopCalls)
	}
}

func TestStopCapturesPropagatesMinimegaError(t *testing.T) {
	getCaptureTestExperiment(t, []string{"EXP_1"}, t.TempDir())

	wantErr := errors.New("boom")

	fake := &captureTestMM{
		captures: []mm.Capture{{VM: "test-vm", Interface: 0, Filepath: "/tmp/0.pcap"}},
		stopErr:  wantErr,
	}
	installCaptureTestMM(t, fake)

	err := vm.StopCaptures("test-experiment", "test-vm")
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected wrapped error %v, got %v", wantErr, err)
	}
}

// ---------------------------------------------------------------------------
// StopCapture (single interface)
// ---------------------------------------------------------------------------

func TestStopCaptureMissingExperimentName(t *testing.T) {
	if err := vm.StopCapture("", "test-vm", 0); err == nil {
		t.Fatal("expected error for missing experiment name")
	}
}

func TestStopCaptureMissingVMName(t *testing.T) {
	if err := vm.StopCapture("test-experiment", "", 0); err == nil {
		t.Fatal("expected error for missing VM name")
	}
}

func TestStopCaptureNegativeInterface(t *testing.T) {
	if err := vm.StopCapture("test-experiment", "test-vm", -1); err == nil {
		t.Fatal("expected error for negative interface index")
	}
}

func TestStopCaptureNoCapturesRunning(t *testing.T) {
	fake := new(captureTestMM)
	installCaptureTestMM(t, fake)

	err := vm.StopCapture("test-experiment", "test-vm", 0)
	if !errors.Is(err, vm.ErrNoCaptures) {
		t.Fatalf("expected ErrNoCaptures, got %v", err)
	}

	if fake.stopCalls != 0 {
		t.Fatalf("expected StopVMCapture to not be called, got %d calls", fake.stopCalls)
	}
}

// TestStopCaptureInterfaceNotCaptured verifies that requesting to stop a
// capture on an interface that isn't currently being captured returns
// ErrNoCaptures and does not touch the captures running on other interfaces
// of the same VM.
func TestStopCaptureInterfaceNotCaptured(t *testing.T) {
	getCaptureTestExperiment(t, []string{"EXP_1", "EXP_2"}, t.TempDir())

	fake := &captureTestMM{
		captures: []mm.Capture{{VM: "test-vm", Interface: 1, Filepath: "/tmp/1.pcap"}},
	}
	installCaptureTestMM(t, fake)

	err := vm.StopCapture("test-experiment", "test-vm", 0)
	if !errors.Is(err, vm.ErrNoCaptures) {
		t.Fatalf("expected ErrNoCaptures, got %v", err)
	}

	if fake.stopCalls != 0 {
		t.Fatalf(
			"expected StopVMCapture to not be called when the requested interface isn't captured, got %d calls",
			fake.stopCalls,
		)
	}
}

// TestStopCaptureStopsOnlyRequestedInterface verifies that, when multiple
// captures are running for a VM, requesting to stop just one interface's
// capture succeeds without erroring about the other running captures. This
// exercises the new minimega "capture pcap delete vm <name> <iface>"
// behavior (sandia-minimega/minimega#1632) which allows stopping a single
// interface's capture in isolation.
func TestStopCaptureStopsOnlyRequestedInterface(t *testing.T) {
	getCaptureTestExperiment(t, []string{"EXP_1", "EXP_2"}, t.TempDir())

	fake := &captureTestMM{
		captures: []mm.Capture{
			{VM: "test-vm", Interface: 0, Filepath: "/tmp/0.pcap"},
			{VM: "test-vm", Interface: 1, Filepath: "/tmp/1.pcap"},
		},
	}
	installCaptureTestMM(t, fake)

	if err := vm.StopCapture("test-experiment", "test-vm", 1); err != nil {
		t.Fatalf("unexpected error stopping capture on interface 1: %v", err)
	}

	if fake.stopCalls != 1 {
		t.Fatalf("expected StopVMCapture to be called once, got %d calls", fake.stopCalls)
	}
}

func TestStopCapturePropagatesMinimegaError(t *testing.T) {
	getCaptureTestExperiment(t, []string{"EXP_1"}, t.TempDir())

	wantErr := errors.New("boom")

	fake := &captureTestMM{
		captures: []mm.Capture{{VM: "test-vm", Interface: 0, Filepath: "/tmp/0.pcap"}},
		stopErr:  wantErr,
	}
	installCaptureTestMM(t, fake)

	err := vm.StopCapture("test-experiment", "test-vm", 0)
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected wrapped error %v, got %v", wantErr, err)
	}
}

// ---------------------------------------------------------------------------
// ResolveInterface
// ---------------------------------------------------------------------------

// getNamedCaptureTestExperiment is like getCaptureTestExperiment, but lets
// the interface name differ from its VLAN so that name-based resolution
// tests aren't accidentally passing just because the name and VLAN happen to
// match.
func getNamedCaptureTestExperiment(t *testing.T, ifaceNames, vlans []string, baseDir string) {
	t.Helper()

	if len(ifaceNames) != len(vlans) {
		t.Fatalf("ifaceNames and vlans must be the same length")
	}

	ctrl := gomock.NewController(t)
	t.Cleanup(ctrl.Finish)

	interfaces := make([]map[string]any, len(vlans))
	for i, vlan := range vlans {
		interfaces[i] = map[string]any{
			"name": ifaceNames[i],
			"vlan": vlan,
		}
	}

	c := store.Config{
		Version: "phenix.sandia.gov/v1",
		Kind:    "Experiment",
		Metadata: store.ConfigMetadata{
			Name: "test-experiment",
		},
		Spec: map[string]any{
			"experimentName": "test-experiment",
			"baseDir":        baseDir,
			"topology": map[string]any{
				"nodes": []map[string]any{
					{
						"type": "VirtualMachine",
						"general": map[string]any{
							"hostname":    "test-vm",
							"do_not_boot": false,
							"snapshot":    false,
						},
						"hardware": map[string]any{
							"vcpus":   2,
							"memory":  512,
							"os_type": "linux",
							"drives": []map[string]any{
								{
									"image":            "test.qc2",
									"inject_partition": 1,
								},
							},
						},
						"network": map[string]any{
							"interfaces": interfaces,
						},
					},
				},
			},
		},
		Status: map[string]any{
			"startTime": "2024-01-01T00:00:00Z",
		},
	}

	m := store.NewMockStore(ctrl)
	m.EXPECT().Get(gomock.Any()).DoAndReturn(func(cfg *store.Config) error {
		*cfg = c

		return nil
	}).AnyTimes()

	store.DefaultStore = m //nolint:reassign // monkey patching for test
}

func TestResolveInterfaceNilVM(t *testing.T) {
	if _, err := vm.ResolveInterface(nil, "0"); err == nil {
		t.Fatal("expected error for nil VM")
	}
}

func TestResolveInterfaceEmptyIdentifier(t *testing.T) {
	getNamedCaptureTestExperiment(t, []string{"IF0"}, []string{"EXP_1"}, t.TempDir())

	fake := &captureTestMM{
		vmInfo: mm.VMs{{Name: "test-vm", Running: true, Networks: []string{"EXP_1"}}},
	}
	installCaptureTestMM(t, fake)

	v, err := vm.Get("test-experiment", "test-vm")
	if err != nil {
		t.Fatalf("unexpected error getting VM: %v", err)
	}

	if _, err := vm.ResolveInterface(v, ""); err == nil {
		t.Fatal("expected error for empty interface identifier")
	}
}

func TestResolveInterfaceByIndex(t *testing.T) {
	getNamedCaptureTestExperiment(t, []string{"IF0", "IF1"}, []string{"EXP_1", "EXP_2"}, t.TempDir())

	fake := &captureTestMM{
		vmInfo: mm.VMs{{Name: "test-vm", Running: true, Networks: []string{"EXP_1", "EXP_2"}}},
	}
	installCaptureTestMM(t, fake)

	v, err := vm.Get("test-experiment", "test-vm")
	if err != nil {
		t.Fatalf("unexpected error getting VM: %v", err)
	}

	idx, err := vm.ResolveInterface(v, "1")
	if err != nil {
		t.Fatalf("unexpected error resolving interface by index: %v", err)
	}

	if idx != 1 {
		t.Fatalf("expected index 1, got %d", idx)
	}
}

func TestResolveInterfaceIndexOutOfRange(t *testing.T) {
	getNamedCaptureTestExperiment(t, []string{"IF0"}, []string{"EXP_1"}, t.TempDir())

	fake := &captureTestMM{
		vmInfo: mm.VMs{{Name: "test-vm", Running: true, Networks: []string{"EXP_1"}}},
	}
	installCaptureTestMM(t, fake)

	v, err := vm.Get("test-experiment", "test-vm")
	if err != nil {
		t.Fatalf("unexpected error getting VM: %v", err)
	}

	for _, idx := range []string{"-1", "1", "5"} {
		if _, err := vm.ResolveInterface(v, idx); err == nil {
			t.Fatalf("expected error for out-of-range index %q", idx)
		}
	}
}

func TestResolveInterfaceByName(t *testing.T) {
	getNamedCaptureTestExperiment(t, []string{"IF0", "IF1"}, []string{"EXP_1", "EXP_2"}, t.TempDir())

	fake := &captureTestMM{
		vmInfo: mm.VMs{{Name: "test-vm", Running: true, Networks: []string{"EXP_1", "EXP_2"}}},
	}
	installCaptureTestMM(t, fake)

	v, err := vm.Get("test-experiment", "test-vm")
	if err != nil {
		t.Fatalf("unexpected error getting VM: %v", err)
	}

	idx, err := vm.ResolveInterface(v, "IF1")
	if err != nil {
		t.Fatalf("unexpected error resolving interface by name: %v", err)
	}

	if idx != 1 {
		t.Fatalf("expected index 1, got %d", idx)
	}
}

// TestResolveInterfaceByNameCaseInsensitive verifies that interface name
// matching ignores case, since minimega/topology names aren't guaranteed to
// be typed with consistent casing by users at the CLI.
func TestResolveInterfaceByNameCaseInsensitive(t *testing.T) {
	getNamedCaptureTestExperiment(t, []string{"IF0", "IF1"}, []string{"EXP_1", "EXP_2"}, t.TempDir())

	fake := &captureTestMM{
		vmInfo: mm.VMs{{Name: "test-vm", Running: true, Networks: []string{"EXP_1", "EXP_2"}}},
	}
	installCaptureTestMM(t, fake)

	v, err := vm.Get("test-experiment", "test-vm")
	if err != nil {
		t.Fatalf("unexpected error getting VM: %v", err)
	}

	idx, err := vm.ResolveInterface(v, "if1")
	if err != nil {
		t.Fatalf("unexpected error resolving interface by name: %v", err)
	}

	if idx != 1 {
		t.Fatalf("expected index 1, got %d", idx)
	}
}

func TestResolveInterfaceUnknownName(t *testing.T) {
	getNamedCaptureTestExperiment(t, []string{"IF0", "IF1"}, []string{"EXP_1", "EXP_2"}, t.TempDir())

	fake := &captureTestMM{
		vmInfo: mm.VMs{{Name: "test-vm", Running: true, Networks: []string{"EXP_1", "EXP_2"}}},
	}
	installCaptureTestMM(t, fake)

	v, err := vm.Get("test-experiment", "test-vm")
	if err != nil {
		t.Fatalf("unexpected error getting VM: %v", err)
	}

	if _, err := vm.ResolveInterface(v, "bogus-iface"); err == nil {
		t.Fatal("expected error for unknown interface name")
	}
}

// TestStartCaptureByInterfaceName is an end-to-end (within the vm package)
// check that StartCapture works when combined with ResolveInterface to
// support specifying an interface by name, mirroring how the CLI layer uses
// these two functions together.
func TestStartCaptureByInterfaceName(t *testing.T) {
	getNamedCaptureTestExperiment(t, []string{"IF0", "IF1"}, []string{"EXP_1", "EXP_2"}, t.TempDir())

	fake := &captureTestMM{
		vmInfo: mm.VMs{{Name: "test-vm", Running: true, Networks: []string{"EXP_1", "EXP_2"}}},
	}
	installCaptureTestMM(t, fake)

	v, err := vm.Get("test-experiment", "test-vm")
	if err != nil {
		t.Fatalf("unexpected error getting VM: %v", err)
	}

	idx, err := vm.ResolveInterface(v, "IF1")
	if err != nil {
		t.Fatalf("unexpected error resolving interface by name: %v", err)
	}

	if err := vm.StartCapture("test-experiment", "test-vm", idx, "out.pcap"); err != nil {
		t.Fatalf("unexpected error starting capture: %v", err)
	}

	if fake.startCalls != 1 {
		t.Fatalf("expected StartVMCapture to be called once, got %d calls", fake.startCalls)
	}
}
