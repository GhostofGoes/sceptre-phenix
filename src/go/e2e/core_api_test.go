//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"phenix/util/common"
	phenixmm "phenix/util/mm"
	"phenix/util/mm/mmcli"
)

// Core API shard: experiment lifecycle through REST, locking, launch
// failures, mixed topologies, restart, delete, create validation, teardown,
// and the transport and discovery contracts phenix keeps with minimega (flows
// F1 and F3). Every experiment runs hollow VMs.

const (
	// corePhenixBase is phenix's base directory (--base-dir.phenix) in the
	// CLI and the UI the harness starts.
	corePhenixBase = "/phenix"

	// coreStartWait bounds how long hollow VMs take to reach RUNNING.
	coreStartWait = 2 * time.Minute

	// coreTeardownWait bounds how long QEMU processes and taps take to go
	// away; minimega reaps taps asynchronously every second.
	coreTeardownWait = 30 * time.Second

	// corePoll is how often the tests poll minimega and the host.
	corePoll = 500 * time.Millisecond

	// coreHollowMemory and coreHollowVCPUs are what the hollow nodes ask for.
	coreHollowMemory = 256
	coreHollowVCPUs  = 1

	// coreSleepApp is the user app TestCoreLocking installs; its pre-start
	// stage sleeps for coreSleepAppSeconds.
	coreSleepApp        = "e2e-core-sleep"
	coreSleepAppSeconds = 6

	// coreFaultDevice is a QEMU device model that does not exist, so a VM
	// asking for it exits as soon as minimega launches it.
	coreFaultDevice = "e2e-no-such-device"

	// coreNote is a label value with double quotes, which must reach minimega
	// unchanged through phenix's rendered `vm config tags` line.
	coreNote = `say "hi"`

	coreStateRunning  = "RUNNING"
	coreStateError    = "ERROR"
	coreStateExternal = "EXTERNAL"
)

var (
	coreVLANEntry   = regexp.MustCompile(`^(.+) \((\d+)\)$`)
	coreNetworkLAN  = regexp.MustCompile(`^LAN \(\d+\)$`)
	coreNetworkWAN  = regexp.MustCompile(`^WAN \(\d+\)$`)
	coreRequestLine = regexp.MustCompile(`got request over socket: (.*)$`)
)

// ---------------------------------------------------------------------------
// topologies

// coreNIC is one static interface of a hollow node.
type coreNIC struct {
	vlan, address string
}

// coreNode describes a hollow VirtualMachine node; yaml renders it in the
// layout hollowNode uses.
type coreNode struct {
	hostname string
	osType   string    // linux when empty
	images   []string  // drives, first one first
	drive0   string    // extra YAML under the first drive (indent 8)
	general  string    // extra YAML under general (indent 6)
	extra    string    // extra YAML under the node (indent 4)
	nics     []coreNIC // interfaces eth0, eth1, ...
}

func (n coreNode) yaml() string {
	osType := n.osType
	if osType == "" {
		osType = "linux"
	}

	var b strings.Builder

	b.WriteString("\n  - type: VirtualMachine")
	b.WriteString(n.extra)
	fmt.Fprintf(&b, "\n    general:\n      hostname: %s%s", n.hostname, n.general)
	fmt.Fprintf(&b, "\n    hardware:\n      vcpus: %d\n      memory: %d\n      os_type: %s\n      drives:",
		coreHollowVCPUs, coreHollowMemory, osType)

	for i, image := range n.images {
		fmt.Fprintf(&b, "\n      - image: %s", image)

		if i == 0 {
			b.WriteString(n.drive0)
		}
	}

	b.WriteString("\n    network:\n      interfaces:")

	for i, nic := range n.nics {
		fmt.Fprintf(&b, "\n      - name: eth%d\n        type: ethernet\n        proto: static", i)
		fmt.Fprintf(&b, "\n        vlan: %s\n        address: %s\n        mask: 24", nic.vlan, nic.address)
	}

	return b.String()
}

// coreExternalNode returns an external (hardware-in-the-loop) node, which
// minimega knows nothing about.
func coreExternalNode(hostname, vlan, address string) string {
	return fmt.Sprintf(`
  - type: HIL
    external: true
    general:
      hostname: %s
    network:
      interfaces:
      - name: eth0
        type: ethernet
        proto: static
        vlan: %s
        address: %s
        mask: 24`, hostname, vlan, address)
}

// coreVMNames are the VMs of coreTopology.
func coreVMNames() []string {
	return []string{"h1", "h2", "h3", "o1"}
}

// coreTopology is the e2e-core topology: h1 and h2 are hollow Linux
// nodes on the ext4 fixture (startup scripts injected into partition 1); h3
// is a hollow Linux node on the blank fixture with inject_partition 0, so
// phenix delivers its startup scripts over C2 instead, two NICs and labels; o1
// is a node of OS type other (no injections). No node is external or delayed,
// so phenix starts the VMs with `vm start all`.
func coreTopology() string {
	h3 := coreNode{
		hostname: "h3",
		images:   []string{blankImage},
		drive0:   "\n        inject_partition: 0",
		extra:    "\n    labels:\n      role: web server\n      note: '" + coreNote + "'",
		nics:     []coreNIC{{"LAN", "10.40.0.3"}, {"WAN", "10.41.0.3"}},
	}

	o1 := coreNode{
		hostname: "o1",
		osType:   "other",
		images:   []string{blankImage},
		nics:     []coreNIC{{"WAN", "10.41.0.4"}},
	}

	return topology(
		hollowNode("h1", "LAN", "10.40.0.1", ""),
		hollowNode("h2", "LAN", "10.40.0.2", ""),
		h3.yaml(),
		o1.yaml(),
	)
}

// coreTopologyIPv4 is the address phenix reports for each interface of the
// coreTopology VMs, in minimega's interface order.
func coreTopologyIPv4() map[string][]string {
	return map[string][]string{
		"h1": {"10.40.0.1"},
		"h2": {"10.40.0.2"},
		"h3": {"10.40.0.3", "10.41.0.3"},
		"o1": {"10.41.0.4"},
	}
}

// coreFaultNode is a node QEMU refuses to start: the advanced qemu-append
// entry phenix renders as `vm config qemu-append` asks for a device model
// that does not exist, so QEMU exits as soon as minimega launches it.
func coreFaultNode(hostname, address string) string {
	return coreNode{
		hostname: hostname,
		osType:   "other",
		images:   []string{blankImage},
		extra:    "\n    advanced:\n      qemu-append: \"-device " + coreFaultDevice + "\"",
		nics:     []coreNIC{{"LAN", address}},
	}.yaml()
}

// ---------------------------------------------------------------------------
// experiments

// coreConfigs creates the topology (and optional scenario) configs named
// name from YAML that may use {{name}}, and deletes them when the test ends.
func coreConfigs(t *testing.T, name, topo, scenario string) {
	t.Helper()

	topo = strings.ReplaceAll(topo, "{{name}}", name)
	phenix(t, "config", "create", writeFile(t, name+"-topology.yaml", topo))

	t.Cleanup(func() { _, _ = phenixRun("config", "delete", "topology/"+name) })

	if scenario != "" {
		scenario = strings.ReplaceAll(scenario, "{{name}}", name)
		phenix(t, "config", "create", writeFile(t, name+"-scenario.yaml", scenario))

		t.Cleanup(func() { _, _ = phenixRun("config", "delete", "scenario/"+name) })
	}
}

// coreExperiment creates the configs and an experiment named after them with
// the CLI.
func coreExperiment(t *testing.T, name, topo, scenario string) *experiment {
	t.Helper()

	coreConfigs(t, name, topo, scenario)

	args := []string{"experiment", "create", name, "--topology", name}
	if scenario != "" {
		args = append(args, "--scenario", name)
	}

	e := adopt(t, name)

	phenix(t, args...)

	return e
}

// coreStagedScripts returns the startup scripts phenix staged for VM vm in
// dir (named <vm>-hostname.sh and so on). The directory also holds the C2
// response directory minimega creates for the namespace, which is not
// phenix's.
func coreStagedScripts(dir, vm string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}

	var scripts []string

	for _, e := range entries {
		if strings.HasPrefix(e.Name(), vm+"-") {
			scripts = append(scripts, e.Name())
		}
	}

	return scripts
}

// coreImageCopy copies the ext4 fixture to a private image for a node that
// boots without a snapshot: QEMU holds a write lock on such an image.
func coreImageCopy(t *testing.T, name string) string {
	t.Helper()

	dst := filepath.Join(cfg.imagesDir, name)

	if err := run("cp", "--sparse=always", filepath.Join(cfg.imagesDir, ext4Image), dst); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = os.Remove(dst) })

	return name
}

// ---------------------------------------------------------------------------
// minimega and host oracles

// coreSnapshot is the injection snapshot phenix creates for a VM.
func coreSnapshot(head, exp, vm string) string {
	return filepath.Join(cfg.imagesDir, fmt.Sprintf("%s_%s_%s_snapshot", trimHostSuffixes(head), exp, vm))
}

// coreVLANID returns the VLAN id of a minimega network entry like `LAN (101)`.
func coreVLANID(t *testing.T, entry string) int {
	t.Helper()

	m := coreVLANEntry.FindStringSubmatch(entry)
	if m == nil {
		t.Fatalf("network %q is not <alias> (<vlan>)", entry)
	}

	id, _ := strconv.Atoi(m[2])

	return id
}

// coreScheduleStatus returns the last `ns schedule status` row of namespace
// ns: minimega's own count of launched and failed VMs.
func coreScheduleStatus(t *testing.T, ns string) map[string]string {
	t.Helper()

	rows := mmTable(t, ns, "ns schedule status")
	if len(rows) == 0 {
		t.Fatalf("minimega reports no launch in namespace %s", ns)
	}

	return rows[len(rows)-1]
}

// coreWantCleanLaunch checks that minimega launched every VM it scheduled.
func coreWantCleanLaunch(t *testing.T, ns string, want int) {
	t.Helper()

	st := coreScheduleStatus(t, ns)
	if st["failures"] != "0" || st["launched"] != strconv.Itoa(want) {
		t.Errorf("ns schedule status in %s = %v, want %d launched and 0 failures", ns, st, want)
	}
}

// coreQEMUBaseline returns the UUIDs of the QEMU processes minimega runs
// before a test starts its VMs. coreWaitNoQEMU leaves them alone, so a VM that
// an earlier test, or another user of the host, left running fails only that
// test.
func coreQEMUBaseline(t *testing.T) map[string]bool {
	t.Helper()

	out := make(map[string]bool)

	for uuid := range resilienceQEMU(t) {
		out[uuid] = true
	}

	return out
}

// coreWaitNoQEMU waits until no QEMU process minimega started is left but
// those in baseline.
func coreWaitNoQEMU(t *testing.T, baseline map[string]bool) {
	t.Helper()

	eventually(t, coreTeardownWait, corePoll, "QEMU processes to exit", func() (bool, string) {
		var left []string

		for uuid, p := range resilienceQEMU(t) {
			if !baseline[uuid] {
				left = append(left, fmt.Sprintf("pid %d (VM id %s, uuid %s)", p.pid, p.id, uuid))
			}
		}

		return len(left) == 0, fmt.Sprintf("%d still running: %v", len(left), left)
	})
}

// coreWantNoNamespace checks that minimega does not hold namespace ns.
func coreWantNoNamespace(t *testing.T, ns string) {
	t.Helper()

	if row, ok := mmNamespaces(t)[ns]; ok {
		t.Errorf("minimega still holds namespace %s: %v", ns, row)
	}
}

// coreTaps returns every tap minimega reports for the VMs of namespace ns.
func coreTaps(t *testing.T, ns string) []string {
	t.Helper()

	info := vmInfo(t, ns)
	taps := make([]string, 0, len(info))

	for _, row := range info {
		taps = append(taps, splitList(row["tap"])...)
	}

	if len(taps) == 0 {
		t.Fatalf("minimega reports no taps in namespace %s", ns)
	}

	return taps
}

// coreWantState waits until minimega reports VM vm of namespace ns in state
// want, and reports an error (without stopping the test) if it never does.
func coreWantState(t *testing.T, ns, vm, want string, timeout time.Duration) {
	t.Helper()

	var got string

	for deadline := time.Now().Add(timeout); ; {
		got = vmInfo(t, ns)[vm]["state"]
		if got == want || time.Now().After(deadline) {
			break
		}

		time.Sleep(corePoll)
	}

	if got != want {
		t.Errorf("minimega reports %s in state %q after %v, want %s", vm, got, timeout, want)
	}
}

// coreBackingFile returns the backing file qemu-img records for a qcow2
// image, or "" when it has none.
func coreBackingFile(t *testing.T, image string) string {
	t.Helper()

	out, err := exec.CommandContext(t.Context(), "qemu-img", "info", "-U", "--output=json", image).Output()
	if err != nil {
		t.Fatalf("qemu-img info %s: %v", image, err)
	}

	var info struct {
		BackingFilename string `json:"backing-filename"`
	}

	if err := json.Unmarshal(out, &info); err != nil {
		t.Fatalf("decoding qemu-img info %s: %v", image, err)
	}

	return info.BackingFilename
}

// coreDiskUsage is the use percentage df reports for the filesystem holding
// path: used blocks over used plus available blocks, rounded up.
func coreDiskUsage(t *testing.T, path string) float64 {
	t.Helper()

	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		t.Fatalf("statfs %s: %v", path, err)
	}

	used := st.Blocks - st.Bfree

	return math.Ceil(float64(used) * 100 / float64(used+st.Bavail))
}

// coreExec runs a command and returns its stdout, stderr and exit code.
func coreExec(t *testing.T, name string, args ...string) (string, string, int) {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), name, args...) //nolint:gosec // test harness

	var stdout, stderr bytes.Buffer

	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()

	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		t.Fatalf("running %s %s: %v", name, strings.Join(args, " "), err)
	}

	return stdout.String(), stderr.String(), cmd.ProcessState.ExitCode()
}

// coreMMDebugLog raises minimega's log level to debug while fn runs and
// returns the lines minimega logged meanwhile; at debug level minimega logs
// every command it receives over its socket.
func coreMMDebugLog(t *testing.T, fn func()) []string {
	t.Helper()

	path := filepath.Join(cfg.mmBase, "minimega.log")

	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("minimega log %s is required: %v", path, err)
	}

	level := strings.ToLower(strings.TrimSpace(mmText(t, "", "log level")))
	if level == "" {
		level = "info"
	}

	restore := func() { _, _ = mmRun("", "log level "+level) }

	t.Cleanup(restore)
	mm(t, "", "log level debug")

	fn()

	restore()

	f, err := os.Open(path) //nolint:gosec // test harness
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	if _, err := f.Seek(st.Size(), io.SeekStart); err != nil {
		t.Fatal(err)
	}

	data, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}

	return strings.Split(string(data), "\n")
}

// ---------------------------------------------------------------------------
// REST

// coreAPIVM is a VM as the phenix REST API returns it (protojson names).
type coreAPIVM struct {
	Name            string            `json:"name"`
	Host            string            `json:"host"`
	IPv4            []string          `json:"ipv4"`
	CPUs            int               `json:"cpus"`
	RAM             int               `json:"ram"`
	Disk            string            `json:"disk"`
	Uptime          float64           `json:"uptime"`
	Networks        []string          `json:"networks"`
	Taps            []string          `json:"taps"`
	State           string            `json:"state"`
	Running         bool              `json:"running"`
	CCActive        bool              `json:"ccActive"`
	Tags            map[string]string `json:"tags"`
	DNB             bool              `json:"dnb"`
	External        bool              `json:"external"`
	Snapshot        bool              `json:"snapshot"`
	InjectPartition int               `json:"inject_partition"`
}

// coreAPIVMList is the body of GET /experiments/{name}/vms.
type coreAPIVMList struct {
	VMs   []coreAPIVM `json:"vms"`
	Total int         `json:"total"`
}

// coreAPIExperiment is an experiment as the phenix REST API returns it.
type coreAPIExperiment struct {
	Running   bool   `json:"running"`
	Status    string `json:"status"`
	StartTime string `json:"start_time"`
	VMCount   int    `json:"vm_count"`
	VLANs     []struct {
		VLAN  int    `json:"vlan"`
		Alias string `json:"alias"`
	} `json:"vlans"`
	VMs []coreAPIVM `json:"vms"`
}

// coreAPIHost is a cluster host as GET /hosts returns it.
type coreAPIHost struct {
	Name        string   `json:"name"`
	CPUs        int      `json:"cpus"`
	Load        []string `json:"load"`
	MemTotal    int      `json:"memtotal"`
	VMs         int      `json:"vms"`
	Schedulable bool     `json:"schedulable"`
	Headnode    bool     `json:"headnode"`
	DiskUsage   struct {
		Phenix   float64 `json:"diskphenix"`
		Minimega float64 `json:"diskminimega"`
	} `json:"diskusage"`
}

// coreAPIError is the JSON error body of phenix's REST handlers.
type coreAPIError struct {
	Message string `json:"message"`
	Cause   string `json:"cause"`
}

// coreResult is the outcome of a REST request made off the test goroutine.
type coreResult struct {
	code int
	body []byte
	err  error
}

// coreRequest calls the REST API without a body; it is safe to call from any
// goroutine.
func coreRequest(ctx context.Context, method, path string) coreResult {
	req, err := http.NewRequestWithContext(ctx, method, "http://"+cfg.uiAddr+"/api/v1"+path, nil)
	if err != nil {
		return coreResult{err: err}
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return coreResult{err: err}
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)

	return coreResult{code: resp.StatusCode, body: body, err: err}
}

// coreAPIFails calls the REST API and fails the test unless it answers with
// status want and a body containing text.
func coreAPIFails(t *testing.T, method, path string, want int, text string) {
	t.Helper()

	code, body := api(t, method, path, nil)
	if code != want || !strings.Contains(string(body), text) {
		t.Errorf("%s %s = %d %s, want %d with %q", method, path, code, body, want, text)
	}
}

// coreVMList returns GET /experiments/{name}/vms keyed by VM name.
func coreVMList(t *testing.T, name string) map[string]coreAPIVM {
	t.Helper()

	var list coreAPIVMList
	apiOK(t, http.MethodGet, "/experiments/"+name+"/vms", nil, &list)

	if list.Total != len(list.VMs) {
		t.Errorf("GET /experiments/%s/vms total = %d for %d VMs", name, list.Total, len(list.VMs))
	}

	vms := make(map[string]coreAPIVM, len(list.VMs))
	for _, vm := range list.VMs {
		vms[vm.Name] = vm
	}

	return vms
}

// ---------------------------------------------------------------------------
// F1-02, F1-07, F1-10, F3-01, F3-02, F3-04, F3-05

// TestCoreRESTLifecycle drives an experiment of hollow VMs through the REST
// API and the websocket (F1-02). Before it starts, it checks that phenix
// resolves disks in the files directory minimega reports (F3-02); while it
// runs, it checks labels (F1-10), GET /hosts (F3-04), phenix mm (F3-01),
// phenix's minimega parsers against raw minimega rows (F3-05), and that REST
// refuses to delete it (F1-07).
func TestCoreRESTLifecycle(t *testing.T) {
	hollowImages(t)

	const name = "e2e-core-rest"

	coreConfigs(t, name, coreTopology(), "")
	exp := adopt(t, name)
	ws := recordWS(t)
	qemu := coreQEMUBaseline(t)

	apiOK(t, http.MethodPost, "/experiments", map[string]any{"name": name, "topology": name}, nil)

	t.Run("F3-02 a stopped experiment resolves disks in minimega's files directory", func(t *testing.T) {
		coreCheckFilesDir(t, name)
	})

	var started coreAPIExperiment
	apiOK(t, http.MethodPost, "/experiments/"+name+"/start", nil, &started)

	if !started.Running {
		t.Errorf("POST /experiments/%s/start returned running = false", name)
	}

	exp.waitRunning(coreStartWait, coreVMNames()...)

	t.Run("minimega launched every VM", func(t *testing.T) { coreWantCleanLaunch(t, name, len(coreVMNames())) })
	t.Run("GET experiment", func(t *testing.T) { coreCheckRESTExperiment(t, name) })
	t.Run("GET VMs", func(t *testing.T) { coreCheckRESTVMs(t, name) })
	t.Run("F1-10 labels reach minimega and REST verbatim", func(t *testing.T) { coreCheckLabels(t, name) })
	t.Run("F3-04 GET hosts", func(t *testing.T) { coreCheckHosts(t) })
	t.Run("F3-01 phenix mm passes commands through", func(t *testing.T) { coreCheckPhenixMM(t, name) })
	t.Run("F3-05 phenix's minimega parsers", func(t *testing.T) { coreCheckMMPackage(t, name) })

	t.Run("F1-07 DELETE of a running experiment is refused", func(t *testing.T) {
		before := vmUUIDs(t, name)

		coreAPIFails(t, http.MethodDelete, "/experiments/"+name, http.StatusBadRequest, "cannot delete a running experiment")

		if !exp.running() {
			t.Error("the experiment stopped after the refused DELETE")
		}

		if after := vmUUIDs(t, name); !maps.Equal(after, before) {
			t.Errorf("VMs changed after the refused DELETE: %v, before %v", after, before)
		}
	})

	taps := coreTaps(t, name)
	head := headnode(t)

	var stopped coreAPIExperiment
	apiOK(t, http.MethodPost, "/experiments/"+name+"/stop", nil, &stopped)

	t.Run("stop tears the experiment down", func(t *testing.T) {
		if stopped.Running || stopped.StartTime != "" {
			t.Errorf("POST /experiments/%s/stop returned running = %v, start_time = %q", name, stopped.Running, stopped.StartTime)
		}

		coreWantNoNamespace(t, name)
		coreWaitNoQEMU(t, qemu)
		wantTapsGone(t, taps)

		for _, vm := range coreVMNames() {
			if _, err := os.Stat(coreSnapshot(head, name, vm)); !os.IsNotExist(err) {
				t.Errorf("injection snapshot of %s was not deleted on stop (stat: %v)", vm, err)
			}
		}
	})

	code, body := api(t, http.MethodDelete, "/experiments/"+name, nil)
	if code != http.StatusNoContent {
		t.Fatalf("DELETE /experiments/%s = %d %s, want 204", name, code, body)
	}

	t.Run("delete removes the experiment", func(t *testing.T) {
		if code, body := api(t, http.MethodGet, "/experiments/"+name, nil); code < 300 {
			t.Errorf("GET /experiments/%s after delete = %d %s", name, code, body)
		}

		phenixFails(t, "config", "get", "experiment/"+name)

		if _, err := os.Stat(filepath.Join(corePhenixBase, "experiments", name)); !os.IsNotExist(err) {
			t.Errorf("experiment base directory still exists after delete (stat: %v)", err)
		}
	})

	t.Run("F1-02 websocket sequence", func(t *testing.T) { coreCheckWSSequence(t, ws, name) })
}

// coreCheckFilesDir checks that phenix places the disks of a stopped
// experiment in the files directory minimega reports in `args`. With
// minimega's -filepath at phenix's fallback, <phenix base>/images, as on the
// CI runners, it cannot tell whether phenix read `args`;
// TestResilienceFilesDir restarts minimega on another -filepath for that.
func coreCheckFilesDir(t *testing.T, name string) {
	t.Helper()

	rows := mmTable(t, "", "args")
	if len(rows) != 1 {
		t.Fatalf("minimega args returned %d rows, want 1: %v", len(rows), rows)
	}

	filesDir := rows[0]["filepath"]
	if filesDir != cfg.imagesDir {
		t.Errorf("minimega -filepath = %q, want %q", filesDir, cfg.imagesDir)
	}

	want := map[string]string{"h1": ext4Image, "h2": ext4Image, "h3": blankImage, "o1": blankImage}

	vms := coreVMList(t, name)
	if len(vms) != len(want) {
		t.Errorf("GET /experiments/%s/vms lists %d VMs, want %d: %v", name, len(vms), len(want), vms)
	}

	for vm, image := range want {
		got := vms[vm]

		if got.Disk != filepath.Join(filesDir, image) {
			t.Errorf("%s disk = %q, want %q", vm, got.Disk, filepath.Join(filesDir, image))
		}

		if got.Running || got.State != "" {
			t.Errorf("%s of a stopped experiment is running = %v, state %q", vm, got.Running, got.State)
		}
	}

	if out := phenix(t, "vm", "info", name); !strings.Contains(out, filepath.Join(filesDir, ext4Image)) {
		t.Errorf("phenix vm info does not show the disk %s:\n%s", filepath.Join(filesDir, ext4Image), out)
	}
}

// coreCheckRESTExperiment checks GET /experiments/{name} of the running
// experiment against minimega's VLANs and phenix's stored status.
func coreCheckRESTExperiment(t *testing.T, name string) {
	t.Helper()

	exp := &experiment{t: t, name: name}

	var got coreAPIExperiment
	apiOK(t, http.MethodGet, "/experiments/"+exp.name, nil, &got)

	if !got.Running {
		t.Error("running = false for a running experiment")
	}

	if _, err := time.Parse(time.RFC3339, got.StartTime); err != nil {
		t.Errorf("start_time %q is not RFC3339: %v", got.StartTime, err)
	}

	if got.VMCount != len(coreVMNames()) {
		t.Errorf("vm_count = %d, want %d", got.VMCount, len(coreVMNames()))
	}

	vlans := make(map[string]int)
	for _, v := range got.VLANs {
		vlans[v.Alias] = v.VLAN
	}

	raw := rawVLANs(t, exp.name)

	if len(raw) != 2 || raw["LAN"] == 0 || raw["WAN"] == 0 {
		t.Errorf("minimega VLANs = %v, want LAN and WAN", raw)
	}

	if !maps.Equal(vlans, raw) {
		t.Errorf("REST vlans = %v, minimega reports %v", vlans, raw)
	}

	doc := exp.doc()

	if !maps.Equal(doc.Status.VLANs, raw) {
		t.Errorf("status.vlans = %v, minimega reports %v", doc.Status.VLANs, raw)
	}

	head := headnode(t)

	for _, vm := range coreVMNames() {
		if doc.Status.Schedules[vm] != head {
			t.Errorf("status.schedules[%s] = %q, want %q", vm, doc.Status.Schedules[vm], head)
		}
	}
}

// coreCheckRESTVMs checks GET /experiments/{name}/vms of the running
// experiment against minimega's rows and Open vSwitch.
func coreCheckRESTVMs(t *testing.T, name string) {
	t.Helper()

	var (
		vms  = coreVMList(t, name)
		raw  = vmInfo(t, name)
		ipv4 = coreTopologyIPv4()
	)

	if len(vms) != len(ipv4) {
		t.Errorf("REST lists %d VMs, want %d", len(vms), len(ipv4))
	}

	for vm, addrs := range ipv4 {
		got, row := vms[vm], raw[vm]

		if got.State != coreStateRunning || !got.Running {
			t.Errorf("%s state = %q, running = %v", vm, got.State, got.Running)
		}

		if got.Host != row["host"] || got.Host == "" {
			t.Errorf("%s host = %q, minimega reports %q", vm, got.Host, row["host"])
		}

		if !slices.Equal(got.Networks, splitList(row["vlan"])) || !slices.Equal(got.Taps, splitList(row["tap"])) {
			t.Errorf("%s networks %v taps %v, minimega reports vlan %s tap %s", vm, got.Networks, got.Taps, row["vlan"], row["tap"])
		}

		if !slices.Equal(got.IPv4, addrs) {
			t.Errorf("%s ipv4 = %v, want %v from the topology", vm, got.IPv4, addrs)
		}

		if got.CCActive || got.CPUs != coreHollowVCPUs || got.RAM != coreHollowMemory || got.Uptime <= 0 {
			t.Errorf("%s ccActive %v cpus %d ram %d uptime %v, want false, %d, %d, > 0",
				vm, got.CCActive, got.CPUs, got.RAM, got.Uptime, coreHollowVCPUs, coreHollowMemory)
		}

		coreCheckVMTaps(t, vm, got.Networks, got.Taps)
	}

	for vm, nw := range map[string]*regexp.Regexp{"h1": coreNetworkLAN, "h3": coreNetworkLAN, "o1": coreNetworkWAN} {
		if n := vms[vm].Networks; len(n) == 0 || !nw.MatchString(n[0]) {
			t.Errorf("%s networks = %v, want the first to match %s", vm, n, nw)
		}
	}

	if n := vms["h3"].Networks; len(n) != 2 || !coreNetworkWAN.MatchString(n[1]) {
		t.Errorf("h3 networks = %v, want LAN then WAN", n)
	}

	if vms["h3"].InjectPartition != 0 || vms["h1"].InjectPartition != 1 {
		t.Errorf("inject_partition h3 = %d, h1 = %d, want 0 and 1", vms["h3"].InjectPartition, vms["h1"].InjectPartition)
	}
}

// coreCheckVMTaps checks that each tap of a VM sits on the experiment bridge
// with the VLAN tag of its network.
func coreCheckVMTaps(t *testing.T, vm string, networks, taps []string) {
	t.Helper()

	if len(taps) != len(networks) {
		t.Errorf("%s has %d taps for %d networks", vm, len(taps), len(networks))

		return
	}

	for i, tap := range taps {
		if br, err := ovsVsctl(t, "port-to-br", tap); err != nil || br != "phenix" {
			t.Errorf("%s tap %s is on bridge %q (%v), want phenix", vm, tap, br, err)
		}

		tag, err := ovsVsctl(t, "get", "port", tap, "tag")
		if want := strconv.Itoa(coreVLANID(t, networks[i])); err != nil || tag != want {
			t.Errorf("%s tap %s has OVS tag %q (%v), want %s for %s", vm, tap, tag, err, want, networks[i])
		}
	}
}

// coreCheckLabels checks that node labels reach minimega as VM tags and come
// back through REST unchanged, including a value with double quotes.
func coreCheckLabels(t *testing.T, name string) {
	t.Helper()

	exp := &experiment{t: t, name: name}
	want := map[string]string{"role": "web server", "note": coreNote}

	var tags map[string]string

	raw := vmInfo(t, exp.name)["h3"]["tags"]
	if err := json.Unmarshal([]byte(raw), &tags); err != nil {
		t.Fatalf("minimega tags of h3 %q: %v", raw, err)
	}

	var vm coreAPIVM
	apiOK(t, http.MethodGet, "/experiments/"+exp.name+"/vms/h3", nil, &vm)

	for k, v := range want {
		if tags[k] != v {
			t.Errorf("minimega tag %s of h3 = %q, want %q (rendered: %s)", k, tags[k], v, coreScriptLine(exp, `vm config tags "`+k+`"`))
		}

		if vm.Tags[k] != v {
			t.Errorf("GET /experiments/%s/vms/h3 tag %s = %q, want %q", exp.name, k, vm.Tags[k], v)
		}
	}
}

// coreScriptLine returns the first line of the rendered minimega script that
// starts with prefix.
func coreScriptLine(exp *experiment, prefix string) string {
	for line := range strings.SplitSeq(exp.mmScript(), "\n") {
		if strings.HasPrefix(line, prefix) {
			return line
		}
	}

	return "<none>"
}

// coreCheckHosts checks GET /hosts against minimega's `host` row and df.
func coreCheckHosts(t *testing.T) {
	t.Helper()

	var cluster struct {
		Hosts []coreAPIHost `json:"hosts"`
	}

	apiOK(t, http.MethodGet, "/hosts", nil, &cluster)

	if len(cluster.Hosts) != 1 {
		t.Fatalf("GET /hosts lists %d hosts, want 1: %+v", len(cluster.Hosts), cluster.Hosts)
	}

	var (
		got  = cluster.Hosts[0]
		head = headnode(t)
		raw  = mmTable(t, "", "host")
	)

	if len(raw) != 1 {
		t.Fatalf("minimega host returned %d rows: %v", len(raw), raw)
	}

	if got.Name != trimHostSuffixes(head) || !got.Headnode || !got.Schedulable {
		t.Errorf("host %q headnode %v schedulable %v, want %q, true, true", got.Name, got.Headnode, got.Schedulable, trimHostSuffixes(head))
	}

	if strconv.Itoa(got.CPUs) != raw[0]["cpus"] || strconv.Itoa(got.MemTotal) != raw[0]["memtotal"] {
		t.Errorf("cpus %d memtotal %d, minimega reports %s and %s", got.CPUs, got.MemTotal, raw[0]["cpus"], raw[0]["memtotal"])
	}

	if strconv.Itoa(got.VMs) != raw[0]["vms"] || got.VMs < len(coreVMNames()) {
		t.Errorf("vms = %d while the experiment runs %d VMs, minimega reports %s", got.VMs, len(coreVMNames()), raw[0]["vms"])
	}

	if len(got.Load) != len(strings.Fields(raw[0]["load"])) || len(got.Load) != 3 {
		t.Errorf("load = %q, minimega reports %q", got.Load, raw[0]["load"])
	}

	for path, usage := range map[string]float64{corePhenixBase: got.DiskUsage.Phenix, cfg.mmBase: got.DiskUsage.Minimega} {
		if want := coreDiskUsage(t, path); usage <= 0 || usage > 100 || math.Abs(usage-want) > 2 {
			t.Errorf("disk usage of %s = %v%%, df reports about %v%%", path, usage, want)
		}
	}
}

// coreCheckPhenixMM checks that phenix mm prints what minimega -e prints for
// the same command, and that it fails when minimega reports an error.
func coreCheckPhenixMM(t *testing.T, name string) {
	t.Helper()

	minimega, err := exec.LookPath("minimega")
	if err != nil {
		t.Fatalf("minimega is required on PATH: %v", err)
	}

	vmInfoCmd := []string{".columns", "name,state", "vm", "info"}

	for _, c := range []struct{ phenix, minimega []string }{
		{phenix: []string{"mm", "version"}, minimega: []string{"-e", "version"}},
		{phenix: append([]string{"mm", "--namespace", name}, vmInfoCmd...), minimega: append([]string{"-namespace", name, "-e"}, vmInfoCmd...)},
	} {
		want, _, _ := coreExec(t, minimega, append([]string{"-base", cfg.mmBase, "-level", "error"}, c.minimega...)...)
		got, stderr, code := coreExec(t, cfg.phenix, phenixArgs(c.phenix...)...)

		if code != 0 || got != want || strings.TrimSpace(got) == "" {
			t.Errorf(
				"phenix %s exited %d and printed\n%s\nminimega -e printed\n%s\nstderr: %s",
				strings.Join(c.phenix, " "),
				code,
				got,
				want,
				stderr,
			)
		}
	}

	table, _, _ := coreExec(t, cfg.phenix, phenixArgs(append([]string{"mm", "--namespace", name}, vmInfoCmd...)...)...)
	for _, vm := range coreVMNames() {
		if !regexp.MustCompile(`(?m)\b` + vm + `\b.*\b` + coreStateRunning + `\b`).MatchString(table) {
			t.Errorf("phenix mm vm info does not show %s RUNNING:\n%s", vm, table)
		}
	}

	_, mmStderr, mmCode := coreExec(t, minimega, "-base", cfg.mmBase, "-level", "error", "-e", "vm", "bogus")

	lines := strings.Split(strings.TrimSpace(mmStderr), "\n")
	mmErr := strings.TrimSpace(lines[len(lines)-1])

	// minimega: `minimega -e` prints a command error on stderr and still exits 0.
	t.Logf("minimega -e vm bogus exited %d with %q", mmCode, mmErr)

	// minimega -e prints each host's error as "Error (<host>): <error>";
	// phenix mm names it as "<host>: <error>".
	want := mmErr
	if m := regexp.MustCompile(`^Error \((.*?)\): (.*)$`).FindStringSubmatch(mmErr); m != nil {
		want = m[1] + ": " + m[2]
	}

	_, stderr, code := coreExec(t, cfg.phenix, phenixArgs("mm", "vm", "bogus")...)
	if code == 0 || mmErr == "" || !strings.Contains(stderr, want) {
		t.Errorf("phenix mm vm bogus exited %d with stderr %q, want nonzero with minimega's error %q", code, stderr, want)
	}
}

// coreMMWarm points phenix's minimega client at the daemon under test, sets
// the host name suffixes phenix strips to the CLI's default (the root command
// sets them, and it does not run in this process), and makes sure the shared
// connection works: after a minimega restart the first command on a stale
// connection fails and the next one redials.
func coreMMWarm(t *testing.T) {
	t.Helper()

	common.MinimegaBase = cfg.mmBase              //nolint:reassign // the client under test dials the daemon under test
	common.HostnameSuffixes = "-minimega,-phenix" //nolint:reassign // phenix's --hostname-suffixes default

	var err error

	for range 2 {
		cmd := mmcli.NewCommand()
		cmd.Command = "version"

		if err = mmcli.ErrorResponse(mmcli.Run(cmd)); err == nil {
			return
		}
	}

	t.Fatalf("phenix's minimega client cannot reach minimega: %v", err)
}

// coreCheckMMPackage runs phenix's own minimega client and parsers (the
// subject under test) against the running experiment and compares what they
// return with raw minimega rows.
func coreCheckMMPackage(t *testing.T, name string) {
	t.Helper()

	coreMMWarm(t)

	raw := vmInfo(t, name)
	vms := phenixmm.GetVMInfo(phenixmm.NS(name))

	if len(vms) != len(raw) || len(vms) != len(coreVMNames()) {
		t.Errorf("mm.GetVMInfo returned %d VMs, minimega reports %d", len(vms), len(raw))
	}

	for _, vm := range vms {
		row, ok := raw[vm.Name]
		if !ok {
			t.Errorf("mm.GetVMInfo returned %s, which minimega does not report", vm.Name)

			continue
		}

		coreCompareVM(t, vm, row)
	}

	vlans, err := phenixmm.GetVLANs(phenixmm.NS(name))
	if want := rawVLANs(t, name); err != nil || !maps.Equal(vlans, want) {
		t.Errorf("mm.GetVLANs = %v (%v), minimega reports %v", vlans, err, want)
	}

	if state, err := phenixmm.GetVMState(phenixmm.NS(name), phenixmm.VMName("h1")); err != nil || state != raw["h1"]["state"] {
		t.Errorf("mm.GetVMState(h1) = %q (%v), minimega reports %q", state, err, raw["h1"]["state"])
	}

	rawArgs := mmTable(t, "", "args")
	if len(rawArgs) != 1 {
		t.Fatalf("minimega args returned %d rows, want 1", len(rawArgs))
	}

	args, err := phenixmm.GetMMArgs()
	if want := rawArgs[0]["filepath"]; err != nil || args["filepath"] != want || want != cfg.imagesDir {
		t.Errorf("mm.GetMMArgs filepath = %q (%v), minimega reports %q", args["filepath"], err, want)
	}

	hosts, err := phenixmm.GetClusterHosts(true)
	if head := trimHostSuffixes(
		headnode(t),
	); err != nil || len(hosts) != 1 || hosts[0].Name != head || !hosts[0].Headnode ||
		!hosts[0].Schedulable {
		t.Errorf("mm.GetClusterHosts(true) = %+v (%v), want only the schedulable headnode %s", hosts, err, head)
	}

	if captures := phenixmm.GetExperimentCaptures(phenixmm.NS(name)); len(captures) != 0 {
		t.Errorf("mm.GetExperimentCaptures = %v for an experiment without captures", captures)
	}

	coreCheckHostColumn(t, name, raw)
}

// coreCompareVM compares one VM phenix parsed with minimega's raw row.
func coreCompareVM(t *testing.T, vm phenixmm.VM, row map[string]string) {
	t.Helper()

	if vm.UUID != row["uuid"] || vm.Host != row["host"] || vm.State != row["state"] || vm.Running != (row["state"] == coreStateRunning) {
		t.Errorf("%s uuid %q host %q state %q running %v, minimega reports %q %q %q",
			vm.Name, vm.UUID, vm.Host, vm.State, vm.Running, row["uuid"], row["host"], row["state"])
	}

	if !slices.Equal(vm.Networks, splitList(row["vlan"])) || !slices.Equal(vm.Taps, splitList(row["tap"])) {
		t.Errorf("%s networks %q taps %q, minimega reports vlan %s tap %s", vm.Name, vm.Networks, vm.Taps, row["vlan"], row["tap"])
	}

	if strconv.Itoa(vm.RAM) != row["memory"] || strconv.Itoa(vm.CPUs) != row["vcpus"] || vm.RAM != coreHollowMemory ||
		vm.CPUs != coreHollowVCPUs {
		t.Errorf("%s ram %d cpus %d, minimega reports memory %s vcpus %s", vm.Name, vm.RAM, vm.CPUs, row["memory"], row["vcpus"])
	}

	if vm.Uptime <= 0 {
		t.Errorf("%s uptime = %v, minimega reports %s", vm.Name, vm.Uptime, row["uptime"])
	}

	var tags map[string]string
	if err := json.Unmarshal([]byte(row["tags"]), &tags); err != nil || !maps.Equal(vm.Tags, tags) {
		t.Errorf("%s tags = %v, minimega reports %s (%v)", vm.Name, vm.Tags, row["tags"], err)
	}

	disks := strings.Fields(row["disks"])
	if len(disks) == 0 {
		t.Errorf("minimega reports no disk for %s", vm.Name)

		return
	}

	// phenix reports the image a snapshot VM boots from, not the snapshot.
	first := strings.Split(disks[0], ",")[0]
	want := first

	if row["snapshot"] == "true" {
		if backing := coreBackingFile(t, first); backing != "" {
			want = backing
		}
	}

	if vm.Disk != want {
		t.Errorf("%s disk = %q, want %q (minimega disks %q)", vm.Name, vm.Disk, want, row["disks"])
	}
}

// coreCheckHostColumn documents how phenix gets a VM's host: minimega's `vm
// info` has no host column, so `.columns host` selects the container
// hostname column by prefix, and phenix fills host from the responding node.
func coreCheckHostColumn(t *testing.T, name string, raw map[string]map[string]string) {
	t.Helper()

	resp := mm(t, name, ".columns host vm info")
	if len(resp) == 0 || !slices.Equal(resp[0].Header, []string{"hostname"}) {
		t.Errorf("minimega .columns host vm info header = %v, want [hostname]", resp)
	}

	cmd := mmcli.NewNamespacedCommand(name)
	cmd.Command = "vm info"
	cmd.Columns = []string{"host", "name"}

	rows := mmcli.RunTabular(cmd)
	if len(rows) != len(raw) {
		t.Errorf("mmcli.RunTabular returned %d rows, minimega reports %d VMs", len(rows), len(raw))
	}

	for _, row := range rows {
		if _, ok := row["hostname"]; ok || row["host"] != raw[row["name"]]["host"] {
			t.Errorf("mmcli.RunTabular row %v, want host %q and no hostname column", row, raw[row["name"]]["host"])
		}
	}
}

// coreCheckWSSequence checks the experiment messages phenix published on the
// websocket: starting, progress within [0, 1], start, stopping, stop, delete.
func coreCheckWSSequence(t *testing.T, ws *wsRecorder, name string) {
	t.Helper()

	var (
		msgs    []wsMessage
		actions []string
	)

	eventually(t, coreTeardownWait, corePoll, "websocket delete message", func() (bool, string) {
		msgs, actions = ws.messages("experiment", name), actions[:0]

		for _, m := range msgs {
			actions = append(actions, m.Resource.Action)
		}

		return slices.Contains(actions, "delete"), fmt.Sprintf("seen %v", actions)
	})

	starting, start := slices.Index(actions, "starting"), slices.Index(actions, "start")
	if starting < 0 || start < starting {
		t.Fatalf("websocket actions %v, want starting before start", actions)
	}

	progress := 0

	for i, m := range msgs {
		switch m.Resource.Action {
		case "progress":
			progress++

			var p struct {
				Percent float64 `json:"percent"`
			}

			if err := json.Unmarshal(m.Result, &p); err != nil || p.Percent < 0 || p.Percent > 1 {
				t.Errorf("progress result %s (%v), want a percent in [0, 1]", m.Result, err)
			}

			if i < starting || i > start {
				t.Errorf("progress message at %d outside starting (%d) .. start (%d): %v", i, starting, start, actions)
			}
		case "errorStarting", "errorStopping":
			t.Errorf("websocket reported %s: %v", m.Resource.Action, actions)
		}
	}

	if progress == 0 {
		t.Errorf("no progress message between starting and start: %v", actions)
	}

	stopping, stop, deleted := slices.Index(actions, "stopping"), slices.Index(actions, "stop"), slices.Index(actions, "delete")
	if stopping < start || stop < stopping || deleted < stop {
		t.Errorf("websocket actions %v, want start, stopping, stop, delete in that order", actions)
	}
}

// ---------------------------------------------------------------------------
// F1-03

// coreInstallSleepApp installs, as a regular file on the PATH of the CLI and
// the UI the harness started, a user app that reads the experiment and sleeps
// in its pre-start stage, so a start holds phenix's experiment lock for a
// while. It prints nothing, which tells phenix it made no change.
func coreInstallSleepApp(t *testing.T) {
	t.Helper()

	var (
		path   = filepath.Join("/usr/local/bin", "phenix-app-"+coreSleepApp)
		script = fmt.Sprintf("#!/bin/sh\n# Installed by the phenix E2E tests (TestCoreLocking).\n"+
			"cat >/dev/null\nif [ \"$1\" = pre-start ]; then sleep %d; fi\n", coreSleepAppSeconds)
	)

	if err := os.WriteFile(path, []byte(script), 0o755); err != nil { //nolint:gosec // an executable user app
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = os.Remove(path) })

	if found, err := exec.LookPath(filepath.Base(path)); err != nil || found != path {
		t.Fatalf("user app %s is not first on PATH (found %q, %v)", path, found, err)
	}
}

// TestCoreLocking checks how phenix serializes starts and stops of one
// experiment and reports state errors (F1-03).
func TestCoreLocking(t *testing.T) {
	hollowImages(t)
	coreInstallSleepApp(t)

	const name = "e2e-core-lock"

	scenario := `apiVersion: phenix.sandia.gov/v2
kind: Scenario
metadata:
  name: {{name}}
  annotations:
    topology: {{name}}
spec:
  apps:
  - name: ` + coreSleepApp

	node := coreNode{hostname: "l1", osType: "other", images: []string{blankImage}, nics: []coreNIC{{"LAN", "10.45.0.1"}}}
	exp := coreExperiment(t, name, topology(node.yaml()), scenario)

	var (
		first = make(chan coreResult, 1)
		done  = make(chan struct{})
	)

	// Runs before the experiment's teardown, which must not race the start
	// should the test end while it holds the lock.
	t.Cleanup(func() { <-done })

	go func() {
		defer close(done)

		first <- coreRequest(context.WithoutCancel(t.Context()), http.MethodPost, "/experiments/"+name+"/start")
	}()

	eventually(t, coreTeardownWait, 100*time.Millisecond, "the first start to lock the experiment", func() (bool, string) {
		var got coreAPIExperiment

		apiOK(t, http.MethodGet, "/experiments/"+name, nil, &got)

		return got.Status == "starting", "status " + got.Status
	})

	coreAPIFails(t, http.MethodPost, "/experiments/"+name+"/start", http.StatusConflict, "unable to lock experiment")

	r := <-first
	if r.err != nil || r.code != http.StatusOK {
		t.Fatalf("first POST /experiments/%s/start = %d %s (%v), want 200", name, r.code, r.body, r.err)
	}

	exp.waitRunning(coreStartWait, "l1")

	before := vmUUIDs(t, name)

	t.Run("start of a running experiment", func(t *testing.T) {
		coreAPIFails(t, http.MethodPost, "/experiments/"+name+"/start", http.StatusBadRequest, "experiment already running")

		out, err := phenixRun("experiment", "start", name)
		t.Logf("phenix experiment start of a running experiment: err %v", err)

		if !strings.Contains(out, "not starting already running experiment") {
			t.Errorf("phenix experiment start of a running experiment did not say so:\n%s", out)
		}

		if after := vmUUIDs(t, name); !maps.Equal(after, before) {
			t.Errorf("VMs changed: %v, before %v", after, before)
		}
	})

	apiOK(t, http.MethodPost, "/experiments/"+name+"/stop", nil, nil)

	t.Run("stop of a stopped experiment", func(t *testing.T) {
		var resp coreAPIError

		code, body := api(t, http.MethodPost, "/experiments/"+name+"/stop", nil)
		if json.Unmarshal(body, &resp) != nil || code != http.StatusBadRequest || resp.Message == "" ||
			!strings.Contains(resp.Cause, "experiment isn't running") {
			t.Errorf("POST stop of a stopped experiment = %d %s, want 400 with cause \"experiment isn't running\"", code, body)
		}
	})
}

// ---------------------------------------------------------------------------
// F1-04

// TestCoreLaunchFailure starts experiments in which QEMU refuses to start
// one VM (F1-04). phenix must fail the start, name the VM, and clear the
// namespace; with --treat-mm-errors-as-warnings it must start the other VM and
// warn about the failed one. Both of phenix's start paths are covered: `vm
// start all`, and one `vm start` per VM, which phenix uses whenever the
// topology has an external or delayed node.
func TestCoreLaunchFailure(t *testing.T) {
	hollowImages(t)

	t.Run("vm start all", func(t *testing.T) { coreLaunchFailure(t, "e2e-core-fault", false) })
	t.Run("per-VM start", func(t *testing.T) { coreLaunchFailure(t, "e2e-core-faultx", true) })
	t.Run("REST start", func(t *testing.T) { coreLaunchFailureREST(t, "e2e-core-faultr") })
}

// coreLaunchFailureREST starts the fault topology through REST. phenix ui
// must answer 400 naming f1, announce errorStarting, and leave no namespace,
// QEMU process or injection snapshot. Its launch progress poller runs while
// the start does, but takes the start's result as soon as the failed start
// cleared the namespace, so this rarely sees a poll that creates the
// namespace again; TestCoreLaunchProgress checks that poll directly.
func coreLaunchFailureREST(t *testing.T, name string) {
	t.Helper()

	exp := coreExperiment(t, name, topology(coreFaultNode("f1", "10.43.0.1"), hollowNode("f2", "LAN", "10.43.0.2", "")), "")
	qemu := coreQEMUBaseline(t)
	ws := recordWS(t)

	code, body := api(t, http.MethodPost, "/experiments/"+name+"/start", nil)
	if code != http.StatusBadRequest || !regexp.MustCompile(`\bVM f1\b`).Match(body) || bytes.Contains(body, []byte("VM f2")) {
		t.Errorf("POST /experiments/%s/start = %d %s, want 400 naming f1 (and only f1)", name, code, bytes.TrimSpace(body))
	}

	ws.waitAction(t, 10*time.Second, "experiment", name, "errorStarting")

	coreWantNoNamespace(t, name)
	coreWaitNoQEMU(t, qemu)
	coreWantNoSnapshots(t, name, "f1", "f2")

	if exp.running() {
		t.Error("the experiment is marked running after its REST start failed")
	}

	coreWantNoNamespace(t, name)
}

// coreWantNoSnapshots checks that the injection snapshots of the given VMs of
// experiment exp are gone. A failed start deletes those its minimega script
// created before the launch failed.
func coreWantNoSnapshots(t *testing.T, exp string, vms ...string) {
	t.Helper()

	head := headnode(t)

	for _, vm := range vms {
		if _, err := os.Stat(coreSnapshot(head, exp, vm)); !os.IsNotExist(err) {
			t.Errorf("the failed start left the injection snapshot of %s behind (stat: %v)", vm, err)
		}
	}
}

func coreLaunchFailure(t *testing.T, name string, perVM bool) {
	t.Helper()

	nodes := []string{coreFaultNode("f1", "10.43.0.1"), hollowNode("f2", "LAN", "10.43.0.2", "")}
	if perVM {
		nodes = append(nodes, coreExternalNode("fext", "LAN", "10.43.0.3"))
	}

	exp := coreExperiment(t, name, topology(nodes...), "")
	failedVM := regexp.MustCompile(`\bVM f1\b`)
	qemu := coreQEMUBaseline(t)

	out := phenixFails(t, "experiment", "start", name)

	if !failedVM.MatchString(out) || strings.Contains(out, "VM f2") {
		t.Errorf("failed start does not name f1 (and only f1):\n%s", out)
	}

	// Started one by one, f1 fails its own `vm start` (util/mm startEach);
	// `vm start all` skips a VM in the ERROR state, so only phenix's launch
	// check names it.
	switch perVMErr := strings.Contains(out, "starting VM f1:"); {
	case perVM && !perVMErr:
		t.Errorf("failed start lacks the error of `vm start f1`, so phenix did not start the VMs one by one:\n%s", out)
	case !perVM && perVMErr:
		t.Errorf("failed start reports an error of `vm start f1` although phenix should use `vm start all`:\n%s", out)
	}

	coreWantNoNamespace(t, name)
	coreWaitNoQEMU(t, qemu)
	coreWantNoSnapshots(t, name, "f1", "f2")

	if exp.running() {
		t.Error("the experiment is marked running after its start failed")
	}

	out = phenix(t, "experiment", "start", name, "--treat-mm-errors-as-warnings")

	if !failedVM.MatchString(out) {
		t.Errorf("start with --treat-mm-errors-as-warnings does not warn about f1:\n%s", out)
	}

	if !exp.running() {
		t.Fatal("the experiment is not running after a start with --treat-mm-errors-as-warnings")
	}

	coreWantState(t, name, "f2", coreStateRunning, coreTeardownWait)

	row := vmInfo(t, name)["f1"]
	if row["state"] != coreStateError {
		t.Errorf("minimega reports f1 in state %q, want ERROR", row["state"])
	}

	t.Logf("minimega's reason for f1: tags %s", row["tags"])

	// phenix reports minimega's reason, the error tag minimega set on f1.
	var tags map[string]string

	_ = json.Unmarshal([]byte(row["tags"]), &tags)

	reason, _, _ := strings.Cut(tags["error"], "\n")
	if reason = strings.TrimSpace(reason); reason == "" || !strings.Contains(out, reason) {
		t.Errorf("the warning about f1 does not carry minimega's reason %q:\n%s", reason, out)
	}

	if !perVM {
		// minimega: `vm launch` succeeds when QEMU exits at launch; only `ns schedule status` counts the failure.
		if st := coreScheduleStatus(t, name); st["failures"] == "0" {
			t.Errorf("ns schedule status = %v, want the launch failure counted", st)
		}
	}

	exp.stop()
	coreWantNoNamespace(t, name)
	coreWaitNoQEMU(t, qemu)
}

// ---------------------------------------------------------------------------
// F1-05

// TestCoreMixedTopology starts a topology that mixes a plain VM, a
// do-not-boot node, an external node, a node booting its image without a
// snapshot, and a node with two drives (F1-05). The external node makes phenix
// start the VMs one by one.
func TestCoreMixedTopology(t *testing.T) {
	hollowImages(t)

	const name = "e2e-core-mixed"

	ns1Image := coreImageCopy(t, "e2e-core-ns1.qc2")

	topo := topology(
		hollowNode("m1", "LAN", "10.42.0.1", ""),
		coreNode{
			hostname: "dnb1",
			images:   []string{ext4Image},
			general:  "\n      do_not_boot: true",
			nics:     []coreNIC{{"LAN", "10.42.0.2"}},
		}.yaml(),
		coreExternalNode("ext1", "LAN", "10.42.0.3"),
		coreNode{
			hostname: "ns1",
			images:   []string{ns1Image},
			general:  "\n      snapshot: false",
			nics:     []coreNIC{{"LAN", "10.42.0.4"}},
		}.yaml(),
		coreNode{
			hostname: "md1",
			images:   []string{ext4Image, blankImage},
			drive0:   "\n        interface: virtio",
			nics:     []coreNIC{{"LAN", "10.42.0.5"}},
		}.yaml(),
	)

	exp := coreExperiment(t, name, topo, "")
	qemu := coreQEMUBaseline(t)
	exp.start()

	booted := []string{"m1", "ns1", "md1"}
	exp.waitRunning(coreStartWait, booted...)

	head := headnode(t)

	t.Run("minimega runs exactly the bootable VMs", func(t *testing.T) {
		info := vmInfo(t, name)

		if got := slices.Sorted(maps.Keys(info)); !slices.Equal(got, slices.Sorted(slices.Values(booted))) {
			t.Errorf("minimega runs %v, want %v", got, booted)
		}

		coreWantCleanLaunch(t, name, len(booted))
	})

	t.Run("disks of the non-snapshot and multi-disk VMs", func(t *testing.T) {
		info := vmInfo(t, name)

		if disks := strings.Fields(info["ns1"]["disks"]); info["ns1"]["snapshot"] != "false" || len(disks) != 1 ||
			strings.Split(disks[0], ",")[0] != filepath.Join(cfg.imagesDir, ns1Image) {
			t.Errorf("ns1 snapshot %q disks %q, want false and %s", info["ns1"]["snapshot"], info["ns1"]["disks"], ns1Image)
		}

		disks := strings.Fields(info["md1"]["disks"])
		if len(disks) != 2 || !strings.HasPrefix(disks[0], coreSnapshot(head, name, "md1")+",virtio") ||
			strings.Split(disks[1], ",")[0] != filepath.Join(cfg.imagesDir, blankImage) {
			t.Errorf("md1 disks %q, want its snapshot on virtio then %s", info["md1"]["disks"], blankImage)
		}

		script := (&experiment{t: t, name: name}).mmScript()

		for _, absent := range []string{"vm launch kvm dnb1", "vm launch kvm ext1", "disk snapshot " + ns1Image} {
			if strings.Contains(script, absent) {
				t.Errorf("rendered script contains %q", absent)
			}
		}
	})

	t.Run("REST VM list", func(t *testing.T) { coreCheckMixedVMs(t, name, ns1Image) })

	t.Run("details of the external and do-not-boot nodes", func(t *testing.T) {
		var ext, dnb coreAPIVM

		apiOK(t, http.MethodGet, "/experiments/"+name+"/vms/ext1", nil, &ext)
		apiOK(t, http.MethodGet, "/experiments/"+name+"/vms/dnb1", nil, &dnb)

		if ext.State != coreStateExternal || !ext.External || ext.Running {
			t.Errorf("GET ext1 = %+v, want state EXTERNAL, external, not running", ext)
		}

		if !dnb.DNB || dnb.State != "" || dnb.Running {
			t.Errorf("GET dnb1 = %+v, want dnb, no state, not running", dnb)
		}

		if out := phenix(t, "vm", "info", name, "ext1"); !strings.Contains(out, "ext1") {
			t.Errorf("phenix vm info %s ext1 does not show ext1:\n%s", name, out)
		}
	})

	exp.stop()
	coreWantNoNamespace(t, name)
	coreWaitNoQEMU(t, qemu)
}

// coreCheckMixedVMs checks GET /experiments/{name}/vms of the mixed topology.
func coreCheckMixedVMs(t *testing.T, name, ns1Image string) {
	t.Helper()

	vms := coreVMList(t, name)
	if len(vms) != 5 {
		t.Errorf("REST lists %d VMs, want 5: %v", len(vms), slices.Sorted(maps.Keys(vms)))
	}

	if v := vms["dnb1"]; !v.DNB || v.State != "" || v.Running {
		t.Errorf("dnb1 dnb %v state %q running %v, want true, empty, false", v.DNB, v.State, v.Running)
	}

	if v := vms["ext1"]; !v.External || v.State != coreStateExternal || v.Running {
		t.Errorf("ext1 external %v state %q running %v, want true, EXTERNAL, false", v.External, v.State, v.Running)
	}

	if v := vms["ns1"]; v.Snapshot || v.State != coreStateRunning || v.Disk != filepath.Join(cfg.imagesDir, ns1Image) {
		t.Errorf("ns1 snapshot %v state %q disk %q, want false, RUNNING, %s", v.Snapshot, v.State, v.Disk, ns1Image)
	}

	for _, vm := range []string{"m1", "md1"} {
		if v := vms[vm]; !v.Snapshot || v.State != coreStateRunning || filepath.Base(v.Disk) != ext4Image {
			t.Errorf(
				"%s snapshot %v state %q disk %q, want true, RUNNING, the %s it boots from",
				vm,
				v.Snapshot,
				v.State,
				v.Disk,
				ext4Image,
			)
		}
	}

	var got coreAPIExperiment
	apiOK(t, http.MethodGet, "/experiments/"+name, nil, &got)
	t.Logf("GET /experiments/%s: vm_count %d, %d VMs listed", name, got.VMCount, len(got.VMs))
}

// ---------------------------------------------------------------------------
// F1-06

// TestCoreRestart restarts a running experiment (F1-06): the same VMs come
// back as new minimega VMs on fresh injection snapshots.
func TestCoreRestart(t *testing.T) {
	hollowImages(t)

	const name = "e2e-core-rst"

	exp := coreExperiment(t, name, coreTopology(), "")
	exp.start()
	exp.waitRunning(coreStartWait, coreVMNames()...)

	var (
		head   = headnode(t)
		before = vmUUIDs(t, name)
		mtimes = make(map[string]time.Time)
	)

	for _, vm := range coreVMNames() {
		st, err := os.Stat(coreSnapshot(head, name, vm))
		if err != nil {
			t.Fatalf("injection snapshot of %s: %v", vm, err)
		}

		mtimes[vm] = st.ModTime()
	}

	phenix(t, "experiment", "restart", name)
	exp.waitRunning(coreStartWait, coreVMNames()...)

	after := vmUUIDs(t, name)
	if got := slices.Sorted(maps.Keys(after)); !slices.Equal(got, slices.Sorted(maps.Keys(before))) {
		t.Errorf("VMs after restart = %v, before %v", got, slices.Sorted(maps.Keys(before)))
	}

	for vm, uuid := range before {
		if after[vm] == uuid {
			t.Errorf("%s kept UUID %s across the restart: it was not relaunched", vm, uuid)
		}

		st, err := os.Stat(coreSnapshot(head, name, vm))
		if err != nil || !st.ModTime().After(mtimes[vm]) {
			t.Errorf("injection snapshot of %s was not recreated by the restart (stat: %v)", vm, err)
		}
	}

	coreWantCleanLaunch(t, name, len(coreVMNames()))

	if exp.doc().Status.StartTime == "" {
		t.Error("status.startTime is empty after the restart")
	}
}

// ---------------------------------------------------------------------------
// F1-07

// TestCoreDelete checks what `phenix experiment delete` does to a running
// experiment, without and with --force, and what it leaves behind (F1-07).
func TestCoreDelete(t *testing.T) {
	hollowImages(t)

	const name = "e2e-core-del"

	exp := coreExperiment(t, name, coreTopology(), "")
	qemu := coreQEMUBaseline(t)
	exp.start()
	exp.waitRunning(coreStartWait, coreVMNames()...)

	var (
		head   = headnode(t)
		staged = filepath.Join(cfg.imagesDir, name)
		before = vmUUIDs(t, name)
	)

	if scripts := coreStagedScripts(staged, "h3"); len(scripts) == 0 {
		t.Errorf("phenix staged no startup scripts for h3's C2 delivery in %s", staged)
	}

	t.Run("without --force a running experiment is kept", func(t *testing.T) {
		out, err := phenixRun("experiment", "delete", name)
		t.Logf("phenix experiment delete of a running experiment: err %v", err)

		if !strings.Contains(out, "not deleting running experiment") {
			t.Errorf("phenix experiment delete of a running experiment did not say it kept it:\n%s", out)
		}

		if !exp.running() {
			t.Fatal("the experiment is gone or stopped after delete without --force")
		}

		if after := vmUUIDs(t, name); !maps.Equal(after, before) {
			t.Errorf("VMs changed: %v, before %v", after, before)
		}
	})

	phenix(t, "experiment", "delete", name, "--force")

	t.Run("with --force it is stopped and deleted", func(t *testing.T) {
		phenixFails(t, "config", "get", "experiment/"+name)

		coreWantNoNamespace(t, name)
		coreWaitNoQEMU(t, qemu)

		if _, err := os.Stat(filepath.Join(corePhenixBase, "experiments", name)); !os.IsNotExist(err) {
			t.Errorf("experiment base directory still exists (stat: %v)", err)
		}

		for _, vm := range coreVMNames() {
			if _, err := os.Stat(coreSnapshot(head, name, vm)); !os.IsNotExist(err) {
				t.Errorf("injection snapshot of %s still exists (stat: %v)", vm, err)
			}
		}

		if scripts := coreStagedScripts(staged, "h3"); len(scripts) > 0 {
			t.Errorf("startup scripts staged for h3 still exist in %s after delete: %v", staged, scripts)
		}
	})
}

// ---------------------------------------------------------------------------
// F1-08

// TestCoreCreateValidation checks the names and bridges phenix refuses when
// creating an experiment, through the CLI and REST (F1-08).
func TestCoreCreateValidation(t *testing.T) {
	hollowImages(t)

	const topo = "e2e-core-val"

	coreConfigs(t, topo, topology(hollowNode("v1", "LAN", "10.44.0.1", "")), "")

	t.Run("reserved name all", func(t *testing.T) {
		for _, name := range []string{"all", "ALL"} {
			// Never `experiment delete all`, which deletes every experiment.
			t.Cleanup(func() { _, _ = phenixRun("config", "delete", "experiment/"+name) })

			if out := phenixFails(t, "experiment", "create", name, "--topology", topo); !strings.Contains(out, "cannot use 'all'") {
				t.Errorf("experiment create %s failed without naming the reserved name:\n%s", name, out)
			}

			phenixFails(t, "config", "get", "experiment/"+name)
		}

		t.Cleanup(func() { _, _ = phenixRun("config", "delete", "experiment/All") })

		code, body := api(t, http.MethodPost, "/experiments", map[string]any{"name": "All", "topology": topo})
		if code != http.StatusBadRequest || !strings.Contains(string(body), "cannot use 'all'") {
			t.Errorf("POST /experiments named All = %d %s, want 400", code, body)
		}
	})

	t.Run("missing topology", func(t *testing.T) {
		const name = "e2e-core-notopo"

		adopt(t, name)

		if out := phenixFails(
			t,
			"experiment",
			"create",
			name,
			"--topology",
			"e2e-core-none",
		); !strings.Contains(
			out,
			"topology doesn't exist",
		) {
			t.Errorf("experiment create with a missing topology:\n%s", out)
		}

		code, body := api(t, http.MethodPost, "/experiments", map[string]any{"name": name, "topology": "e2e-core-none"})
		if code != http.StatusBadRequest || !strings.Contains(string(body), "topology doesn't exist") {
			t.Errorf("POST /experiments with a missing topology = %d %s, want 400", code, body)
		}
	})

	t.Run("auto bridge mode limits the name to 15 characters", func(t *testing.T) {
		const long, fits = "e2e-core-sixteen", "e2e-core-auto15"

		adopt(t, long)
		adopt(t, fits)

		if out := phenixFails(
			t,
			"--bridge-mode",
			"auto",
			"experiment",
			"create",
			long,
			"--topology",
			topo,
		); !strings.Contains(
			out,
			"must be 15 characters or less",
		) {
			t.Errorf("auto bridge mode accepted the 16-character name %s or failed for another reason:\n%s", long, out)
		}

		phenix(t, "--bridge-mode", "auto", "experiment", "create", fits, "--topology", topo)

		if bridge := (&experiment{t: t, name: fits}).doc().Spec["defaultBridge"]; bridge != fits {
			t.Errorf("auto bridge mode set the default bridge of %s to %v", fits, bridge)
		}
	})

	t.Run("default bridges", func(t *testing.T) {
		for _, name := range []string{"e2e-core-br1", "e2e-core-br2", "e2e-core-br3", "e2e-core-br4", "e2e-core-br5"} {
			adopt(t, name)
		}

		phenix(t, "experiment", "create", "e2e-core-br1", "--topology", topo, "--default-bridge", "e2ecorebr")

		if out := phenixFails(
			t,
			"experiment",
			"create",
			"e2e-core-br2",
			"--topology",
			topo,
			"--default-bridge",
			"e2ecorebr",
		); !strings.Contains(
			out,
			"already using default bridge e2ecorebr",
		) {
			t.Errorf("a second experiment on bridge e2ecorebr failed for another reason:\n%s", out)
		}

		// The default bridge, phenix, is shared by every experiment that keeps it.
		phenix(t, "experiment", "create", "e2e-core-br3", "--topology", topo)
		phenix(t, "experiment", "create", "e2e-core-br4", "--topology", topo)

		if out := phenixFails(
			t,
			"experiment",
			"create",
			"e2e-core-br5",
			"--topology",
			topo,
			"--default-bridge",
			"e2ecorebridge16x",
		); !strings.Contains(
			out,
			"must be 15 characters or less",
		) {
			t.Errorf("a 16-character default bridge failed for another reason:\n%s", out)
		}
	})
}

// ---------------------------------------------------------------------------
// F1-09

// TestCoreTeardown stops an experiment with minimega logging every command
// it receives, and checks that phenix clears the namespace and then leaves it
// alone, and that no VM, tap or snapshot survives (F1-09).
func TestCoreTeardown(t *testing.T) {
	hollowImages(t)

	const name = "e2e-core-tear"

	exp := coreExperiment(t, name, coreTopology(), "")
	qemu := coreQEMUBaseline(t)
	exp.start()
	exp.waitRunning(coreStartWait, coreVMNames()...)

	var (
		head = headnode(t)
		taps = coreTaps(t, name)
	)

	lines := coreMMDebugLog(t, func() { exp.stop() })

	var (
		clearNS   = regexp.MustCompile(`\bclear namespace ` + regexp.QuoteMeta(name) + `\b`)
		namespace = regexp.MustCompile(`\bnamespace "` + regexp.QuoteMeta(name) + `"`)
		clears    int
		after     []string
	)

	for _, line := range lines {
		m := coreRequestLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}

		switch {
		case clearNS.MatchString(m[1]):
			clears++
			after = after[:0]
		case clears > 0 && namespace.MatchString(m[1]):
			after = append(after, m[1])
		}
	}

	t.Logf("phenix sent `clear namespace %s` %d times during the stop", name, clears)

	if clears == 0 {
		t.Errorf("minimega received no `clear namespace %s` during the stop (%d log lines)", name, len(lines))
	}

	if len(after) > 0 {
		t.Errorf("after clearing the namespace phenix sent commands that re-create it: %q", after)
	}

	coreWantNoNamespace(t, name)
	coreWaitNoQEMU(t, qemu)
	wantTapsGone(t, taps)

	for _, vm := range coreVMNames() {
		if _, err := os.Stat(coreSnapshot(head, name, vm)); !os.IsNotExist(err) {
			t.Errorf("injection snapshot of %s still exists after stop (stat: %v)", vm, err)
		}
	}

	if st := exp.doc().Status.StartTime; st != "" {
		t.Errorf("status.startTime = %q after stop, want empty", st)
	}
}

// ---------------------------------------------------------------------------
// F1-12

// TestCoreLaunchProgress checks phenix's launch progress (what the UI shows
// while an experiment starts) against the queue minimega reports (F1-12).
// Nothing is launched: the namespace only queues VMs.
func TestCoreLaunchProgress(t *testing.T) {
	const ns = "e2e-core-q"

	coreMMWarm(t)

	before := qemuProcesses(t)

	// minimega creates <files dir>/<namespace>/miniccc_responses for the
	// namespace and leaves it behind when the namespace is cleared.
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Join(cfg.imagesDir, ns)) })
	t.Cleanup(func() { _, _ = mmRun("", "clear namespace "+ns) })

	mm(t, ns, "ns queueing true")

	for _, vm := range []string{"q1", "q2", "q3"} {
		mm(t, ns, "vm launch kvm "+vm)
	}

	t.Run("one VM per launch, as phenix renders its scripts", func(t *testing.T) {
		queue := mmText(t, ns, "ns queue")

		for _, vm := range []string{"q1", "q2", "q3"} {
			if !regexp.MustCompile(`(?m)^Names: ` + vm + `$`).MatchString(queue) {
				t.Errorf("ns queue does not list %s on its own:\n%s", vm, queue)
			}
		}

		if p, err := phenixmm.GetLaunchProgress(ns, 3); err != nil || math.Abs(p-1) > 1e-9 {
			t.Errorf("mm.GetLaunchProgress = %v (%v) with 3 of 3 VMs queued, want 1", p, err)
		}
	})

	mm(t, ns, "vm launch kvm r[1-3]")

	t.Run("several VMs per launch", func(t *testing.T) {
		queue := mmText(t, ns, "ns queue")

		// minimega: ns queue prints the names of one launch compressed into a range (ranges.UnsplitList).
		if !strings.Contains(queue, "Names: r[1-3]") {
			t.Errorf("ns queue does not list r[1-3]:\n%s", queue)
		}

		if p, err := phenixmm.GetLaunchProgress(ns, 6); err != nil || math.Abs(p-1) > 1e-9 {
			t.Errorf("mm.GetLaunchProgress = %v (%v) with 6 of 6 VMs queued, want 1", p, err)
		}
	})

	mm(t, "", "clear namespace "+ns)
	coreWantNoNamespace(t, ns)

	// phenix ui polls the progress before a start reads its minimega script
	// and after a failed start cleared the namespace; the poll must not
	// create it.
	t.Run("a namespace minimega does not list", func(t *testing.T) {
		if p, err := phenixmm.GetLaunchProgress(ns, 3); err != nil || p != 0 {
			t.Errorf("mm.GetLaunchProgress = %v (%v) for the cleared namespace, want 0", p, err)
		}

		coreWantNoNamespace(t, ns)
	})

	if n := qemuProcesses(t); n != before {
		t.Errorf("%d QEMU processes after only queueing VMs, %d before", n, before)
	}
}
