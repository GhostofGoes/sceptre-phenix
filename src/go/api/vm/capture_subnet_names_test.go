package vm_test

import (
	"errors"
	"path"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"phenix/api/vm"
	"phenix/util/file"
	"phenix/util/mm"
)

// subnetNamesMM is a minimega double for CaptureSubnet on the experiment of
// getTestExperiment: test-vm runs with 10.0.0.1 on EXP_1. It records the
// capture files it is asked to write.
type subnetNamesMM struct {
	mm.MM

	files []string
}

func subnetCaptureVMs() mm.VMs {
	return mm.VMs{{Name: "test-vm", State: "RUNNING", Running: true, Networks: []string{"EXP_1"}, IPv4: []string{"10.0.0.1"}}}
}

func (*subnetNamesMM) GetVMInfo(...mm.Option) mm.VMs { return subnetCaptureVMs() }

func (*subnetNamesMM) GetVMInfoErr(...mm.Option) (mm.VMs, error) { return subnetCaptureVMs(), nil }

func (*subnetNamesMM) GetVMState(...mm.Option) (string, error) { return "RUNNING", nil }

func (*subnetNamesMM) GetVMCaptures(...mm.Option) []mm.Capture { return nil }

// StartVMCapture records the capture file. mm keeps the options unexported,
// so the file is read by reflection.
func (m *subnetNamesMM) StartVMCapture(opts ...mm.Option) error {
	m.files = append(m.files, reflect.ValueOf(mm.NewOptions(opts...)).FieldByName("captureFile").String())

	return nil
}

// subnetCaptureFiles lists paths, relative to the experiment's files
// directory, as the files the cluster holds there, or fails with err as
// MMClusterFiles does when a node's listing fails.
type subnetCaptureFiles struct {
	file.ClusterFiles

	paths []string
	err   error
}

func (f subnetCaptureFiles) GetExperimentFilePaths(string) ([]string, error) {
	if f.err != nil {
		return nil, f.err
	}

	return f.paths, nil
}

// useSubnetCapture makes CaptureSubnet run against fake and files until the
// test ends.
func useSubnetCapture(t *testing.T, fake *subnetNamesMM, files subnetCaptureFiles) {
	t.Helper()

	getTestExperiment(t, "test-vm", nil, "2026-10-18T00:00:00Z")

	originalMM, originalFiles := mm.DefaultMM, file.DefaultClusterFiles

	t.Cleanup(func() {
		mm.DefaultMM = originalMM                //nolint:reassign // restore test double
		file.DefaultClusterFiles = originalFiles //nolint:reassign // restore test double
	})

	mm.DefaultMM = fake              //nolint:reassign // install test double
	file.DefaultClusterFiles = files //nolint:reassign // install test double
}

// TestCaptureSubnetKeepsExistingCaptureFiles is a regression test: a subnet
// capture named its file after the interface and the second it started in, so
// a capture of the interface stopped and started again within that second got
// the earlier capture's name, and minimega truncated the earlier file.
func TestCaptureSubnetKeepsExistingCaptureFiles(t *testing.T) {
	// Earlier captures of test-vm's interface 0, with and without the first
	// suffix, for every second this test may run in.
	var taken []string

	now := time.Now()

	for d := -time.Second; d <= time.Minute; d += time.Second {
		base := "test-vm_0_" + now.Add(d).Format("20060102_150405")
		taken = append(taken, base+".pcap", base+"_1.pcap")
	}

	fake := new(subnetNamesMM)
	useSubnetCapture(t, fake, subnetCaptureFiles{paths: taken})

	if _, err := vm.CaptureSubnet("test-experiment", "10.0.0.0/24", nil); err != nil {
		t.Fatalf("CaptureSubnet() = %v", err)
	}

	if len(fake.files) != 1 {
		t.Fatalf("CaptureSubnet() started captures to %q, want one", fake.files)
	}

	dir, name := path.Split(fake.files[0])

	if dir != "test-experiment/files/" || slices.Contains(taken, name) ||
		!regexp.MustCompile(`^test-vm_0_\d{8}_\d{6}_2\.pcap$`).MatchString(name) {
		t.Errorf("CaptureSubnet() captures to %s, want test-experiment/files/test-vm_0_<stamp>_2.pcap, "+
			"since minimega truncates the existing <stamp>.pcap and <stamp>_1.pcap", fake.files[0])
	}
}

// TestCaptureSubnetNamesAFreeFileByTheStamp checks that a subnet capture whose
// name is free gets no suffix.
func TestCaptureSubnetNamesAFreeFileByTheStamp(t *testing.T) {
	fake := new(subnetNamesMM)
	useSubnetCapture(t, fake, subnetCaptureFiles{paths: []string{"other.pcap"}})

	if _, err := vm.CaptureSubnet("test-experiment", "10.0.0.0/24", nil); err != nil {
		t.Fatalf("CaptureSubnet() = %v", err)
	}

	want := regexp.MustCompile(`^test-experiment/files/test-vm_0_\d{8}_\d{6}\.pcap$`)
	if len(fake.files) != 1 || !want.MatchString(fake.files[0]) {
		t.Errorf("CaptureSubnet() started captures to %q, want one to test-experiment/files/test-vm_0_<stamp>.pcap", fake.files)
	}
}

// TestCaptureSubnetFailsWithoutTheFileListing checks that CaptureSubnet starts
// no capture when it cannot tell which file names are taken.
func TestCaptureSubnetFailsWithoutTheFileListing(t *testing.T) {
	fake := new(subnetNamesMM)
	useSubnetCapture(t, fake, subnetCaptureFiles{err: errors.New("mesh down")})

	_, err := vm.CaptureSubnet("test-experiment", "10.0.0.0/24", nil)
	if err == nil || !strings.Contains(err.Error(), "listing the files of experiment test-experiment: mesh down") {
		t.Errorf("CaptureSubnet() = %v, want the listing error", err)
	}

	if len(fake.files) != 0 {
		t.Errorf("CaptureSubnet() started captures to %q without a file listing", fake.files)
	}
}
