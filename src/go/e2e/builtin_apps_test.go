//go:build e2e

// End-to-end tests for the apps compiled into phenix (startup, ntp, serial,
// vrouter, tap, soh and scorch with its built-in components). Each test checks
// what an app changed where it lands: the files phenix renders, what minimega
// injects into disks, what minimega and Open vSwitch report, what the guests
// report over C2, and the REST API. Real guests boot only where a check needs
// a guest OS; everything else runs on hollow VMs.

package e2e

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	// builtinComponent is the SCORCH user component the tests install; it
	// records its arguments, stdin and environment in the run directory.
	builtinComponent = "e2ebiecho"

	// builtinP3Image is a hollow disk with three ext4 partitions, for the
	// VyOS inject_partition 3 path.
	builtinP3Image = "e2e-builtin-p3.qc2"

	// builtinNetNSDir is where `ip netns add` creates named namespaces.
	builtinNetNSDir = "/var/run/netns"

	// builtinPhenixBase is phenix's base directory; experiment base and files
	// directories hang off it.
	builtinPhenixBase = "/phenix"

	builtinOVSBridge = "phenix"

	// builtinRouterBoot bounds how long a minirouter guest takes to boot,
	// register with C2 and apply its committed config.
	builtinRouterBoot = 5 * time.Minute

	// builtinTapGone bounds how long minimega's tap reaper and phenix's netns
	// cleanup take to remove a host tap.
	builtinTapGone = 15 * time.Second

	// builtinReapWait bounds how long builtinCleanTaps leaves minimega's tap
	// reaper, which runs about once a second, to remove the OVS ports of the
	// host taps minimega destroyed.
	builtinReapWait = 5 * time.Second
)

// builtinTapSuffixes are the name suffixes of the host taps the tap app and
// the SCORCH tap and break components create.
var builtinTapSuffixes = []string{"-tapapp", "-tapcomp", "-tapbrk"} //nolint:gochecknoglobals // constant list

// ---------------------------------------------------------------------------
// topology and scenario builders

// builtinIface is one interface of a builtinNode. Every address has a /24
// mask. A serial interface uses UDP port 8989 at 9600 baud.
type builtinIface struct {
	name, vlan, proto, addr, gw string

	dns       []string
	mtu       int
	rulesetIn string
	serial    bool
	device    string
}

func (i builtinIface) yaml() string {
	var b strings.Builder

	typ, proto := "ethernet", i.proto
	if i.serial {
		typ = "serial"
	}

	if proto == "" {
		proto = "static"
	}

	fmt.Fprintf(&b, "\n      - name: %s\n        type: %s\n        proto: %s\n        vlan: %s", i.name, typ, proto, i.vlan)

	if i.addr != "" {
		fmt.Fprintf(&b, "\n        address: %s\n        mask: 24", i.addr)
	}

	if i.gw != "" {
		fmt.Fprintf(&b, "\n        gateway: %s", i.gw)
	}

	if len(i.dns) > 0 {
		b.WriteString("\n        dns:")

		for _, d := range i.dns {
			fmt.Fprintf(&b, "\n        - %s", d)
		}
	}

	if i.mtu > 0 {
		fmt.Fprintf(&b, "\n        mtu: %d", i.mtu)
	}

	if i.rulesetIn != "" {
		fmt.Fprintf(&b, "\n        ruleset_in: %s", i.rulesetIn)
	}

	if i.serial {
		fmt.Fprintf(&b, "\n        udp_port: 8989\n        baud_rate: 9600\n        device: %s", i.device)
	}

	return b.String()
}

// builtinNode is a topology node. netExtra is appended under network
// (indent 6) and extra under the node (indent 4).
type builtinNode struct {
	kind, host, image, os string

	memory    int
	partition int
	ifaces    []builtinIface
	netExtra  string
	extra     string
}

func (n builtinNode) yaml() string {
	var (
		b      strings.Builder
		kind   = n.kind
		osType = n.os
		memory = n.memory
	)

	if kind == "" {
		kind = "VirtualMachine"
	}

	if osType == "" {
		osType = "linux"
	}

	if memory == 0 {
		memory = 256
	}

	fmt.Fprintf(&b, `
  - type: %s
    general:
      hostname: %s
    hardware:
      vcpus: 1
      memory: %d
      os_type: %s
      drives:
      - image: %s`, kind, n.host, memory, osType, n.image)

	if n.partition > 0 {
		fmt.Fprintf(&b, "\n        inject_partition: %d", n.partition)
	}

	b.WriteString("\n    network:\n      interfaces:")

	for _, i := range n.ifaces {
		b.WriteString(i.yaml())
	}

	b.WriteString(n.netExtra)
	b.WriteString(n.extra)

	return b.String()
}

// builtinTopology wraps nodes in a Topology config named {{name}}.
func builtinTopology(nodes ...builtinNode) string {
	parts := make([]string, len(nodes))

	for i, n := range nodes {
		parts[i] = n.yaml()
	}

	return topology(parts...)
}

// builtinScenario wraps apps (list items at indent 2) in a v2 Scenario named
// {{name}} for the topology of the same name.
func builtinScenario(apps string) string {
	return `apiVersion: phenix.sandia.gov/v2
kind: Scenario
metadata:
  name: {{name}}
  annotations:
    topology: {{name}}
spec:
  apps:` + apps + "\n"
}

// builtinHollow is a hollow Linux node with one static interface.
func builtinHollow(host, vlan, addr string) builtinNode {
	return builtinNode{
		host:   host,
		image:  ext4Image,
		ifaces: []builtinIface{{name: "eth0", vlan: vlan, addr: addr}},
	}
}

// ---------------------------------------------------------------------------
// host, file and status helpers

// builtinSnapshot is the injection snapshot minimega creates for a VM.
func builtinSnapshot(t *testing.T, exp, vm string) string {
	t.Helper()

	return filepath.Join(cfg.imagesDir, fmt.Sprintf("%s_%s_%s_snapshot", trimHostSuffixes(headnode(t)), exp, vm))
}

// builtinBaseDir is the experiment's phenix base directory, where apps
// render the files they inject.
func builtinBaseDir(exp string) string {
	return filepath.Join(builtinPhenixBase, "experiments", exp)
}

// builtinFilesDir is the experiment's files directory (SCORCH output).
func builtinFilesDir(exp string) string {
	return filepath.Join(builtinPhenixBase, "images", exp, "files")
}

// builtinWantLines reports every want that is not a whole (trimmed) line of
// text.
func builtinWantLines(t *testing.T, what, text string, wants ...string) {
	t.Helper()

	lines := make(map[string]bool)

	for l := range strings.SplitSeq(text, "\n") {
		lines[strings.TrimSpace(l)] = true
	}

	for _, w := range wants {
		if !lines[w] {
			t.Errorf("%s has no line %q:\n%s", what, w, text)
		}
	}
}

// builtinMMBlock returns the part of a rendered minimega script for one VM.
func builtinMMBlock(script, vm string) string {
	_, block, found := strings.Cut(script, "## VM: "+vm+" ##")
	if !found {
		return ""
	}

	if next := strings.Index(block, "## VM: "); next >= 0 {
		block = block[:next]
	}

	return block
}

// builtinInjectLine returns the `disk inject ... files` line of a VM's block.
func builtinInjectLine(script, vm string) string {
	for l := range strings.SplitSeq(builtinMMBlock(script, vm), "\n") {
		if strings.HasPrefix(l, "disk inject ") && strings.Contains(l, " files ") {
			return l
		}
	}

	return ""
}

// builtinConfig returns the experiment config phenix stores, as generic JSON.
func builtinConfig(t *testing.T, name string) map[string]any {
	t.Helper()

	out := phenix(t, "config", "get", "experiment/"+name, "--output", "json")

	var c map[string]any
	if err := json.Unmarshal([]byte(jsonPart(out)), &c); err != nil {
		t.Fatalf("decoding experiment %s: %v\n%s", name, err, out)
	}

	return c
}

func builtinStatusMap(t *testing.T, name, key string) map[string]any {
	t.Helper()

	status, _ := builtinConfig(t, name)["status"].(map[string]any)
	m, _ := status[key].(map[string]any)

	return m
}

// builtinAppStatus decodes status.apps.<app> into out and reports whether
// phenix recorded a status for the app.
func builtinAppStatus(t *testing.T, name, app string, out any) bool {
	t.Helper()

	v, ok := builtinStatusMap(t, name, "apps")[app]
	if !ok || v == nil {
		return false
	}

	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}

	if err := json.Unmarshal(data, out); err != nil {
		t.Fatalf("decoding status.apps.%s %s: %v", app, data, err)
	}

	return true
}

// builtinAppRunning reports status.appRunningStageStatus.<app>.
func builtinAppRunning(t *testing.T, name, app string) bool {
	t.Helper()

	running, _ := builtinStatusMap(t, name, "appRunningStageStatus")[app].(bool)

	return running
}

// builtinSpecNode returns the stored spec of one topology node.
func builtinSpecNode(t *testing.T, name, host string) map[string]any {
	t.Helper()

	spec, _ := builtinConfig(t, name)["spec"].(map[string]any)
	topo, _ := spec["topology"].(map[string]any)
	nodes, _ := topo["nodes"].([]any)

	for _, n := range nodes {
		node, _ := n.(map[string]any)
		general, _ := node["general"].(map[string]any)

		if general["hostname"] == host {
			return node
		}
	}

	t.Fatalf("experiment %s has no node %s", name, host)

	return nil
}

// builtinExperiment is newExperiment that also removes what the experiment
// left in minimega's files directory (SCORCH runs, staged C2 scripts, SOH test
// scripts, minirouter configs) once the experiment is torn down.
func builtinExperiment(t *testing.T, name, topology, scenario string) *experiment {
	t.Helper()

	// Registered before the experiment's cleanups, so it runs after them.
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Join(cfg.imagesDir, name)) })

	return newExperiment(t, name, topology, scenario)
}

// builtinWantNoNamespace fails when minimega still has the namespace.
func builtinWantNoNamespace(t *testing.T, name string) {
	t.Helper()

	if ns, ok := mmNamespaces(t)[name]; ok {
		t.Errorf("minimega namespace %s exists: %v", name, ns)
	}
}

// ---------------------------------------------------------------------------
// host taps

func builtinNetNS(name string) bool {
	_, err := os.Stat(filepath.Join(builtinNetNSDir, name))

	return err == nil
}

func builtinIsTap(name string) bool {
	for _, s := range builtinTapSuffixes {
		if strings.HasSuffix(name, s) {
			return true
		}
	}

	return false
}

// builtinTapLeftovers lists the host taps, namespaces and links the tap app
// and the SCORCH components name. A source it cannot read fails the test, so
// that a leftover check does not pass on the others alone.
func builtinTapLeftovers(t *testing.T) map[string]bool {
	t.Helper()

	out := make(map[string]bool)

	// ip creates the directory with the first named namespace.
	if entries, err := os.ReadDir(builtinNetNSDir); err == nil {
		for _, e := range entries {
			if builtinIsTap(e.Name()) {
				out[e.Name()] = true
			}
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("listing host taps' network namespaces: %v", err)
	}

	for _, p := range builtinBridgePorts(t) {
		if builtinIsTap(p) {
			out[p] = true
		}
	}

	if links, err := os.ReadDir("/sys/class/net"); err == nil {
		for _, l := range links {
			if builtinIsTap(l.Name()) {
				out[l.Name()] = true
			}
		}
	} else {
		t.Errorf("listing host links: %v", err)
	}

	return out
}

// builtinBridgePorts lists the ports of the phenix bridge: none before
// minimega has created the bridge.
func builtinBridgePorts(t *testing.T) []string {
	t.Helper()

	// ovs-vsctl br-exists exits 2 for a bridge that does not exist.
	if _, err := hostCmd("ovs-vsctl", "br-exists", builtinOVSBridge); err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 2 {
			t.Errorf("ovs-vsctl br-exists %s: %v", builtinOVSBridge, err)
		}

		return nil
	}

	ports, err := hostCmd("ovs-vsctl", "list-ports", builtinOVSBridge)
	if err != nil {
		t.Errorf("ovs-vsctl list-ports %s: %v: %s", builtinOVSBridge, err, ports)

		return nil
	}

	return strings.Fields(ports)
}

// builtinNewTaps lists the leftovers not in before whose names end in suffix.
func builtinNewTaps(t *testing.T, before map[string]bool, suffix string) []string {
	t.Helper()

	var names []string

	for name := range builtinTapLeftovers(t) {
		if !before[name] && strings.HasSuffix(name, suffix) {
			names = append(names, name)
		}
	}

	slices.Sort(names)

	return names
}

// builtinNewTapPorts lists the ports on the phenix bridge, not in before,
// that are named like the host taps of the tap app and the SCORCH components.
func builtinNewTapPorts(t *testing.T, before map[string]bool) []string {
	t.Helper()

	var names []string

	for _, p := range builtinBridgePorts(t) {
		if builtinIsTap(p) && !before[p] {
			names = append(names, p)
		}
	}

	return names
}

// builtinCleanTaps snapshots the host taps and, registered before a test's
// experiment so it runs after the experiment's teardown, removes any new tap,
// namespace, link or iptables rule a failing test left on the runner.
func builtinCleanTaps(t *testing.T) map[string]bool {
	t.Helper()

	before := builtinTapLeftovers(t)

	t.Cleanup(func() {
		// minimega only marks a host tap it destroys defunct. Its reaper then
		// deletes the OVS ports of every defunct tap in one ovs-vsctl
		// transaction without --if-exists and keeps them all when that fails,
		// so a port deleted here first makes every later reap fail with "no
		// such port", and no host tap's port goes for the rest of the run
		// (minimega internal/bridge/bridge.go reapTaps). Leave the reaper its
		// ports first; force only what it leaves.
		ports := builtinNewTapPorts(t, before)

		for deadline := time.Now().Add(builtinReapWait); len(ports) > 0 && time.Now().Before(deadline); {
			time.Sleep(500 * time.Millisecond)

			ports = builtinNewTapPorts(t, before)
		}

		if len(ports) > 0 {
			t.Logf("cleanup: minimega did not remove the OVS ports %v within %v", ports, builtinReapWait)
		}

		for name := range builtinTapLeftovers(t) {
			if before[name] {
				continue
			}

			t.Logf("cleanup: removing host tap %s left behind", name)

			_, _ = hostCmd("ip", "netns", "delete", name)
			_, _ = hostCmd("ovs-vsctl", "--if-exists", "del-port", builtinOVSBridge, name)
			_, _ = hostCmd("ip", "link", "delete", name)

			builtinDropRules(name)
		}
	})

	return before
}

// builtinDropRules deletes host iptables rules that name an interface.
func builtinDropRules(iface string) {
	for _, table := range []string{"filter", "nat"} {
		rules, err := hostCmd("iptables", "-t", table, "-S")
		if err != nil {
			continue
		}

		for rule := range strings.SplitSeq(rules, "\n") {
			if !strings.HasPrefix(rule, "-A ") || !strings.Contains(rule, iface) {
				continue
			}

			args := append([]string{"-t", table, "-D"}, strings.Fields(rule)[1:]...)
			_, _ = hostCmd("iptables", args...)
		}
	}
}

// builtinWaitTapsGone waits until none of the named host taps remains.
func builtinWaitTapsGone(t *testing.T, names ...string) {
	t.Helper()

	eventually(t, builtinTapGone, time.Second, fmt.Sprintf("host taps %v to go", names), func() (bool, string) {
		left := builtinTapLeftovers(t)

		var still []string

		for _, n := range names {
			if left[n] {
				still = append(still, n)
			}
		}

		return len(still) == 0, fmt.Sprintf("still there: %v", still)
	})
}

// builtinWantHostTap checks a host tap the tap app or a SCORCH component
// created: an OVS internal port on the phenix bridge, tagged with the VLAN,
// moved into a network namespace of its own name with the given address.
func builtinWantHostTap(t *testing.T, tap, cidr string, vlanID int) {
	t.Helper()

	if !builtinNetNS(tap) {
		t.Errorf("host tap %s has no network namespace of its name", tap)
	}

	addr, err := hostCmd("ip", "netns", "exec", tap, "ip", "-4", "-o", "addr", "show", "dev", tap)
	if err != nil || !strings.Contains(addr, "inet "+cidr+" ") {
		t.Errorf("host tap %s in its namespace has no %s (%v): %s", tap, cidr, err, addr)
	}

	link, err := hostCmd("ip", "netns", "exec", tap, "ip", "-o", "link", "show", "dev", tap)
	if err != nil || !strings.Contains(link, ",UP") {
		t.Errorf("host tap %s is not up in its namespace (%v): %s", tap, err, link)
	}

	if br, err := hostCmd("ovs-vsctl", "port-to-br", tap); err != nil || br != builtinOVSBridge {
		t.Errorf("host tap %s is on OVS bridge %q (%v), want %s", tap, br, err, builtinOVSBridge)
	}

	if typ, err := hostCmd("ovs-vsctl", "get", "interface", tap, "type"); err != nil || typ != "internal" {
		t.Errorf("host tap %s has OVS type %q (%v), want internal", tap, typ, err)
	}

	if vlanID > 0 {
		if tag, err := hostCmd("ovs-vsctl", "get", "port", tap, "tag"); err != nil || tag != strconv.Itoa(vlanID) {
			t.Errorf("host tap %s has OVS tag %q (%v), want the experiment VLAN %d", tap, tag, err, vlanID)
		}
	}
}

// builtinPing pings addr from a host tap's network namespace.
func builtinPing(t *testing.T, tap, addr string) {
	t.Helper()

	if out, err := hostCmd("ip", "netns", "exec", tap, "ping", "-c", "3", "-W", "2", addr); err != nil {
		t.Errorf("pinging %s from host tap %s: %v\n%s", addr, tap, err, out)
	}
}

// builtinTapStatus is status.apps.tap.
type builtinTapStatus struct {
	Host string `json:"host"`
	Taps []struct {
		Name     string `json:"name"`
		VLAN     string `json:"vlan"`
		IP       string `json:"ip"`
		Subnet   string `json:"subnet"`
		External struct {
			Enabled bool `json:"enabled"`
		} `json:"externalAccess"`
	} `json:"taps"`
}

// ---------------------------------------------------------------------------
// guests

// builtinCheck is one shell check run in a guest.
type builtinCheck struct {
	name, script string
}

// builtinGuest runs every check in one C2 round trip (each costs a miniccc
// heartbeat) and returns each check's trimmed stdout by name.
func builtinGuest(t *testing.T, ns, vm string, checks ...builtinCheck) map[string]string {
	t.Helper()

	var script strings.Builder

	for i, c := range checks {
		fmt.Fprintf(&script, "echo '@@%d@@'; ( %s ) 2>/dev/null; echo; ", i, c.script)
	}

	out := guestShell(t, ns, vm, script.String())
	results := make(map[string]string, len(checks))

	for i, c := range checks {
		_, after, found := strings.Cut(out, fmt.Sprintf("@@%d@@", i))
		if !found {
			results[c.name] = "(no output)"

			continue
		}

		if next := strings.Index(after, fmt.Sprintf("@@%d@@", i+1)); next >= 0 {
			after = after[:next]
		}

		results[c.name] = strings.TrimSpace(after)
	}

	return results
}

// builtinWantGuest checks guest results against exact wants.
func builtinWantGuest(t *testing.T, vm string, got, want map[string]string) {
	t.Helper()

	names := make([]string, 0, len(want))
	for n := range want {
		names = append(names, n)
	}

	slices.Sort(names)

	for _, n := range names {
		if got[n] != want[n] {
			t.Errorf("%s: %s = %q, want %q", vm, n, got[n], want[n])
		}
	}
}

// builtinClient returns minimega's cc clients row for a VM, found by UUID so
// that it does not depend on the hostname the guest reports.
func builtinClient(t *testing.T, ns, vm string) map[string]string {
	t.Helper()

	uuid := vmInfo(t, ns)[vm]["uuid"]

	for _, row := range mmTable(t, ns, "cc clients") {
		if uuid != "" && row["uuid"] == uuid {
			return row
		}
	}

	return nil
}

// builtinWaitClient waits until a VM's miniccc client registers.
func builtinWaitClient(t *testing.T, ns, vm string, timeout time.Duration) map[string]string {
	t.Helper()

	var row map[string]string

	eventually(t, timeout, 2*time.Second, vm+" to register with C2", func() (bool, string) {
		row = builtinClient(t, ns, vm)

		return row != nil, "not registered"
	})

	return row
}

// builtinNTPRequest is a 48-byte NTPv3 client (mode 3) request, base64.
func builtinNTPRequest() string {
	packet := make([]byte, 48)
	packet[0] = 0x1b // LI 0, version 3, mode 3 (client)

	return base64.StdEncoding.EncodeToString(packet)
}

// ---------------------------------------------------------------------------
// phenix processes, SCORCH and REST

// builtinStart runs the phenix CLI in the background; the test's cleanup
// kills it if it is still running.
func builtinStart(t *testing.T, args ...string) *phenixProc {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), phenixTimeout)
	p := &phenixProc{done: make(chan struct{})}

	cmd := exec.CommandContext(ctx, cfg.phenix, phenixArgs(args...)...) //nolint:gosec // test harness
	cmd.Stdout = p
	cmd.Stderr = p

	if err := startChild(cmd); err != nil {
		cancel()
		t.Fatalf("starting phenix %s: %v", strings.Join(args, " "), err)
	}

	go func() {
		err := cmd.Wait()

		p.mu.Lock()
		p.err = err
		p.mu.Unlock()

		close(p.done)
		cancel()
	}()

	t.Cleanup(func() {
		cancel()
		<-p.done
	})

	return p
}

// builtinInstallComponent installs the SCORCH user component as a regular
// file on the PATH of phenix and phenix ui, and removes it when the test ends.
func builtinInstallComponent(t *testing.T) {
	t.Helper()

	const script = `#!/bin/bash
set -eu
d="$PHENIX_FILES_DIR/scorch/run-$3/` + builtinComponent + `"
mkdir -p "$d"
k="$1-$2-$3-$4-$5"
printf '%s\n' "$@" > "$d/$k.args"
cat > "$d/$k.stdin.json"
env | grep '^PHENIX_' | sort > "$d/$k.env"
echo "` + builtinComponent + ` $1 ok"
echo '{"level":"INFO","msg":"` + builtinComponent + ` stderr"}' >&2
`

	path := "/usr/local/bin/phenix-scorch-component-" + builtinComponent

	if err := os.WriteFile(path, []byte(script), 0o755); err != nil { //nolint:gosec // executable component
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = os.Remove(path) })
}

// builtinPhenixCommit is the commit `phenix version` reports.
func builtinPhenixCommit(t *testing.T) string {
	t.Helper()

	out := phenix(t, "version")

	m := regexp.MustCompile(`\(commit ([^)]*)\)`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("phenix version reports no commit: %s", out)
	}

	return m[1]
}

// builtinArchive lists the entries of a gzipped tar archive.
func builtinArchive(t *testing.T, path string) []string {
	t.Helper()

	f, err := os.Open(path) //nolint:gosec // product output
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("%s is not gzipped: %v", path, err)
	}

	var (
		tr    = tar.NewReader(gz)
		names []string
	)

	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}

		names = append(names, h.Name)
	}

	return names
}

// builtinGlobOne returns the single file matching pattern.
func builtinGlobOne(t *testing.T, pattern string) string {
	t.Helper()

	matches, err := filepath.Glob(pattern)
	if err != nil || len(matches) != 1 {
		t.Fatalf("want one file matching %s, have %v (%v)", pattern, matches, err)
	}

	return matches[0]
}

// builtinTerminal is one SCORCH web terminal phenix ui reports.
type builtinTerminal struct {
	Exp   string `json:"exp"`
	Run   int    `json:"run"`
	Stage string `json:"stage"`
	Name  string `json:"name"`
}

func builtinTerminals(t *testing.T, exp string) []builtinTerminal {
	t.Helper()

	var body struct {
		Terminals []builtinTerminal `json:"terminals"`
	}

	apiOK(t, http.MethodGet, "/experiments/"+exp+"/scorch/terminals", nil, &body)

	return body.Terminals
}

// builtinPipelineNode is one node of a SCORCH pipeline phenix ui reports.
type builtinPipelineNode struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

// builtinComponentOutput returns a finished SCORCH component's output from
// phenix ui, or "" while it still runs.
func builtinComponentOutput(t *testing.T, exp string, run int, stage, cmp string) string {
	t.Helper()

	var body struct {
		Output *string `json:"output"`
	}

	apiOK(t, http.MethodGet, fmt.Sprintf("/experiments/%s/scorch/components/%d/0/%s/%s", exp, run, stage, cmp), nil, &body)

	if body.Output == nil {
		return ""
	}

	return *body.Output
}

// ---------------------------------------------------------------------------
// state of health

// builtinSOHState is one SOH check result.
type builtinSOHState struct {
	Success  string         `json:"success"`
	Error    string         `json:"error"`
	Metadata map[string]any `json:"metadata"`
}

// builtinSOHHost is one host's SOH results.
type builtinSOHHost struct {
	Hostname     string            `json:"hostname"`
	CPULoad      string            `json:"cpuLoad"`
	Networking   []builtinSOHState `json:"networking"`
	Reachability []builtinSOHState `json:"reachability"`
	Files        []builtinSOHState `json:"files"`
	FilesAbsent  []builtinSOHState `json:"filesAbsent"`
	Services     []builtinSOHState `json:"services"`
	Processes    []builtinSOHState `json:"processes"`
	Listeners    []builtinSOHState `json:"listeners"`
	CustomTests  []builtinSOHState `json:"customTests"`
}

func (h builtinSOHHost) all() []builtinSOHState {
	groups := [][]builtinSOHState{
		h.Networking, h.Reachability, h.Files, h.FilesAbsent, h.Services, h.Processes, h.Listeners, h.CustomTests,
	}

	n := 0
	for _, g := range groups {
		n += len(g)
	}

	all := make([]builtinSOHState, 0, n)
	for _, g := range groups {
		all = append(all, g...)
	}

	return all
}

// builtinSOHStatus is status.apps.soh.
type builtinSOHStatus struct {
	Initialized bool             `json:"initialized"`
	Hosts       []builtinSOHHost `json:"hosts"`
}

func (s builtinSOHStatus) host(name string) (builtinSOHHost, bool) {
	for _, h := range s.Hosts {
		if h.Hostname == name {
			return h, true
		}
	}

	return builtinSOHHost{}, false
}

// builtinSuccessCount counts the states that succeeded with exactly msg.
func builtinSuccessCount(states []builtinSOHState, msg string) int {
	n := 0

	for _, s := range states {
		if s.Success == msg {
			n++
		}
	}

	return n
}

// builtinWantSuccess checks that states hold at least count successes msg.
func builtinWantSuccess(t *testing.T, host, kind string, states []builtinSOHState, count int, msg string) {
	t.Helper()

	if got := builtinSuccessCount(states, msg); got < count {
		t.Errorf("SOH %s %s: %d x %q, want %d: %+v", host, kind, got, msg, count, states)
	}
}

// builtinWantNoSOHErrors checks that no SOH check of a host failed.
func builtinWantNoSOHErrors(t *testing.T, h builtinSOHHost) {
	t.Helper()

	for _, s := range h.all() {
		if s.Error != "" {
			t.Errorf("SOH %s: check failed: %q (%v)", h.Hostname, s.Error, s.Metadata)
		}
	}

	if _, err := strconv.ParseFloat(h.CPULoad, 64); err != nil {
		t.Errorf("SOH %s: cpuLoad = %q, want a load average", h.Hostname, h.CPULoad)
	}
}

// builtinSOHView is GET /experiments/{name}/soh.
type builtinSOHView struct {
	Started     bool `json:"started"`
	Initialized bool `json:"soh_initialized"`
	Running     bool `json:"soh_running"`
	Nodes       []struct {
		Label  string `json:"label"`
		Status string `json:"status"`
		SOH    *struct {
			Hostname string `json:"hostname"`
			Errors   bool   `json:"errors"`
		} `json:"soh"`
	} `json:"nodes"`
}

func builtinSOHREST(t *testing.T, exp string) builtinSOHView {
	t.Helper()

	var view builtinSOHView

	apiOK(t, http.MethodGet, "/experiments/"+exp+"/soh", nil, &view)

	return view
}

// builtinWantSOHNode checks one VM node of the REST SOH view.
func builtinWantSOHNode(t *testing.T, view builtinSOHView, vm string, errs bool) {
	t.Helper()

	for _, n := range view.Nodes {
		if n.Label != vm {
			continue
		}

		if n.Status != "running" {
			t.Errorf("REST SOH node %s status = %q, want running", vm, n.Status)
		}

		if n.SOH == nil {
			t.Errorf("REST SOH node %s has no SOH results", vm)
		} else if n.SOH.Errors != errs {
			t.Errorf("REST SOH node %s errors = %t, want %t", vm, n.SOH.Errors, errs)
		}

		return
	}

	t.Errorf("REST SOH view has no node %s: %+v", vm, view.Nodes)
}

// ---------------------------------------------------------------------------
// disks

// builtinMakeP3Image creates a hollow qcow2 disk with an msdos label and three
// ext4 partitions, and removes it when the test ends (after its experiments,
// whose snapshots it backs).
func builtinMakeP3Image(t *testing.T) {
	t.Helper()

	dst := filepath.Join(cfg.imagesDir, builtinP3Image)
	raw := filepath.Join(cfg.workDir, "e2e-builtin-p3.raw")

	t.Cleanup(func() { _ = os.Remove(dst) })

	defer os.Remove(raw)

	if err := run("truncate", "-s", "96M", raw); err != nil {
		t.Fatal(err)
	}

	sfdisk := exec.Command("sfdisk", "-q", raw)
	sfdisk.Stdin = strings.NewReader("label: dos\n,24M,83\n,24M,83\n,,83\n")

	if out, err := sfdisk.CombinedOutput(); err != nil {
		t.Fatalf("sfdisk: %v\n%s", err, out)
	}

	loop, err := hostCmd("losetup", "-fP", "--show", raw)
	if err != nil {
		t.Fatalf("losetup: %v: %s", err, loop)
	}

	for p := 1; p <= 3; p++ {
		if err := run("mkfs.ext4", "-q", "-L", fmt.Sprintf("e2e-p%d", p), fmt.Sprintf("%sp%d", loop, p)); err != nil {
			_ = run("losetup", "-d", loop)
			t.Fatal(err)
		}
	}

	if err := run("losetup", "-d", loop); err != nil {
		t.Fatal(err)
	}

	if err := run("qemu-img", "convert", "-q", "-f", "raw", "-O", "qcow2", raw, dst); err != nil {
		t.Fatal(err)
	}
}

// ===========================================================================
// Real guests: startup, ntp, serial, tap, soh and scorch on two ntp.qc2 guests

// builtinGuestNodes are two real guests on VLAN LAN (behind the tap app's
// tap as their gateway) with a serial interface each on VLAN SERIAL.
func builtinGuestNodes() []builtinNode {
	guest := func(host, addr, serial string, mtu int, netExtra string) builtinNode {
		return builtinNode{
			host:   host,
			image:  cfg.guest,
			memory: 1024,
			ifaces: []builtinIface{
				{name: "eth0", vlan: "LAN", addr: addr, gw: "10.10.0.200", dns: []string{"10.10.0.53"}, mtu: mtu},
				{name: "eth1", vlan: "SERIAL", addr: serial, serial: true, device: "/dev/ttyE2E0"},
			},
			netExtra: netExtra,
		}
	}

	return []builtinNode{
		guest("g1", "10.10.0.1", "10.30.0.1", 0, `
      routes:
      - destination: 10.99.0.0/24
        next: 10.10.0.2`),
		guest("g2", "10.10.0.2", "10.30.0.2", 1400, ""),
	}
}

// builtinGuestApps is the scenario of the real-guest experiment. The tap app
// gives the guests a gateway to ping; SOH runs every check the guest image
// supports, with the phenix service checked on g2 only, since g1's domain
// join fails it (see builtinCheckStartupGuests); SCORCH run 0 runs the tap,
// soh, user and pause components, run 1 fails in start, run 2 breaks into a
// web terminal and run 3 is short, for the REST output.
func builtinGuestApps() string {
	return fmt.Sprintf(`
  - name: startup
    hosts:
    - hostname: g1
      metadata:
        domain_controller:
          domain: e2e.example
          username: e2e-admin
          password: e2e-fake-password
  - name: tap
    metadata:
      taps:
      - bridge: phenix
        vlan: LAN
        ip: 10.10.0.200/24
  - name: ntp
    metadata:
      defaultSource:
        hostname: g1
        interface: eth0
    hosts:
    - hostname: g1
      metadata:
        server: ntpd
    - hostname: g2
      metadata:
        client: ntp
  - name: soh
    metadata:
      c2Timeout: 4m
      testReachability: full
      hostFiles:
        g1: [/etc/ntp.conf, /etc/phenix/startup/1_hostname-start.sh]
        g2: [/etc/phenix/serial-startup.bash]
      hostFilesAbsent:
        g1: [/etc/e2e-builtin-absent]
      hostServices:
        g1: [ntp, miniccc]
        g2: [ntp, serial-startup, phenix]
      hostProcesses:
        g1: [ntpd]
      hostListeners:
        g1: [":123"]
      testCustomReachability:
      - src: g2
        dst: 10.10.0.1
        proto: udp
        port: 123
        wait: 5s
        udpPacketBase64: %s
      hostCustomTests:
        g1:
        - name: ntp server config
          testScript: "grep -c '^server 127.127.1.1' /etc/ntp.conf"
          testStdout: "1"
  - name: scorch
    metadata:
      runs:
      - start: [tap1, soh1, echo1, pause1]
        stop: [tap1, echo1]
        cleanup: [echo1]
      - start: [tap1, fail1]
        stop: [tap1]
      - start: [brk1]
      - start: [echo1, pause2]
      components:
      - name: tap1
        type: tap
        metadata:
          bridge: phenix
          vlan: LAN
          ip: 10.10.0.210/24
      - name: soh1
        type: soh
        metadata:
          checks: [network-config, processes, cpu-load]
          c2Timeout: 1m
          failOnError: true
      - name: echo1
        type: %s
        metadata:
          greeting: hi
      - name: pause1
        type: pause
        metadata:
          duration: 5s
      - name: pause2
        type: pause
        metadata:
          duration: 2s
      - name: fail1
        type: pause
        metadata:
          duration: 1s
          failStages: [start]
      - name: brk1
        type: break
        metadata:
          tap:
            bridge: phenix
            vlan: LAN
            ip: 10.10.0.220/24`, builtinNTPRequest(), builtinComponent)
}

// TestBuiltinGuestApps boots two real guests once and checks every built-in
// app on them (ST-01, ST-09, NT-01, SE-01, TA-01, SO-01, SC-01..SC-05).
func TestBuiltinGuestApps(t *testing.T) {
	requireImage(t, cfg.guest)
	builtinInstallComponent(t)
	builtinCleanTaps(t)

	const name = "e2e-ba-guest"

	exp := builtinExperiment(t, name, builtinTopology(builtinGuestNodes()...), builtinScenario(builtinGuestApps()))

	t.Run("SE-01 configure stages the serial injections", func(t *testing.T) { builtinCheckSerialInjects(t, name) })

	began := time.Now()
	out := exp.start()
	t.Logf("experiment start (through SOH post-start) took %v", time.Since(began).Round(time.Second))

	t.Run("RG-01 every app ran in pre-start and post-start", func(t *testing.T) { builtinCheckAppsRan(t, out) })
	t.Run("ST-01 startup renders and injects the scripts", func(t *testing.T) { builtinCheckStartupFiles(t, exp) })

	registered := t.Run("ST-01 guests register with their startup hostnames", func(t *testing.T) {
		waitCCClients(t, name, "g1", "g2")
		t.Logf("both guests registered %v after start", time.Since(began).Round(time.Second))
	})

	// Every later check talks to the guests over C2; without them each one
	// would only wait out its own C2 timeout.
	if !registered {
		t.FailNow()
	}

	t.Run("ST-01 startup configured each guest", func(t *testing.T) { builtinCheckStartupGuests(t, name) })
	t.Run("ST-09 startup sets the interface MTU", func(t *testing.T) { builtinCheckMTU(t, name) })
	t.Run("NT-01 ntp renders and injects server and client configs", func(t *testing.T) { builtinCheckNTPFiles(t, exp) })
	// ntp.qc2 has no socat, which the serial app runs; TestBuiltinSerialLink
	// checks the link itself on the tools image.
	t.Run("SE-01 serial units in the guests", func(t *testing.T) { builtinCheckSerial(t, name, false) })
	t.Run("TA-01 tap app tap is on the VLAN in its own namespace", func(t *testing.T) { builtinCheckTapApp(t, exp) })
	t.Run("SO-01 SOH post-start results", func(t *testing.T) { builtinCheckSOHGuests(t, name, out) })
	t.Run("SO-01 SOH REST view", func(t *testing.T) {
		view := builtinSOHREST(t, name)
		if !view.Started || !view.Initialized || view.Running {
			t.Errorf("REST SOH started=%t initialized=%t running=%t, want true true false", view.Started, view.Initialized, view.Running)
		}

		builtinWantSOHNode(t, view, "g1", false)
		builtinWantSOHNode(t, view, "g2", false)
	})
	t.Run("SC-01 CLI run 0 with tap, soh, user and pause components", func(t *testing.T) { builtinCheckScorchRun0(t, name) })
	t.Run("SC-02 CLI run 1 fails in start and still runs stop", func(t *testing.T) { builtinCheckScorchRun1(t, name) })

	t.Run("SC-05 an unknown run ID is refused", func(t *testing.T) {
		out := phenixFails(t, "experiment", "scorch", name, "--run", "9")
		wantContains(t, "phenix experiment scorch -r 9", out, "invalid Scorch run ID")
	})

	t.Run("SC-04 REST run 3 and component output", func(t *testing.T) { builtinCheckScorchREST(t, name) })
	t.Run("SC-03 REST run 2 break terminal and cancel", func(t *testing.T) { builtinCheckScorchBreak(t, name) })
	t.Run("NT-01 the client reaches the server, which syncs to its local clock", func(t *testing.T) { builtinCheckNTPSync(t, name) })

	t.Run("TA-01 stop removes the tap and SOH clears the C2 responses", func(t *testing.T) {
		var tap builtinTapStatus

		builtinAppStatus(t, name, "tap", &tap)
		exp.with(t).stop()
		builtinWantNoNamespace(t, name)

		for _, tp := range tap.Taps {
			builtinWaitTapsGone(t, tp.Name)
		}

		entries, err := os.ReadDir(filepath.Join(cfg.imagesDir, name, "miniccc_responses"))
		if err == nil && len(entries) > 0 {
			t.Errorf("C2 responses of %s remain after stop: %d entries", name, len(entries))
		}
	})

	t.Run("SC-05 a stopped experiment is refused", func(t *testing.T) {
		out := phenixFails(t, "experiment", "scorch", name, "--run", "0")
		wantContains(t, "phenix experiment scorch on a stopped experiment", out, "is not running")
	})
}

// TestBuiltinSerialLink boots two tools guests, whose image has the socat the
// serial app runs, and checks the multicast serial link between them (SE-01).
// The serial interface is each guest's only one, with the addresses and
// device of the TestBuiltinGuestApps guests.
func TestBuiltinSerialLink(t *testing.T) {
	requireImage(t, cfg.tools)

	const name = "e2e-ba-serial"

	guest := func(host, addr string) builtinNode {
		return builtinNode{
			host:   host,
			image:  cfg.tools,
			memory: 1024,
			ifaces: []builtinIface{{name: "eth0", vlan: "SERIAL", addr: addr, serial: true, device: "/dev/ttyE2E0"}},
		}
	}

	exp := builtinExperiment(t, name, builtinTopology(guest("g1", "10.30.0.1"), guest("g2", "10.30.0.2")), "")
	exp.start()

	if !t.Run("SE-01 guests register", func(t *testing.T) { waitCCClients(t, name, "g1", "g2") }) {
		t.FailNow()
	}

	t.Run("SE-01 serial units and links in the guests", func(t *testing.T) { builtinCheckSerial(t, name, true) })

	t.Run("stop", func(t *testing.T) {
		exp.with(t).stop()
		builtinWantNoNamespace(t, name)
	})
}

// builtinCheckSerialInjects checks the injections the serial app adds at
// configure (create) time.
func builtinCheckSerialInjects(t *testing.T, name string) {
	t.Helper()

	base := builtinBaseDir(name)

	for _, vm := range []string{"g1", "g2"} {
		injections, _ := builtinSpecNode(t, name, vm)["injections"].([]any)

		have := make(map[string]map[string]any)

		for _, i := range injections {
			inj, _ := i.(map[string]any)
			dst, _ := inj["dst"].(string)
			have[dst] = inj
		}

		for dst, src := range map[string]string{
			"/etc/phenix/serial-startup.bash":                                    base + "/startup/" + vm + "-serial.bash",
			"/etc/systemd/system/serial-startup.service":                         base + "/startup/serial-startup.service",
			"/etc/systemd/system/multi-user.target.wants/serial-startup.service": base + "/startup/symlinks/serial-startup.service",
		} {
			inj, ok := have[dst]
			if !ok {
				t.Errorf("%s has no serial injection to %s: %v", vm, dst, injections)

				continue
			}

			if inj["src"] != src {
				t.Errorf("%s injects %v to %s, want %s", vm, inj["src"], dst, src)
			}
		}

		if p := have["/etc/phenix/serial-startup.bash"]["permissions"]; p != "0755" {
			t.Errorf("%s serial script injection permissions = %v, want 0755", vm, p)
		}
	}
}

// builtinCheckAppsRan checks that experiment start ran each app's stages.
func builtinCheckAppsRan(t *testing.T, out string) {
	t.Helper()

	for _, app := range []string{"ntp", "serial", "startup", "vrouter"} {
		for _, stage := range []string{"pre-start", "post-start"} {
			wantContains(t, "experiment start output", out, fmt.Sprintf("[✓] '%s' default app (%s)", app, stage))
		}
	}

	for _, app := range []string{"tap", "soh", "scorch"} {
		for _, stage := range []string{"pre-start", "post-start"} {
			wantContains(t, "experiment start output", out, fmt.Sprintf("[✓] '%s' user app (%s)", app, stage))
		}
	}

	// An app that failed a stage logs "[✗] '<app>' ..."; SOH's own failed
	// checks are SO-01's concern.
	wantLacks(t, "experiment start output", out, "[✗] '")
}

// builtinCheckStartupFiles checks the startup scripts phenix renders and the
// minimega injections it asks for.
func builtinCheckStartupFiles(t *testing.T, exp *experiment) {
	t.Helper()

	dir := filepath.Join(builtinBaseDir(exp.name), "startup")

	wantContains(t, "g1-interfaces.sh", readFile(t, filepath.Join(dir, "g1-interfaces.sh")),
		`ip addr add 10.10.0.1/24 dev "$dev"`,
		`ip route add default via 10.10.0.200 dev "$dev"`,
		`echo "nameserver 10.10.0.53" >> /etc/resolv.conf`,
		`ip route add 10.99.0.0/24 via 10.10.0.2`,
		`ip addr add 10.30.0.1/24 dev "$dev"`,
	)
	wantContains(t, "g1-hostname.sh", readFile(t, filepath.Join(dir, "g1-hostname.sh")),
		`sed -i -E 's/(^|[[:space:]])phenix([.[:space:]]|$)/\1g1\2/g; s/(^|[[:space:]])phenix([.[:space:]]|$)/\1g1\2/g' /etc/hosts`,
		"hostnamectl set-hostname g1")
	wantContains(t, "g1-timezone.sh", readFile(t, filepath.Join(dir, "g1-timezone.sh")),
		"timedatectl set-timezone Etc/UTC")
	wantContains(t, "g1-domain.sh", readFile(t, filepath.Join(dir, "g1-domain.sh")),
		"realm join --user=e2e-admin e2e.example")

	script := exp.with(t).mmScript()

	for vm, wants := range map[string][]string{
		"g1": {"1_hostname", "2_timezone", "3_interfaces", "4_domain"},
		"g2": {"1_hostname", "2_timezone", "3_interfaces"},
	} {
		line := builtinInjectLine(script, vm)

		for _, w := range wants {
			src := fmt.Sprintf(`"%s/%s-%s.sh":"/etc/phenix/startup/%s-start.sh"`, dir, vm, strings.SplitN(w, "_", 2)[1], w)
			wantContains(t, vm+" disk inject line", line, src)
		}
	}

	wantLacks(t, "g2 disk inject line", builtinInjectLine(script, "g2"), "4_domain-start.sh")
}

// builtinCheckStartupGuests checks inside each guest what the startup
// scripts did.
//
// g1's last script, 4_domain-start.sh, runs realm join for a domain the
// experiment has no controller for, so the join fails, and phenix-start.sh
// exits with its last script's status (api/image/constants.go), which leaves
// the oneshot phenix.service failed on g1 and active on g2.
func builtinCheckStartupGuests(t *testing.T, name string) {
	t.Helper()

	const started = "/etc/phenix/startup/[0-9]_[a-z]*-start.sh"

	for vm, w := range map[string]struct{ addrs, scripts, count, last, service string }{
		"g1": {
			"10.10.0.1/24 10.30.0.1/24",
			"1_hostname-start.sh 2_timezone-start.sh 3_interfaces-start.sh 4_domain-start.sh",
			"4",
			"/etc/phenix/startup/4_domain-start.sh",
			"failed",
		},
		"g2": {
			"10.10.0.2/24 10.30.0.2/24",
			"1_hostname-start.sh 2_timezone-start.sh 3_interfaces-start.sh",
			"3",
			"/etc/phenix/startup/3_interfaces-start.sh",
			"active",
		},
	} {
		got := builtinGuest(
			t,
			name,
			vm,
			builtinCheck{"hostname", "hostname"},
			builtinCheck{"hosts entry", `awk '$1 == "127.0.1.1" {print $2}' /etc/hosts`},
			builtinCheck{"hosts phenix", "grep -c phenix /etc/hosts"},
			builtinCheck{"addresses", "ip -4 -o addr show scope global | awk '{print $4}' | sort | tr '\\n' ' '"},
			builtinCheck{"eth0 address", "ip -4 -o addr show dev eth0 | awk '{print $4}'"},
			builtinCheck{"default route", "ip route show default | grep -c 'default via 10.10.0.200 dev eth0'"},
			builtinCheck{"timezone", "timedatectl show -p Timezone --value"},
			builtinCheck{"phenix service", "systemctl is-active phenix.service"},
			builtinCheck{"staged scripts", "ls /etc/phenix/startup | sort | tr '\\n' ' '"},
			builtinCheck{"scripts run", "journalctl -b -u phenix.service --no-pager -o cat | grep -cx '" + started + "'"},
			builtinCheck{"last script", "journalctl -b -u phenix.service --no-pager -o cat | grep -x '" + started + "' | tail -n 1"},
			builtinCheck{"nameserver", "grep -c '^nameserver 10.10.0.53$' /etc/resolv.conf"},
			builtinCheck{"static route", "ip route show 10.99.0.0/24 | grep -c 'via 10.10.0.2'"},
		)

		want := map[string]string{
			"hostname":       vm,
			"hosts phenix":   "0",
			"addresses":      w.addrs,
			"eth0 address":   strings.Fields(w.addrs)[0],
			"default route":  "1",
			"timezone":       "Etc/UTC",
			"phenix service": w.service,
			"staged scripts": w.scripts,
			"scripts run":    w.count,
			"last script":    w.last,
			"nameserver":     "1",
		}

		if vm == "g1" {
			want["static route"] = "1"
		}

		// The hostname script renames the image's "phenix" in /etc/hosts; an
		// image without a 127.0.1.1 entry has nothing to rename.
		if got["hosts entry"] != "" {
			want["hosts entry"] = vm
		}

		builtinWantGuest(t, vm, got, want)
	}
}

// builtinCheckMTU checks that the interface MTU from the topology is set in
// the guest.
func builtinCheckMTU(t *testing.T, name string) {
	t.Helper()

	wantContains(t, "g2-interfaces.sh", readFile(t, filepath.Join(builtinBaseDir(name), "startup", "g2-interfaces.sh")),
		`ip link set dev "$dev" mtu 1400`)

	got := builtinGuest(t, name, "g2", builtinCheck{"mtu", "cat /sys/class/net/eth0/mtu"})

	builtinWantGuest(t, "g2", got, map[string]string{"mtu": "1400"})
}

// builtinCheckNTPFiles checks the ntp app's rendered configs and injections.
func builtinCheckNTPFiles(t *testing.T, exp *experiment) {
	t.Helper()

	dir := filepath.Join(builtinBaseDir(exp.name), "ntp")

	server := readFile(t, filepath.Join(dir, "g1"))
	builtinWantLines(t, "g1 ntp.conf", server,
		"server 127.127.1.1 iburst prefer",
		"fudge 127.127.1.1 stratum 10",
		"restrict -4 default kod notrap nomodify nopeer noquery limited",
	)
	wantLacks(t, "g1 ntp.conf", server, "noserve")

	client := readFile(t, filepath.Join(dir, "g2"))
	builtinWantLines(t, "g2 ntp.conf", client,
		"server 10.10.0.1 iburst prefer maxpoll 6",
		"restrict -4 default kod notrap nomodify nopeer noquery noserve limited",
	)

	script := exp.with(t).mmScript()

	for _, vm := range []string{"g1", "g2"} {
		wantContains(t, vm+" disk inject line", builtinInjectLine(script, vm), fmt.Sprintf(`"%s/%s":"/etc/ntp.conf"`, dir, vm))
	}

	got := builtinGuest(t, exp.name, "g2",
		builtinCheck{"client config", "grep -cx 'server 10.10.0.1 iburst prefer maxpoll 6' /etc/ntp.conf"},
		builtinCheck{"ntp service", "systemctl is-active ntp.service"},
	)
	builtinWantGuest(t, "g2", got, map[string]string{"client config": "1", "ntp service": "active"})
}

// builtinCheckSerial checks the serial app's rendered files and, in the
// guests, its unit; with link, also the multicast serial link it starts with
// socat and data sent over it from g1 to g2.
func builtinCheckSerial(t *testing.T, name string, link bool) {
	t.Helper()

	dir := filepath.Join(builtinBaseDir(name), "startup")

	if link, err := os.Readlink(
		filepath.Join(dir, "symlinks", "serial-startup.service"),
	); err != nil ||
		link != "../serial-startup.service" {
		t.Errorf("staged serial-startup.service symlink = %q (%v), want ../serial-startup.service", link, err)
	}

	wantContains(t, "g1-serial.bash", readFile(t, filepath.Join(dir, "g1-serial.bash")),
		"until ping -c1 10.30.0.1",
		"pty,raw,echo=0,link=/dev/ttyE2E0,b9600 UDP4-DATAGRAM:224.1.0.1:8989,bind=:8989,range=10.30.0.0/24,"+
			"ip-add-membership=224.1.0.1:10.30.0.1,ip-multicast-loop=0",
	)

	checks := []builtinCheck{
		{"script mode", "stat -c %a /etc/phenix/serial-startup.bash"},
		{"wants link", "readlink /etc/systemd/system/multi-user.target.wants/serial-startup.service"},
		{"enabled", "systemctl is-enabled serial-startup.service"},
		{"active", "systemctl is-active serial-startup.service"},
	}

	want := map[string]string{
		"script mode": "755",
		"wants link":  "../serial-startup.service",
		"enabled":     "enabled",
		"active":      "active",
	}

	builtinWantGuest(t, "g1", builtinGuest(t, name, "g1", checks...), want)
	builtinWantGuest(t, "g2", builtinGuest(t, name, "g2", checks...), want)

	if !link {
		return
	}

	// The unit backgrounds socat and exits 0 whether or not socat runs, so
	// only the link and the data over it show that the serial app works.
	links := []builtinCheck{
		{"socat", "command -v socat >/dev/null && echo present || echo missing"},
		{"link", "test -L /dev/ttyE2E0 && echo link || echo none"},
	}

	for _, vm := range []string{"g1", "g2"} {
		eventually(t, time.Minute, 5*time.Second, vm+"'s serial link /dev/ttyE2E0", func() (bool, string) {
			got := builtinGuest(t, name, vm, links...)

			return got["socat"] == "present" && got["link"] == "link", fmt.Sprintf("socat %s, link %s", got["socat"], got["link"])
		})
	}

	guestShell(t, name, "g2", "rm -f /tmp/e2e-serial.out; nohup timeout 60 cat /dev/ttyE2E0 > /tmp/e2e-serial.out 2>/dev/null &")
	guestShell(t, name, "g1", "sleep 2; printf 'e2e-serial-ok\\n' > /dev/ttyE2E0")

	eventually(t, 30*time.Second, 5*time.Second, "g2 to read g1's serial line", func() (bool, string) {
		got := guestShell(t, name, "g2", "cat /tmp/e2e-serial.out")

		return strings.Contains(got, "e2e-serial-ok"), got
	})
}

// builtinCheckTapApp checks the tap app's host tap (status, namespace, OVS,
// minimega) and its data path to both guests.
func builtinCheckTapApp(t *testing.T, exp *experiment) {
	t.Helper()

	var tap builtinTapStatus

	if !builtinAppStatus(t, exp.name, "tap", &tap) {
		t.Fatal("status.apps.tap is missing")
	}

	if host := trimHostSuffixes(headnode(t)); tap.Host != host {
		t.Errorf("status.apps.tap.host = %q, want the headnode %q", tap.Host, host)
	}

	if len(tap.Taps) != 1 {
		t.Fatalf("status.apps.tap.taps = %+v, want one tap", tap.Taps)
	}

	tp := tap.Taps[0]

	if !regexp.MustCompile(`^[0-9A-Za-z]{8}-tapapp$`).MatchString(tp.Name) || tp.VLAN != "LAN" || tp.IP != "10.10.0.200/24" {
		t.Errorf("status.apps.tap.taps[0] = %+v, want <8 chars>-tapapp on LAN with 10.10.0.200/24", tp)
	}

	builtinWantHostTap(t, tp.Name, "10.10.0.200/24", exp.with(t).doc().Status.VLANs["LAN"])

	var listed bool

	for _, row := range mmTable(t, exp.name, "tap") {
		listed = listed || row["tap"] == tp.Name
	}

	if !listed {
		t.Errorf("minimega does not list host tap %s in namespace %s", tp.Name, exp.name)
	}

	builtinPing(t, tp.Name, "10.10.0.1")
	builtinPing(t, tp.Name, "10.10.0.2")

	eventually(t, 30*time.Second, time.Second, "minimega to learn g1's address from ARP", func() (bool, string) {
		ip := vmInfo(t, exp.name)["g1"]["ip"]

		return strings.Contains(ip, "10.10.0.1"), "vm info ip " + ip
	})
}

// builtinCheckSOHGuests checks the results SOH recorded in post-start.
func builtinCheckSOHGuests(t *testing.T, name, out string) {
	t.Helper()

	var soh builtinSOHStatus

	if !builtinAppStatus(t, name, "soh", &soh) || !soh.Initialized {
		t.Fatalf("status.apps.soh = %+v, want initialized", soh)
	}

	if len(soh.Hosts) != 2 {
		t.Errorf("SOH has results for %d hosts, want 2 (g1, g2)", len(soh.Hosts))
	}

	g1, ok1 := soh.host("g1")
	g2, ok2 := soh.host("g2")

	if !ok1 || !ok2 {
		t.Fatalf("SOH hosts = %+v, want g1 and g2", soh.Hosts)
	}

	for h, addr := range map[*builtinSOHHost]string{&g1: "10.10.0.1", &g2: "10.10.0.2"} {
		builtinWantSuccess(t, h.Hostname, "networking", h.Networking, 1, "IP "+addr+" configured")
		builtinWantSuccess(t, h.Hostname, "networking", h.Networking, 1, "gateway 10.10.0.200 configured")
		builtinWantSuccess(t, h.Hostname, "networking", h.Networking, 1, "gateway 10.10.0.200 is up")
		builtinWantNoSOHErrors(t, *h)
	}

	builtinWantSuccess(t, "g1", "reachability", g1.Reachability, 1, "pinging 10.10.0.2 succeeded")
	builtinWantSuccess(t, "g2", "reachability", g2.Reachability, 1, "pinging 10.10.0.1 succeeded")
	builtinWantSuccess(t, "g2", "reachability", g2.Reachability, 1, "connection to udp://10.10.0.1:123 succeeded")
	builtinWantSuccess(t, "g1", "files", g1.Files, 2, "file found")
	builtinWantSuccess(t, "g2", "files", g2.Files, 1, "file found")
	builtinWantSuccess(t, "g1", "filesAbsent", g1.FilesAbsent, 1, "file not found")
	builtinWantSuccess(t, "g1", "services", g1.Services, 2, "service running")
	builtinWantSuccess(t, "g2", "services", g2.Services, 3, "service running")
	builtinWantSuccess(t, "g1", "processes", g1.Processes, 1, "process running")
	builtinWantSuccess(t, "g1", "listeners", g1.Listeners, 1, "listening on port")
	builtinWantSuccess(t, "g1", "customTests", g1.CustomTests, 1, "STDOUT contained 1")

	wantContains(t, "experiment start output", out, "starting SOH checks")
	wantLacks(t, "experiment start output", out, "Error running initial SoH checks")
}

// builtinCheckScorchRun0 runs SCORCH run 0 from the CLI and checks its
// components and artifacts.
func builtinCheckScorchRun0(t *testing.T, name string) {
	t.Helper()

	before := builtinTapLeftovers(t)
	proc := builtinStart(t, "experiment", "scorch", name, "--run", "0")

	var tapcomp string

	eventually(t, time.Minute, 500*time.Millisecond, "the SCORCH tap component's host tap", func() (bool, string) {
		if taps := builtinNewTaps(t, before, "-tapcomp"); len(taps) == 1 {
			tapcomp = taps[0]

			return true, ""
		}

		return proc.finished(), "no -tapcomp tap yet"
	})

	if tapcomp != "" {
		builtinWantHostTap(t, tapcomp, "10.10.0.210/24", 0)
		builtinPing(t, tapcomp, "10.10.0.1")
	} else {
		t.Error("SCORCH run 0 finished before its tap component's tap appeared")
	}

	out, err := proc.wait(t, 5*time.Minute)
	if err != nil {
		t.Fatalf("phenix experiment scorch -r 0: %v\n%s", err, out)
	}

	if tapcomp != "" {
		builtinWaitTapsGone(t, tapcomp)
	}

	builtinCheckRun0Artifacts(t, name)
	builtinCheckEcho(t, name, 0)

	var scorch map[string]any
	if builtinAppStatus(t, name, "scorch", &scorch) {
		t.Errorf("status.apps.scorch = %v after the run, want it removed", scorch)
	}

	if builtinAppRunning(t, name, "scorch") {
		t.Error("status.appRunningStageStatus.scorch is still true after the run")
	}
}

// builtinCheckRun0Artifacts checks the run directory, run info and archive.
func builtinCheckRun0Artifacts(t *testing.T, name string) {
	t.Helper()

	runDir := filepath.Join(builtinFilesDir(name), "scorch", "run-0")

	var soh builtinSOHStatus

	if err := json.Unmarshal([]byte(readFile(t, filepath.Join(runDir, "soh1", "loop-0-count-0", "soh.json"))), &soh); err != nil {
		t.Errorf("soh.json does not parse: %v", err)
	} else if len(soh.Hosts) != 2 {
		t.Errorf("soh.json has %d hosts, want 2: %+v", len(soh.Hosts), soh.Hosts)
	} else {
		for _, h := range soh.Hosts {
			builtinWantNoSOHErrors(t, h)
			builtinWantSuccess(t, h.Hostname, "networking", h.Networking, 1, "gateway 10.10.0.200 is up")
		}
	}

	infoPath := builtinGlobOne(t, filepath.Join(runDir, name+"-scorch-run-0-*.json"))

	var info struct {
		Experiment struct {
			Name string `json:"name"`
		} `json:"experiment"`
		Run struct {
			Index int `json:"index"`
		} `json:"run"`
		Phenix struct {
			Commit string `json:"commit"`
		} `json:"phenix_version"`
		Minimega string `json:"minimega_version"`
	}

	if err := json.Unmarshal([]byte(readFile(t, infoPath)), &info); err != nil {
		t.Fatalf("run info %s does not parse: %v", infoPath, err)
	}

	if info.Experiment.Name != name || info.Run.Index != 0 {
		t.Errorf("run info experiment %q run %d, want %s 0", info.Experiment.Name, info.Run.Index, name)
	}

	if mmVersion := mmText(t, "", "version"); info.Minimega != mmVersion {
		t.Errorf("run info minimega_version = %q, want minimega's %q", info.Minimega, mmVersion)
	}

	if commit := builtinPhenixCommit(t); info.Phenix.Commit != commit {
		t.Errorf("run info phenix_version.commit = %q, want %q", info.Phenix.Commit, commit)
	}

	archive := builtinGlobOne(t, filepath.Join(builtinFilesDir(name), "scorch-run-0_*.tgz"))
	entries := strings.Join(builtinArchive(t, archive), "\n")

	wantContains(t, "run 0 archive", entries,
		filepath.Base(infoPath), "soh1/loop-0-count-0/soh.json", builtinComponent+"/start-echo1-0-0-0.args")
}

// builtinCheckEcho checks what the SCORCH user component recorded in a run.
func builtinCheckEcho(t *testing.T, name string, run int) {
	t.Helper()

	dir := filepath.Join(builtinFilesDir(name), "scorch", fmt.Sprintf("run-%d", run), builtinComponent)
	key := fmt.Sprintf("start-echo1-%d-0-0", run)

	if args := readFile(t, filepath.Join(dir, key+".args")); args != fmt.Sprintf("start\necho1\n%d\n0\n0\n", run) {
		t.Errorf("component arguments = %q, want stage, name, run, loop and count", args)
	}

	env := readFile(t, filepath.Join(dir, key+".env"))
	builtinWantLines(t, "component environment", env,
		"PHENIX_DIR="+builtinPhenixBase,
		"PHENIX_FILES_DIR="+builtinFilesDir(name),
		"PHENIX_LOG_FILE=stderr",
		"PHENIX_DRYRUN=false",
		"PHENIX_HOSTNAME_SUFFIXES="+appsHostnameSuffixes(),
	)

	m := regexp.MustCompile(`(?m)^PHENIX_SCORCH_STARTTIME=(.*)$`).FindStringSubmatch(env)
	if m == nil {
		t.Error("component environment has no PHENIX_SCORCH_STARTTIME")
	} else if _, err := time.Parse(time.RubyDate, m[1]); err != nil {
		t.Errorf("PHENIX_SCORCH_STARTTIME %q is not in RubyDate format: %v", m[1], err)
	}

	var doc struct {
		Spec struct {
			Scenario struct {
				Apps []struct {
					Name     string `json:"name"`
					Metadata struct {
						Components []struct {
							Name     string         `json:"name"`
							Metadata map[string]any `json:"metadata"`
						} `json:"components"`
					} `json:"metadata"`
				} `json:"apps"`
			} `json:"scenario"`
		} `json:"spec"`
	}

	if err := json.Unmarshal([]byte(readFile(t, filepath.Join(dir, key+".stdin.json"))), &doc); err != nil {
		t.Fatalf("component stdin is not JSON: %v", err)
	}

	var greeting any

	for _, a := range doc.Spec.Scenario.Apps {
		for _, c := range a.Metadata.Components {
			if a.Name == "scorch" && c.Name == "echo1" {
				greeting = c.Metadata["greeting"]
			}
		}
	}

	if greeting != "hi" {
		t.Errorf("component stdin carries echo1 metadata greeting %v, want hi", greeting)
	}
}

// builtinCheckScorchRun1 checks that a start failure still runs stop.
func builtinCheckScorchRun1(t *testing.T, name string) {
	t.Helper()

	before := builtinTapLeftovers(t)

	out := phenixFails(t, "experiment", "scorch", name, "--run", "1")
	wantContains(t, "phenix experiment scorch -r 1", out, "failing as instructed")

	eventually(t, builtinTapGone, time.Second, "the run's tap to be removed by its stop stage", func() (bool, string) {
		left := builtinNewTaps(t, before, "-tapcomp")

		return len(left) == 0, fmt.Sprintf("left %v", left)
	})

	if builtinAppRunning(t, name, "scorch") {
		t.Error("status.appRunningStageStatus.scorch is still true after the failed run")
	}
}

// builtinCheckScorchREST runs SCORCH run 3 through phenix ui and checks the
// component output it keeps.
func builtinCheckScorchREST(t *testing.T, name string) {
	t.Helper()

	path := "/experiments/" + name + "/scorch/pipelines/3"

	if code, body := api(t, http.MethodPost, path, nil); code != http.StatusNoContent {
		t.Fatalf("POST %s: status %d, want 204: %s", path, code, body)
	}

	eventually(t, 2*time.Minute, time.Second, "run 3's echo1 output", func() (bool, string) {
		out := builtinComponentOutput(t, name, 3, "start", "echo1")

		return strings.Contains(out, builtinComponent+" start ok"), "output " + out
	})

	eventually(t, time.Minute, time.Second, "run 3's pause2 output", func() (bool, string) {
		out := builtinComponentOutput(t, name, 3, "start", "pause2")

		return strings.Contains(out, "pausing..."), "output " + out
	})

	eventually(t, time.Minute, time.Second, "run 3 to finish", func() (bool, string) {
		return !builtinAppRunning(t, name, "scorch"), "scorch still running"
	})

	builtinCheckEcho(t, name, 3)
}

// builtinCheckScorchBreak runs SCORCH run 2 through phenix ui: the break
// component opens a web terminal in its own tap's namespace until the run is
// canceled.
func builtinCheckScorchBreak(t *testing.T, name string) {
	t.Helper()

	before := builtinTapLeftovers(t)
	path := "/experiments/" + name + "/scorch/pipelines/2"

	if code, body := api(t, http.MethodPost, path, nil); code != http.StatusNoContent {
		t.Fatalf("POST %s: status %d, want 204: %s", path, code, body)
	}

	eventually(t, time.Minute, time.Second, "the break component's web terminal", func() (bool, string) {
		for _, term := range builtinTerminals(t, name) {
			if term.Name == "brk1" && term.Stage == "start" && term.Run == 2 {
				return true, ""
			}
		}

		return false, fmt.Sprintf("terminals %+v", builtinTerminals(t, name))
	})

	taps := builtinNewTaps(t, before, "-tapbrk")
	if len(taps) != 1 {
		t.Errorf("break component host taps = %v, want one", taps)
	} else {
		builtinWantHostTap(t, taps[0], "10.10.0.220/24", 0)
		builtinPing(t, taps[0], "10.10.0.1")
	}

	// phenix ui refuses a second run of a running experiment with 409, as
	// scorchexe.Check refuses it (web/scorch/handlers.go StartPipeline).
	if code, body := api(t, http.MethodPost, path, nil); code != http.StatusConflict || !strings.Contains(string(body), "already running") {
		t.Errorf("second POST %s: status %d %s, want 409 saying SCORCH is already running", path, code, body)
	}

	var pipeline struct {
		Pipeline []builtinPipelineNode `json:"pipeline"`
	}

	apiOK(t, http.MethodGet, path+"/0", nil, &pipeline)

	// phenix shows a running break component as unstable, the state that asks
	// the user to attend to it (web/scorch/pipeline.go updatePipeline).
	if !slices.Contains(pipeline.Pipeline, builtinPipelineNode{Name: "brk1", Status: "unstable"}) {
		t.Errorf("pipeline 2 = %+v, want brk1 unstable (a running break component)", pipeline.Pipeline)
	}

	if code, body := api(t, http.MethodDelete, path, nil); code != http.StatusNoContent {
		t.Errorf("DELETE %s: status %d, want 204: %s", path, code, body)
	}

	eventually(t, 30*time.Second, time.Second, "the canceled run's terminal, tap and status to go", func() (bool, string) {
		terms := builtinTerminals(t, name)
		left := builtinNewTaps(t, before, "-tapbrk")
		running := builtinAppRunning(t, name, "scorch")

		return len(terms) == 0 && len(left) == 0 && !running,
			fmt.Sprintf("terminals %+v, taps %v, scorch running %t", terms, left, running)
	})
}

// builtinCheckNTPSync checks that g2 reaches g1's ntpd and that g1, serving
// its local clock, reports stratum 11 (the local clock's 10 plus one).
func builtinCheckNTPSync(t *testing.T, name string) {
	t.Helper()

	// ntpd needs about four 64 s polls of its local clock to select it; by the
	// time this runs the guests have been up for several minutes, so the wait
	// only covers a slow first selection. Whether g2 has also selected g1
	// (tally '*') depends on how many polls have passed, so only reach is
	// asserted.
	eventually(t, 3*time.Minute, 10*time.Second, "g1 to sync to its local clock and g2 to reach it", func() (bool, string) {
		g1 := builtinGuest(t, name, "g1", builtinCheck{"stratum", "ntpq -c rv | grep -o 'stratum=[0-9]*'"})
		g2 := builtinGuest(t, name, "g2",
			builtinCheck{"reach", `ntpq -pn | awk '$1 ~ /10\.10\.0\.1$/ {print $7}'`},
		)

		ok := g1["stratum"] == "stratum=11" && g2["reach"] != "" && g2["reach"] != "0"

		return ok, fmt.Sprintf("g1 %s, g2 reach %q", g1["stratum"], g2["reach"])
	})
}

// ===========================================================================
// Real minirouter: vrouter's router API applied in a minirouter guest

// builtinRouterNode is a minirouter on three VLANs with a static route and a
// firewall chain that drops TCP 22 from LAN2 to 10.10.0.1.
func builtinRouterNode(image string, memory int) builtinNode {
	return builtinNode{
		kind:   "Router",
		host:   "rtr",
		image:  image,
		os:     "minirouter",
		memory: memory,
		ifaces: []builtinIface{
			{name: "eth0", vlan: "LAN", addr: "10.10.0.254"},
			{name: "eth1", vlan: "LAN2", addr: "10.20.0.254", rulesetIn: "lan2-in"},
			{name: "eth2", vlan: "LAN3", addr: "10.40.0.254"},
		},
		netExtra: `
      routes:
      - destination: 10.99.0.0/24
        next: 10.20.0.1
      rulesets:
      - name: lan2-in
        default: accept
        rules:
        - id: 10
          action: drop
          protocol: tcp
          source:
            address: 10.20.0.0/24
          destination:
            address: 10.10.0.1
            port: 22`,
	}
}

// builtinVrouterApp configures DHCP on LAN3 and a DNS record on rtr.
const builtinVrouterApp = `
  - name: vrouter
    hosts:
    - hostname: rtr
      metadata:
        dhcp:
        - listenAddress: 10.40.0.254
          ranges:
          - lowAddress: 10.40.0.100
            highAddress: 10.40.0.110
          defaultRoute: 10.40.0.254
          dnsServers:
          - 10.40.0.254
        dns:
          "10.10.0.1": ra.e2e.local`

// TestBuiltinRouter boots a minirouter between three real guests (VR-02,
// SO-04): minirouter applies vrouter's config, routes between the VLANs,
// enforces the firewall chain, leases DHCP addresses and resolves the DNS
// record, and SOH reaches the router over C2 by UUID.
func TestBuiltinRouter(t *testing.T) {
	requireImage(t, cfg.guest)
	requireImage(t, cfg.router)

	const name = "e2e-ba-rtr"

	guest := func(host string, iface builtinIface) builtinNode {
		return builtinNode{host: host, image: cfg.guest, memory: 1024, ifaces: []builtinIface{iface}}
	}

	topo := builtinTopology(
		builtinRouterNode(cfg.router, 1024),
		guest("ra", builtinIface{name: "eth0", vlan: "LAN", addr: "10.10.0.1", gw: "10.10.0.254"}),
		guest("rb", builtinIface{name: "eth0", vlan: "LAN2", addr: "10.20.0.1", gw: "10.20.0.254"}),
		guest("rd", builtinIface{name: "eth0", vlan: "LAN3", proto: "dhcp"}),
	)

	scenario := builtinScenario(builtinVrouterApp + `
  - name: soh
    metadata:
      c2Timeout: 4m
      testReachability: full
      hostsToUseUUIDForC2Active: [rtr]
      skipHosts: [rd]`)

	exp := builtinExperiment(t, name, topo, scenario)

	began := time.Now()
	out := exp.start()
	t.Logf("experiment start (through SOH post-start) took %v", time.Since(began).Round(time.Second))

	wantContains(t, "experiment start output", out, "[✓] 'vrouter' default app (post-start)")

	answered := t.Run("VR-02 the router answers its minirouter commands", func(t *testing.T) {
		builtinCheckMinirouterCommands(t, name)
	})

	// Every later check needs the router guest up and configured.
	if !answered {
		t.FailNow()
	}

	t.Run("VR-02 minirouter reports interface counters as VM tags", func(t *testing.T) {
		builtinCheckMinirouterTags(t, name)
	})

	t.Run("VR-02 the router reports its topology hostname over C2", func(t *testing.T) {
		builtinWaitClient(t, name, "rtr", builtinRouterBoot)

		// Hostname-mode C2 (SOH, delay.c2, autotunnel) finds the router by
		// this name; the first heartbeats may still carry the image's, phenix.
		eventually(t, time.Minute, 2*time.Second, "rtr to report hostname rtr over C2", func() (bool, string) {
			row := builtinClient(t, name, "rtr")

			return row["hostname"] == "rtr", fmt.Sprintf("cc clients hostname %q", row["hostname"])
		})
	})

	t.Run("VR-02 router guest state", func(t *testing.T) { builtinCheckRouterGuest(t, name) })
	t.Run("VR-02 routing and the firewall chain", func(t *testing.T) { builtinCheckRouting(t, name) })
	t.Run("VR-02 DHCP lease and DNS record from the router", func(t *testing.T) { builtinCheckDHCP(t, name) })
	t.Run("SO-04 SOH reaches the router by UUID and routes across it", func(t *testing.T) { builtinCheckRouterSOH(t, name) })

	t.Run("stop", func(t *testing.T) {
		exp.with(t).stop()
		builtinWantNoNamespace(t, name)
	})
}

// builtinCheckMinirouterCommands checks, from the host, that the minirouter
// guest received its config: it answered the three cc commands the commit
// queued (remove the old config, receive the new one, apply it).
func builtinCheckMinirouterCommands(t *testing.T, name string) {
	t.Helper()

	eventually(t, builtinRouterBoot, 5*time.Second, "rtr to answer its three minirouter commands", func() (bool, string) {
		var rows []string

		answered := 0

		for _, row := range mmTable(t, name, "cc commands") {
			if row["prefix"] != "minirouter-rtr" {
				continue
			}

			rows = append(rows, row["command"]+" responses="+row["responses"])

			if row["responses"] != "0" {
				answered++
			}
		}

		return len(rows) == 3 && answered == 3, fmt.Sprintf("%v", rows)
	})
}

// builtinCheckMinirouterTags checks that the minirouter daemon runs: it pushes
// its interface counters to minimega as VM tags through miniccc.
func builtinCheckMinirouterTags(t *testing.T, name string) {
	t.Helper()

	counters := regexp.MustCompile(`minirouter_[A-Za-z0-9]+_rx_bytes`)

	eventually(t, time.Minute, 5*time.Second, "rtr's minirouter interface counters in vm info tags", func() (bool, string) {
		tags := vmInfo(t, name)["rtr"]["tags"]

		return counters.MatchString(tags), "tags " + tags
	})
}

// builtinCheckRouterGuest checks inside the minirouter what it applied.
func builtinCheckRouterGuest(t *testing.T, name string) {
	t.Helper()

	got := builtinGuest(
		t,
		name,
		"rtr",
		builtinCheck{"addresses", "ip -4 -o addr show scope global | awk '{print $4}' | sort | tr '\\n' ' '"},
		builtinCheck{"dhcp range", "grep -c 'dhcp-range=set:10.40.0.254,10.40.0.100,10.40.0.110$' /etc/dnsmasq.conf"},
		builtinCheck{"dhcp router", "grep -c 'option:router, 10.40.0.254$' /etc/dnsmasq.conf"},
		builtinCheck{"dns record", "grep -cx 'host-record=ra.e2e.local,10.10.0.1' /etc/dnsmasq.conf"},
		builtinCheck{"chain drop", "iptables -S lan2-in | grep -- '-j DROP'"},
		builtinCheck{"chain default", "iptables -S lan2-in | grep -cx -- '-A lan2-in -j ACCEPT'"},
		builtinCheck{
			"chain iface",
			`d=$(iptables -S FORWARD | awk '/-j lan2-in/ {for (i = 1; i <= NF; i++) if ($i == "-i") print $(i+1)}'); ` +
				`ip -4 -o addr show dev "$d" | awk '{print $4}'`,
		},
		builtinCheck{"forwarding", "sysctl -n net.ipv4.ip_forward"},
		builtinCheck{"static route", "ip route show 10.99.0.0/24 | grep -c 'via 10.20.0.1'"},
		builtinCheck{"dnsmasq", "pgrep -x dnsmasq >/dev/null && echo running"},
		builtinCheck{"bird", "pgrep -x bird >/dev/null && echo running"},
	)

	builtinWantGuest(t, "rtr", got, map[string]string{
		"addresses":     "10.10.0.254/24 10.20.0.254/24 10.40.0.254/24",
		"dhcp range":    "1",
		"dhcp router":   "1",
		"dns record":    "1",
		"chain default": "1",
		"chain iface":   "10.20.0.254/24",
		"forwarding":    "1",
		"static route":  "1",
		"dnsmasq":       "running",
		"bird":          "running",
	})

	wantContains(t, "rtr iptables lan2-in DROP rule", got["chain drop"], "-s 10.20.0.0/24", "-d 10.10.0.1/32", "--dport 22", "-p tcp")
}

// builtinTCPProbe is a guest check that connects with bash's /dev/tcp and
// prints the exit status: 0 connected, 1 refused, 124 timed out (dropped).
func builtinTCPProbe(name, addr string, port int) builtinCheck {
	return builtinCheck{name, fmt.Sprintf("timeout 5 bash -c 'exec 3<>/dev/tcp/%s/%d' >/dev/null 2>&1; echo $?", addr, port)}
}

// builtinCheckRouting checks forwarding across the router and that the
// firewall chain drops only TCP 22 from LAN2 to 10.10.0.1.
func builtinCheckRouting(t *testing.T, name string) {
	t.Helper()

	ra := builtinGuest(t, name, "ra",
		builtinCheck{"ping rb", "ping -c 3 -W 2 10.20.0.1 >/dev/null && echo ok"},
		builtinTCPProbe("tcp rb 22", "10.20.0.1", 22),
	)
	rb := builtinGuest(t, name, "rb",
		builtinCheck{"ping ra", "ping -c 3 -W 2 10.10.0.1 >/dev/null && echo ok"},
		builtinTCPProbe("tcp ra 22", "10.10.0.1", 22),
		builtinTCPProbe("tcp ra 23", "10.10.0.1", 23),
	)

	builtinWantGuest(t, "ra", ra, map[string]string{"ping rb": "ok"})
	builtinWantGuest(t, "rb", rb, map[string]string{"ping ra": "ok", "tcp ra 22": "124"})

	for vm, got := range map[string]string{"ra -> rb:22": ra["tcp rb 22"], "rb -> ra:23": rb["tcp ra 23"]} {
		if got == "124" || got == "" {
			t.Errorf("TCP %s timed out (%q); the lan2-in chain should drop only TCP 22 from LAN2 to 10.10.0.1", vm, got)
		}
	}
}

// builtinCheckDHCP checks the lease rd gets from the router's dnsmasq, its
// routing through the router, and the router's DNS record.
func builtinCheckDHCP(t *testing.T, name string) {
	t.Helper()

	leased := regexp.MustCompile(`\b10\.40\.0\.(10[0-9]|110)\b`)

	var addr string

	eventually(t, builtinRouterBoot, 5*time.Second, "rd to lease an address from rtr", func() (bool, string) {
		row := builtinClient(t, name, "rd")
		if row == nil {
			return false, "rd not registered"
		}

		addr = leased.FindString(row["ip"])

		return addr != "", "cc clients ip " + row["ip"]
	})

	got := builtinGuest(t, name, "rd",
		builtinCheck{"address", "ip -4 -o addr show scope global | awk '{print $4}'"},
		builtinCheck{"default route", "ip route show default | grep -c 'default via 10.40.0.254'"},
		builtinCheck{"ping ra", "ping -c 3 -W 2 10.10.0.1 >/dev/null && echo ok"},
		builtinCheck{"dns", "getent hosts ra.e2e.local"},
	)

	builtinWantGuest(t, "rd", got, map[string]string{"address": addr + "/24", "default route": "1", "ping ra": "ok"})

	// The guest's resolver can pick up the leased DNS server after the lease
	// itself, so a first miss is asked again.
	resolves := func(out string) bool {
		f := strings.Fields(out)

		return len(f) >= 2 && f[0] == "10.10.0.1" && slices.Contains(f[1:], "ra.e2e.local")
	}

	if !resolves(got["dns"]) {
		eventually(t, time.Minute, 5*time.Second, "rd to resolve ra.e2e.local through rtr's DNS", func() (bool, string) {
			dns := builtinGuest(t, name, "rd", builtinCheck{"dns", "getent hosts ra.e2e.local"})["dns"]

			return resolves(dns), fmt.Sprintf("getent hosts ra.e2e.local = %q, want 10.10.0.1", dns)
		})
	}

	eventually(t, 30*time.Second, time.Second, "minimega to learn rd's leased address from ARP", func() (bool, string) {
		ip := vmInfo(t, name)["rd"]["ip"]

		return strings.Contains(ip, addr), "vm info ip " + ip
	})
}

// builtinCheckRouterSOH checks the SOH results of the router experiment.
func builtinCheckRouterSOH(t *testing.T, name string) {
	t.Helper()

	var soh builtinSOHStatus

	if !builtinAppStatus(t, name, "soh", &soh) || !soh.Initialized {
		t.Fatalf("status.apps.soh = %+v, want initialized", soh)
	}

	if _, ok := soh.host("rd"); ok {
		t.Error("SOH has results for rd, which skipHosts excludes")
	}

	rtr, ok := soh.host("rtr")
	if !ok {
		t.Fatalf("SOH has no results for rtr: %+v", soh.Hosts)
	}

	for _, addr := range []string{"10.10.0.254", "10.20.0.254", "10.40.0.254"} {
		builtinWantSuccess(t, "rtr", "networking", rtr.Networking, 1, "IP "+addr+" configured")
	}

	builtinWantNoSOHErrors(t, rtr)

	for vm, w := range map[string]struct{ addr, gw, far string }{
		"ra": {"10.10.0.1", "10.10.0.254", "10.20.0.1"},
		"rb": {"10.20.0.1", "10.20.0.254", "10.10.0.1"},
	} {
		h, ok := soh.host(vm)
		if !ok {
			t.Errorf("SOH has no results for %s", vm)

			continue
		}

		builtinWantSuccess(t, vm, "networking", h.Networking, 1, "IP "+w.addr+" configured")
		builtinWantSuccess(t, vm, "networking", h.Networking, 1, "gateway "+w.gw+" is up")
		builtinWantSuccess(t, vm, "reachability", h.Reachability, 1, "pinging "+w.far+" succeeded")
		builtinWantNoSOHErrors(t, h)
	}
}

// ===========================================================================
// Hollow router: vrouter's minimega router API contract

// TestBuiltinRouterHollow checks every router command vrouter sends for a
// minirouter on a hollow VM, the config minimega commits and the cc commands
// it queues (VR-01), the app registry and stage guards (RG-01), and that
// re-triggering vrouter's post-start is idempotent.
func TestBuiltinRouterHollow(t *testing.T) {
	hollowImages(t)

	const name = "e2e-ba-rtrh"

	// Registered first, so it runs after the experiment's teardown.
	t.Cleanup(func() { _, _ = mmRun("", "clear namespace "+name) })

	exp := builtinExperiment(t, name, builtinTopology(builtinRouterNode(ext4Image, 256)), builtinScenario(builtinVrouterApp))

	t.Run("VR-07 a dry run of a minirouter experiment succeeds without minimega", func(t *testing.T) {
		out, err := phenixRun("experiment", "start", name, "--dry-run")
		if err != nil {
			t.Errorf("phenix experiment start %s --dry-run: %v\n%s", name, err, out)
		}

		builtinWantNoNamespace(t, name)

		if exp.running() {
			exp.with(t).stop()
		}

		_, _ = mmRun("", "clear namespace "+name)
	})

	out := exp.start()
	wantContains(t, "experiment start output", out, "[✓] 'vrouter' default app (post-start)")

	t.Run("VR-01 minimega holds the staged router config", func(t *testing.T) {
		wantContains(t, "minimega router rtr", mmText(t, name, "router rtr"),
			"Network: 0: [10.10.0.254/24]", "Network: 1: [10.20.0.254/24]", "Network: 2: [10.40.0.254/24]",
			"10.40.0.100", "10.40.0.110", "10.10.0.1\tra.e2e.local",
			"Static Routes:", "10.99.0.0/24\t10.20.0.1", "Firewall Chains:", "lan2-in")
	})

	t.Run("VR-01 commit writes the minirouter config", func(t *testing.T) {
		builtinWantLines(t, "minirouter-rtr", readFile(t, filepath.Join(cfg.imagesDir, name, "minirouter-rtr")),
			"ip flush",
			"ip add 0 10.10.0.254/24",
			"ip add 1 10.20.0.254/24",
			"ip add 2 10.40.0.254/24",
			"dnsmasq dhcp range 10.40.0.254 10.40.0.100 10.40.0.110",
			"dnsmasq dhcp option router 10.40.0.254 10.40.0.254",
			"dnsmasq dhcp option dns 10.40.0.254 10.40.0.254",
			"dnsmasq dns 10.10.0.1 ra.e2e.local",
			"dnsmasq commit",
			"bird static 10.99.0.0/24 10.20.0.1 null",
			"bird commit",
			"fw chain lan2-in action drop 10.20.0.0/24 10.10.0.1:22 tcp",
			"fw chain lan2-in default action accept",
			"fw chain lan2-in apply in 1",
		)
	})

	t.Run("VR-01 commit queues three cc commands for the router", func(t *testing.T) { builtinWantRouterCommands(t, name) })

	t.Run("RG-01 app listing and stage guards", func(t *testing.T) {
		apps := phenix(t, "experiment", "apps")
		wantContains(t, "phenix experiment apps", apps, "tap", "soh", "scorch")

		_, listed, _ := strings.Cut(apps, "Apps:")
		for app := range strings.SplitSeq(listed, ",") {
			if app = strings.TrimSpace(app); slices.Contains([]string{"ntp", "serial", "startup", "vrouter", "user-shell"}, app) {
				t.Errorf("phenix experiment apps lists %s, which is a default app or the user app wrapper", app)
			}
		}

		out := phenixFails(t, "experiment", "trigger", "running", name, "startup")
		wantContains(t, "trigger running startup", out, "the running stage is not applicable for app startup")

		out = phenixFails(t, "experiment", "trigger", "running", name, "nosuch")
		wantContains(t, "trigger running nosuch", out, "app nosuch is not part of experiment "+name)
	})

	t.Run("VR-01 re-triggering vrouter post-start is idempotent", func(t *testing.T) {
		out, err := phenixRun("experiment", "trigger", "post-start", name, "vrouter")
		if err != nil {
			t.Errorf("phenix experiment trigger post-start %s vrouter: %v\n%s", name, err, out)
		}

		builtinWantRouterCommands(t, name)

		// The re-committed config holds each address, DNS record and firewall
		// rule once, not again beside what the first post-start staged.
		config := readFile(t, filepath.Join(cfg.imagesDir, name, "minirouter-rtr"))
		lines := make(map[string]int)

		for l := range strings.SplitSeq(config, "\n") {
			lines[strings.TrimSpace(l)]++
		}

		for _, line := range []string{
			"ip add 0 10.10.0.254/24",
			"dnsmasq dns 10.10.0.1 ra.e2e.local",
			"fw chain lan2-in action drop 10.20.0.0/24 10.10.0.1:22 tcp",
			"fw chain lan2-in apply in 1",
		} {
			if lines[line] != 1 {
				t.Errorf("minirouter-rtr has %d lines %q after the re-trigger, want 1:\n%s", lines[line], line, config)
			}
		}
	})
}

// builtinWantRouterCommands checks the cc commands a minirouter commit
// queues: exactly the three of its prefix (an earlier commit's are replaced).
func builtinWantRouterCommands(t *testing.T, name string) {
	t.Helper()

	var commands []string

	for _, row := range mmTable(t, name, "cc commands") {
		if row["prefix"] == "minirouter-rtr" {
			commands = append(commands, row["command"]+" "+row["sent"])
		}
	}

	want := []string{
		"[rm /tmp/miniccc/files/minirouter-rtr] []",
		"[] [minirouter-rtr]",
		"[minirouter -u /tmp/miniccc/files/minirouter-rtr] []",
	}

	if !slices.Equal(commands, want) {
		t.Errorf("cc commands with prefix minirouter-rtr = %q, want %q", commands, want)
	}
}

// ===========================================================================
// Default app order

// TestBuiltinDefaultAppOrder dry-runs one experiment three times and checks
// that the default apps run in the documented order every time (RG-02).
func TestBuiltinDefaultAppOrder(t *testing.T) {
	hollowImages(t)

	const name = "e2e-ba-order"

	exp := builtinExperiment(t, name, builtinTopology(builtinHollow("o1", "LAN", "10.81.0.1")), "")
	stage := regexp.MustCompile(`'(\w+)' default app \(([a-z-]+)\)`)
	orders := make(map[string][]string)

	for range 3 {
		out := exp.start("--dry-run") + exp.stop()

		byStage := make(map[string][]string)

		for _, m := range stage.FindAllStringSubmatch(out, -1) {
			byStage[m[2]] = append(byStage[m[2]], m[1])
		}

		for s, apps := range byStage {
			orders[strings.Join(apps, ",")] = append(orders[strings.Join(apps, ",")], s)
		}
	}

	if len(orders) == 0 {
		t.Fatal("dry runs logged no default app")
	}

	if len(orders) != 1 {
		t.Errorf("default apps ran in %d different orders across stages and runs: %v", len(orders), orders)
	}

	// The order docs/content/apps.md lists them in.
	if want := "ntp,serial,startup,vrouter"; orders[want] == nil {
		t.Errorf("default apps did not run in the order %s: %v", want, orders)
	}
}

// ===========================================================================
// Rendering on hollow VMs: Windows startup, opt-outs, ntp client types,
// Vyatta and VyOS configs

// builtinRenderNodes are hollow nodes for every rendering path.
func builtinRenderNodes() []builtinNode {
	win := func(host, addr, extra string) builtinNode {
		return builtinNode{
			host: host, image: ext4Image, os: "windows",
			ifaces: []builtinIface{{name: "eth0", vlan: "RND", addr: addr}},
			extra:  extra,
		}
	}

	n1 := builtinHollow("n1", "RND", "10.70.0.1")
	n1.ifaces[0].gw = "10.99.0.1"

	l1 := builtinHollow("l1", "RND", "10.70.0.5")
	l1.extra = "\n    labels:\n      disable-injects: \"true\""

	l2 := builtinHollow("l2", "RND", "10.70.0.6")
	l2.extra = "\n    annotations:\n      phenix/default-apps: false"

	return []builtinNode{
		win("w1", "10.60.0.1", ""),
		win("wc", "10.60.0.2", "\n    annotations:\n      phenix/startup-via-cc: true"),
		{
			kind: "Router", host: "v1", image: ext4Image, os: "vyatta",
			ifaces: []builtinIface{
				{name: "eth0", vlan: "RND", addr: "10.60.0.254", mtu: 1400},
				{name: "eth1", vlan: "RND2", proto: "dhcp"},
			},
			netExtra: "\n      nat:\n      - out: eth1\n        in: [eth0]",
		},
		{
			kind: "Router", host: "y1", image: builtinP3Image, os: "vyos", partition: 3,
			ifaces: []builtinIface{{name: "eth0", vlan: "RND", addr: "10.60.0.253"}},
		},
		l1, l2, n1,
		builtinHollow("c1", "RND", "10.70.0.2"),
		builtinHollow("ct", "RND", "10.70.0.3"),
	}
}

const builtinRenderApps = `
  - name: ntp
    metadata:
      defaultSource:
        hostname: n1
        interface: eth0
    hosts:
    - hostname: n1
      metadata:
        server: ntpd
    - hostname: c1
      metadata:
        client: chrony
    - hostname: ct
      metadata:
        client: systemd
    - hostname: w1
      metadata:
        client: windows
  - name: vrouter
    hosts:
    - hostname: v1
      metadata:
        snat:
        - interface: eth1
          srcAddr: 10.60.0.0/24
          translation: 192.0.2.10`

// TestBuiltinRender starts hollow VMs for every rendering path of startup,
// ntp and vrouter and checks what minimega injected into their disks
// (ST-04c, ST-05, ST-06, NT-03, VR-04, VR-06).
func TestBuiltinRender(t *testing.T) {
	hollowImages(t)
	builtinMakeP3Image(t)

	const name = "e2e-ba-render"

	exp := builtinExperiment(t, name, builtinTopology(builtinRenderNodes()...), builtinScenario(builtinRenderApps))
	out := exp.start()
	exp.waitRunning(30*time.Second, "w1", "wc", "v1", "y1", "l1", "l2", "n1", "c1", "ct")

	t.Run("ST-04 a gateway outside its subnet is a warning", func(t *testing.T) {
		wantContains(
			t,
			"experiment start output",
			out,
			`node "n1" interface "eth0" gateway 10.99.0.1 is outside the interface subnet 10.70.0.0/24`,
		)
	})

	t.Run("ST-05 Windows startup, wrapper, scheduler and ntp injected", func(t *testing.T) { builtinCheckWindows(t, name) })
	t.Run("ST-05 Windows startup over C2 skips the wrapper", func(t *testing.T) { builtinCheckWindowsC2(t, exp) })
	t.Run("ST-06 opt-outs and routers get no startup injections", func(t *testing.T) { builtinCheckOptOuts(t, exp) })
	t.Run("NT-03 ntp server and client types injected", func(t *testing.T) { builtinCheckNTPTypes(t, name) })
	t.Run("VR-04 Vyatta and VyOS configs rendered and injected", func(t *testing.T) { builtinCheckVyatta(t, name) })

	t.Run("VR-06 app SNAT takes precedence over topology NAT", func(t *testing.T) {
		boot := readFile(t, filepath.Join(builtinBaseDir(name), "vrouter", "v1.boot"))

		wantContains(t, "v1.boot", boot, "address 192.0.2.10")
		wantLacks(t, "v1.boot", boot, "address masquerade")
	})
}

func builtinCheckWindows(t *testing.T, name string) {
	t.Helper()

	mountDisk(t, builtinSnapshot(t, name, "w1"), 1, "-r", "ro,noload", func(root string) {
		for path, want := range map[string]string{
			"phenix/startup/20-startup.ps1": `$computer_info.Rename("w1")`,
			"phenix/phenix-startup.ps1":     `Get-ChildItem '/phenix/startup/*.ps1'`,
			"ProgramData/Microsoft/Windows/Start Menu/Programs/Startup/startup_scheduler.cmd": "schtasks.exe /create /sc onstart",
			"phenix/startup/25-ntp.ps1": `/manualpeerlist:"10.70.0.1"`,
		} {
			got, ok := guestFile(root, path)
			if !ok {
				t.Errorf("w1 disk has no %s", path)

				continue
			}

			wantContains(t, "w1 "+path, got, want)
		}
	})
}

func builtinCheckWindowsC2(t *testing.T, exp *experiment) {
	t.Helper()

	mountDisk(t, builtinSnapshot(t, exp.name, "wc"), 1, "-r", "ro,noload", func(root string) {
		for _, path := range []string{
			"phenix/startup/20-startup.ps1",
			"phenix/phenix-startup.ps1",
			"ProgramData/Microsoft/Windows/Start Menu/Programs/Startup/startup_scheduler.cmd",
		} {
			if _, ok := guestFile(root, path); ok {
				t.Errorf("wc (startup over C2) has %s injected", path)
			}
		}
	})

	cc := readFile(t, filepath.Join(builtinBaseDir(exp.name), "mm_files", exp.name+"-cc.mm"))
	guestPath := "/tmp/miniccc/files/" + exp.name + "/wc-startup.ps1"

	builtinWantLines(t, exp.name+"-cc.mm", cc, "cc filter name=wc", "cc send "+exp.name+"/wc-startup.ps1")

	// exec, not exec-once: minimega marks an exec-once command sent when phenix
	// queues it, before the guest connects.
	run := regexp.MustCompile(
		`(?m)^cc exec cmd /c 'powershell\.exe -noprofile -executionpolicy bypass -file ` +
			regexp.QuoteMeta(guestPath) + `'$`,
	)
	if !run.MatchString(cc) {
		t.Errorf("%s-cc.mm does not run %s with powershell:\n%s", exp.name, guestPath, cc)
	}

	wantContains(t, "staged wc-startup.ps1", readFile(t, filepath.Join(cfg.imagesDir, exp.name, "wc-startup.ps1")), `Rename("wc")`)
}

func builtinCheckOptOuts(t *testing.T, exp *experiment) {
	t.Helper()

	script := exp.with(t).mmScript()
	startup := filepath.Join(builtinBaseDir(exp.name), "startup")

	if line := builtinInjectLine(script, "l1"); line != "" {
		t.Errorf("l1 (disable-injects label) has a disk inject line: %s", line)
	}

	if _, err := os.Stat(filepath.Join(startup, "l1-hostname.sh")); err != nil {
		t.Errorf("startup scripts are not rendered for l1 (only its injections are disabled): %v", err)
	}

	if line := builtinInjectLine(script, "l2"); line != "" {
		t.Errorf("l2 (default apps off) has a disk inject line: %s", line)
	}

	if matches, _ := filepath.Glob(filepath.Join(startup, "l2-*")); len(matches) > 0 {
		t.Errorf("startup scripts rendered for l2 (default apps off): %v", matches)
	}

	for _, vm := range []string{"v1", "y1"} {
		wantLacks(t, vm+" disk inject line", builtinInjectLine(script, vm), "/etc/phenix/startup/")
	}
}

func builtinCheckNTPTypes(t *testing.T, name string) {
	t.Helper()

	for vm, w := range map[string]struct {
		path        string
		want, lacks []string
	}{
		"n1": {"etc/ntp.conf", []string{"server 127.127.1.1 iburst prefer", "fudge 127.127.1.1 stratum 10"}, []string{"noserve"}},
		"c1": {"etc/chrony/chrony.conf", []string{"server 10.70.0.1 iburst prefer maxpoll 6"}, []string{"allow all"}},
		"ct": {"etc/systemd/timesyncd.conf", []string{"NTP=10.70.0.1", "PollIntervalMaxSec=64"}, nil},
	} {
		mountDisk(t, builtinSnapshot(t, name, vm), 1, "-r", "ro,noload", func(root string) {
			got, ok := guestFile(root, w.path)
			if !ok {
				t.Errorf("%s disk has no %s", vm, w.path)

				return
			}

			builtinWantLines(t, vm+" "+w.path, got, w.want...)
			wantLacks(t, vm+" "+w.path, got, w.lacks...)
		})
	}
}

func builtinCheckVyatta(t *testing.T, name string) {
	t.Helper()

	dir := filepath.Join(builtinBaseDir(name), "vrouter")
	v1 := readFile(t, filepath.Join(dir, "v1.boot"))
	y1 := readFile(t, filepath.Join(dir, "y1.boot"))

	builtinWantLines(t, "v1.boot", v1, "host-name v1", "ethernet eth0 {", "address 10.60.0.254/24", "mtu 1400", "address dhcp")

	if !strings.HasPrefix(y1, "#!/bin/vbash") {
		t.Errorf("y1.boot does not start with #!/bin/vbash:\n%s", y1)
	}

	builtinWantLines(t, "y1.boot", y1, "set system host-name y1", "set interface ethernet eth0 address 10.60.0.253/24")

	mountDisk(t, builtinSnapshot(t, name, "v1"), 1, "-r", "ro,noload", func(root string) {
		if got, _ := guestFile(root, "opt/vyatta/etc/config/config.boot"); got != v1 {
			t.Errorf("v1 disk config.boot differs from the rendered v1.boot:\n%s", got)
		}
	})

	mountDisk(t, builtinSnapshot(t, name, "y1"), 3, "-r", "ro,noload", func(root string) {
		if got, _ := guestFile(root, "boot/vyos/rw/config/scripts/custom/vyos.script"); got != y1 {
			t.Errorf("y1 disk partition 3 vyos.script differs from the rendered y1.boot:\n%s", got)
		}
	})
}

// TestBuiltinNTPLabelMode dry-runs the legacy ntp-server label mode (NT-02):
// every other node becomes a client of the labeled node, which is left for
// the user to configure, as the documentation says.
func TestBuiltinNTPLabelMode(t *testing.T) {
	hollowImages(t)

	const name = "e2e-ba-ntplbl"

	server := builtinHollow("n1", "RND", "10.71.0.1")
	server.extra = "\n    labels:\n      ntp-server: eth0"

	exp := builtinExperiment(t, name, builtinTopology(
		server,
		builtinHollow("c1", "RND", "10.71.0.2"),
		builtinNode{host: "w1", image: ext4Image, os: "windows", ifaces: []builtinIface{{name: "eth0", vlan: "RND", addr: "10.71.0.3"}}},
		builtinNode{
			kind:   "Router",
			host:   "v1",
			image:  ext4Image,
			os:     "vyatta",
			ifaces: []builtinIface{{name: "eth0", vlan: "RND", addr: "10.71.0.254"}},
		},
	), "")

	exp.start("--dry-run")

	dir := filepath.Join(builtinBaseDir(name), "ntp")
	script := exp.mmScript()

	builtinWantLines(t, "c1_ntp", readFile(t, filepath.Join(dir, "c1_ntp")), "server 10.71.0.1 iburst prefer maxpoll 6")
	wantContains(t, "w1_ntp", readFile(t, filepath.Join(dir, "w1_ntp")), `/manualpeerlist:"10.71.0.1"`)
	builtinWantLines(t, "v1_ntp", readFile(t, filepath.Join(dir, "v1_ntp")), "server 10.71.0.1 iburst prefer maxpoll 6")

	for vm, dst := range map[string]string{"c1": "/etc/ntp.conf", "w1": "/phenix/startup/25-ntp.ps1", "v1": "/opt/vyatta/etc/ntp.conf"} {
		wantContains(t, vm+" disk inject line", builtinInjectLine(script, vm), fmt.Sprintf(`"%s/%s_ntp":"%s"`, dir, vm, dst))
	}

	if _, err := os.Stat(filepath.Join(dir, "n1_ntp")); err == nil {
		t.Error("the labeled ntp server got a client config")
	}

	wantLacks(t, "n1 disk inject line", builtinInjectLine(script, "n1"), "/etc/ntp.conf")
}

// ===========================================================================
// Pre-start guards

// TestBuiltinPreStartGuards checks the configurations startup and ntp refuse
// in pre-start, before anything reaches minimega (ST-04, NT-04), and the
// phenix/default-apps values the default apps read or refuse (ST-06).
func TestBuiltinPreStartGuards(t *testing.T) {
	hollowImages(t)

	refused := func(t *testing.T, name, topo, scenario, want string) {
		t.Helper()

		builtinExperiment(t, name, topo, scenario)

		out := phenixFails(t, "experiment", "start", name)
		wantContains(t, "experiment start "+name, out, want)
		builtinWantNoNamespace(t, name)
	}

	t.Run("ST-04 duplicate private IP on a VLAN", func(t *testing.T) {
		refused(t, "e2e-ba-dupip",
			builtinTopology(builtinHollow("x1", "LAN", "10.80.0.1"), builtinHollow("x2", "LAN", "10.80.0.1")), "",
			"duplicate private IP detected on VLAN LAN: x1 and x2 both have 10.80.0.1 configured")
	})

	t.Run("ST-04 the same IP on different VLANs starts", func(t *testing.T) {
		exp := builtinExperiment(t, "e2e-ba-dupok",
			builtinTopology(builtinHollow("x1", "LAN", "10.80.0.1"), builtinHollow("x2", "WAN", "10.80.0.1")), "")
		exp.start()
		exp.waitRunning(30*time.Second, "x1", "x2")
	})

	topo := builtinTopology(builtinHollow("n1", "LAN", "10.82.0.1"), builtinHollow("c1", "LAN", "10.82.0.2"))
	ntp := func(metadata, hosts string) string {
		return builtinScenario("\n  - name: ntp" + metadata + "\n    hosts:" + hosts)
	}

	defaultSource := "\n    metadata:\n      defaultSource:\n        hostname: n1\n        interface: eth0"

	for _, c := range []struct{ name, scenario, want string }{
		{
			"e2e-ba-ntpnos", ntp("", "\n    - hostname: c1\n      metadata:\n        client: ntp"),
			"no NTP source configured for host c1 (and no default source configured)",
		},
		{
			"e2e-ba-ntpcli", ntp(defaultSource, "\n    - hostname: c1\n      metadata:\n        client: foo"),
			"unknown NTP client type foo provided for host c1",
		},
		{
			"e2e-ba-ntpsrv", ntp(defaultSource, "\n    - hostname: n1\n      metadata:\n        server: foo"),
			"unknown NTP server type foo provided for host n1",
		},
		{
			"e2e-ba-ntpnon", ntp(defaultSource, "\n    - hostname: c1\n      metadata:\n        source:\n          address: 10.82.0.9"),
			"host c1 missing NTP client/server type",
		},
	} {
		t.Run("NT-04 "+c.want, func(t *testing.T) { refused(t, c.name, topo, c.scenario, c.want) })
	}

	t.Run("ST-06 a quoted false turns the default apps off", func(t *testing.T) {
		q1 := builtinHollow("q1", "LAN", "10.83.0.1")
		q1.extra = "\n    annotations:\n      phenix/default-apps: \"false\""

		exp := builtinExperiment(t, "e2e-ba-dastr", builtinTopology(q1), "")
		exp.start("--dry-run")

		if line := builtinInjectLine(exp.mmScript(), "q1"); line != "" {
			t.Errorf("q1 (default apps off) has a disk inject line: %s", line)
		}

		if matches, _ := filepath.Glob(filepath.Join(builtinBaseDir(exp.name), "startup", "q1-*")); len(matches) > 0 {
			t.Errorf("startup scripts rendered for q1 (default apps off): %v", matches)
		}
	})

	t.Run("ST-06 an unreadable default-apps annotation is refused at create", func(t *testing.T) {
		const name = "e2e-ba-dabad"

		q2 := builtinHollow("q2", "LAN", "10.83.0.2")
		q2.extra = "\n    annotations:\n      phenix/default-apps: \"no\""

		topo := strings.ReplaceAll(builtinTopology(q2), "{{name}}", name)
		phenix(t, "config", "create", writeFile(t, name+"-topology.yaml", topo))

		t.Cleanup(func() { _, _ = phenixRun("config", "delete", "topology/"+name) })
		t.Cleanup(func() { _, _ = phenixRun("experiment", "delete", name, "--force") })

		out := phenixFails(t, "experiment", "create", name, "--topology", name)
		wantContains(t, "experiment create "+name, out, `node q2: annotation phenix/default-apps is "no", want true or false`)
		builtinWantNoNamespace(t, name)
	})
}

// ===========================================================================
// tap app on hollow VMs

// builtinTapApp is a tap app scenario with one tap on LAN.
func builtinTapApp(stage, ip string, external bool, extra string) string {
	app := "\n  - name: tap\n    metadata:"

	if stage != "" {
		app += "\n      stage: " + stage
	}

	app += "\n      taps:\n      - bridge: phenix\n        vlan: LAN\n        ip: " + ip

	if external {
		app += "\n        externalAccess:\n          enabled: true"
	}

	return builtinScenario(app + extra)
}

// TestBuiltinTapApp checks the tap app's external access (TA-03), its
// validation (TA-04) and re-triggering its post-start (TA-05).
func TestBuiltinTapApp(t *testing.T) {
	hollowImages(t)

	t.Run("TA-03 external access", builtinCheckTapExternal)
	t.Run("TA-04 validation", builtinCheckTapValidation)
	t.Run("TA-05 re-triggering post-start", builtinCheckTapRetrigger)
	t.Run("TA-06 a dry run creates no host tap", builtinCheckTapDryRun)
}

// builtinCheckTapDryRun dry-runs an experiment with the tap app, which must
// leave the host and minimega alone.
func builtinCheckTapDryRun(t *testing.T) {
	const name = "e2e-ba-tapdry"

	before := builtinCleanTaps(t)

	t.Cleanup(func() { _, _ = mmRun("", "clear namespace "+name) })

	exp := builtinExperiment(
		t,
		name,
		builtinTopology(builtinHollow("h1", "LAN", "10.10.0.1")),
		builtinTapApp("", "10.10.0.201/24", false, ""),
	)
	exp.start("--dry-run")

	if left := builtinNewTaps(t, before, "-tapapp"); len(left) > 0 {
		t.Errorf("the dry run created host taps: %v", left)
	}

	builtinWantNoNamespace(t, name)
}

// builtinCheckTapExternal starts two experiments whose taps have external
// access and checks the /30 pairs, veths, routes and NAT rules.
func builtinCheckTapExternal(t *testing.T) {
	builtinCleanTaps(t)

	topo := builtinTopology(builtinHollow("h1", "LAN", "10.10.0.1"))
	first := builtinExperiment(t, "e2e-ba-tapx", topo, builtinTapApp("", "10.10.0.201/24", true, ""))
	second := builtinExperiment(t, "e2e-ba-tapx2", topo, builtinTapApp("", "10.10.0.201/24", true, ""))

	first.start()
	second.start()

	taps := make([]string, 0, 2)

	for i, w := range []struct {
		exp            *experiment
		subnet, host   string
		guest, gateway string
	}{
		{first, "10.213.47.0/30", "10.213.47.1/30", "10.213.47.2/30", "10.213.47.1"},
		{second, "10.213.47.4/30", "10.213.47.5/30", "10.213.47.6/30", "10.213.47.5"},
	} {
		var status builtinTapStatus

		if !builtinAppStatus(t, w.exp.name, "tap", &status) || len(status.Taps) != 1 {
			t.Fatalf("%s status.apps.tap = %+v, want one tap", w.exp.name, status)
		}

		tap := status.Taps[0]
		taps = append(taps, tap.Name)

		if tap.Subnet != w.subnet || !tap.External.Enabled {
			t.Errorf("%s tap %d subnet = %q (external %t), want %s: pairs of running experiments are skipped",
				w.exp.name, i, tap.Subnet, tap.External.Enabled, w.subnet)
		}

		builtinWantHostTap(t, tap.Name, "10.10.0.201/24", w.exp.doc().Status.VLANs["LAN"])
		builtinCheckTapVeth(t, tap.Name, w.host, w.guest, w.gateway)
	}

	first.stop()
	builtinWaitTapsGone(t, taps[0])

	if out, err := hostCmd("iptables", "-C", "FORWARD", "-i", taps[0], "!", "-o", taps[0], "-j", "ACCEPT"); err == nil {
		t.Errorf("FORWARD rule for %s remains after stop: %s", taps[0], out)
	}

	if out, err := hostCmd(
		"iptables",
		"-t",
		"nat",
		"-C",
		"POSTROUTING",
		"-s",
		"10.213.47.2",
		"!",
		"-o",
		taps[0],
		"-j",
		"MASQUERADE",
	); err == nil {
		t.Errorf("MASQUERADE rule for %s remains after stop: %s", taps[0], out)
	}

	second.stop()
	builtinWaitTapsGone(t, taps[1])
}

// builtinCheckTapVeth checks a tap's external access veth and NAT rules.
func builtinCheckTapVeth(t *testing.T, tap, host, guest, gateway string) {
	t.Helper()

	if out, err := hostCmd("ip", "-4", "-o", "addr", "show", "dev", tap); err != nil || !strings.Contains(out, "inet "+host+" ") {
		t.Errorf("host end of %s's veth has no %s (%v): %s", tap, host, err, out)
	}

	if out, err := hostCmd("ip", "netns", "exec", tap, "ip", "-4", "-o", "addr", "show", "dev", "eth0"); err != nil ||
		!strings.Contains(out, "inet "+guest+" ") {
		t.Errorf("namespace end (eth0) of %s's veth has no %s (%v): %s", tap, guest, err, out)
	}

	if out, err := hostCmd("ip", "netns", "exec", tap, "ip", "route", "show", "default"); err != nil ||
		!strings.Contains(out, "default via "+gateway) {
		t.Errorf("namespace %s has no default route via %s (%v): %s", tap, gateway, err, out)
	}

	natIP := strings.Split(guest, "/")[0]

	for _, rule := range [][]string{
		{"-t", "nat", "-C", "POSTROUTING", "-s", natIP, "!", "-o", tap, "-j", "MASQUERADE"},
		{"-C", "FORWARD", "-i", tap, "!", "-o", tap, "-j", "ACCEPT"},
		{"-C", "FORWARD", "-o", tap, "-m", "conntrack", "--ctstate", "RELATED,ESTABLISHED", "-j", "ACCEPT"},
	} {
		if out, err := hostCmd("iptables", rule...); err != nil {
			t.Errorf("host iptables %v: %v %s", rule, err, out)
		}
	}

	if out, err := hostCmd(
		"ip",
		"netns",
		"exec",
		tap,
		"iptables",
		"-t",
		"nat",
		"-C",
		"POSTROUTING",
		"-o",
		"eth0",
		"-j",
		"MASQUERADE",
	); err != nil {
		t.Errorf("namespace %s has no MASQUERADE on eth0: %v %s", tap, err, out)
	}

	if out, err := hostCmd("ip", "netns", "exec", tap, "ping", "-c", "2", "-W", "2", gateway); err != nil {
		t.Errorf("namespace %s cannot ping the host end %s: %v %s", tap, gateway, err, out)
	}
}

// builtinCheckTapValidation checks the configurations the tap app refuses.
func builtinCheckTapValidation(t *testing.T) {
	topo := builtinTopology(builtinHollow("h1", "LAN", "10.10.0.1"))

	t.Run("two taps on one VLAN", func(t *testing.T) {
		before := builtinCleanTaps(t)

		const name = "e2e-ba-tapdup"

		builtinExperiment(
			t,
			name,
			topo,
			builtinTapApp("", "10.10.0.201/24", false, "\n      - bridge: phenix\n        vlan: LAN\n        ip: 10.10.0.202/24"),
		)

		out := phenixFails(t, "experiment", "start", name)
		wantContains(t, "experiment start "+name, out, "tap already created for VLAN LAN")
		builtinWantNoNamespace(t, name)

		if left := builtinNewTaps(t, before, "-tapapp"); len(left) > 0 {
			t.Errorf("the refused start left host tap namespaces behind: %v", left)
		}
	})

	t.Run("an unknown stage", func(t *testing.T) {
		const name = "e2e-ba-tapstg"

		builtinExperiment(t, name, topo, builtinTapApp("during", "10.10.0.201/24", false, ""))

		out := phenixFails(t, "experiment", "start", name)
		wantContains(t, "experiment start "+name, out, `invalid stage "during" for tap app: must be "pre-start" or "post-start"`)
		builtinWantNoNamespace(t, name)
	})

	t.Run("external access for an address without a mask", func(t *testing.T) {
		const name = "e2e-ba-tapmask"

		before := builtinCleanTaps(t)

		t.Cleanup(func() { _, _ = mmRun("", "clear namespace "+name) })

		// Post-start, so a clean refusal goes through the start's failure
		// cleanup, which clears the namespace.
		builtinExperiment(t, name, topo, builtinTapApp("", "10.10.0.201", true, ""))

		out := phenixFails(t, "experiment", "start", name)

		wantLacks(t, "experiment start "+name, out, "panic:")
		wantContains(t, "experiment start "+name, out,
			`external access for the tap on VLAN LAN needs an IP address with a prefix length`, `got "10.10.0.201"`)

		builtinWantNoNamespace(t, name)

		if left := builtinNewTaps(t, before, "-tapapp"); len(left) > 0 {
			t.Errorf("the refused start left host taps behind: %v", left)
		}
	})

	t.Run("SCORCH tap and break refuse invalid taps", func(t *testing.T) {
		const name = "e2e-ba-scmask"

		before := builtinCleanTaps(t)

		// Runs 0 and 1 want external access for an address without a mask;
		// runs 2 and 3 a VLAN and an address minimega and `ip addr add`
		// would take as several words.
		exp := builtinExperiment(t, name, topo, builtinScenario(`
  - name: scorch
    metadata:
      runs:
      - start: [tapx]
      - start: [brkx]
      - start: [tapv]
      - start: [brki]
      components:
      - name: tapx
        type: tap
        metadata:
          bridge: phenix
          vlan: LAN
          ip: 10.10.0.221
          externalAccess:
            enabled: true
      - name: brkx
        type: break
        metadata:
          tap:
            bridge: phenix
            vlan: LAN
            ip: 10.10.0.222
            externalAccess:
              enabled: true
      - name: tapv
        type: tap
        metadata:
          bridge: phenix
          vlan: LAN bridge x
          ip: 10.10.0.223/24
      - name: brki
        type: break
        metadata:
          tap:
            bridge: phenix
            vlan: LAN
            ip: 10.0.0.1/24 dev lo`))
		exp.with(t).start()

		const noMask = `external access for the tap on VLAN LAN needs an IP address with a prefix length`

		for run, wants := range [][]string{
			{noMask, `got "10.10.0.221"`},
			{noMask, `got "10.10.0.222"`},
			{`the VLAN of a tap must be one word`, `got "LAN bridge x"`},
			{`the tap on VLAN LAN needs an IP address, optionally with a prefix length`, `got "10.0.0.1/24 dev lo"`},
		} {
			what := fmt.Sprintf("phenix experiment scorch -r %d", run)
			out := phenixFails(t, "experiment", "scorch", name, "--run", strconv.Itoa(run))

			wantLacks(t, what, out, "panic:")
			wantContains(t, what, out, wants...)
		}

		for _, suffix := range []string{"-tapcomp", "-tapbrk"} {
			if left := builtinNewTaps(t, before, suffix); len(left) > 0 {
				t.Errorf("the refused SCORCH runs left host taps, namespaces or links behind: %v", left)
			}
		}

		exp.with(t).stop()
		builtinWantNoNamespace(t, name)
	})
}

// builtinCheckTapRetrigger re-triggers the tap app's post-start and checks
// that the trigger replaces the first tap and that stop removes every tap it
// created. The experiment also has a SCORCH soh component without the soh app
// (SC-05).
func builtinCheckTapRetrigger(t *testing.T) {
	before := builtinCleanTaps(t)

	const name = "e2e-ba-tapre"

	exp := builtinExperiment(t, name, builtinTopology(builtinHollow("h1", "LAN", "10.10.0.1")), builtinTapApp("", "10.10.0.201/24", false, `
  - name: scorch
    metadata:
      runs:
      - start: [soh9]
      components:
      - name: soh9
        type: soh
        metadata: {}`))

	exp.start()

	t.Run("SC-05 the soh component needs the soh app", func(t *testing.T) {
		out := phenixFails(t, "experiment", "scorch", name, "--run", "0")
		wantContains(t, "phenix experiment scorch", out, "soh app not configured")
	})

	var first builtinTapStatus
	if !builtinAppStatus(t, name, "tap", &first) || len(first.Taps) != 1 {
		t.Fatalf("status.apps.tap = %+v, want one tap", first)
	}

	phenix(t, "experiment", "trigger", "post-start", name, "tap")

	// The trigger replaces the first tap, so the new status records every tap.
	var second builtinTapStatus
	if !builtinAppStatus(t, name, "tap", &second) || len(second.Taps) != 1 || second.Taps[0].Name == first.Taps[0].Name {
		t.Errorf("status.apps.tap after re-triggering post-start = %+v, want one tap replacing %s", second, first.Taps[0].Name)
	}

	builtinWaitTapsGone(t, first.Taps[0].Name)

	if taps := builtinNewTaps(t, before, "-tapapp"); len(taps) != 1 {
		t.Errorf("host taps after re-triggering post-start = %v, want one", taps)
	}

	exp.stop()

	eventually(t, builtinTapGone, time.Second, "every tap of "+name+" to go", func() (bool, string) {
		left := builtinNewTaps(t, before, "-tapapp")

		return len(left) == 0, fmt.Sprintf("left %v", left)
	})
}

// ===========================================================================
// SOH failures on hollow VMs (no miniccc)

// TestBuiltinSOHNegative checks SOH against a hollow VM that never registers
// with C2 (SO-02, SO-03), the SCORCH soh component's failOnError (SC-05) and
// that it fails while SOH already runs (SC-06).
func TestBuiltinSOHNegative(t *testing.T) {
	hollowImages(t)

	topo := builtinTopology(builtinHollow("h1", "LAN", "10.50.0.1"))

	t.Run("SO-02 running-stage trigger surfaces errors", func(t *testing.T) {
		const name = "e2e-ba-neg"

		exp := builtinExperiment(t, name, topo, builtinScenario(`
  - name: soh
    metadata:
      c2Timeout: 15s
  - name: scorch
    metadata:
      runs:
      - start: [soh1]
      components:
      - name: soh1
        type: soh
        metadata:
          c2Timeout: 5s
          failOnError: true`))

		exp.start()
		builtinCheckSOHInactive(t, name)
		builtinCheckSOHBusy(t, name)

		t.Run("SC-05 failOnError fails the run", func(t *testing.T) {
			out := phenixFails(t, "experiment", "scorch", name, "--run", "0")
			wantContains(t, "phenix experiment scorch", out, "state of health checks")
		})
	})

	t.Run("SO-03 exitOnError fails the start", func(t *testing.T) {
		const name = "e2e-ba-negx"

		exp := builtinExperiment(t, name, topo, builtinScenario(`
  - name: soh
    metadata:
      c2Timeout: 10s
      exitOnError: true`))

		out := phenixFails(t, "experiment", "start", name)
		wantContains(t, "experiment start "+name, out, "running initial SoH checks")
		builtinWantNoNamespace(t, name)

		if st := exp.doc().Status.StartTime; st != "" {
			t.Errorf("status.startTime = %q after the failed start, want empty", st)
		}
	})
}

// builtinCheckSOHInactive checks what SOH recorded for a VM without C2.
func builtinCheckSOHInactive(t *testing.T, name string) {
	t.Helper()

	var soh builtinSOHStatus

	if !builtinAppStatus(t, name, "soh", &soh) || !soh.Initialized {
		t.Fatalf("status.apps.soh = %+v, want initialized", soh)
	}

	h, ok := soh.host("h1")
	if !ok || len(h.Networking) == 0 || !strings.Contains(h.Networking[0].Error, "C2 client not active") {
		t.Errorf("SOH h1 networking = %+v, want a C2 client not active error", h.Networking)
	}

	builtinWantSOHNode(t, builtinSOHREST(t, name), "h1", true)
}

// builtinCheckSOHBusy triggers SOH's running stage, runs SCORCH's soh
// component (failOnError) while it runs, and checks both outcomes.
func builtinCheckSOHBusy(t *testing.T, name string) {
	t.Helper()

	trigger := builtinStart(t, "experiment", "trigger", "running", name, "soh")

	eventually(t, 30*time.Second, 200*time.Millisecond, "SOH's running stage to start", func() (bool, string) {
		return builtinAppRunning(t, name, "soh") || trigger.finished(), "not running yet"
	})

	if trigger.finished() {
		t.Error("the SOH running stage finished before SCORCH could overlap it")
	}

	out, err := phenixRun("experiment", "scorch", name, "--run", "0")

	if err == nil || !strings.Contains(out, "app already running") {
		t.Errorf("SCORCH soh component with failOnError while SOH runs: err %v, want app already running\n%s", err, out)
	}

	out, err = trigger.wait(t, 2*time.Minute)
	if err == nil {
		t.Errorf("phenix experiment trigger running %s soh succeeded, want the SOH errors surfaced\n%s", name, out)
	}

	wantContains(t, "trigger running soh", out, "errors encountered in state of health app")

	if builtinAppRunning(t, name, "soh") {
		t.Error("status.appRunningStageStatus.soh is still true after the running stage")
	}
}
