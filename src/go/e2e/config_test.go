//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Config shard: what an experiment's settings do once minimega runs it: VLAN
// ranges and aliases, default bridges and the bridge modes, deploy modes, GRE
// on a single node and reconfigure (F13), dry-run isolation (F15), and phenix
// workflow apply through the CLI and REST (F16). Every experiment runs hollow
// VMs.

const (
	// configStartWait bounds how long hollow VMs take to reach RUNNING.
	configStartWait = 2 * time.Minute

	// configTeardownWait bounds how long taps and QEMU processes take to go
	// away; minimega reaps taps asynchronously every second.
	configTeardownWait = 30 * time.Second

	// configPoll is how often the tests poll minimega and the host.
	configPoll = 500 * time.Millisecond

	// configWatchPoll is how often a namespace watch polls minimega.
	configWatchPoll = 50 * time.Millisecond

	// configMemory is the memory of every hollow node, in MB.
	configMemory = 256

	// configDryRunSuffix ends the start time of a dry-run experiment.
	configDryRunSuffix = "-DRYRUN"

	// configPhenixBridge is the default bridge experiments share.
	configPhenixBridge = "phenix"

	// configPhenixBase is phenix's base directory (--base-dir.phenix) in the
	// CLI and the UI the harness starts.
	configPhenixBase = "/phenix"

	// configInjectsBase is where workflow apply stages injects: the injects
	// directory the phenix ui the harness starts reports.
	configInjectsBase = configPhenixBase + "/injects"

	// configMIMEYAML is the content type phenix's workflow endpoints take
	// YAML bodies with.
	configMIMEYAML = "application/x-yaml"

	// configDefaultVLANMin and configDefaultVLANMax bound minimega's default
	// VLAN pool, [101, 4096).
	configDefaultVLANMin = 101
	configDefaultVLANMax = 4096
)

var configVLANEntry = regexp.MustCompile(`^(.+) \((\d+)\)$`)

// ---------------------------------------------------------------------------
// topologies

// configNIC is one static interface of a node. bridge is set only when the
// topology pins the interface to a bridge.
type configNIC struct {
	vlan, address, bridge string
}

// configNode describes a hollow VirtualMachine node.
type configNode struct {
	hostname string
	osType   string // other when empty
	image    string // blankImage when empty
	memory   int    // configMemory when 0
	drive    string // extra YAML under the drive (indent 8)
	extra    string // extra YAML under the node (indent 4)
	nics     []configNIC
}

// yaml renders the node as an entry of a topology's nodes list.
func (n configNode) yaml() string {
	osType, image, memory := n.osType, n.image, n.memory

	if osType == "" {
		osType = "other"
	}

	if image == "" {
		image = blankImage
	}

	if memory == 0 {
		memory = configMemory
	}

	var b strings.Builder

	fmt.Fprintf(&b, "\n  - type: VirtualMachine\n    general:\n      hostname: %s", n.hostname)
	fmt.Fprintf(&b, "\n    hardware:\n      vcpus: 1\n      memory: %d\n      os_type: %s", memory, osType)
	fmt.Fprintf(&b, "\n      drives:\n      - image: %s%s", image, n.drive)
	b.WriteString("\n    network:\n      interfaces:")

	for i, nic := range n.nics {
		fmt.Fprintf(&b, "\n      - name: eth%d\n        type: ethernet\n        proto: static", i)
		fmt.Fprintf(&b, "\n        vlan: %s\n        address: %s\n        mask: 24", nic.vlan, nic.address)

		if nic.bridge != "" {
			fmt.Fprintf(&b, "\n        bridge: %s", nic.bridge)
		}
	}

	b.WriteString(n.extra)

	return b.String()
}

// configInjection is the node YAML (indent 4) for one file injection.
func configInjection(src, dst string) string {
	return fmt.Sprintf("\n    injections:\n    - src: %q\n      dst: %s", src, dst)
}

// configSwitchVMs are the nodes of configSwitchTopology.
func configSwitchVMs() []string {
	return []string{"v1", "v2", "v3"}
}

// configSwitchAliases are the VLANs of configSwitchTopology, one per node.
func configSwitchAliases() []string {
	return []string{"SW1", "SW2", "SW3"}
}

// configSwitchTopology is three hollow nodes of OS type other, each alone on
// its own VLAN: v1 on SW1, v2 on SW2 and v3 on SW3, with addresses in subnet
// (the first three octets).
func configSwitchTopology(subnet string) string {
	var (
		vms     = configSwitchVMs()
		aliases = configSwitchAliases()
		nodes   = make([]string, 0, len(vms))
	)

	for i, vm := range vms {
		nic := configNIC{vlan: aliases[i], address: fmt.Sprintf("%s.%d", subnet, i+1)}
		nodes = append(nodes, configNode{hostname: vm, nics: []configNIC{nic}}.yaml())
	}

	return topology(nodes...)
}

// ---------------------------------------------------------------------------
// experiments

// configCreateTopology creates a topology config named name from YAML that
// may use {{name}}, and deletes it when the test ends.
func configCreateTopology(t *testing.T, name, topo string) {
	t.Helper()

	topo = strings.ReplaceAll(topo, "{{name}}", name)
	phenix(t, "config", "create", writeFile(t, name+"-topology.yaml", topo))

	configDeleteConfig(t, "topology/"+name)
}

// configDeleteConfig deletes a config when the test ends.
func configDeleteConfig(t *testing.T, ref string) {
	t.Helper()

	t.Cleanup(func() { _, _ = phenixRun("config", "delete", ref) })
}

// configDoc is the part of a stored experiment config these tests read.
type configDoc struct {
	Metadata struct {
		Annotations map[string]string `json:"annotations"`
	} `json:"metadata"`
	Spec struct {
		DefaultBridge string `json:"defaultBridge"`
		DeployMode    string `json:"deployMode"`
		UseGREMesh    bool   `json:"useGREMesh"`
		VLANs         struct {
			Aliases map[string]int `json:"aliases"`
			Min     int            `json:"min"`
			Max     int            `json:"max"`
		} `json:"vlans"`
	} `json:"spec"`
	Status struct {
		StartTime string         `json:"startTime"`
		VLANs     map[string]int `json:"vlans"`
	} `json:"status"`
}

// configGetDoc returns experiment name as phenix stores it.
func configGetDoc(t *testing.T, name string) configDoc {
	t.Helper()

	out := phenix(t, "config", "get", "experiment/"+name, "--output", "json")

	var doc configDoc
	if err := json.Unmarshal([]byte(jsonPart(out)), &doc); err != nil {
		t.Fatalf("decoding experiment %s: %v\n%s", name, err, out)
	}

	return doc
}

// configStart starts experiment name with the CLI and returns the output.
func configStart(t *testing.T, name string, args ...string) string {
	t.Helper()

	return phenix(t, append([]string{"experiment", "start", name}, args...)...)
}

// configStop stops experiment name with the CLI.
func configStop(t *testing.T, name string) {
	t.Helper()

	phenix(t, "experiment", "stop", name)
}

// configWaitRunning waits until minimega reports every named VM of namespace
// ns RUNNING.
func configWaitRunning(t *testing.T, ns string, vms ...string) {
	t.Helper()

	(&experiment{t: t, name: ns}).waitRunning(configStartWait, vms...)
}

// ---------------------------------------------------------------------------
// rendered minimega scripts

// configScript returns the minimega script phenix rendered for experiment
// name.
func configScript(t *testing.T, name string) string {
	t.Helper()

	return (&experiment{t: t, name: name}).mmScript()
}

// configScriptPath is where phenix renders a minimega script of experiment
// name: suffix .mm for the experiment, -cc.mm for its C2 commands.
func configScriptPath(name, suffix string) string {
	return filepath.Join(configPhenixBase, "experiments", name, "mm_files", name+suffix)
}

// configLines returns the non-empty lines of a script, trimmed.
func configLines(script string) []string {
	var lines []string

	for line := range strings.SplitSeq(script, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}

	return lines
}

// configLinesWithPrefix returns the lines of a script that start with prefix.
func configLinesWithPrefix(script, prefix string) []string {
	var lines []string

	for _, line := range configLines(script) {
		if strings.HasPrefix(line, prefix) {
			lines = append(lines, line)
		}
	}

	return lines
}

// configHasLine reports whether a script holds line.
func configHasLine(script, line string) bool {
	return slices.Contains(configLines(script), line)
}

// configBlock returns the lines phenix rendered for VM vm, from its
// `## VM: <vm> ##` header to the next one.
func configBlock(script, vm string) []string {
	var (
		header = "## VM: " + vm + " ##"
		block  []string
		in     bool
	)

	for _, line := range configLines(script) {
		switch {
		case line == header:
			in = true
		case strings.HasPrefix(line, "## VM: "):
			in = false
		case in:
			block = append(block, line)
		}
	}

	return block
}

// configWantInOrder checks that every line of want appears in lines, in
// that order.
func configWantInOrder(t *testing.T, what string, lines, want []string) {
	t.Helper()

	next := 0

	for _, line := range lines {
		if next < len(want) && line == want[next] {
			next++
		}
	}

	if next < len(want) {
		t.Errorf("%s lacks %q (in this order, after %q):\n%s", what, want[next], want[:next], strings.Join(lines, "\n"))
	}
}

// configSnapshotOf returns the path of the injection snapshot phenix asked
// minimega to create for VM vm, from its `disk snapshot` line.
func configSnapshotOf(t *testing.T, script, vm string) string {
	t.Helper()

	for _, line := range configBlock(script, vm) {
		// disk snapshot <image> <snapshot>
		if rest, ok := strings.CutPrefix(line, "disk snapshot "); ok {
			fields := strings.Fields(rest)

			return filepath.Join(cfg.imagesDir, fields[len(fields)-1])
		}
	}

	t.Fatalf("the rendered script has no disk snapshot line for %s", vm)

	return ""
}

// configSnapshots returns the injection snapshots of experiment name that
// exist in minimega's files directory.
func configSnapshots(name string) []string {
	matches, _ := filepath.Glob(filepath.Join(cfg.imagesDir, "*_"+name+"_*_snapshot"))

	return matches
}

// ---------------------------------------------------------------------------
// minimega and host oracles

// configVLANRange returns the VLAN range minimega allocates from in namespace
// ns, or in its default pool when ns is empty, as [min, max).
func configVLANRange(t *testing.T, ns string) [2]int {
	t.Helper()

	rows := mmTable(t, ns, "vlans range")
	if len(rows) != 1 {
		t.Fatalf("minimega lists %d VLAN ranges in namespace %q, want 1: %v", len(rows), ns, rows)
	}

	lo, err := strconv.Atoi(rows[0]["min"])
	if err != nil {
		t.Fatalf("vlans range row %v: %v", rows[0], err)
	}

	hi, err := strconv.Atoi(rows[0]["max"])
	if err != nil {
		t.Fatalf("vlans range row %v: %v", rows[0], err)
	}

	return [2]int{lo, hi}
}

// configRenderedRange checks that the one `vlans range` line of a script
// passes the experiment range lo-hi to minimega unchanged, so minimega
// allocates from [lo, hi), and returns its ends.
func configRenderedRange(t *testing.T, script string, lo, hi int) [2]int {
	t.Helper()

	lines := configLinesWithPrefix(script, "vlans range ")
	if len(lines) != 1 {
		t.Fatalf("the rendered script has VLAN range lines %q, want one", lines)
	}

	var got [2]int

	if _, err := fmt.Sscanf(lines[0], "vlans range %d %d", &got[0], &got[1]); err != nil {
		t.Fatalf("parsing %q: %v", lines[0], err)
	}

	if got != [2]int{lo, hi} {
		t.Errorf("the rendered script has %q for the range %d-%d, want \"vlans range %d %d\"", lines[0], lo, hi, lo, hi)
	}

	return got
}

// configWantVLANs checks that minimega gave each alias a VLAN of its own
// from lo up to, but not including, hi.
func configWantVLANs(t *testing.T, vlans map[string]int, aliases []string, lo, hi int) {
	t.Helper()

	owner := make(map[int]string)

	for _, alias := range aliases {
		id, ok := vlans[alias]
		if !ok {
			t.Errorf("minimega holds no VLAN for %s: %v", alias, vlans)

			continue
		}

		if id < lo || id >= hi {
			t.Errorf("%s is on VLAN %d, outside %d-%d", alias, id, lo, hi-1)
		}

		if other, taken := owner[id]; taken {
			t.Errorf("%s and %s share VLAN %d", alias, other, id)
		}

		owner[id] = alias
	}
}

// configWantLaunched checks minimega's own count of the last launch in
// namespace ns: want VMs launched and none failed.
func configWantLaunched(t *testing.T, ns string, want int) {
	t.Helper()

	rows := mmTable(t, ns, "ns schedule status")
	if len(rows) == 0 {
		t.Fatalf("minimega reports no launch in namespace %s", ns)
	}

	if st := rows[len(rows)-1]; st["failures"] != "0" || st["launched"] != strconv.Itoa(want) {
		t.Errorf("ns schedule status in %s = %v, want %d launched and 0 failures", ns, st, want)
	}
}

// configWantNoNamespace checks that minimega does not hold namespace ns.
func configWantNoNamespace(t *testing.T, ns string) {
	t.Helper()

	if row, ok := mmNamespaces(t)[ns]; ok {
		t.Errorf("minimega holds namespace %s: %v", ns, row)
	}
}

// configNet is one NIC of a VM as minimega's vm info reports it.
type configNet struct {
	alias  string
	vlan   int
	tap    string
	bridge string
}

// configNets returns the NICs of every VM minimega reports in namespace ns.
func configNets(t *testing.T, ns string) map[string][]configNet {
	t.Helper()

	nets := make(map[string][]configNet)

	for name, row := range vmInfo(t, ns) {
		var (
			vlans   = splitList(row["vlan"])
			taps    = splitList(row["tap"])
			bridges = splitList(row["bridge"])
		)

		if len(taps) != len(vlans) || len(bridges) != len(vlans) {
			t.Fatalf("%s: vm info lists VLANs %q, taps %q and bridges %q", name, row["vlan"], row["tap"], row["bridge"])
		}

		for i, entry := range vlans {
			m := configVLANEntry.FindStringSubmatch(entry)
			if m == nil {
				t.Fatalf("%s: network %q is not <alias> (<vlan>)", name, entry)
			}

			id, _ := strconv.Atoi(m[2])

			nets[name] = append(nets[name], configNet{alias: m[1], vlan: id, tap: taps[i], bridge: bridges[i]})
		}
	}

	return nets
}

// configTaps returns the taps of nets.
func configTaps(nets map[string][]configNet) []string {
	var taps []string

	for _, nics := range nets {
		for _, nic := range nics {
			taps = append(taps, nic.tap)
		}
	}

	return taps
}

// configWantNets checks every NIC minimega reports in namespace ns against
// minimega's VLAN table and Open vSwitch: the NIC is on the VLAN minimega
// holds for its alias, and its tap is on the expected bridge (bridges[vm], or
// bridges[""] for VMs not listed), tagged with that VLAN.
func configWantNets(t *testing.T, ns string, bridges map[string]string, vlans map[string]int) {
	t.Helper()

	nets := configNets(t, ns)
	if len(nets) == 0 {
		t.Fatalf("minimega reports no VMs in namespace %s", ns)
	}

	for vm, nics := range nets {
		want, ok := bridges[vm]
		if !ok {
			want = bridges[""]
		}

		for _, nic := range nics {
			if id, ok := vlans[nic.alias]; !ok || id != nic.vlan {
				t.Errorf("%s: vm info puts %s on VLAN %d, minimega's vlans holds %v", vm, nic.alias, nic.vlan, vlans)
			}

			if nic.bridge != want {
				t.Errorf("%s: vm info puts tap %s on bridge %q, want %q", vm, nic.tap, nic.bridge, want)
			}

			if br, err := ovsVsctl(t, "port-to-br", nic.tap); err != nil || br != want {
				t.Errorf("%s: Open vSwitch has tap %s on bridge %q (%v), want %q", vm, nic.tap, br, err, want)
			}

			if tag, err := ovsVsctl(t, "get", "port", nic.tap, "tag"); err != nil || tag != strconv.Itoa(nic.vlan) {
				t.Errorf("%s: Open vSwitch tags tap %s with %q (%v), want %d", vm, nic.tap, tag, err, nic.vlan)
			}
		}
	}
}

// configGREInterfaces returns the GRE interfaces Open vSwitch holds.
func configGREInterfaces(t *testing.T) []string {
	t.Helper()

	out, err := ovsVsctl(t, "--bare", "--columns=name", "find", "interface", "type=gre")
	if err != nil {
		t.Fatalf("listing GRE interfaces: %v\n%s", err, out)
	}

	return strings.Fields(out)
}

// configDestroyBridge removes, when the test ends, an Open vSwitch bridge
// minimega created for the test's experiments: minimega keeps a bridge until
// its daemon exits. Register it before the experiment, so it runs after the
// experiment's teardown.
func configDestroyBridge(t *testing.T, bridge string) {
	t.Helper()

	t.Cleanup(func() {
		// minimega's own command keeps its bridge table in step; Open vSwitch
		// covers a bridge minimega does not know.
		_, _ = mmRun("", "bridge destroy "+bridge)

		_ = exec.Command("ovs-vsctl", "--if-exists", "del-br", bridge).Run()
	})
}

// configQEMUPids returns the PIDs of the QEMU processes of minimega's VMs, as
// qemuProcesses counts them, or none when /proc cannot be read: the namespace
// watch calls it from a goroutine, which cannot fail the test.
func configQEMUPids() map[string]bool {
	pids, err := qemuPids()
	if err != nil {
		return map[string]bool{}
	}

	return pids
}

// configNewPids returns the QEMU processes running now that were not in
// before.
func configNewPids(before map[string]bool) []string {
	var fresh []string

	for pid := range configQEMUPids() {
		if !before[pid] {
			fresh = append(fresh, pid)
		}
	}

	slices.Sort(fresh)

	return fresh
}

// configSeen is what a namespace watch saw.
type configSeen struct {
	namespace bool     // minimega listed the namespace at least once
	qemu      []string // QEMU processes that were not running when the watch began
	polls     int      // successful polls of minimega's namespace list
	err       error    // the last error talking to minimega
}

// configWatch polls minimega's namespace list and the host's QEMU processes
// while a command runs, without entering (and so creating) the namespace it
// watches. It never touches a [testing.T], so it can run in its own
// goroutine.
type configWatch struct {
	ns     string
	before map[string]bool
	done   chan struct{}
	result chan configSeen
	once   sync.Once
	seen   configSeen
}

// configStartWatch starts watching for namespace ns and for new QEMU
// processes. The watch ends when stop is called, or else when the test ends.
func configStartWatch(t *testing.T, ns string) *configWatch {
	t.Helper()

	w := &configWatch{ns: ns, before: configQEMUPids(), done: make(chan struct{}), result: make(chan configSeen, 1)}

	go w.run()

	t.Cleanup(func() { w.stop() })

	return w
}

// stop ends the watch after one last poll and returns what it saw. Later
// calls return the same.
func (w *configWatch) stop() configSeen {
	w.once.Do(func() {
		close(w.done)

		w.seen = <-w.result
	})

	return w.seen
}

func (w *configWatch) run() {
	var (
		seen  configSeen
		fresh = make(map[string]bool)
	)

	poll := func() {
		resp, err := mmRun("", "namespace")
		if err != nil {
			seen.err = err
		} else {
			seen.polls++

			for _, row := range mmRows(resp) {
				if row["namespace"] == w.ns {
					seen.namespace = true
				}
			}
		}

		for pid := range configQEMUPids() {
			if !w.before[pid] {
				fresh[pid] = true
			}
		}
	}

	for {
		poll()

		select {
		case <-w.done:
			poll()

			seen.qemu = slices.Sorted(maps.Keys(fresh))
			w.result <- seen

			return
		case <-time.After(configWatchPoll):
		}
	}
}

// configWantUntouched checks that a watch saw neither namespace ns nor a new
// QEMU process while what ran.
func configWantUntouched(t *testing.T, ns, what string, seen configSeen) {
	t.Helper()

	if seen.polls == 0 {
		t.Fatalf("the namespace watch never reached minimega during %s: %v", what, seen.err)
	}

	if seen.namespace {
		t.Errorf("minimega held namespace %s during or after %s", ns, what)
	}

	if len(seen.qemu) > 0 {
		t.Errorf("QEMU processes %v started during %s", seen.qemu, what)
	}
}

// ---------------------------------------------------------------------------
// REST

// configAPIYAML calls the phenix REST API with a YAML body.
func configAPIYAML(t *testing.T, method, path, body string) (int, []byte) {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), method, "http://"+cfg.uiAddr+"/api/v1"+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}

	req.Header.Set("Content-Type", configMIMEYAML)

	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s %s: reading body: %v", method, path, err)
	}

	return resp.StatusCode, data
}

// configPlan is the body of a workflow apply response.
type configPlan struct {
	Action     string `json:"action"`
	Experiment string `json:"experiment"`
	Reason     string `json:"reason"`
	DryRun     bool   `json:"dryRun"`
}

// configApply posts a workflow config to POST /workflow/apply/{branch} with
// the query query, fails the test unless the status is want, and decodes a
// successful response.
func configApply(t *testing.T, branch, query, body string, want int) (configPlan, []byte) {
	t.Helper()

	path := "/workflow/apply/" + branch
	if query != "" {
		path += "?" + query
	}

	code, data := configAPIYAML(t, http.MethodPost, path, body)
	if code != want {
		t.Fatalf("POST %s = %d %s, want %d", path, code, data, want)
	}

	var plan configPlan

	if code == http.StatusOK {
		if err := json.Unmarshal(data, &plan); err != nil {
			t.Fatalf("POST %s: decoding %s: %v", path, data, err)
		}
	}

	return plan, data
}

// ---------------------------------------------------------------------------
// user apps

// configInstallEnvApp installs, as a regular file on the PATH of the CLI and
// the UI the harness started, a user app that records the PHENIX_ variables
// phenix gives each stage in <dir>/<stage>.env. It prints nothing, which
// tells phenix it made no change.
func configInstallEnvApp(t *testing.T, name, dir string) {
	t.Helper()

	// The work directory outlives local runs; what an earlier run recorded
	// must not stand in for a stage this run skipped.
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}

	var (
		path   = filepath.Join("/usr/local/bin", "phenix-app-"+name)
		script = "#!/bin/sh\n# Installed by the phenix E2E tests (" + t.Name() + ").\n" +
			"cat >/dev/null\nenv | grep '^PHENIX_' >'" + dir + "/'\"$1\".env\nexit 0\n"
	)

	if err := os.WriteFile(path, []byte(script), 0o755); err != nil { //nolint:gosec // an executable user app
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = os.Remove(path) })

	if found, err := exec.LookPath(filepath.Base(path)); err != nil || found != path {
		t.Fatalf("user app %s is not first on PATH (found %q, %v)", path, found, err)
	}
}

// configAppEnv returns the PHENIX_ variables the env app recorded for stage.
func configAppEnv(t *testing.T, dir, stage string) map[string]string {
	t.Helper()

	data, err := os.ReadFile(filepath.Join(dir, stage+".env")) //nolint:gosec // test harness
	if err != nil {
		t.Fatalf("the user app recorded nothing for the %s stage: %v", stage, err)
	}

	vars := make(map[string]string)

	for line := range strings.SplitSeq(strings.TrimSpace(string(data)), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			vars[k] = v
		}
	}

	return vars
}

// ---------------------------------------------------------------------------
// F13-01, F13-02, F13-03, F13-05

// TestConfigVLANRange checks experiment VLAN ranges against minimega's VLAN
// table and Open vSwitch: the range is set in the experiment's namespace,
// every VLAN comes from it, from the min up to, but not including, the max as
// documented, a range that cannot hold the topology fails the start, and a
// range with one end set, an inverted one or one minimega would refuse is
// refused before anything is stored (F13-01, F13-02, F13-03, F13-05).
func TestConfigVLANRange(t *testing.T) {
	hollowImages(t)

	t.Run("range", func(t *testing.T) {
		const (
			name   = "e2e-cfg-range"
			lo, hi = 300, 305
		)

		newExperiment(t, name, configSwitchTopology("10.50.0"), "", "--vlan-min", strconv.Itoa(lo), "--vlan-max", strconv.Itoa(hi))

		if doc := configGetDoc(t, name); doc.Spec.VLANs.Min != lo || doc.Spec.VLANs.Max != hi {
			t.Errorf("spec.vlans range = %d-%d, want %d-%d", doc.Spec.VLANs.Min, doc.Spec.VLANs.Max, lo, hi)
		}

		configStart(t, name)
		configWaitRunning(t, name, configSwitchVMs()...)
		configWantLaunched(t, name, len(configSwitchVMs()))

		script := configScript(t, name)
		rendered := configRenderedRange(t, script, lo, hi)

		if pinned := configLinesWithPrefix(script, "vlans add "); len(pinned) > 0 {
			t.Errorf("the rendered script pins VLANs no alias asked for: %q", pinned)
		}

		vlans := rawVLANs(t, name)

		configWantVLANs(t, vlans, configSwitchAliases(), lo, hi)

		if st := configGetDoc(t, name).Status.VLANs; !maps.Equal(st, vlans) {
			t.Errorf("status.vlans = %v, minimega holds %v", st, vlans)
		}

		configWantNets(t, name, map[string]string{"": configPhenixBridge}, vlans)

		// The range belongs to the experiment's namespace; minimega's
		// default pool, used by every other namespace, is unchanged.
		if got := configVLANRange(t, name); got != rendered {
			t.Errorf("namespace %s allocates from %v, want the rendered [%d %d)", name, got, rendered[0], rendered[1])
		}

		if got := configVLANRange(t, ""); got != [2]int{configDefaultVLANMin, configDefaultVLANMax} {
			t.Errorf("minimega's default pool is %v, want [%d %d)", got, configDefaultVLANMin, configDefaultVLANMax)
		}

		taps := configTaps(configNets(t, name))

		configStop(t, name)
		configWantNoNamespace(t, name)
		wantTapsGone(t, taps)
	})

	t.Run("minimega assigns from the minimum up to the maximum", func(t *testing.T) {
		const (
			name   = "e2e-cfg-vmax"
			lo, hi = 310, 313
		)

		newExperiment(t, name, configSwitchTopology("10.50.1"), "", "--vlan-min", strconv.Itoa(lo), "--vlan-max", strconv.Itoa(hi))

		// docs/content/workflow.md: minimega assigns "from min up to, but not
		// including, max", so 310-313 holds three VLANs, 310 to 312.
		out, err := phenixRun("experiment", "start", name)
		if err != nil {
			t.Fatalf("three VLANs do not fit the range %d-%d: %v\n%s", lo, hi, err, out)
		}

		configWaitRunning(t, name, configSwitchVMs()...)
		configWantVLANs(t, rawVLANs(t, name), configSwitchAliases(), lo, hi)
	})

	t.Run("a pool too small for the topology", func(t *testing.T) {
		const (
			name   = "e2e-cfg-pool"
			lo, hi = 345, 347
		)

		// Two VLANs, 345 and 346, for three aliases: minimega never assigns
		// the max, 347.
		newExperiment(t, name, configSwitchTopology("10.50.3"), "", "--vlan-min", strconv.Itoa(lo), "--vlan-max", strconv.Itoa(hi))

		before := configQEMUPids()

		if out := phenixFails(t, "experiment", "start", name); !strings.Contains(out, "out of VLANs") {
			t.Errorf("the failed start does not say minimega ran out of VLANs:\n%s", out)
		}

		configWantNoNamespace(t, name)

		if st := configGetDoc(t, name).Status.StartTime; st != "" {
			t.Errorf("status.startTime = %q after a failed start, want empty", st)
		}

		if fresh := configNewPids(before); len(fresh) > 0 {
			t.Errorf("QEMU processes %v were left by the failed start", fresh)
		}
	})

	t.Run("a range with one end set", func(t *testing.T) {
		const name = "e2e-cfg-vone"

		configCreateTopology(t, name, configSwitchTopology("10.50.2"))
		adopt(t, name)

		// The minimega script renders a range only when both ends are set, so
		// one end alone would be ignored.
		for _, c := range []struct {
			flags []string
			want  string
		}{
			{flags: []string{"--vlan-min", "320"}, want: "max VLAN ID is unset while min VLAN ID is 320"},
			{flags: []string{"--vlan-max", "330"}, want: "min VLAN ID is unset while max VLAN ID is 330"},
		} {
			args := append([]string{"experiment", "create", name, "--topology", name}, c.flags...)

			if out := phenixFails(t, args...); !strings.Contains(out, c.want) {
				t.Errorf("phenix %s was refused for another reason, want %q:\n%s", strings.Join(args, " "), c.want, out)
			}

			phenixFails(t, "config", "get", "experiment/"+name)
		}

		// A start refuses one end too, before minimega hears of the experiment.
		phenix(t, "experiment", "create", name, "--topology", name)

		if out := phenixFails(t, "experiment", "start", name, "--vlan-min", "320"); !strings.Contains(
			out, "max VLAN ID is unset while min VLAN ID is 320",
		) {
			t.Errorf("the start with only --vlan-min failed for another reason:\n%s", out)
		}

		configWantNoNamespace(t, name)

		if doc := configGetDoc(t, name); doc.Status.StartTime != "" || doc.Spec.VLANs.Min != 0 || doc.Spec.VLANs.Max != 0 {
			t.Errorf("status.startTime %q, spec.vlans range %d-%d after the refused start, want none",
				doc.Status.StartTime, doc.Spec.VLANs.Min, doc.Spec.VLANs.Max)
		}
	})

	t.Run("an inverted range", func(t *testing.T) {
		const (
			name   = "e2e-cfg-vinv"
			lo, hi = 360, 350
		)

		configCreateTopology(t, name, configSwitchTopology("10.50.4"))
		adopt(t, name)

		if out := phenixFails(t, "experiment", "create", name, "--topology", name,
			"--vlan-min", strconv.Itoa(lo), "--vlan-max", strconv.Itoa(hi)); !strings.Contains(out, "must be less than max") {
			t.Errorf("experiment create refused the VLAN range %d-%d for another reason:\n%s", lo, hi, out)
		}

		phenixFails(t, "config", "get", "experiment/"+name)

		if out := phenixFails(t, "vlan", "range", name, strconv.Itoa(lo), strconv.Itoa(hi)); !strings.Contains(
			out, "must be less than max",
		) {
			t.Errorf("phenix vlan range refused the inverted range for another reason:\n%s", out)
		}
	})

	t.Run("ranges minimega would refuse", func(t *testing.T) {
		const name = "e2e-cfg-vbad"

		configCreateTopology(t, name, configSwitchTopology("10.50.5"))
		adopt(t, name)

		// minimega takes VLAN IDs below 4096, so a range's max is 4096 at most,
		// and refuses a range whose max is not above its min.
		for _, c := range []struct {
			flags []string
			want  string
		}{
			{flags: []string{"--vlan-min", "4096", "--vlan-max", "4096"}, want: "min VLAN ID 4096 is outside 1-4095"},
			{flags: []string{"--vlan-min", "300", "--vlan-max", "4097"}, want: "max VLAN ID 4097 is outside 2-4096"},
			{flags: []string{"--vlan-min", "350", "--vlan-max", "350"}, want: "min VLAN ID must be less than max VLAN ID"},
		} {
			args := append([]string{"experiment", "create", name, "--topology", name}, c.flags...)

			if out := phenixFails(t, args...); !strings.Contains(out, c.want) {
				t.Errorf("phenix %s was refused for another reason, want %q:\n%s", strings.Join(args, " "), c.want, out)
			}

			phenixFails(t, "config", "get", "experiment/"+name)
		}
	})
}

// ---------------------------------------------------------------------------
// F13-04

// TestConfigVLANAlias pins a topology VLAN to an ID with phenix vlan alias
// and follows it into minimega and Open vSwitch, then checks that minimega
// refuses the same VLAN to a second experiment until the first one stops
// (F13-04).
func TestConfigVLANAlias(t *testing.T) {
	hollowImages(t)

	const (
		first, second = "e2e-cfg-pin1", "e2e-cfg-pin2"
		pinned        = 3300
	)

	topo := func(prefix, subnet string) string {
		return topology(
			configNode{hostname: prefix + "1", nics: []configNIC{{vlan: "SW1", address: subnet + ".1"}}}.yaml(),
			configNode{hostname: prefix + "2", nics: []configNIC{{vlan: "SW2", address: subnet + ".2"}}}.yaml(),
		)
	}

	newExperiment(t, first, topo("p", "10.53.0"), "")
	newExperiment(t, second, topo("q", "10.53.1"), "")

	id := strconv.Itoa(pinned)

	t.Run("pinning a topology VLAN", func(t *testing.T) {
		out, err := phenixRun("vlan", "alias", first, "SW1", id)

		if err != nil {
			t.Errorf("phenix vlan alias %s SW1 %s failed although SW1 has no VLAN yet: %v\n%s", first, id, err, out)

			phenix(t, "vlan", "alias", first, "SW1", id, "--force")
		}

		if got := configGetDoc(t, first).Spec.VLANs.Aliases["SW1"]; got != pinned {
			t.Errorf("spec.vlans.aliases.SW1 of %s = %d, want %d", first, got, pinned)
		}

		if out := phenix(t, "vlan", "alias", first); !strings.Contains(out, id) {
			t.Errorf("phenix vlan alias %s does not list VLAN %s:\n%s", first, id, out)
		}

		phenix(t, "vlan", "alias", second, "SW1", id, "--force")
	})

	t.Run("the pinned VLAN reaches minimega and Open vSwitch", func(t *testing.T) {
		configStart(t, first)
		configWaitRunning(t, first, "p1", "p2")

		if script := configScript(t, first); !configHasLine(script, "vlans add SW1 "+id) {
			t.Errorf("the rendered script does not pin SW1: %q", configLinesWithPrefix(script, "vlans "))
		}

		vlans := rawVLANs(t, first)
		if vlans["SW1"] != pinned {
			t.Errorf("minimega put SW1 on VLAN %d, want %d", vlans["SW1"], pinned)
		}

		if vlans["SW2"] == pinned || vlans["SW2"] == 0 {
			t.Errorf("minimega put SW2 on VLAN %d", vlans["SW2"])
		}

		if st := configGetDoc(t, first).Status.VLANs; !maps.Equal(st, vlans) {
			t.Errorf("status.vlans = %v, minimega holds %v", st, vlans)
		}

		configWantNets(t, first, map[string]string{"": configPhenixBridge}, vlans)
	})

	t.Run("a VLAN in use is refused to a second experiment", func(t *testing.T) {
		var (
			before = configQEMUPids()
			uuids  = vmUUIDs(t, first)
		)

		if out := phenixFails(t, "experiment", "start", second); !strings.Contains(out, "vlan already in use") {
			t.Errorf("the start of %s does not say VLAN %s is in use:\n%s", second, id, out)
		}

		configWantNoNamespace(t, second)

		if st := configGetDoc(t, second).Status.StartTime; st != "" {
			t.Errorf("status.startTime of %s = %q after a failed start, want empty", second, st)
		}

		if fresh := configNewPids(before); len(fresh) > 0 {
			t.Errorf("QEMU processes %v were left by the failed start", fresh)
		}

		if after := vmUUIDs(t, first); !maps.Equal(after, uuids) {
			t.Errorf("the failed start of %s changed the VMs of %s: %v, before %v", second, first, after, uuids)
		}

		if got := rawVLANs(t, first)["SW1"]; got != pinned {
			t.Errorf("%s lost VLAN %d to the failed start: SW1 is on %d", first, pinned, got)
		}
	})

	t.Run("the VLAN is free again once the first experiment stops", func(t *testing.T) {
		configStop(t, first)
		configWantNoNamespace(t, first)

		configStart(t, second)
		configWaitRunning(t, second, "q1", "q2")

		vlans := rawVLANs(t, second)
		if vlans["SW1"] != pinned {
			t.Errorf("minimega put SW1 of %s on VLAN %d, want %d", second, vlans["SW1"], pinned)
		}

		configWantNets(t, second, map[string]string{"": configPhenixBridge}, vlans)
	})
}

// ---------------------------------------------------------------------------
// F13-06, F13-07

// TestConfigBridges checks where the taps of an experiment's VMs end up on
// Open vSwitch for a custom default bridge, for interfaces whose topology
// names a bridge, and in the auto bridge mode (F13-06, F13-07).
func TestConfigBridges(t *testing.T) {
	hollowImages(t)

	t.Run("custom default bridge", func(t *testing.T) {
		const (
			name   = "e2e-cfg-bridge"
			bridge = "e2ecfgbr"
			pinned = "e2ecfgpin"
		)

		configDestroyBridge(t, bridge)
		configDestroyBridge(t, pinned)

		// b2's interface names a bridge of its own, which it keeps; b3's names
		// phenix, which counts as no bridge (docs/content/bridge-mode.md).
		topo := topology(
			configNode{hostname: "b1", nics: []configNIC{{vlan: "LAN", address: "10.54.0.1"}}}.yaml(),
			configNode{hostname: "b2", nics: []configNIC{{vlan: "LAN", address: "10.54.0.2", bridge: pinned}}}.yaml(),
			configNode{hostname: "b3", nics: []configNIC{{vlan: "LAN", address: "10.54.0.3", bridge: configPhenixBridge}}}.yaml(),
		)

		newExperiment(t, name, topo, "", "--default-bridge", bridge)

		if got := configGetDoc(t, name).Spec.DefaultBridge; got != bridge {
			t.Errorf("spec.defaultBridge = %q, want %q", got, bridge)
		}

		configStart(t, name)
		configWaitRunning(t, name, "b1", "b2", "b3")
		configWantLaunched(t, name, 3)

		script := configScript(t, name)

		for vm, want := range map[string]string{"b1": bridge, "b2": pinned, "b3": bridge} {
			if line := "vm config net " + want + ",LAN"; !slices.Contains(configBlock(script, vm), line) {
				t.Errorf("the rendered script does not render %q for %s:\n%s", line, vm, strings.Join(configBlock(script, vm), "\n"))
			}
		}

		for _, br := range []string{bridge, pinned} {
			if out, err := ovsVsctl(t, "br-exists", br); err != nil {
				t.Errorf("Open vSwitch has no bridge %s: %v %s", br, err, out)
			}
		}

		configWantNets(t, name, map[string]string{"": bridge, "b2": pinned}, rawVLANs(t, name))

		configStop(t, name)
		configWantNoNamespace(t, name)

		// The taps leave the bridges; a bridge that is gone has none left.
		for _, br := range []string{bridge, pinned} {
			eventually(t, configTeardownWait, configPoll, "no ports on bridge "+br, func() (bool, string) {
				if _, err := ovsVsctl(t, "br-exists", br); err != nil {
					return true, ""
				}

				out, err := ovsVsctl(t, "list-ports", br)

				return err == nil && out == "", fmt.Sprintf("%q (%v)", out, err)
			})
		}

		// minimega keeps a bridge until its daemon exits or `bridge destroy`;
		// phenix leaves an experiment's bridge behind on stop.
		for _, br := range []string{bridge, pinned} {
			_, err := ovsVsctl(t, "br-exists", br)
			t.Logf("after the stop, Open vSwitch bridge %s exists: %v", br, err == nil)
		}
	})

	t.Run("auto bridge mode", func(t *testing.T) {
		// 15 characters, the longest name the auto bridge mode accepts.
		const name = "e2e-cfg-auto15x"

		configDestroyBridge(t, name)

		topo := topology(
			configNode{hostname: "a1", nics: []configNIC{{vlan: "LAN", address: "10.54.1.1"}}}.yaml(),
			configNode{hostname: "a2", nics: []configNIC{{vlan: "WAN", address: "10.54.2.2"}}}.yaml(),
		)

		newExperiment(t, name, topo, "", "--bridge-mode", "auto")

		if got := configGetDoc(t, name).Spec.DefaultBridge; got != name {
			t.Errorf("spec.defaultBridge = %q, want the experiment name %q", got, name)
		}

		configStart(t, name)
		configWaitRunning(t, name, "a1", "a2")

		// docs/content/bridge-mode.md: the default bridge applies to every
		// interface that names no bridge of its own, in both modes.
		if script := configScript(t, name); !configHasLine(script, "vm config net "+name+",LAN") {
			t.Errorf("the rendered script does not put a1 on bridge %s: %q", name, configLinesWithPrefix(script, "vm config net"))
		}

		configWantNets(t, name, map[string]string{"": name}, rawVLANs(t, name))

		taps := configTaps(configNets(t, name))

		configStop(t, name)
		configWantNoNamespace(t, name)
		wantTapsGone(t, taps)
	})
}

// ---------------------------------------------------------------------------
// F13-08

// TestConfigDeployModes creates one experiment per deploy mode, one of them
// through REST, and checks the namespace hosts phenix renders and minimega
// ends up with, and where minimega runs the VMs (F13-08). On one node every
// mode deploys on the headnode.
func TestConfigDeployModes(t *testing.T) {
	hollowImages(t)

	head := headnode(t)

	modes := []struct {
		name, mode string
		rest       bool     // create and start through REST
		want       []string // host lines, in order
		absent     []string
	}{
		{name: "e2e-cfg-dall", mode: "all", want: []string{"ns add-host localhost"}, absent: []string{"ns del-host all"}},
		{
			name: "e2e-cfg-donly", mode: "only-headnode", rest: true,
			want: []string{"ns del-host all", "ns add-host localhost"},
		},
		{name: "e2e-cfg-dnone", mode: "no-headnode", absent: []string{"ns add-host localhost", "ns del-host all"}},
	}

	for i, m := range modes {
		topo := topology(configNode{hostname: "m1", nics: []configNIC{{vlan: "LAN", address: fmt.Sprintf("10.55.%d.1", i)}}}.yaml())

		if !m.rest {
			newExperiment(t, m.name, topo, "", "--deploy-mode", m.mode)
			configStart(t, m.name)

			continue
		}

		configCreateTopology(t, m.name, topo)
		adopt(t, m.name)

		apiOK(t, http.MethodPost, "/experiments", map[string]any{"name": m.name, "topology": m.name, "deploy_mode": m.mode}, nil)
		apiOK(t, http.MethodPost, "/experiments/"+m.name+"/start", nil, nil)
	}

	for _, m := range modes {
		t.Run(m.mode, func(t *testing.T) {
			if got := configGetDoc(t, m.name).Spec.DeployMode; got != m.mode {
				t.Errorf("spec.deployMode = %q, want %q", got, m.mode)
			}

			script := configScript(t, m.name)
			hosts := configLinesWithPrefix(script, "ns ")

			if !slices.Equal(slices.DeleteFunc(slices.Clone(hosts), func(l string) bool { return l == "ns queueing true" }), m.want) {
				t.Errorf("the rendered script has namespace host lines %q, want %q", hosts, m.want)
			}

			for _, line := range m.absent {
				if configHasLine(script, line) {
					t.Errorf("the rendered script has %q", line)
				}
			}

			configWaitRunning(t, m.name, "m1")
			configWantLaunched(t, m.name, 1)

			if got := strings.TrimSpace(mmText(t, m.name, "ns hosts")); got != head {
				t.Errorf("namespace %s holds hosts %q, want the headnode %q", m.name, got, head)
			}

			if got := vmInfo(t, m.name)["m1"]["host"]; got != head {
				t.Errorf("minimega runs m1 on %q, want the headnode %q", got, head)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// F13-09

// TestConfigGREMesh starts an experiment that asks for a GRE mesh on a
// single node, where minimega needs no tunnel, and checks that phenix asks
// minimega for the mesh and that the experiment runs with no tunnel (F13-09).
// It also checks that REST stores the setting.
func TestConfigGREMesh(t *testing.T) {
	hollowImages(t)

	const (
		name = "e2e-cfg-gre"
		rest = "e2e-cfg-gre2"
	)

	topo := topology(
		configNode{hostname: "g1", nics: []configNIC{{vlan: "LAN", address: "10.60.0.1"}}}.yaml(),
		configNode{hostname: "g2", nics: []configNIC{{vlan: "LAN", address: "10.60.0.2"}}}.yaml(),
	)

	newExperiment(t, name, topo, "", "--use-gre-mesh")

	if !configGetDoc(t, name).Spec.UseGREMesh {
		t.Error("spec.useGREMesh is false after experiment create --use-gre-mesh")
	}

	before := configGREInterfaces(t)

	// On one host minimega's `ns bridge` does nothing, so only the request
	// shows that phenix asked for the mesh.
	requests := coreMMDebugLog(t, func() { configStart(t, name) })
	configWantRequest(t, requests, `namespace "`+regexp.QuoteMeta(name)+`" ns bridge `+configPhenixBridge+` gre\b`)

	configWaitRunning(t, name, "g1", "g2")
	configWantLaunched(t, name, 2)

	if after := configGREInterfaces(t); !slices.Equal(after, before) {
		t.Errorf("Open vSwitch GRE interfaces %v after the start, %v before: one node needs no tunnel", after, before)
	}

	configWantNets(t, name, map[string]string{"": configPhenixBridge}, rawVLANs(t, name))

	configStop(t, name)
	configWantNoNamespace(t, name)

	t.Run("REST", func(t *testing.T) {
		configCreateTopology(t, rest, topo)
		adopt(t, rest)

		apiOK(t, http.MethodPost, "/experiments", map[string]any{"name": rest, "topology": rest, "use_gre_mesh": true}, nil)

		if !configGetDoc(t, rest).Spec.UseGREMesh {
			t.Error("spec.useGREMesh is false after POST /experiments with use_gre_mesh")
		}
	})
}

// configWantRequest checks that minimega received a request matching pattern
// among the log lines coreMMDebugLog returned.
func configWantRequest(t *testing.T, lines []string, pattern string) {
	t.Helper()

	re := regexp.MustCompile(pattern)

	var requests []string

	for _, line := range lines {
		if m := coreRequestLine.FindStringSubmatch(line); m != nil {
			if re.MatchString(m[1]) {
				return
			}

			requests = append(requests, m[1])
		}
	}

	t.Errorf("minimega received no request matching %q; it received %q", pattern, requests)
}

// ---------------------------------------------------------------------------
// F13-10

// TestConfigReconfigure checks phenix experiment reconfigure: it leaves a
// running experiment alone, and on a stopped one it applies the settings
// in force, here the auto bridge mode, to the next start (F13-10).
func TestConfigReconfigure(t *testing.T) {
	hollowImages(t)

	const name = "e2e-cfg-recon"

	configDestroyBridge(t, name)

	newExperiment(t, name, topology(configNode{hostname: "r1", nics: []configNIC{{vlan: "LAN", address: "10.61.0.1"}}}.yaml()), "")

	configStart(t, name)
	configWaitRunning(t, name, "r1")

	t.Run("a running experiment is left alone", func(t *testing.T) {
		var (
			started = configGetDoc(t, name).Status.StartTime
			uuids   = vmUUIDs(t, name)
		)

		out, err := phenixRun("--bridge-mode", "auto", "experiment", "reconfigure", name)
		t.Logf("phenix experiment reconfigure of a running experiment: err %v", err)

		if !strings.Contains(out, "not reconfiguring running experiment") {
			t.Errorf("reconfigure of a running experiment did not say it skipped it:\n%s", out)
		}

		doc := configGetDoc(t, name)

		if doc.Status.StartTime != started || doc.Spec.DefaultBridge != configPhenixBridge {
			t.Errorf("reconfigure changed the running experiment: start time %q (was %q), default bridge %q",
				doc.Status.StartTime, started, doc.Spec.DefaultBridge)
		}

		if after := vmUUIDs(t, name); !maps.Equal(after, uuids) {
			t.Errorf("reconfigure changed the running VMs: %v, before %v", after, uuids)
		}

		configWantNets(t, name, map[string]string{"": configPhenixBridge}, rawVLANs(t, name))
	})

	configStop(t, name)

	t.Run("a stopped experiment takes the settings in force", func(t *testing.T) {
		phenix(t, "--bridge-mode", "auto", "experiment", "reconfigure", name)

		if got := configGetDoc(t, name).Spec.DefaultBridge; got != name {
			t.Errorf("spec.defaultBridge = %q after reconfigure in the auto bridge mode, want %q", got, name)
		}

		configWantNoNamespace(t, name)

		configStart(t, name)
		configWaitRunning(t, name, "r1")

		if script := configScript(t, name); !configHasLine(script, "vm config net "+name+",LAN") {
			t.Errorf("the start after reconfigure does not put r1 on bridge %s: %q", name, configLinesWithPrefix(script, "vm config net"))
		}

		configWantNets(t, name, map[string]string{"": name}, rawVLANs(t, name))
	})
}

// ---------------------------------------------------------------------------
// F15-01, F15-03, F15-04, F15-05, F15-06

// configDryRun is the experiment TestConfigDryRun runs. d1 is a Linux node
// whose startup scripts and motd phenix injects into partition 1; d2 a Linux
// node with inject_partition 0, whose startup scripts phenix delivers over C2;
// d3 a node of OS type other.
type configDryRun struct {
	name string
	motd string // the file injected into d1 as /etc/motd
	head string // the headnode name phenix puts in snapshot names
}

func (d configDryRun) vms() []string {
	return []string{"d1", "d2", "d3"}
}

func (d configDryRun) topology() string {
	return topology(
		configNode{
			hostname: "d1", osType: "linux", image: ext4Image, extra: configInjection(d.motd, "/etc/motd"),
			nics: []configNIC{{vlan: "LAN", address: "10.56.0.1"}},
		}.yaml(),
		configNode{
			hostname: "d2", osType: "linux", drive: "\n        inject_partition: 0",
			nics: []configNIC{{vlan: "LAN", address: "10.56.0.2"}, {vlan: "WAN", address: "10.57.0.2"}},
		}.yaml(),
		configNode{hostname: "d3", nics: []configNIC{{vlan: "WAN", address: "10.57.0.3"}}}.yaml(),
	)
}

// snap is the name of the injection snapshot phenix renders for vm.
func (d configDryRun) snap(vm string) string {
	return fmt.Sprintf("%s_%s_%s_snapshot", d.head, d.name, vm)
}

// checkStatus checks the start time and VLANs a dry run stores.
func (d configDryRun) checkStatus(t *testing.T) {
	t.Helper()

	doc := configGetDoc(t, d.name)

	st, ok := strings.CutSuffix(doc.Status.StartTime, configDryRunSuffix)
	if !ok {
		t.Fatalf("status.startTime = %q, want a time ending in %s", doc.Status.StartTime, configDryRunSuffix)
	}

	if _, err := time.Parse(time.RFC3339, st); err != nil {
		t.Errorf("status.startTime %q does not start with an RFC 3339 time: %v", doc.Status.StartTime, err)
	}

	// A dry run allocates nothing; the status lists the spec's aliases.
	if keys := slices.Sorted(maps.Keys(doc.Status.VLANs)); !slices.Equal(keys, []string{"LAN", "WAN"}) {
		t.Errorf("status.vlans = %v, want the topology's LAN and WAN", doc.Status.VLANs)
	}

	t.Logf("status.vlans of the dry run: %v", doc.Status.VLANs)
}

// checkScript checks the minimega script the dry run rendered, VM by VM,
// and has minimega compile it without running it.
func (d configDryRun) checkScript(t *testing.T) {
	t.Helper()

	script := configScript(t, d.name)

	configWantInOrder(t, "the block of d1", configBlock(script, "d1"), []string{
		"disk snapshot " + ext4Image + " " + d.snap("d1"),
		"clear vm config",
		"vm config vcpus 1",
		fmt.Sprintf("vm config memory %d", configMemory),
		"vm config snapshot true",
		"vm config disk " + d.snap("d1") + ",writeback",
		"vm config qemu-append -vga qxl",
		"vm config net phenix,LAN",
		"vm launch kvm d1",
	})

	var inject string

	for _, line := range configBlock(script, "d1") {
		if strings.HasPrefix(line, "disk inject "+d.snap("d1")+":1 files ") {
			inject = line
		}
	}

	for _, dst := range []string{"/etc/motd", "/etc/phenix/startup/1_hostname-start.sh", "/etc/phenix/startup/3_interfaces-start.sh"} {
		if !strings.Contains(inject, fmt.Sprintf(":%q", dst)) {
			t.Errorf("d1's disk inject line does not inject %s: %q", dst, inject)
		}
	}

	if !strings.Contains(inject, fmt.Sprintf("%q:%q", d.motd, "/etc/motd")) {
		t.Errorf("d1's disk inject line does not take /etc/motd from %s: %q", d.motd, inject)
	}

	configWantInOrder(t, "the block of d2", configBlock(script, "d2"), []string{
		"disk snapshot " + blankImage + " " + d.snap("d2"),
		"vm config disk " + d.snap("d2") + ",writeback",
		"vm config qemu-append -vga qxl",
		"vm config net phenix,LAN phenix,WAN",
		"vm launch kvm d2",
	})

	configWantInOrder(t, "the block of d3", configBlock(script, "d3"), []string{
		"disk snapshot " + blankImage + " " + d.snap("d3"),
		"vm config net phenix,WAN",
		"vm launch kvm d3",
	})

	for _, vm := range []string{"d2", "d3"} {
		if inject := configLinesWithPrefix(strings.Join(configBlock(script, vm), "\n"), "disk inject"); len(inject) > 0 {
			t.Errorf("%s has disk inject lines, but phenix must not inject into it: %q", vm, inject)
		}
	}

	if slices.Contains(configBlock(script, "d3"), "vm config qemu-append -vga qxl") {
		t.Error("d3, of OS type other, gets the Linux -vga qxl argument")
	}

	// `read ... check` compiles every line without running it.
	if _, err := mmRun("", "read "+configScriptPath(d.name, ".mm")+" check"); err != nil {
		t.Errorf("minimega cannot compile the rendered script: %v", err)
	}
}

// checkCC checks the C2 script the dry run rendered for d2's startup
// scripts, and that every file it sends is staged.
func (d configDryRun) checkCC(t *testing.T) {
	t.Helper()

	data, err := os.ReadFile(configScriptPath(d.name, "-cc.mm")) //nolint:gosec // test harness
	if err != nil {
		t.Fatalf("phenix rendered no cc script for d2's startup scripts: %v", err)
	}

	var (
		script = string(data)
		sends  = configLinesWithPrefix(script, "cc send ")
	)

	if !configHasLine(script, "cc filter name=d2") || len(sends) == 0 {
		t.Fatalf("the cc script does not send d2 its startup scripts:\n%s", script)
	}

	for _, line := range sends {
		rel := strings.TrimPrefix(line, "cc send ")

		if !strings.HasPrefix(rel, d.name+"/") {
			t.Errorf("%q sends a file outside the experiment's directory", line)
		}

		if _, err := os.Stat(filepath.Join(cfg.imagesDir, rel)); err != nil {
			t.Errorf("%q sends a file phenix did not stage in minimega's files directory: %v", line, err)
		}

		if want := "cc exec bash /tmp/miniccc/files/" + rel; !configHasLine(script, want) {
			t.Errorf("the cc script sends %s but never runs it with %q", rel, want)
		}
	}
}

// checkListing lists the VMs of the dry run with the CLI and REST, which
// must answer from the spec without asking minimega.
func (d configDryRun) checkListing(t *testing.T) {
	t.Helper()

	watch := configStartWatch(t, d.name)

	out := phenix(t, "vm", "info", d.name)

	var restExp struct {
		StartTime string `json:"start_time"`
		Running   bool   `json:"running"`
	}

	apiOK(t, http.MethodGet, "/experiments/"+d.name, nil, &restExp)

	var restVMs struct {
		VMs []struct {
			Name    string `json:"name"`
			Running bool   `json:"running"`
		} `json:"vms"`
	}

	apiOK(t, http.MethodGet, "/experiments/"+d.name+"/vms", nil, &restVMs)

	configWantUntouched(t, d.name, "phenix vm info and the REST listing", watch.stop())

	for _, vm := range d.vms() {
		if !strings.Contains(out, vm) {
			t.Errorf("phenix vm info does not list %s:\n%s", vm, out)
		}
	}

	if !strings.HasSuffix(restExp.StartTime, configDryRunSuffix) {
		t.Errorf("GET /experiments/%s start_time = %q, want the dry run's", d.name, restExp.StartTime)
	}

	t.Logf("GET /experiments/%s reports running %v for the dry run", d.name, restExp.Running)

	if len(restVMs.VMs) != len(d.vms()) {
		t.Errorf("GET /experiments/%s/vms lists %d VMs, want %d", d.name, len(restVMs.VMs), len(d.vms()))
	}

	for _, vm := range restVMs.VMs {
		if vm.Running {
			t.Errorf("GET /experiments/%s/vms reports %s running in a dry run", d.name, vm.Name)
		}
	}
}

// checkRealStart checks an experiment that a start after a dry run started:
// its start time, its VMs and its injection snapshots.
func (d configDryRun) checkRealStart(t *testing.T, how string) {
	t.Helper()

	if st := configGetDoc(t, d.name).Status.StartTime; st == "" || strings.HasSuffix(st, configDryRunSuffix) {
		t.Fatalf("%s after a dry run left status.startTime %q", how, st)
	}

	configWaitRunning(t, d.name, d.vms()...)
	configWantLaunched(t, d.name, len(d.vms()))

	for _, vm := range d.vms() {
		if _, err := os.Stat(filepath.Join(cfg.imagesDir, d.snap(vm))); err != nil {
			t.Errorf("%s made no snapshot for %s: %v", how, vm, err)
		}
	}

	inspectDisk(t, filepath.Join(cfg.imagesDir, d.snap("d1")), func(root string) {
		if got := readGuestFile(t, root, "etc/motd"); got != "dry run\n" {
			t.Errorf("d1's /etc/motd = %q, want the injected file", got)
		}
	})
}

// TestConfigDryRun checks that a dry run renders everything and touches
// nothing in minimega: the namespace never exists during and after the dry
// start, the listing of its VMs and its stop, no QEMU process starts and no
// snapshot is made. Then a start after the dry run must start the
// experiment for real, through the CLI and through REST (F15-01, F15-03,
// F15-04, F15-05, F15-06).
func TestConfigDryRun(t *testing.T) {
	hollowImages(t)

	d := configDryRun{
		name: "e2e-cfg-dry",
		motd: writeFile(t, "motd", "dry run\n"),
		head: trimHostSuffixes(headnode(t)),
	}

	t.Cleanup(func() { _ = os.RemoveAll(filepath.Join(cfg.imagesDir, d.name)) })

	newExperiment(t, d.name, d.topology(), "")
	configWantNoNamespace(t, d.name)

	watch := configStartWatch(t, d.name)
	configStart(t, d.name, "--dry-run")
	seen := watch.stop()

	t.Run("the dry run leaves minimega alone", func(t *testing.T) {
		configWantUntouched(t, d.name, "the dry-run start", seen)
		configWantNoNamespace(t, d.name)

		if snaps := configSnapshots(d.name); len(snaps) > 0 {
			t.Errorf("the dry run created snapshots %v", snaps)
		}
	})

	t.Run("status marks the dry run", d.checkStatus)

	t.Run("rendered minimega script", func(t *testing.T) {
		d.checkScript(t)
		configWantNoNamespace(t, d.name)
	})

	t.Run("C2 delivery is staged for the partition 0 node", d.checkCC)
	t.Run("listing the VMs leaves minimega alone", d.checkListing)

	t.Run("stopping the dry run leaves minimega alone", func(t *testing.T) {
		watch := configStartWatch(t, d.name)
		configStop(t, d.name)
		configWantUntouched(t, d.name, "the stop of the dry run", watch.stop())

		if st := configGetDoc(t, d.name).Status.StartTime; st != "" {
			t.Errorf("status.startTime = %q after the stop, want empty", st)
		}
	})

	configStart(t, d.name, "--dry-run")

	t.Run("a CLI start after a dry run starts for real", func(t *testing.T) {
		out := configStart(t, d.name)

		st := configGetDoc(t, d.name).Status.StartTime

		if st == "" || strings.HasSuffix(st, configDryRunSuffix) {
			t.Fatalf("experiment start after a dry run left status.startTime %q:\n%s", st, out)
		}

		d.checkRealStart(t, "experiment start")
		configStop(t, d.name)
	})

	t.Run("a REST start after a dry run starts for real", func(t *testing.T) {
		// Whatever the CLI start did, start from a dry run.
		if st := configGetDoc(t, d.name).Status.StartTime; !strings.HasSuffix(st, configDryRunSuffix) {
			if st != "" {
				configStop(t, d.name)
			}

			configStart(t, d.name, "--dry-run")
		}

		apiOK(t, http.MethodPost, "/experiments/"+d.name+"/start", nil, nil)

		d.checkRealStart(t, "POST /experiments/"+d.name+"/start")

		taps := configTaps(configNets(t, d.name))

		configStop(t, d.name)
		configWantNoNamespace(t, d.name)
		wantTapsGone(t, taps)

		if snaps := configSnapshots(d.name); len(snaps) > 0 {
			t.Errorf("snapshots %v survived the stop", snaps)
		}
	})
}

// ---------------------------------------------------------------------------
// F15-02

// TestConfigDryRunApps runs a dry run of an experiment whose scenario has a
// user app, and checks what the app is told and that running it leaves
// minimega alone (F15-02).
func TestConfigDryRunApps(t *testing.T) {
	hollowImages(t)

	const (
		name = "e2e-cfg-dryapp"
		app  = "e2e-cfg-env"
	)

	dir := filepath.Join(cfg.workDir, t.Name(), "app")

	configInstallEnvApp(t, app, dir)

	// Registered first, so it runs after the experiment's teardown.
	t.Cleanup(func() { _, _ = mmRun("", "clear namespace "+name) })

	scenario := `apiVersion: phenix.sandia.gov/v2
kind: Scenario
metadata:
  name: {{name}}
  annotations:
    topology: {{name}}
spec:
  apps:
  - name: ` + app + "\n"

	newExperiment(t, name, topology(configNode{hostname: "u1", nics: []configNIC{{vlan: "LAN", address: "10.58.0.1"}}}.yaml()), scenario)

	t.Run("creating the experiment leaves minimega alone", func(t *testing.T) {
		configWantNoNamespace(t, name)
	})

	// Start the dry run from a minimega without the namespace, whatever the
	// create did.
	_, _ = mmRun("", "clear namespace "+name)

	watch := configStartWatch(t, name)
	configStart(t, name, "--dry-run")
	seen := watch.stop()

	t.Run("the dry run leaves minimega alone", func(t *testing.T) {
		configWantUntouched(t, name, "the dry-run start", seen)
	})

	t.Run("the user app is told about the dry run", func(t *testing.T) {
		for _, stage := range []struct{ name, want string }{
			{"configure", "false"},
			{"pre-start", "true"},
			{"post-start", "true"},
		} {
			if got := configAppEnv(t, dir, stage.name)["PHENIX_DRYRUN"]; got != stage.want {
				t.Errorf("the %s stage got PHENIX_DRYRUN=%q, want %q", stage.name, got, stage.want)
			}
		}
	})

	_, _ = mmRun("", "clear namespace "+name)

	watch = configStartWatch(t, name)
	configStop(t, name)
	seen = watch.stop()

	t.Run("stopping the dry run", func(t *testing.T) {
		if got := configAppEnv(t, dir, "cleanup")["PHENIX_DRYRUN"]; got != "true" {
			t.Errorf("the cleanup stage of the dry run got PHENIX_DRYRUN=%q, want true", got)
		}

		configWantUntouched(t, name, "the stop of the dry run", seen)
	})
}

// ---------------------------------------------------------------------------
// F16

// configWorkflowTopology is the topology config of the workflow tests, named
// after the branch: h1 is a hollow Linux node into which phenix injects the
// motd the workflow staged, and o1 a node of OS type other with memory MB.
func configWorkflowTopology(subnet string, memory int) string {
	return `apiVersion: phenix.sandia.gov/v1
kind: Topology
metadata:
  name: "${BRANCH_NAME}-topo"
spec:
  nodes:` + configNode{
		hostname: "h1", osType: "linux", image: ext4Image,
		extra: configInjection(configInjectsBase+"/${BRANCH_NAME}/motd", "/etc/motd"),
		nics:  []configNIC{{vlan: "LAN", address: subnet + ".1"}},
	}.yaml() + configNode{
		hostname: "o1", memory: memory, nics: []configNIC{{vlan: "LAN", address: subnet + ".2"}},
	}.yaml() + "\n"
}

// configWorkflow is a workflow config that creates an experiment named after
// the branch from the branch's topology; auto is appended under auto (indent
// 4) and spec under spec (indent 2).
func configWorkflow(auto, spec string) string {
	return `apiVersion: phenix.sandia.gov/v0
kind: Workflow
spec:
  auto:
    create: "${BRANCH_NAME}"` + auto + `
  topology: "${BRANCH_NAME}-topo"` + spec + "\n"
}

// configTopologyMemory returns the memory a stored topology config gives
// node vm.
func configTopologyMemory(t *testing.T, topo, vm string) int {
	t.Helper()

	out := phenix(t, "config", "get", "topology/"+topo, "--output", "json")

	var doc struct {
		Spec struct {
			Nodes []struct {
				General struct {
					Hostname string `json:"hostname"`
				} `json:"general"`
				Hardware struct {
					Memory int `json:"memory"`
				} `json:"hardware"`
			} `json:"nodes"`
		} `json:"spec"`
	}

	if err := json.Unmarshal([]byte(jsonPart(out)), &doc); err != nil {
		t.Fatalf("decoding topology %s: %v\n%s", topo, err, out)
	}

	for _, n := range doc.Spec.Nodes {
		if n.General.Hostname == vm {
			return n.Hardware.Memory
		}
	}

	t.Fatalf("topology %s has no node %s", topo, vm)

	return 0
}

// configWorkflowDir is the topology directory TestConfigWorkflowCLI deploys
// with phenix workflow apply, and what the test knows of its experiment.
type configWorkflowDir struct {
	dir      string // the topology directory; its base name is the branch
	branch   string // the workflow branch, and the experiment auto.create names
	other    string // a second branch
	subnet   string
	vlanSpec string // the spec entries of the workflow configs other than auto and topology
	started  string // the start time of the experiment, once it runs
}

func (w *configWorkflowDir) topoName() string {
	return w.branch + "-topo"
}

// workflow is the workflow config phenix.yml holds.
func (w *configWorkflowDir) workflow() string {
	return configWorkflow("", w.vlanSpec)
}

// noRestart is the workflow config norestart.yml holds.
func (w *configWorkflowDir) noRestart() string {
	return configWorkflow("\n    restart: false", w.vlanSpec)
}

// conflict is the workflow config conflict.yml holds: whatever the branch,
// auto.create names the experiment of w.branch.
func (w *configWorkflowDir) conflict() string {
	return strings.Replace(configWorkflow("", ""), `"${BRANCH_NAME}"`, w.branch, 1)
}

// write creates the topology directory: an inject, the topology config and
// the workflow configs.
func (w *configWorkflowDir) write(t *testing.T) {
	t.Helper()

	for path, content := range map[string]string{
		"phenix-injects/motd":          "wf1\n",
		"phenix-configs/topology.yaml": configWorkflowTopology(w.subnet, configMemory),
		"phenix.yml":                   w.workflow(),
		"bad.yml":                      "apiVersion: phenix.sandia.gov/v0\nkind: Workflow\nspec:\n  bogus: 1\n",
		"norestart.yml":                w.noRestart(),
		"conflict.yml":                 w.conflict(),
	} {
		path = filepath.Join(w.dir, path)

		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// wantStarted checks that the experiment still runs from the start time
// w.started; what names the step that must have left it alone.
func (w *configWorkflowDir) wantStarted(t *testing.T, what string) {
	t.Helper()

	if st := configGetDoc(t, w.branch).Status.StartTime; st != w.started {
		t.Errorf("%s restarted the experiment: start time %q, was %q", what, st, w.started)
	}
}

// checkDryRun checks that workflow apply -n changes nothing.
func (w *configWorkflowDir) checkDryRun(t *testing.T) {
	t.Helper()

	out := phenix(t, "workflow", "apply", w.dir, "-n")

	for _, want := range []string{"dry run complete", "createAndStart"} {
		if !strings.Contains(out, want) {
			t.Errorf("workflow apply -n does not log %q:\n%s", want, out)
		}
	}

	if _, err := os.Stat(filepath.Join(configInjectsBase, w.branch)); !os.IsNotExist(err) {
		t.Errorf("workflow apply -n staged the injects (stat: %v)", err)
	}

	phenixFails(t, "config", "get", "topology/"+w.topoName())
	phenixFails(t, "config", "get", "experiment/"+w.branch)
}

// checkCreate applies the directory and checks every step: the staged
// inject, the upserted topology, and the experiment created, mapped, tagged,
// configured as the workflow says and started, with the inject in its VM.
func (w *configWorkflowDir) checkCreate(t *testing.T) {
	t.Helper()

	if out := phenix(t, "workflow", "apply", w.dir); !strings.Contains(out, "workflow apply complete") {
		t.Errorf("workflow apply does not log its completion:\n%s", out)
	}

	data, err := os.ReadFile(filepath.Join(configInjectsBase, w.branch, "motd"))
	if err != nil || string(data) != "wf1\n" {
		t.Errorf("staged motd = %q (%v), want wf1", data, err)
	}

	phenix(t, "config", "get", "topology/"+w.topoName())

	doc := configGetDoc(t, w.branch)

	if got := doc.Metadata.Annotations["phenix.workflow/branch"]; got != w.branch {
		t.Errorf("annotation phenix.workflow/branch = %q, want %q", got, w.branch)
	}

	tags := doc.Metadata.Annotations["phenix.workflow/tags"]

	for _, tag := range []string{"method=workflow", "branch=" + w.branch} {
		if !slices.Contains(strings.Split(tags, ","), tag) {
			t.Errorf("annotation phenix.workflow/tags = %q lacks %s", tags, tag)
		}
	}

	if doc.Spec.DeployMode != "all" || doc.Spec.VLANs.Min != 330 || doc.Spec.VLANs.Max != 340 {
		t.Errorf("spec deployMode %q, VLAN range %d-%d; want all and 330-340",
			doc.Spec.DeployMode, doc.Spec.VLANs.Min, doc.Spec.VLANs.Max)
	}

	if doc.Status.StartTime == "" {
		t.Fatal("the experiment the workflow created is not running")
	}

	w.started = doc.Status.StartTime

	configWaitRunning(t, w.branch, "h1", "o1")
	configWantLaunched(t, w.branch, 2)

	if got := rawVLANs(t, w.branch)["LAN"]; got != 330 || doc.Status.VLANs["LAN"] != 330 {
		t.Errorf("LAN is on VLAN %d in minimega and %d in the status, want 330", got, doc.Status.VLANs["LAN"])
	}

	script := configScript(t, w.branch)

	configRenderedRange(t, script, 330, 340)

	for _, line := range []string{"vlans add LAN 330", "ns add-host localhost"} {
		if !configHasLine(script, line) {
			t.Errorf("the rendered script lacks %q", line)
		}
	}

	inspectDisk(t, configSnapshotOf(t, script, "h1"), func(root string) {
		if got := readGuestFile(t, root, "etc/motd"); got != "wf1\n" {
			t.Errorf("h1's /etc/motd = %q, want the staged inject", got)
		}
	})
}

// checkForce changes the topology of the running experiment and applies it,
// without -f, which must change nothing, then with -f, which must restart
// the experiment with the change.
func (w *configWorkflowDir) checkForce(t *testing.T) {
	t.Helper()

	uuids := vmUUIDs(t, w.branch)
	topo := configWorkflowTopology(w.subnet, 2*configMemory)

	if err := os.WriteFile(filepath.Join(w.dir, "phenix-configs", "topology.yaml"), []byte(topo), 0o600); err != nil {
		t.Fatal(err)
	}

	if out := phenixFails(t, "workflow", "apply", w.dir); !strings.Contains(out, "re-run with -f") {
		t.Errorf("workflow apply of a running experiment does not ask for -f:\n%s", out)
	}

	w.wantStarted(t, "the refused apply")

	if after := vmUUIDs(t, w.branch); !maps.Equal(after, uuids) {
		t.Errorf("the refused apply changed the VMs: %v, before %v", after, uuids)
	}

	if got := configTopologyMemory(t, w.topoName(), "o1"); got != configMemory {
		t.Errorf("the refused apply upserted the topology: o1 has %d MB", got)
	}

	if out := phenix(t, "workflow", "apply", w.dir, "-f"); !strings.Contains(out, "workflow apply complete") {
		t.Errorf("workflow apply -f does not log its completion:\n%s", out)
	}

	st := configGetDoc(t, w.branch).Status.StartTime
	if st == "" || st == w.started {
		t.Fatalf("workflow apply -f did not restart the experiment: start time %q, was %q", st, w.started)
	}

	w.started = st

	configWaitRunning(t, w.branch, "h1", "o1")

	if got := vmInfo(t, w.branch)["o1"]["memory"]; got != strconv.Itoa(2*configMemory) {
		t.Errorf("minimega runs o1 with %s MB after the restart, want %d", got, 2*configMemory)
	}

	if after := vmUUIDs(t, w.branch); after["o1"] == uuids["o1"] {
		t.Errorf("o1 kept UUID %s across the restart", after["o1"])
	}
}

// checkInvalid applies a workflow config with an unknown key through the
// CLI and REST.
func (w *configWorkflowDir) checkInvalid(t *testing.T) {
	t.Helper()

	if out := phenixFails(t, "workflow", "apply", w.dir, "-c", "bad.yml"); !strings.Contains(out, "bogus") {
		t.Errorf("workflow apply of an invalid config does not name the bad key:\n%s", out)
	}

	body := `{"apiVersion":"phenix.sandia.gov/v0","kind":"Workflow","spec":{"bogus":1}}`

	code, data := api(t, http.MethodPost, "/workflow/apply/"+w.branch+"?dryRun=true", body)
	if code != http.StatusBadRequest || !bytes.Contains(data, []byte("bogus")) {
		t.Errorf("POST /workflow/apply/%s with an unknown key = %d %s, want 400 naming it", w.branch, code, data)
	}

	w.wantStarted(t, "the refused config")
}

// checkNoRestart applies a workflow config with auto.restart false to the
// running experiment through the CLI, and plans it through REST.
func (w *configWorkflowDir) checkNoRestart(t *testing.T) {
	t.Helper()

	out := phenix(t, "workflow", "apply", w.dir, "-c", "norestart.yml")

	for _, want := range []string{"no experiment changed", "running and auto.restart is false"} {
		if !strings.Contains(out, want) {
			t.Errorf("workflow apply with auto.restart false does not log %q:\n%s", want, out)
		}
	}

	w.wantStarted(t, "auto.restart false")

	plan, _ := configApply(t, w.branch, "dryRun=true", w.noRestart(), http.StatusOK)
	if plan.Action != "none" || plan.Reason != "running and auto.restart is false" {
		t.Errorf("dry-run plan = %+v, want none because the experiment runs", plan)
	}
}

// checkConflict applies, on another branch, a workflow config whose
// auto.create names the experiment of w.branch.
func (w *configWorkflowDir) checkConflict(t *testing.T) {
	t.Helper()

	out := phenixFails(t, "workflow", "apply", w.dir, "-b", w.other, "-c", "conflict.yml", "-n")
	if !strings.Contains(out, "already exists and is not mapped") {
		t.Errorf("workflow apply of a conflicting auto.create failed for another reason:\n%s", out)
	}

	_, data := configApply(t, w.other, "dryRun=true", w.conflict(), http.StatusConflict)
	if !bytes.Contains(data, []byte("already exists and is not mapped")) {
		t.Errorf("409 for a conflicting auto.create does not say why: %s", data)
	}

	phenixFails(t, "config", "get", "topology/"+w.other+"-topo")

	w.wantStarted(t, "the conflicting apply")
}

// TestConfigWorkflowCLI deploys a topology directory with phenix workflow
// apply and follows it through the server: a dry run changes nothing, an
// apply stages the injects, upserts the topology and creates and starts the
// experiment, a re-apply while it runs needs -f, and an invalid config, a
// plan of none and a name conflict leave it alone (F16-01 to F16-06).
func TestConfigWorkflowCLI(t *testing.T) {
	hollowImages(t)

	const branch = "e2e-cfg-wf"

	w := &configWorkflowDir{
		dir:      filepath.Join(cfg.workDir, t.Name(), branch),
		branch:   branch,
		other:    "e2e-cfg-wfx",
		subnet:   "10.59.0",
		vlanSpec: "\n  vlans:\n    LAN: 330\n  vlanRange:\n    min: 330\n    max: 340\n  deployMode: all",
	}

	w.write(t)

	// Runs last: removes the injects directory when staging created it and
	// nothing else is in it.
	t.Cleanup(func() { _ = os.Remove(configInjectsBase) })

	for _, b := range []string{w.branch, w.other} {
		t.Cleanup(func() { _ = os.RemoveAll(filepath.Join(configInjectsBase, b)) })
		configDeleteConfig(t, "topology/"+b+"-topo")
		adopt(t, b)
	}

	t.Run("a dry run changes nothing", w.checkDryRun)
	t.Run("apply creates and starts the experiment", w.checkCreate)

	if w.started == "" {
		t.Fatal("the workflow did not start the experiment")
	}

	t.Run("REST plans a restart of the running experiment", func(t *testing.T) {
		plan, _ := configApply(t, w.branch, "dryRun=true", w.workflow(), http.StatusOK)

		if plan.Action != "restart" || plan.Experiment != w.branch || !plan.DryRun {
			t.Errorf("dry-run plan = %+v, want a dry-run restart of %s", plan, w.branch)
		}

		w.wantStarted(t, "the dry run")
	})

	t.Run("a re-apply while running needs -f", w.checkForce)
	t.Run("an invalid workflow config is refused", w.checkInvalid)
	t.Run("auto.restart false leaves the running experiment alone", w.checkNoRestart)
	t.Run("auto.create of an experiment mapped to another branch", w.checkConflict)
}

// TestConfigWorkflowREST deploys a workflow branch through the REST
// endpoints alone: a config dry run and upsert, an apply that only creates
// the experiment, a refused apply whose plan changed, and an apply that
// updates and starts it (F16).
func TestConfigWorkflowREST(t *testing.T) {
	hollowImages(t)

	const branch = "e2e-cfg-wfr"

	var (
		topoName = branch + "-topo"
		topo     = `apiVersion: phenix.sandia.gov/v1
kind: Topology
metadata:
  name: "${BRANCH_NAME}-topo"
spec:
  nodes:` + configNode{hostname: "w1", nics: []configNIC{{vlan: "LAN", address: "10.62.0.1"}}}.yaml() + "\n"
		create = configWorkflow("\n    restart: false", "\n  deployMode: only-headnode")
		start  = configWorkflow("", "\n  deployMode: only-headnode")
	)

	configDeleteConfig(t, "topology/"+topoName)
	adopt(t, branch)

	t.Run("config dry run and upsert", func(t *testing.T) {
		code, data := configAPIYAML(t, http.MethodPost, "/workflow/configs/"+branch+"?dryRun=true", topo)
		if code != http.StatusOK {
			t.Fatalf("config dry run = %d %s, want 200", code, data)
		}

		var result struct {
			Action string `json:"action"`
			Kind   string `json:"kind"`
			Name   string `json:"name"`
			DryRun bool   `json:"dryRun"`
		}

		if err := json.Unmarshal(data, &result); err != nil {
			t.Fatalf("decoding %s: %v", data, err)
		}

		if result.Action != "create" || result.Kind != "Topology" || result.Name != topoName || !result.DryRun {
			t.Errorf("config dry run = %+v, want a dry-run create of Topology %s", result, topoName)
		}

		phenixFails(t, "config", "get", "topology/"+topoName)

		if code, data := configAPIYAML(t, http.MethodPost, "/workflow/configs/"+branch, topo); code != http.StatusCreated {
			t.Fatalf("config upsert = %d %s, want 201", code, data)
		}

		phenix(t, "config", "get", "topology/"+topoName)
	})

	t.Run("an apply with auto.restart false only creates", func(t *testing.T) {
		plan, _ := configApply(t, branch, "dryRun=true", create, http.StatusOK)
		if plan.Action != "create" || plan.Experiment != branch {
			t.Errorf("dry-run plan = %+v, want create of %s", plan, branch)
		}

		phenixFails(t, "config", "get", "experiment/"+branch)

		if plan, _ := configApply(t, branch, "expect=create", create, http.StatusOK); plan.Action != "create" || plan.DryRun {
			t.Errorf("apply = %+v, want a real create", plan)
		}

		doc := configGetDoc(t, branch)

		if doc.Metadata.Annotations["phenix.workflow/branch"] != branch || doc.Spec.DeployMode != "only-headnode" {
			t.Errorf("created experiment has annotations %v and deploy mode %q", doc.Metadata.Annotations, doc.Spec.DeployMode)
		}

		if doc.Status.StartTime != "" {
			t.Errorf("auto.restart false started the experiment at %s", doc.Status.StartTime)
		}

		configWantNoNamespace(t, branch)
	})

	t.Run("an apply whose plan changed is refused", func(t *testing.T) {
		configApply(t, branch, "expect=create", start, http.StatusConflict)

		if st := configGetDoc(t, branch).Status.StartTime; st != "" {
			t.Errorf("the refused apply started the experiment at %s", st)
		}
	})

	t.Run("an apply updates and starts", func(t *testing.T) {
		if plan, _ := configApply(t, branch, "expect=updateAndStart", start, http.StatusOK); plan.Action != "updateAndStart" {
			t.Errorf("apply = %+v, want updateAndStart", plan)
		}

		if st := configGetDoc(t, branch).Status.StartTime; st == "" {
			t.Fatal("the experiment is not running after updateAndStart")
		}

		configWaitRunning(t, branch, "w1")
		configWantLaunched(t, branch, 1)

		configWantInOrder(
			t,
			"the rendered script",
			configLines(configScript(t, branch)),
			[]string{"ns del-host all", "ns add-host localhost"},
		)
	})
}
