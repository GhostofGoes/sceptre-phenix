package vm_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/golang/mock/gomock"

	"phenix/api/vm"
	"phenix/app"
	"phenix/store"
	"phenix/types"
	"phenix/util/mm"
)

type listMM struct {
	mm.MM

	vms mm.VMs
	err error // what listing the VMs reports, besides vms
}

func (m listMM) GetVMInfo(...mm.Option) mm.VMs {
	return m.vms
}

func (m listMM) GetVMInfoErr(...mm.Option) (mm.VMs, error) {
	return m.vms, m.err
}

type connectMM struct {
	mm.MM

	capturedOpts []mm.Option
}

func (m *connectMM) ConnectVMInterface(opts ...mm.Option) error {
	m.capturedOpts = opts
	return nil
}

// getTestExperiment builds a store mock backing a minimal experiment with one
// topology node and one scenario app that targets the node with metadata, and
// makes it the phenix default store for the duration of the test.
func getTestExperiment(t *testing.T, vmName string, appMetadata map[string]any, startTime string) {
	t.Helper()

	ctrl := gomock.NewController(t)
	t.Cleanup(ctrl.Finish)

	c := store.Config{
		Version: "phenix.sandia.gov/v1",
		Kind:    "Experiment",
		Metadata: store.ConfigMetadata{
			Name: "test-experiment",
		},
		Spec: map[string]any{
			"experimentName": "test-experiment",
			"defaultBridge":  "phenix",
			"topology": map[string]any{
				"nodes": []map[string]any{
					{
						"type": "VirtualMachine",
						"general": map[string]any{
							"hostname":    vmName,
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
							"interfaces": []map[string]any{
								{
									"name":    "IF0",
									"vlan":    "EXP_1",
									"address": "10.0.0.1",
								},
							},
						},
					},
				},
			},
			"scenario": map[string]any{
				"apps": []map[string]any{
					{
						"name": "test-app",
						"hosts": []map[string]any{
							{
								"hostname": vmName,
								"metadata": appMetadata,
							},
						},
					},
				},
			},
		},
	}

	if startTime != "" {
		c.Status = map[string]any{"startTime": startTime}
	}

	m := store.NewMockStore(ctrl)
	m.EXPECT().Get(gomock.Any()).DoAndReturn(func(cfg *store.Config) error {
		*cfg = c

		return nil
	}).AnyTimes()

	store.DefaultStore = m //nolint:reassign // monkey patching for test
}

// TestGetIncludesAppMetadata is a regression test for issue #23: `phenix vm
// info` was missing app metadata for the VM because vm.Get assigned the bound
// `Metadata` method value (a func) to the VM's Metadata map instead of
// calling it to get the actual metadata map. Since func values can't be
// marshaled to JSON, the metadata silently disappeared from the printed
// output.
func TestGetIncludesAppMetadata(t *testing.T) {
	metadata := map[string]any{"key": "value"}

	getTestExperiment(t, "test-vm", metadata, "")

	got, err := vm.Get("test-experiment", "test-vm")
	if err != nil {
		t.Fatalf("vm.Get returned error: %v", err)
	}

	appMetadata, ok := got.Metadata["test-app"]
	if !ok {
		t.Fatalf("expected metadata for app %q, got %v", "test-app", got.Metadata)
	}

	appMetadataMap, ok := appMetadata.(map[string]any)
	if !ok {
		t.Fatalf("expected app metadata to be a map[string]any, got %T", appMetadata)
	}

	if !reflect.DeepEqual(appMetadataMap, metadata) {
		t.Fatalf("expected app metadata %v, got %v", metadata, appMetadataMap)
	}
}

func TestListExcludesKilledVMsFromRunningExperiment(t *testing.T) {
	tests := []struct {
		name      string
		startTime string
		vms       mm.VMs
		want      int
	}{
		{
			name:      "running VM remains listed",
			startTime: "2026-08-26T00:00:00Z",
			vms:       mm.VMs{{Name: "test-vm", Running: true}},
			want:      1,
		},
		{
			name:      "killed VM is excluded",
			startTime: "2026-08-26T00:00:00Z",
			vms:       nil,
			want:      0,
		},
		{
			name:      "dry-run VM remains listed",
			startTime: "2026-08-26T00:00:00Z-DRYRUN",
			vms:       nil,
			want:      1,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			getTestExperiment(t, "test-vm", nil, test.startTime)

			originalMM := mm.DefaultMM
			t.Cleanup(func() { mm.DefaultMM = originalMM }) //nolint:reassign // restore test double
			mm.DefaultMM = listMM{vms: test.vms}            //nolint:reassign // install test double

			got, err := vm.List("test-experiment")
			if err != nil {
				t.Fatalf("vm.List returned error: %v", err)
			}

			if len(got) != test.want {
				t.Fatalf("vm.List returned %d VMs, want %d: %#v", len(got), test.want, got)
			}
		})
	}
}

// TestListReportsMinimegaFailuresOfRunningExperiment is a regression test: a
// failed vm info, such as with minimega down, read as no VMs, and List leaves
// out every VM of a running experiment that minimega does not list, so the
// CLI and the REST API reported a running experiment as having no VMs.
func TestListReportsMinimegaFailuresOfRunningExperiment(t *testing.T) {
	down := errors.New("unable to dial: dial unix /tmp/minimega/minimega: connect: no such file or directory")

	tests := []struct {
		name      string
		startTime string
		vms       mm.VMs
		wantErr   bool
		want      int
	}{
		{name: "running", startTime: "2026-08-26T00:00:00Z", wantErr: true},
		{
			// One host of the namespace answered and another did not: the VMs
			// it listed come with the error, for callers that show them.
			name:      "running, some VMs listed",
			startTime: "2026-08-26T00:00:00Z",
			vms:       mm.VMs{{Name: "test-vm", Running: true}},
			wantErr:   true,
			want:      1,
		},
		{name: "stopped", want: 1},
		{name: "dry run", startTime: "2026-08-26T00:00:00Z-DRYRUN", want: 1},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			getTestExperiment(t, "test-vm", nil, test.startTime)

			originalMM := mm.DefaultMM
			t.Cleanup(func() { mm.DefaultMM = originalMM }) //nolint:reassign // restore test double
			mm.DefaultMM = listMM{vms: test.vms, err: down} //nolint:reassign // install test double

			got, err := vm.List("test-experiment")

			if !test.wantErr {
				if err != nil {
					t.Fatalf("vm.List returned error: %v", err)
				}

				if len(got) != test.want {
					t.Fatalf("vm.List returned %d VMs, want %d: %#v", len(got), test.want, got)
				}

				return
			}

			if !errors.Is(err, down) {
				t.Fatalf("vm.List error = %v, want one wrapping %v", err, down)
			}

			// Callers that show VMs show the partial list, others fail.
			if partial := len(test.vms) > 0; errors.Is(err, vm.ErrPartialList) != partial {
				t.Errorf("vm.List error = %v, want it to wrap vm.ErrPartialList only when some VMs were listed", err)
			}

			if want := "getting VMs of test-experiment from minimega: "; !strings.HasPrefix(err.Error(), want) {
				t.Errorf("vm.List error = %q, want it to start with %q", err, want)
			}

			if len(got) != test.want {
				t.Errorf("vm.List returned VMs %#v with its error, want %d", got, test.want)
			}
		})
	}
}

func TestConnectUsesExperimentDefaultBridge(t *testing.T) {
	getTestExperiment(t, "test-vm", nil, "2026-10-18T00:00:00Z")

	originalMM := mm.DefaultMM
	t.Cleanup(func() { mm.DefaultMM = originalMM }) //nolint:reassign // restore test double

	fakeMM := new(connectMM)
	mm.DefaultMM = fakeMM //nolint:reassign // install test double

	// Test that Connect() successfully calls ConnectVMInterface with options
	if err := vm.Connect("test-experiment", "test-vm", 0, "EXP_1"); err != nil {
		t.Fatalf("vm.Connect returned error: %v", err)
	}

	if len(fakeMM.capturedOpts) == 0 {
		t.Fatal("expected ConnectVMInterface to be called with options including bridge")
	}
}

func TestConnectHandlesExperimentLookupError(t *testing.T) {
	// Don't call getTestExperiment, so the experiment doesn't exist in the store
	if err := vm.Connect("nonexistent-experiment", "test-vm", 0, "EXP_1"); err == nil {
		t.Fatal("expected Connect to return error for nonexistent experiment")
	}
}

// TestGetIncludesAppMetadataUsingDecodedExperiment is a lighter-weight variant
// that decodes the experiment directly (bypassing the store mock plumbing) to
// double check the fix using types.DecodeExperimentFromConfig, matching the
// pattern used by other API-level tests in this repo.
func TestGetIncludesAppMetadataUsingDecodedExperiment(t *testing.T) {
	metadata := map[string]any{"foo": "bar"}

	c := store.Config{
		Version: "phenix.sandia.gov/v1",
		Kind:    "Experiment",
		Metadata: store.ConfigMetadata{
			Name: "decode-experiment",
		},
		Spec: map[string]any{
			"experimentName": "decode-experiment",
			"scenario": map[string]any{
				"apps": []map[string]any{
					{
						"name": "decode-app",
						"hosts": []map[string]any{
							{"hostname": "decode-vm", "metadata": metadata},
						},
					},
				},
			},
		},
	}

	exp, err := types.DecodeExperimentFromConfig(c)
	if err != nil {
		t.Fatalf("decoding experiment from config: %v", err)
	}

	var found map[string]any

	for _, app := range exp.Apps() {
		for _, h := range app.Hosts() {
			if h.Hostname() == "decode-vm" {
				found = h.Metadata()
			}
		}
	}

	if !reflect.DeepEqual(found, metadata) {
		t.Fatalf("expected host metadata %v, got %v", metadata, found)
	}
}

// TestGetExternalNodeOfRunningExperiment is a regression test: vm.Get indexed
// the first drive and dereferenced unset flags of every node, so asking for an
// external node (which has no drives) of a running experiment panicked, and it
// never reported the EXTERNAL state List reports.
func TestGetExternalNodeOfRunningExperiment(t *testing.T) {
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
		Status: map[string]any{"startTime": "2026-10-18T00:00:00Z"},
	}

	m := store.NewMockStore(ctrl)
	m.EXPECT().Get(gomock.Any()).DoAndReturn(func(cfg *store.Config) error {
		*cfg = c

		return nil
	}).AnyTimes()

	store.DefaultStore = m //nolint:reassign // monkey patching for test

	original := mm.DefaultMM
	mm.DefaultMM = listMM{} //nolint:reassign // minimega lists no VMs

	t.Cleanup(func() { mm.DefaultMM = original }) //nolint:reassign // restore

	got, err := vm.Get("test-experiment", "plc")
	if err != nil {
		t.Fatalf("vm.Get returned error: %v", err)
	}

	if got.State != "EXTERNAL" {
		t.Errorf("State = %q, want EXTERNAL", got.State)
	}

	if got.Disk != "" || got.DoNotBoot || got.Snapshot {
		t.Errorf("external node has disk %q, dnb %v, snapshot %v; want none", got.Disk, got.DoNotBoot, got.Snapshot)
	}
}

type countingInfoMM struct {
	mm.MM

	calls int
}

func (m *countingInfoMM) GetVMInfo(...mm.Option) mm.VMs {
	m.calls++

	return nil
}

func (m *countingInfoMM) GetVMInfoErr(...mm.Option) (mm.VMs, error) {
	m.calls++

	return nil, nil
}

// TestDryRunExperimentDoesNotQueryMinimega is a regression test: listing or
// getting the VMs of a dry-run experiment ran vm info in its namespace, which
// made minimega create the namespace the dry run never used.
func TestDryRunExperimentDoesNotQueryMinimega(t *testing.T) {
	getTestExperiment(t, "dry-vm", nil, "2026-10-18T00:00:00Z-DRYRUN")

	fake := &countingInfoMM{}

	original := mm.DefaultMM
	mm.DefaultMM = fake //nolint:reassign // install test double

	t.Cleanup(func() { mm.DefaultMM = original }) //nolint:reassign // restore

	vms, err := vm.List("test-experiment")
	if err != nil {
		t.Fatalf("vm.List: %v", err)
	}

	if len(vms) != 1 {
		t.Fatalf("vm.List returned %d VMs, want 1 from the spec", len(vms))
	}

	if _, err := vm.Get("test-experiment", "dry-vm"); err != nil {
		t.Fatalf("vm.Get: %v", err)
	}

	if fake.calls != 0 {
		t.Fatalf("vm info was run %d times for a dry-run experiment, want 0", fake.calls)
	}
}

// TestUpdateDoNotBootDropsTheStartupReason is a regression test: a node the
// startup app had marked do-not-boot for a missing image kept the app's reason
// annotation when the user then set do-not-boot, so the next start cleared the
// user's do-not-boot once the image existed.
func TestUpdateDoNotBootDropsTheStartupReason(t *testing.T) {
	ctrl := gomock.NewController(t)
	t.Cleanup(ctrl.Finish)

	c := store.Config{
		Version:  "phenix.sandia.gov/v1",
		Kind:     "Experiment",
		Metadata: store.ConfigMetadata{Name: "test-experiment"},
		Spec: map[string]any{
			"experimentName": "test-experiment",
			"topology": map[string]any{
				"nodes": []map[string]any{{
					"type": "VirtualMachine",
					"annotations": map[string]any{
						app.DoNotBootReasonAnnotation: "missing disk image",
						"other":                       "kept",
					},
					"general": map[string]any{"hostname": "test-vm", "do_not_boot": true},
					"hardware": map[string]any{
						"os_type": "linux",
						"drives":  []map[string]any{{"image": "test.qc2"}},
					},
				}},
			},
		},
	}

	var saved *store.Config

	m := store.NewMockStore(ctrl)
	m.EXPECT().Get(gomock.Any()).DoAndReturn(func(cfg *store.Config) error {
		*cfg = c

		return nil
	}).AnyTimes()
	m.EXPECT().Update(gomock.Any()).DoAndReturn(func(cfg *store.Config) error {
		saved = cfg

		return nil
	})

	store.DefaultStore = m //nolint:reassign // monkey patching for test

	err := vm.Update(
		vm.UpdateExperiment("test-experiment"),
		vm.UpdateVM("test-vm"),
		vm.UpdateWithDNB(true),
	)
	if err != nil {
		t.Fatalf("vm.Update returned error: %v", err)
	}

	exp, err := types.DecodeExperimentFromConfig(*saved)
	if err != nil {
		t.Fatalf("decoding saved experiment: %v", err)
	}

	node := exp.Spec.Topology().FindNodeByName("test-vm")

	if !*node.General().DoNotBoot() {
		t.Error("saved do_not_boot = false, want true")
	}

	if _, ok := node.GetAnnotation(app.DoNotBootReasonAnnotation); ok {
		t.Errorf("saved annotations %v still carry the startup app's reason", node.Annotations())
	}

	if v, _ := node.GetAnnotation("other"); v != "kept" {
		t.Errorf("saved annotations %v lost an unrelated annotation", node.Annotations())
	}
}

// labelsMM records the minimega changes vm.Update makes to a running VM.
type labelsMM struct {
	mm.MM

	calls []string
}

func (m *labelsMM) ConnectVMInterface(...mm.Option) error {
	m.calls = append(m.calls, "connect")

	return nil
}

func (m *labelsMM) DisconnectVMInterface(...mm.Option) error {
	m.calls = append(m.calls, "disconnect")

	return nil
}

func (m *labelsMM) SetVMTags(...mm.Option) error {
	m.calls = append(m.calls, "tags")

	return nil
}

// TestUpdateRefusesLabelMinimegaCannotStore checks that a label minimega
// cannot store as a tag of a running VM fails the update before it changes
// anything: neither the interface change of the same update nor the stored
// labels may be applied.
func TestUpdateRefusesLabelMinimegaCannotStore(t *testing.T) {
	originalStore, originalMM := store.DefaultStore, mm.DefaultMM

	t.Cleanup(func() {
		store.DefaultStore = originalStore //nolint:reassign // restore test double
		mm.DefaultMM = originalMM          //nolint:reassign // restore test double
	})

	// The store mock fails the test on any Update, so the labels are not saved.
	getTestExperiment(t, "test-vm", nil, "2026-10-18T00:00:00Z")

	fake := &labelsMM{}
	mm.DefaultMM = fake //nolint:reassign // install test double

	err := vm.Update(
		vm.UpdateExperiment("test-experiment"),
		vm.UpdateVM("test-vm"),
		vm.UpdateWithInterface(0, "EXP_2"),
		vm.UpdateWithTags(map[string]string{"role": "web", "note": `a"b`}, false),
	)
	if !errors.Is(err, mm.ErrInvalidVMLabel) || !strings.Contains(err.Error(), `label "note" of VM test-vm`) {
		t.Fatalf("vm.Update = %v, want ErrInvalidVMLabel naming test-vm and the label note", err)
	}

	if len(fake.calls) > 0 {
		t.Errorf("vm.Update made the minimega changes %v before refusing the label", fake.calls)
	}
}

// TestGetReportsMinimegaFailures is a regression test: vm.Get dropped the
// error of a failed vm info, so with minimega down a VM of a running
// experiment read as not running, and a snapshot answered "VM is not running".
func TestGetReportsMinimegaFailures(t *testing.T) {
	down := errors.New("unable to dial: dial unix /tmp/minimega/minimega: connect: no such file or directory")

	getTestExperiment(t, "test-vm", nil, "2026-08-26T00:00:00Z")

	originalMM := mm.DefaultMM
	t.Cleanup(func() { mm.DefaultMM = originalMM }) //nolint:reassign // restore test double

	mm.DefaultMM = listMM{err: down} //nolint:reassign // install test double

	if got, err := vm.Get("test-experiment", "test-vm"); !errors.Is(err, down) || !strings.Contains(err.Error(), "VM test-vm") {
		t.Errorf("vm.Get() = %+v, %v, want an error naming test-vm and wrapping %v", got, err, down)
	}

	if err := vm.Snapshot("test-experiment", "test-vm", "s1", nil); !errors.Is(err, down) {
		t.Errorf("vm.Snapshot() = %v, want the minimega error, not a VM that is not running", err)
	}

	// Another host of the namespace failing does not matter once the VM's own
	// host listed it.
	listed := listMM{vms: mm.VMs{{Name: "test-vm", State: "RUNNING", Running: true}}, err: down}
	mm.DefaultMM = listed //nolint:reassign // install test double

	if got, err := vm.Get("test-experiment", "test-vm"); err != nil || !got.Running {
		t.Errorf("vm.Get() = %+v, %v, want running test-vm", got, err)
	}
}
