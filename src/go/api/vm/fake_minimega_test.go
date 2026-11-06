package vm_test

import (
	"strings"
	"sync"
	"testing"

	"github.com/sandia-minimega/minimega/v2/pkg/minicli"

	"phenix/util/file"
	"phenix/util/mm/mmtest"
)

const (
	// fakeHost is the headnode of the cluster that useMinimega fakes.
	fakeHost = "head"
	fakeUUID = "11111111-2222-3333-4444-555555555555"
)

// queries are the commands that only read state. Tests compare the other
// commands, which change the VM.
var queries = map[string]bool{ //nolint:gochecknoglobals // test fixture
	"args":            true,
	"host":            true,
	"capture":         true,
	"cc client":       true,
	"vm info":         true,
	"vm info summary": true,
}

// fakeMinimega answers minimega commands for one VM and records them.
type fakeMinimega struct {
	mu sync.Mutex

	vm     string
	vmType string
	vmHost string
	state  string
	// disks and scratch are the VM's disks and firecracker-scratch columns.
	disks   string
	scratch string

	polled   map[string]bool
	received func() []mmtest.Command
}

// useFakeMinimega points mmcli at a fake minimega (see useMinimega), which
// answers for the VM vmName of type vmType, in the RUNNING state, on host
// vmHost.
func useFakeMinimega(t *testing.T, vmName, vmType, vmHost string) *fakeMinimega {
	t.Helper()

	fake := &fakeMinimega{vm: vmName, vmType: vmType, vmHost: vmHost, state: "RUNNING"}
	fake.received = useMinimega(t, fake.handle)

	return fake
}

func (f *fakeMinimega) handle(cmd mmtest.Command) []*minicli.Response {
	base := strings.TrimSpace(cmd.Base)

	f.mu.Lock()
	defer f.mu.Unlock()

	resp := &minicli.Response{Host: fakeHost}

	switch {
	case base == "host":
		resp.Header = []string{"name"}
		resp.Tabular = [][]string{{fakeHost}}
	case base == "vm info" || base == "vm info summary":
		resp.Host = f.vmHost
		resp.Header = []string{"id", "name", "state", "type", "uuid", "snapshot", "disks", "firecracker-scratch"}
		resp.Tabular = [][]string{{"0", f.vm, f.state, f.vmType, fakeUUID, "true", f.disks, f.scratch}}
	case base == "vm save" || base == "vm export-disk" || base == "vm memory-dump":
		// The first poll finds the task half done, and the next one finds it completed.
		resp.Header = []string{"name", "status", "complete (%)"}
		resp.Tabular = [][]string{{f.vm, "completed", "100"}}

		if f.polled == nil {
			f.polled = make(map[string]bool)
		}

		if !f.polled[base] {
			f.polled[base] = true
			resp.Tabular = [][]string{{f.vm, "running", "50"}}
		}
	case strings.HasPrefix(base, "vm kill "), strings.HasPrefix(base, "vm shutdown "),
		strings.Contains(base, "system_powerdown"):
		f.state = "QUIT"
	case strings.HasPrefix(base, "vm start "):
		f.state = "RUNNING"
	}

	return []*minicli.Response{resp}
}

// changes returns the commands that change the VM, in the order sent.
func (f *fakeMinimega) changes() []string {
	var changes []string

	for _, cmd := range f.received() {
		if base := strings.TrimSpace(cmd.Base); !queries[base] {
			changes = append(changes, base)
		}
	}

	return changes
}

// fakeFiles stands in for the cluster file listing and transfers.
type fakeFiles struct {
	file.ClusterFiles

	files     file.Files
	snapshots []string
	copied    []string
	synced    []string
}

func (f *fakeFiles) GetExperimentFiles(string, string) (file.Files, error) {
	return f.files, nil
}

func (f *fakeFiles) GetExperimentSnapshots(string) ([]string, error) {
	return f.snapshots, nil
}

func (f *fakeFiles) CopyFile(path, dest string, _ file.CopyStatus) error {
	f.copied = append(f.copied, path+" to "+dest)

	return nil
}

func (f *fakeFiles) SyncFile(path string) error {
	f.synced = append(f.synced, path)

	return nil
}

func useFakeFiles(t *testing.T, files *fakeFiles) {
	t.Helper()

	prev := file.DefaultClusterFiles
	file.DefaultClusterFiles = files //nolint:reassign // test fixture

	t.Cleanup(func() { file.DefaultClusterFiles = prev }) //nolint:reassign // test fixture
}

// useTypedExperiment makes the store hold only testExp with one node, vmName
// of type vmType, running unless running is false (see useExperiment).
func useTypedExperiment(t *testing.T, vmName, vmType string, running bool) {
	t.Helper()

	started := ""
	if running {
		started = startTime
	}

	useExperiment(t, started, map[string]any{
		"type": "VirtualMachine",
		"general": map[string]any{
			"hostname":    vmName,
			"vm_type":     vmType,
			"do_not_boot": false,
			"snapshot":    true,
		},
		"hardware": map[string]any{
			"vcpus":   1,
			"memory":  512,
			"os_type": "linux",
			"drives": []map[string]any{
				{"image": "test.raw", "inject_partition": 1},
			},
		},
		"injections": []map[string]any{
			{"src": "/tmp/motd", "dst": "/etc/motd"},
		},
	})
}
