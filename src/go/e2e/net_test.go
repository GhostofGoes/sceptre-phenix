//go:build e2e

package e2e

// Packet captures and netflow (F7, F17): per-interface VM captures through
// the CLI and REST, what pause and shutdown do to a VM's captures, subnet
// captures, the experiment files listing and download, and netflow from the
// experiment bridge to the netflow websocket. Two real guests (cfg.guest)
// make the traffic with ping over C2; hollow VMs stand in where no guest OS
// is needed. The pcap files minimega writes are parsed here, so the checks do
// not depend on tcpdump being installed on the runner.

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/url"
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

	"github.com/gorilla/websocket"
)

const (
	netExp        = "e2e-net"
	netPhxExp     = "e2e-net-phx"
	netCLIExp     = "e2e-net-cli"
	netMissingExp = "e2e-net-none" // never created

	// Each experiment that leaves the phenix bridge needs a bridge of its
	// own: phenix refuses two experiments on one default bridge.
	netBridge    = "e2enetbr"
	netCLIBridge = "e2enetcli"

	netLAN      = "10.0.0.0/24"
	netLAN2     = "10.0.1.0/24"
	netK1LAN    = "10.0.0.1"
	netK1LAN2   = "10.0.1.1"
	netK2LAN    = "10.0.0.2"
	netH1LAN    = "10.0.0.10"
	netH2LAN    = "10.0.0.11"
	netNobody   = "10.0.1.2" // no VM owns it; pinging it makes k1 ARP on LAN2
	netStateRun = "RUNNING"

	// netShutdownWait covers phenix's 30 s wait for an ACPI power-off before
	// it kills the VM (api/vm/vm.go shutdownTimeout); hollow VMs never power
	// off by themselves.
	netShutdownWait = 45 * time.Second

	// netFlowWait covers minimega's 10 s netflow active timeout
	// (cmd/minimega/capture.go captureNFTimeout) and Open vSwitch expiring
	// the idle ping flow, with margin.
	netFlowWait = 60 * time.Second

	// netWSCloseWait is how long a netflow websocket may stay open after the
	// netflow it streams was stopped.
	netWSCloseWait = 15 * time.Second

	netFlowStopped = "netflow stopped" // close text of web/netflow.go
)

// pcap and packet layout, for parsing the files minimega writes with gopacket's
// pcapgo writer.
const (
	netPcapMagic           = "\xd4\xc3\xb2\xa1" // 0xa1b2c3d4 little-endian, microseconds
	netPcapHeaderLen       = 24
	netPcapLinkTypeOffset  = 20
	netPcapRecordHeaderLen = 16
	netPcapInclLenOffset   = 8
	netLinkTypeEthernet    = 1

	netEtherTypeOffset = 12
	netEtherHeaderLen  = 14
	netVLANTagLen      = 4
	netEtherTypeIPv4   = 0x0800
	netEtherTypeARP    = 0x0806
	netEtherTypeVLAN   = 0x8100

	netIPv4MinLen      = 20
	netIPv4IHLMask     = 0x0f
	netIPv4WordLen     = 4
	netIPv4ProtoOffset = 9
	netIPv4SrcOffset   = 12
	netIPv4DstOffset   = 16
	netIPv4AddrLen     = 4
	netIPProtoICMP     = 1
	netICMPEchoReply   = 0
	netICMPEchoRequest = 8

	netARPLen        = 28
	netARPOpOffset   = 6
	netARPTPAOffset  = 24
	netARPOpRequest  = 1
	netPingsPerProbe = 5 // pings k1 sends in P-01; each has a reply
)

// netCapture is one entry of phenix's REST capture lists (web/proto/vm.proto).
type netCapture struct {
	VM        string `json:"vm"`
	Interface int    `json:"interface"`
	Filepath  string `json:"filepath"`
}

// netCaptureList is GET .../captures and the captureSubnet reply.
type netCaptureList struct {
	Captures []netCapture `json:"captures"`
}

// netFile is one entry of GET /experiments/{name}/files (util/file/types.go).
type netFile struct {
	Name       string   `json:"name"`
	Path       string   `json:"path"`
	Size       int64    `json:"size"`
	Categories []string `json:"categories"`
}

// netFlowFrame is one record phenix streams on the netflow websocket.
type netFlowFrame struct {
	Proto   int    `json:"proto"`
	Src     string `json:"src"`
	Sport   int    `json:"sport"`
	Dst     string `json:"dst"`
	Dport   int    `json:"dport"`
	Packets int    `json:"packets"`
	Bytes   int    `json:"bytes"`
}

// netFlowWS records what phenix sends on one netflow websocket.
type netFlowWS struct {
	mu     sync.Mutex
	got    []netFlowFrame
	err    error // why reading stopped
	closed chan struct{}
}

// TestNetCapturesAndNetflow runs one experiment on its own bridge with two
// real guests, k1 (LAN and LAN2) and k2 (LAN), and two hollow VMs on LAN,
// and checks packet captures (P-01 to P-05) and netflow (N-01) on it. The
// hollow-VM checks run while the guests boot.
func TestNetCapturesAndNetflow(t *testing.T) {
	requireImage(t, cfg.guest)
	hollowImages(t)
	netCleanupHost(t, netExp, netBridge)

	exp := newExperiment(t, netExp, netTopology(), "", "--default-bridge", netBridge)

	// Runs before the experiment's teardown, so a netflow the UI still holds
	// after a failed check is released; the CLI stop in the teardown does not
	// reach the UI's netflow.
	t.Cleanup(func() { _, _ = api(t, http.MethodDelete, "/experiments/"+netExp+"/netflow", nil) })

	start := time.Now()

	exp.start()
	exp.waitRunning(time.Minute, "k1", "k2", "h1", "h2")

	ns := exp.name

	t.Run("VMs run on the experiment's default bridge", func(t *testing.T) { netCheckBridge(t, exp) })
	t.Run("P-02 capture start rejects unknown interfaces", func(t *testing.T) { netCheckBadInterfaces(t, ns) })
	t.Run("P-04 pause stops a VM's captures first", func(t *testing.T) { netCheckPause(t, ns) })
	t.Run("P-04 shutdown stops a VM's captures first", func(t *testing.T) { netCheckShutdown(t, ns) })

	if !t.Run("guests register with C2 and reach each other", func(t *testing.T) {
		waitCCClients(t, ns, "k1", "k2")
		t.Logf("k1 and k2 registered with C2 %v after start", time.Since(start).Round(time.Second))
		netWaitGuestNetwork(t, ns)
	}) {
		t.FailNow()
	}

	t.Run("P-01 per-interface captures", func(t *testing.T) { netCheckVMCaptures(t, ns) })
	t.Run("P-05 experiment files listing and download", func(t *testing.T) { netCheckFiles(t, ns) })
	t.Run("P-03 subnet captures", func(t *testing.T) { netCheckSubnetCaptures(t, ns) })
	t.Run("N-01 netflow from the experiment bridge", func(t *testing.T) { netCheckNetflow(t, ns) })
	t.Run("N-01e stopping the experiment through REST ends netflow", func(t *testing.T) { netCheckRESTStop(t, exp) })
}

// TestNetNetflowRefusals checks the netflow and capture requests phenix must
// refuse: an unknown experiment, a stopped one, and netflow on an experiment
// that shares the phenix bridge (N-01a). A stopped experiment must not reach
// minimega at all.
func TestNetNetflowRefusals(t *testing.T) {
	hollowImages(t)
	netCleanupHost(t, netPhxExp, "")

	t.Run("an unknown experiment", func(t *testing.T) {
		netWantStatus(t, http.MethodPost, "/experiments/"+netMissingExp+"/netflow", http.StatusBadRequest, "unable to find experiment")

		if _, ok := mmNamespaces(t)[netMissingExp]; ok {
			t.Errorf("a netflow request for the unknown experiment %s created its minimega namespace", netMissingExp)
		}
	})

	exp := newExperiment(t, netPhxExp, topology(hollowNode("h1", "LAN", netH1LAN, "")), "")
	flow := "/experiments/" + exp.name + "/netflow"

	t.Run("a stopped experiment", func(t *testing.T) {
		_, hadNamespace := mmNamespaces(t)[exp.name]

		netWantStatus(t, http.MethodPost, flow, http.StatusConflict, "cannot start netflow on stopped experiment")
		netWantStatus(t, http.MethodGet, flow, http.StatusNotFound, "")
		netWantStatus(t, http.MethodDelete, flow, http.StatusNotFound, "not found")
		netWantStatus(t, http.MethodGet, flow+"/ws", http.StatusNotFound, "not found")

		for _, c := range []struct {
			args []string
			want string
		}{
			{[]string{"start", exp.name, "h1", "0", "x"}, "VM is not running"},
			{[]string{"start-subnet", exp.name, netLAN}, "packet captures can only be started for a running experiment"},
			{[]string{"stop-all", exp.name}, "packet captures can only be stopped for a running experiment"},
		} {
			if out := phenixFails(t, append([]string{"vm", "capture"}, c.args...)...); !strings.Contains(out, c.want) {
				t.Errorf("phenix vm capture %v failed without saying %q:\n%s", c.args, c.want, out)
			}
		}

		code, body := api(t, http.MethodPost, "/experiments/"+exp.name+"/captureSubnet", map[string]any{"subnet": netLAN})
		if code != http.StatusBadRequest || !strings.Contains(string(body), "experiment not running") {
			t.Errorf("POST captureSubnet on a stopped experiment = %d %q, want 400 saying it is not running", code, body)
		}

		// VM operations refuse the stopped experiment before they reach minimega.
		for _, op := range []string{"pause", "resume", "kill"} {
			if out := phenixFails(t, "vm", op, exp.name, "h1"); !strings.Contains(out, "experiment not running") {
				t.Errorf("phenix vm %s on a stopped experiment failed without saying it is not running:\n%s", op, out)
			}
		}

		if code, body := api(t, http.MethodPost, "/experiments/"+exp.name+"/vms/h1/start", nil); code != http.StatusBadRequest ||
			!strings.Contains(string(body), "experiment not running") {
			t.Errorf("POST vms/h1/start on a stopped experiment = %d %q, want 400 saying it is not running", code, body)
		}

		if _, ok := mmNamespaces(t)[exp.name]; ok && !hadNamespace {
			t.Errorf("capture, netflow and VM requests for the stopped experiment %s created its minimega namespace", exp.name)
		}
	})

	exp.start()
	exp.waitRunning(time.Minute, "h1")

	t.Run("N-01a netflow on the phenix bridge", func(t *testing.T) {
		refused := "cannot start netflow on experiment with default bridge set to 'phenix'"

		netWantStatus(t, http.MethodPost, flow, http.StatusConflict, refused)
		netWantStatus(t, http.MethodGet, flow, http.StatusNotFound, "")

		if rows := netNetflowRows(t, exp.name); len(rows) != 0 {
			t.Errorf("minimega has netflow captures for %s after phenix refused netflow: %v", exp.name, rows)
		}
	})
}

// TestNetNetflowCLIStop starts netflow through the UI on an experiment that
// is then stopped and started again with the CLI, as an operator at a shell
// would. Stopping an experiment ends its netflow (the stop hook in
// api/experiment/netflow.go), and the UI and the CLI share one store, so the
// UI must also let go of a netflow whose experiment another process stopped.
func TestNetNetflowCLIStop(t *testing.T) {
	hollowImages(t)
	netCleanupHost(t, netCLIExp, netCLIBridge)

	exp := newExperiment(t, netCLIExp, topology(hollowNode("h1", "LAN", netH1LAN, "")), "", "--default-bridge", netCLIBridge)
	flow := "/experiments/" + exp.name + "/netflow"

	// Runs before the experiment's teardown, so a netflow the UI still holds
	// is released on the running experiment.
	t.Cleanup(func() { _, _ = api(t, http.MethodDelete, flow, nil) })

	exp.start()
	exp.waitRunning(time.Minute, "h1")

	netWantStatus(t, http.MethodPost, flow, http.StatusNoContent, "")

	if rows := netNetflowRows(t, exp.name); len(rows) != 1 {
		t.Fatalf("minimega netflow captures for %s = %v, want one", exp.name, rows)
	}

	ws := netOpenFlowWS(t, exp.name)

	exp.stop()

	ws.waitClosed(t, netWSCloseWait)

	netWantStatus(t, http.MethodGet, flow, http.StatusNotFound, "")

	exp.start()
	exp.waitRunning(time.Minute, "h1")

	if code, body := api(t, http.MethodPost, flow, nil); code != http.StatusNoContent {
		t.Errorf("POST %s after a CLI stop and start = %d %q, want 204", flow, code, body)
	} else if rows := netNetflowRows(t, exp.name); len(rows) != 1 {
		t.Errorf("minimega netflow captures for %s after a restart = %v, want one", exp.name, rows)
	}
}

// ---------------------------------------------------------------------------
// topology and host cleanup

func netTopology() string {
	return topology(
		netGuest("k1", staticIface("eth0", "LAN", netK1LAN), staticIface("eth1", "LAN2", netK1LAN2)),
		netGuest("k2", staticIface("eth0", "LAN", netK2LAN)),
		hollowNode("h1", "LAN", netH1LAN, ""),
		hollowNode("h2", "LAN", netH2LAN, ""),
	)
}

// netGuest returns a real guest node booting cfg.guest with the given
// interfaces (from staticIface).
func netGuest(hostname string, ifaces ...string) string {
	return fmt.Sprintf(`
  - type: VirtualMachine
    general:
      hostname: %s
    hardware:
      vcpus: 1
      memory: 1024
      os_type: linux
      drives:
      - image: %s
    network:
      interfaces:%s`, hostname, cfg.guest, strings.Join(ifaces, ""))
}

// netCleanupHost removes what an experiment leaves on the host after phenix
// deletes it: its files directory under minimega's -filepath, which holds the
// captures, and the Open vSwitch bridge minimega created for it (none when
// bridge is empty). Call it before newExperiment, so it runs after the
// experiment's own teardown.
func netCleanupHost(t *testing.T, exp, bridge string) {
	t.Helper()

	t.Cleanup(func() {
		if err := os.RemoveAll(filepath.Join(cfg.imagesDir, exp)); err != nil {
			t.Errorf("cleanup: removing the files of %s: %v", exp, err)
		}

		if bridge == "" {
			return
		}

		if _, err := mmRun("", "bridge destroy "+bridge); err != nil {
			t.Logf("cleanup: minimega could not destroy bridge %s (%v); removing it with ovs-vsctl", bridge, err)

			out, err := exec.Command("ovs-vsctl", "--if-exists", "del-br", bridge).CombinedOutput() //nolint:gosec // test
			if err != nil {
				t.Errorf("cleanup: ovs-vsctl del-br %s: %v\n%s", bridge, err, out)
			}
		}
	})
}

// ---------------------------------------------------------------------------
// checks on hollow VMs, run while the guests boot

func netCheckBridge(t *testing.T, exp *experiment) {
	t.Helper()

	doc := exp.with(t).doc()

	if got := doc.Spec["defaultBridge"]; got != netBridge {
		t.Errorf("spec.defaultBridge = %v, want %s", got, netBridge)
	}

	for name, row := range vmInfo(t, exp.name) {
		for tap := range strings.SplitSeq(strings.Trim(row["tap"], "[]"), ", ") {
			if br, err := ovsVsctl(t, "port-to-br", tap); err != nil || br != netBridge {
				t.Errorf("%s tap %q is on bridge %q (%v), want %s", name, tap, br, err, netBridge)
			}
		}
	}
}

func netCheckBadInterfaces(t *testing.T, ns string) {
	t.Helper()

	for _, c := range []struct{ iface, want string }{
		{"if9", `no interface named "if9"`},
		{"5", "interface index 5 is out of range"},
	} {
		if out := phenixFails(t, "vm", "capture", "start", ns, "h1", c.iface, "z"); !strings.Contains(out, c.want) {
			t.Errorf("capture start on h1 interface %s failed without saying %q:\n%s", c.iface, c.want, out)
		}
	}

	path := "/experiments/" + ns + "/vms/h1/captures"

	code, body := api(t, http.MethodPost, path, map[string]any{"interface": 5, "filename": "z"})
	if code < 400 || !strings.Contains(string(body), "invalid interface") {
		t.Errorf("POST %s with interface 5 = %d %q, want an invalid interface error", path, code, body)
	}

	netWantCaptures(t, ns, nil)

	if _, err := os.Stat(netPcapPath(ns, "z.pcap")); !os.IsNotExist(err) {
		t.Errorf("a refused capture created z.pcap (stat: %v)", err)
	}
}

func netCheckPause(t *testing.T, ns string) {
	t.Helper()

	// The interface is named case-insensitively, and only the base of the
	// file name is used, inside the experiment's files directory.
	phenix(t, "vm", "capture", "start", ns, "h1", "ETH0", "../../h1cap")
	netWantCaptures(t, ns, map[string]string{"h1:0": "h1cap.pcap"})

	if _, err := os.Stat(filepath.Join(cfg.imagesDir, "h1cap.pcap")); !os.IsNotExist(err) {
		t.Errorf("a capture file name with ../ escaped the experiment files directory (stat: %v)", err)
	}

	// Pause sends the capture delete and `vm stop` back to back, too close
	// together for a sampler to see them overlap, so the order is read from
	// the requests minimega logged.
	requests := coreMMDebugLog(t, func() { phenix(t, "vm", "pause", ns, "h1") })
	netWantRequestOrder(t, requests,
		`namespace "`+regexp.QuoteMeta(ns)+`" capture pcap delete vm h1\b`,
		`namespace "`+regexp.QuoteMeta(ns)+`" vm stop h1\b`,
	)

	if state := vmInfo(t, ns)["h1"]["state"]; state != "PAUSED" {
		t.Errorf("h1 is %q after phenix vm pause, want PAUSED", state)
	}

	netWantCaptures(t, ns, nil)
	t.Logf("h1cap.pcap holds %d frames", len(netReadPcap(t, netPcapPath(ns, "h1cap.pcap"))))

	if out := phenixFails(t, "vm", "capture", "start", ns, "h1", "0", "h1again"); !strings.Contains(out, "VM is not running") {
		t.Errorf("capture start on paused h1 failed without saying the VM is not running:\n%s", out)
	}

	phenix(t, "vm", "resume", ns, "h1")
	netWantState(t, ns, "h1", netStateRun)

	// Resuming a VM does not resume the captures pausing it stopped.
	netWantCaptures(t, ns, nil)
}

func netCheckShutdown(t *testing.T, ns string) {
	t.Helper()

	phenix(t, "vm", "capture", "start", ns, "h2", "0", "h2cap.pcap")
	netWantCaptures(t, ns, map[string]string{"h2:0": "h2cap.pcap"})

	began := time.Now()

	// Shutdown stops the captures, asks for an ACPI power-off, and after
	// waiting for it kills the VM; the order is read from the requests
	// minimega logged.
	requests := coreMMDebugLog(t, func() { phenix(t, "vm", "shutdown", ns, "h2") })
	netWantRequestOrder(t, requests,
		`namespace "`+regexp.QuoteMeta(ns)+`" capture pcap delete vm h2\b`,
		`namespace "`+regexp.QuoteMeta(ns)+`" vm qmp h2 .*system_powerdown`,
	)

	t.Logf("phenix vm shutdown of hollow h2 returned after %v (no OS, so phenix falls back to vm kill)",
		time.Since(began).Round(time.Second))

	if took := time.Since(began); took > netShutdownWait {
		t.Errorf("phenix vm shutdown of h2 took %v, want at most %v", took, netShutdownWait)
	}

	row, ok := vmInfo(t, ns)["h2"]
	if !ok || row["state"] != "QUIT" {
		t.Errorf("h2 after phenix vm shutdown: listed %v, state %q; want listed and QUIT", ok, row["state"])
	}

	netWantCaptures(t, ns, nil)
	netReadPcap(t, netPcapPath(ns, "h2cap.pcap"))
}

// netWantRequestOrder checks that minimega received, among the log lines
// coreMMDebugLog returned, a request matching first before any request
// matching then.
func netWantRequestOrder(t *testing.T, lines []string, first, then string) {
	t.Helper()

	var (
		firstRE = regexp.MustCompile(first)
		thenRE  = regexp.MustCompile(then)
		at      = map[*regexp.Regexp]int{firstRE: -1, thenRE: -1}
		seen    []string
	)

	for _, line := range lines {
		m := coreRequestLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}

		seen = append(seen, m[1])

		for re, i := range at {
			if i < 0 && re.MatchString(m[1]) {
				at[re] = len(seen) - 1
			}
		}
	}

	if at[firstRE] < 0 || at[thenRE] < 0 || at[firstRE] > at[thenRE] {
		t.Errorf("minimega received %q at request %d and %q at request %d, want the first before the second; requests: %q",
			first, at[firstRE], then, at[thenRE], seen)
	}
}

// netWantState waits for minimega to report vm in state.
func netWantState(t *testing.T, ns, vm, state string) {
	t.Helper()

	eventually(t, 30*time.Second, time.Second, vm+" "+state, func() (bool, string) {
		got := vmInfo(t, ns)[vm]["state"]

		return got == state, got
	})
}

// ---------------------------------------------------------------------------
// checks on the real guests

// netWaitGuestNetwork waits until the startup injection gave k1 both its
// addresses and k1 reaches k2 over LAN.
func netWaitGuestNetwork(t *testing.T, ns string) {
	t.Helper()

	script := fmt.Sprintf(
		"for _ in $(seq 1 45); do "+
			"if ip -4 -o addr show | grep -q ' %s/24 ' && ip -4 -o addr show | grep -q ' %s/24 '; then break; fi; sleep 1; done; "+
			"ip -4 -o addr show | grep -q ' %s/24 ' && ip -4 -o addr show | grep -q ' %s/24 ' && echo addressed || echo unaddressed; "+
			"for _ in $(seq 1 30); do if ping -c 1 -W 1 %s >/dev/null 2>&1; then echo reachable; exit 0; fi; sleep 1; done; echo unreachable",
		netK1LAN, netK1LAN2, netK1LAN, netK1LAN2, netK2LAN,
	)

	if out := strings.Fields(guestShell(t, ns, "k1", script)); !slices.Equal(out, []string{"addressed", "reachable"}) {
		t.Fatalf("k1 network: %v, want its addresses %s and %s and k2 (%s) reachable", out, netK1LAN, netK1LAN2, netK2LAN)
	}
}

// netCheckVMCaptures is P-01: captures on each of k1's interfaces, through
// interface names and indexes, the traffic each file holds, and stopping one
// interface at a time.
func netCheckVMCaptures(t *testing.T, ns string) {
	t.Helper()

	phenix(t, "vm", "capture", "start", ns, "k1", "eth0", "a0")
	phenix(t, "vm", "capture", "start", ns, "k1", "1", "a1.pcap")

	both := map[string]string{"k1:0": "a0.pcap", "k1:1": "a1.pcap"}
	netWantCaptures(t, ns, both)
	netWantRESTCaptures(t, "/experiments/"+ns+"/captures", ns, both)
	netWantRESTCaptures(t, "/experiments/"+ns+"/vms/k1/captures", ns, both)
	netWantRESTCaptures(t, "/experiments/"+ns+"/vms/k2/captures", ns, nil)

	if out := phenixFails(t, "vm", "capture", "start", ns, "k1", "eth0", "a0-again"); !strings.Contains(out, "capture already exists") {
		t.Errorf("a second capture on k1 eth0 failed without saying the capture exists:\n%s", out)
	}

	netWantCaptures(t, ns, both)

	if _, err := os.Stat(netPcapPath(ns, "a0-again.pcap")); !os.IsNotExist(err) {
		t.Errorf("the refused second capture created a0-again.pcap (stat: %v)", err)
	}

	ping := fmt.Sprintf("ping -c %d -i 0.2 -W 2 %s >/dev/null 2>&1 && echo ok || echo fail", netPingsPerProbe, netK2LAN)
	if out := strings.TrimSpace(guestShell(t, ns, "k1", ping)); out != "ok" {
		t.Fatalf("k1 cannot ping k2 while captured: %q", out)
	}

	a0 := netPcapPath(ns, "a0.pcap")
	netWaitFrames(t, a0, "the ICMP echo packets between k1 and k2", 2*netPingsPerProbe, netCountK1K2Echo)

	phenix(t, "vm", "capture", "stop", ns, "k1", "0")
	netWantCaptures(t, ns, map[string]string{"k1:1": "a1.pcap"})
	netWantRESTCaptures(t, "/experiments/"+ns+"/vms/k1/captures", ns, map[string]string{"k1:1": "a1.pcap"})

	if n := netCountK1K2Echo(netReadPcap(t, a0)); n < 2*netPingsPerProbe {
		t.Errorf("a0.pcap holds %d ICMP echo packets between k1 and k2, want at least %d", n, 2*netPingsPerProbe)
	}

	sizeA0 := netFileSize(t, a0)

	// Traffic on both of k1's interfaces: a0 is stopped, a1 still captures.
	traffic := fmt.Sprintf(
		"ping -c 3 -i 0.2 -W 2 %s >/dev/null 2>&1; ip neigh flush to %s >/dev/null 2>&1; ping -c 2 -W 1 %s >/dev/null 2>&1; echo done",
		netK2LAN, netNobody, netNobody,
	)
	if out := strings.TrimSpace(guestShell(t, ns, "k1", traffic)); out != "done" {
		t.Fatalf("k1 traffic script: %q", out)
	}

	if size := netFileSize(t, a0); size != sizeA0 {
		t.Errorf("a0.pcap grew from %d to %d bytes after its capture was stopped", sizeA0, size)
	}

	a1 := netPcapPath(ns, "a1.pcap")
	netWaitFrames(t, a1, "an ARP request for "+netNobody, 1, func(frames [][]byte) int {
		return netCountARPRequests(frames, netNobody)
	})

	phenix(t, "vm", "capture", "stop", ns, "k1")
	netWantCaptures(t, ns, nil)

	frames := netReadPcap(t, a1)
	if n := netCountARPRequests(frames, netNobody); n == 0 {
		t.Errorf("a1.pcap (k1 on LAN2) holds no ARP request for %s, which k1 pinged over LAN2", netNobody)
	}

	if n := netCountK1K2Echo(frames); n != 0 {
		t.Errorf("a1.pcap (k1 on LAN2) holds %d ICMP echo packets of the LAN ping between k1 and k2", n)
	}

	netCheckStopWithoutCaptures(t, ns)
}

// netCheckStopWithoutCaptures checks that stopping captures k1 does not have
// fails and says so, through the CLI and REST.
func netCheckStopWithoutCaptures(t *testing.T, ns string) {
	t.Helper()

	for _, args := range [][]string{{"k1"}, {"k1", "eth1"}} {
		cmd := append([]string{"vm", "capture", "stop", ns}, args...)
		if out := phenixFails(t, cmd...); !strings.Contains(out, "no captures exist") {
			t.Errorf("phenix %v without captures failed without saying so:\n%s", cmd, out)
		}
	}

	path := "/experiments/" + ns + "/vms/k1/captures"

	for _, query := range []string{"", "?iface=0"} {
		if code, body := api(t, http.MethodDelete, path+query, nil); code < 400 || !strings.Contains(string(body), "no captures exist") {
			t.Errorf("DELETE %s%s without captures = %d %q, want an error saying no captures exist", path, query, code, body)
		}
	}

	netWantStatus(t, http.MethodDelete, path+"?iface=eth0", http.StatusBadRequest, "iface query parameter must be an integer")
}

// netCheckFiles is P-05: the files listing and download of capture files, and
// that a running capture blocks the download of its own file only.
func netCheckFiles(t *testing.T, ns string) {
	t.Helper()

	data, err := os.ReadFile(netPcapPath(ns, "a0.pcap"))
	if err != nil {
		t.Fatalf("P-01 left no a0.pcap: %v", err)
	}

	var listing struct {
		Files []netFile `json:"files"`
	}

	apiOK(t, http.MethodGet, "/experiments/"+ns+"/files", nil, &listing)

	files := make(map[string]netFile, len(listing.Files))
	for _, f := range listing.Files {
		files[f.Path] = f
	}

	for _, name := range []string{"a0.pcap", "a1.pcap", "h1cap.pcap", "h2cap.pcap"} {
		f, ok := files[name]
		if !ok {
			t.Errorf("GET files does not list %s: %v", name, listing.Files)

			continue
		}

		if f.Name != name || f.Size != netFileSize(t, netPcapPath(ns, name)) || !slices.Contains(f.Categories, "Packet Capture") {
			t.Errorf("GET files lists %s as %+v, want its name, on-disk size and the Packet Capture category", name, f)
		}
	}

	echo := netCountK1K2Echo(netParsePcap(t, "a0.pcap", data))

	for _, query := range []string{"?path=a0.pcap", ""} {
		netWantDownload(t, ns, "a0.pcap"+query, data, echo)
	}

	apiOK(t, http.MethodPost, "/experiments/"+ns+"/vms/k2/captures", map[string]any{"interface": 0, "filename": "b"}, nil)
	netWantCaptures(t, ns, map[string]string{"k2:0": "b.pcap"})

	netWantDownload(t, ns, "a0.pcap", data, echo)

	for _, file := range []string{"b.pcap", "b.pcap?path=b.pcap"} {
		code, _, body := apiGet(t, "/experiments/"+ns+"/files/"+file)
		if code != http.StatusBadRequest || !strings.Contains(string(body), "capture still in progress") {
			t.Errorf("GET files/%s while b.pcap is captured = %d %q, want 400 capture still in progress", file, code, body)
		}
	}

	for _, file := range []string{"missing.pcap", "x?path=" + url.QueryEscape("../../../../etc/hostname")} {
		code, _, body := apiGet(t, "/experiments/"+ns+"/files/"+file)
		if code < 400 || !strings.Contains(string(body), "file not found") {
			t.Errorf("GET files/%s = %d %q, want file not found", file, code, body)
		}
	}

	apiOK(t, http.MethodDelete, "/experiments/"+ns+"/vms/k2/captures?iface=0", nil, nil)
	netWantCaptures(t, ns, nil)

	code, _, body := apiGet(t, "/experiments/"+ns+"/files/b.pcap")
	if code != http.StatusOK {
		t.Fatalf("GET files/b.pcap after its capture stopped = %d %q, want 200", code, body)
	}

	netParsePcap(t, "the b.pcap download", body)
}

// netWantDownload downloads files/<file> (which may carry a query) and checks
// it is the capture file, byte for byte.
func netWantDownload(t *testing.T, ns, file string, want []byte, echo int) {
	t.Helper()

	code, header, body := apiGet(t, "/experiments/"+ns+"/files/"+file)
	if code != http.StatusOK {
		t.Errorf("GET files/%s = %d %q, want 200", file, code, body)

		return
	}

	if got := header.Get("Content-Disposition"); got != "attachment; filename=a0.pcap" {
		t.Errorf("GET files/%s Content-Disposition = %q, want attachment; filename=a0.pcap", file, got)
	}

	if !bytes.Equal(body, want) {
		t.Errorf("GET files/%s returned %d bytes that differ from the %d on disk", file, len(body), len(want))
	}

	if n := netCountK1K2Echo(netParsePcap(t, "the download of "+file, body)); n != echo {
		t.Errorf("the download of %s holds %d ICMP echo packets, the file %d", file, n, echo)
	}
}

// netCheckSubnetCaptures is P-03: phenix captures the interface of every
// running VM whose address is in a subnet, and stops them by subnet or all at
// once.
func netCheckSubnetCaptures(t *testing.T, ns string) {
	t.Helper()

	info := vmInfo(t, ns)

	var want []string

	for _, vm := range []string{"k1", "k2", "h1", "h2"} {
		if info[vm]["state"] == netStateRun {
			want = append(want, vm+":0")
		}
	}

	t.Logf("VM states: k1 %s, k2 %s, h1 %s, h2 %s", info["k1"]["state"], info["k2"]["state"], info["h1"]["state"], info["h2"]["state"])

	began := time.Now()

	phenix(t, "vm", "capture", "start-subnet", ns, netLAN)

	ended := time.Now()
	first := netWantSubnetCaptures(t, ns, want...)
	netCheckSubnetStamps(t, first, began, ended)

	phenix(t, "vm", "capture", "stop-subnet", ns, netLAN)
	netWantCaptures(t, ns, nil)

	// The REST capture below captures k2:0 again at once, often within the
	// second of the first capture's stamp. minimega truncates an existing
	// capture file, so phenix must name it after a free file. Files named for
	// the next seconds make it find its stamped name taken in every run.
	firstK2, k2Captured := first["k2:0"]

	var firstK2Data []byte

	if k2Captured {
		firstK2Data = netReadFile(t, firstK2)
	}

	decoys := netSubnetDecoys(t, ns, "k2", 0, time.Now(), netDecoySeconds)

	// REST, limited to a list of VMs.
	var list netCaptureList

	apiOK(t, http.MethodPost, "/experiments/"+ns+"/captureSubnet", map[string]any{"subnet": netLAN, "vms": []string{"k2"}}, &list)

	again := netWantSubnetCaptures(t, ns, "k2:0")
	netWantListed(t, "captureSubnet "+netLAN+" for k2", list.Captures, again)

	existing := maps.Clone(decoys)

	if k2Captured {
		existing[firstK2] = firstK2Data
	}

	for path, data := range existing {
		if again["k2:0"] == path {
			t.Errorf("the second subnet capture of k2:0 reuses %s, which existed and which minimega truncates", path)
		}

		if got := netReadFile(t, path); !bytes.Equal(got, data) {
			t.Errorf("the second subnet capture of k2:0 changed the existing file %s: %d bytes, %d before",
				path, len(got), len(data))
		}
	}

	netRemoveDecoys(t, decoys)

	began = time.Now()

	apiOK(t, http.MethodPost, "/experiments/"+ns+"/captureSubnet", map[string]any{"subnet": netLAN2}, &list)

	rows := netWantSubnetCaptures(t, ns, "k1:1", "k2:0")
	netWantListed(t, "captureSubnet "+netLAN2, list.Captures, map[string]string{"k1:1": rows["k1:1"]})
	netCheckSubnetStamps(t, map[string]string{"k1:1": rows["k1:1"]}, began, time.Now())

	var stopped struct {
		VMs []string `json:"vms"`
	}

	apiOK(t, http.MethodPost, "/experiments/"+ns+"/stopCaptureSubnet", map[string]any{"subnet": netLAN2}, &stopped)

	if !slices.Equal(stopped.VMs, []string{"k1"}) {
		t.Errorf("stopCaptureSubnet %s stopped the captures of %v, want [k1]", netLAN2, stopped.VMs)
	}

	netWantSubnetCaptures(t, ns, "k2:0")

	phenix(t, "vm", "capture", "stop-all", ns)
	netWantCaptures(t, ns, nil)

	if out := phenixFails(t, "vm", "capture", "start-subnet", ns, "not-a-subnet"); !strings.Contains(out, "invalid ipv4 subnet") {
		t.Errorf("start-subnet with a bad subnet failed without saying so:\n%s", out)
	}

	netWantCaptures(t, ns, nil)
}

// netWantSubnetCaptures checks that minimega lists exactly the given VM
// interfaces as captured, each into <vm>_<iface>_<timestamp>.pcap in the
// experiment's files directory (api/vm/vm.go CaptureSubnet), and returns the
// rows.
func netWantSubnetCaptures(t *testing.T, ns string, ifaces ...string) map[string]string {
	t.Helper()

	rows := netCaptureRows(t, ns)

	if got := slices.Sorted(maps.Keys(rows)); !slices.Equal(got, slices.Sorted(slices.Values(ifaces))) {
		t.Errorf("minimega captures %v after the subnet request, want %v", got, ifaces)
	}

	dir := filepath.Join(cfg.imagesDir, ns, "files")

	for iface, path := range rows {
		vm, idx, _ := strings.Cut(iface, ":")

		base := filepath.Base(path)

		if m := netSubnetStamp.FindStringSubmatch(base); filepath.Dir(path) != dir || m == nil || base != vm+"_"+idx+m[0] {
			t.Errorf("subnet capture %s writes %s, want %s/%s_%s_<yyyymmdd>_<hhmmss>[_<n>].pcap", iface, path, dir, vm, idx)
		}
	}

	return rows
}

// netSubnetStamp matches the timestamp phenix puts at the end of a subnet
// capture's file name, <yyyymmdd>_<hhmmss>, and the _<n> it adds when a file
// of that name exists.
var netSubnetStamp = regexp.MustCompile(`_(\d{8}_\d{6})(?:_\d+)?\.pcap$`)

// netStampLayout is the layout of netSubnetStamp's timestamp.
const netStampLayout = "20060102_150405"

// netCheckSubnetStamps checks that each subnet capture file in rows is named
// after the second its capture started, which lies between began and ended.
// A coarser name makes a later subnet capture of the interface within the
// same hour or minute truncate the earlier capture's file.
func netCheckSubnetStamps(t *testing.T, rows map[string]string, began, ended time.Time) {
	t.Helper()

	for iface, path := range rows {
		m := netSubnetStamp.FindStringSubmatch(filepath.Base(path))
		if m == nil {
			continue // netWantSubnetCaptures reports the malformed name
		}

		//nolint:gosmopolitan // phenix stamps the name with its own local time
		stamp, err := time.ParseInLocation(netStampLayout, m[1], time.Local)

		if err != nil || stamp.Before(began.Truncate(time.Second)) || stamp.After(ended) {
			t.Errorf("subnet capture %s is named %s, want the second it started (%s to %s)",
				iface, filepath.Base(path), began.Format(netStampLayout), ended.Format(netStampLayout))
		}
	}
}

// netDecoySeconds is how many seconds of subnet capture names
// netSubnetDecoys takes: more than the REST call after it may take to start.
const netDecoySeconds = 30

// netSubnetDecoys creates in the experiment's files directory the subnet
// capture files of interface iface of vm for each of the seconds seconds from
// from on, where none exists, and returns their paths and contents. They are
// removed when the test ends.
func netSubnetDecoys(t *testing.T, ns, vm string, iface int, from time.Time, seconds int) map[string][]byte {
	t.Helper()

	decoys := make(map[string][]byte, seconds)

	t.Cleanup(func() {
		for path := range decoys {
			_ = os.Remove(path)
		}
	})

	for i := range seconds {
		stamp := from.Add(time.Duration(i) * time.Second).Format(netStampLayout)
		path := netPcapPath(ns, fmt.Sprintf("%s_%d_%s.pcap", vm, iface, stamp))
		data := []byte("e2e: an earlier capture " + stamp + "\n")

		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644) //nolint:gosec // the test's experiment
		if errors.Is(err, os.ErrExist) {
			continue // a real capture, which the caller checks
		}

		if err != nil {
			t.Fatalf("creating %s: %v", path, err)
		}

		decoys[path] = data

		_, err = f.Write(data)
		if cerr := f.Close(); err == nil {
			err = cerr
		}

		if err != nil {
			t.Fatalf("writing %s: %v", path, err)
		}
	}

	return decoys
}

// netRemoveDecoys removes the files netSubnetDecoys created, so the checks
// after it see only real captures.
func netRemoveDecoys(t *testing.T, decoys map[string][]byte) {
	t.Helper()

	for path := range decoys {
		if err := os.Remove(path); err != nil {
			t.Errorf("removing %s: %v", path, err)
		}
	}
}

// netReadFile returns the content of a capture file.
func netReadFile(t *testing.T, path string) []byte {
	t.Helper()

	data, err := os.ReadFile(path) //nolint:gosec // a capture file of the test's experiment
	if err != nil {
		t.Fatalf("reading capture file %s: %v", path, err)
	}

	return data
}

// netCheckNetflow is N-01 b-d: netflow on the experiment bridge, its records
// on the websocket, and stopping it.
func netCheckNetflow(t *testing.T, ns string) {
	t.Helper()

	flow := "/experiments/" + ns + "/netflow"

	netWantStatus(t, http.MethodPost, flow, http.StatusNoContent, "")
	netWantStatus(t, http.MethodGet, flow, http.StatusNoContent, "")

	rows := netNetflowRows(t, ns)
	if len(rows) != 1 {
		t.Fatalf("minimega netflow captures for %s = %v, want one", ns, rows)
	}

	row := rows[0]

	host, port, ok := strings.Cut(strings.TrimPrefix(row["path"], "udp:"), ":")
	if _, err := strconv.Atoi(port); row["bridge"] != netBridge || row["mode"] != "ascii" || !ok ||
		host != headnode(t) || err != nil {
		t.Errorf("minimega netflow capture %v, want bridge %s, mode ascii, path udp:%s:<port>", row, netBridge, headnode(t))
	}

	netWantStatus(t, http.MethodPost, flow, http.StatusBadRequest, "neflow already started for experiment")

	ws := netOpenFlowWS(t, ns)

	ping := "ping -c 20 -i 0.25 -W 2 " + netK2LAN + " >/dev/null 2>&1 && echo ok || echo fail"
	if out := strings.TrimSpace(guestShell(t, ns, "k1", ping)); out != "ok" {
		t.Fatalf("k1 cannot ping k2 while netflow runs: %q", out)
	}

	eventually(t, netFlowWait, 500*time.Millisecond, "a netflow record of k1 pinging k2", func() (bool, string) {
		frames := ws.frames()

		return slices.ContainsFunc(frames, func(f netFlowFrame) bool {
			return f.Src == netK1LAN && f.Dst == netK2LAN && f.Proto == netIPProtoICMP && f.Packets >= 1
		}), fmt.Sprintf("%d records: %v", len(frames), frames)
	})

	netWantStatus(t, http.MethodDelete, flow, http.StatusNoContent, "")
	ws.waitClosed(t, netWSCloseWait)

	netWantStatus(t, http.MethodDelete, flow, http.StatusNotFound, "not found")
	netWantStatus(t, http.MethodGet, flow, http.StatusNotFound, "")
	netWantStatus(t, http.MethodGet, flow+"/ws", http.StatusNotFound, "not found")

	if rows := netNetflowRows(t, ns); len(rows) != 0 {
		t.Errorf("minimega still has netflow captures for %s after DELETE: %v", ns, rows)
	}
}

// netCheckRESTStop is N-01e: stopping the experiment through the UI that runs
// its netflow ends the netflow and closes its websocket.
func netCheckRESTStop(t *testing.T, exp *experiment) {
	t.Helper()

	flow := "/experiments/" + exp.name + "/netflow"

	netWantStatus(t, http.MethodPost, flow, http.StatusNoContent, "")

	ws := netOpenFlowWS(t, exp.name)

	apiOK(t, http.MethodPost, "/experiments/"+exp.name+"/stop", nil, nil)

	ws.waitClosed(t, netWSCloseWait)
	netWantStatus(t, http.MethodGet, flow, http.StatusNotFound, "")

	if ns, ok := mmNamespaces(t)[exp.name]; ok {
		t.Errorf("namespace %s exists after the REST stop: %v", exp.name, ns)
	}

	eventually(t, 30*time.Second, time.Second, "QEMU processes to exit", func() (bool, string) {
		n := qemuProcesses(t)

		return n == 0, fmt.Sprintf("%d still running", n)
	})
}

// ---------------------------------------------------------------------------
// minimega captures

// netPcapPath is where minimega writes a capture phenix names file.
func netPcapPath(ns, file string) string {
	return filepath.Join(cfg.imagesDir, ns, "files", file)
}

// netCaptureRows returns minimega's VM pcap captures in namespace ns as
// "<vm>:<iface>" -> the absolute path minimega writes.
func netCaptureRows(t *testing.T, ns string) map[string]string {
	t.Helper()

	out := make(map[string]string)

	for _, row := range mmTable(t, ns, "capture") {
		if row["type"] == "pcap" && row["interface"] != "" {
			out[row["interface"]] = row["path"]
		}
	}

	return out
}

// netNetflowRows returns minimega's netflow captures in namespace ns.
func netNetflowRows(t *testing.T, ns string) []map[string]string {
	t.Helper()

	var out []map[string]string

	for _, row := range mmTable(t, ns, "capture") {
		if row["type"] == "netflow" {
			out = append(out, row)
		}
	}

	return out
}

// netWantCaptures checks that minimega lists exactly the given VM captures,
// as "<vm>:<iface>" -> file name in the experiment's files directory.
func netWantCaptures(t *testing.T, ns string, want map[string]string) {
	t.Helper()

	paths := make(map[string]string, len(want))
	for iface, file := range want {
		paths[iface] = netPcapPath(ns, file)
	}

	if got := netCaptureRows(t, ns); !maps.Equal(got, paths) {
		t.Errorf("minimega captures in %s = %v, want %v", ns, got, paths)
	}
}

// netWantRESTCaptures checks that a REST capture list names exactly the given
// captures, as netWantCaptures does.
func netWantRESTCaptures(t *testing.T, path, ns string, want map[string]string) {
	t.Helper()

	var list netCaptureList

	apiOK(t, http.MethodGet, path, nil, &list)

	paths := make(map[string]string, len(want))
	for iface, file := range want {
		paths[iface] = netPcapPath(ns, file)
	}

	netWantListed(t, "GET "+path, list.Captures, paths)
}

// netWantListed compares a REST capture list with "<vm>:<iface>" -> path.
func netWantListed(t *testing.T, what string, got []netCapture, want map[string]string) {
	t.Helper()

	listed := make(map[string]string, len(got))
	for _, c := range got {
		listed[fmt.Sprintf("%s:%d", c.VM, c.Interface)] = c.Filepath
	}

	if len(listed) != len(got) || !maps.Equal(listed, want) {
		t.Errorf("%s lists captures %v, want %v", what, got, want)
	}
}

// ---------------------------------------------------------------------------
// pcap files

func netFileSize(t *testing.T, path string) int64 {
	t.Helper()

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	return fi.Size()
}

// netReadPcap reads a capture file and returns its frames.
func netReadPcap(t *testing.T, path string) [][]byte {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading capture %s: %v", path, err)
	}

	return netParsePcap(t, path, data)
}

// netParsePcap checks that data is a complete little-endian, microsecond,
// Ethernet pcap file, as minimega writes, and returns its frames.
func netParsePcap(t *testing.T, name string, data []byte) [][]byte {
	t.Helper()

	frames, off := netPcapFrames(t, name, data)
	if off != len(data) {
		t.Errorf("%s ends in a truncated record at offset %d of %d", name, off, len(data))
	}

	return frames
}

// netPcapFrames checks the pcap file header of data and returns the complete
// records and where they end; a file minimega is still writing may end inside
// a record.
func netPcapFrames(t *testing.T, name string, data []byte) ([][]byte, int) {
	t.Helper()

	if len(data) < netPcapHeaderLen {
		t.Fatalf("%s is %d bytes, shorter than a pcap file header", name, len(data))
	}

	if magic := data[:len(netPcapMagic)]; string(magic) != netPcapMagic {
		t.Fatalf("%s starts with % x, want the pcap magic d4 c3 b2 a1", name, magic)
	}

	if lt := binary.LittleEndian.Uint32(data[netPcapLinkTypeOffset:netPcapHeaderLen]); lt != netLinkTypeEthernet {
		t.Fatalf("%s has link type %d, want Ethernet (1)", name, lt)
	}

	var (
		frames [][]byte
		off    = netPcapHeaderLen
	)

	for off+netPcapRecordHeaderLen <= len(data) {
		incl := int(binary.LittleEndian.Uint32(data[off+netPcapInclLenOffset:]))
		begin := off + netPcapRecordHeaderLen

		if begin+incl > len(data) {
			break
		}

		frames = append(frames, data[begin:begin+incl])
		off = begin + incl
	}

	return frames, off
}

// netWaitFrames waits until the capture minimega is writing to path holds
// frames that count reports as at least want. libpcap hands packets over up
// to its 1 s read timeout late, and minimega drops what it has not read when
// a capture stops.
func netWaitFrames(t *testing.T, path, what string, want int, count func([][]byte) int) {
	t.Helper()

	eventually(t, 15*time.Second, 200*time.Millisecond, what+" in "+filepath.Base(path), func() (bool, string) {
		data, err := os.ReadFile(path)
		if err != nil {
			return false, err.Error()
		}

		frames, _ := netPcapFrames(t, path, data)
		n := count(frames)

		return n >= want, fmt.Sprintf("%d of %d frames match", n, len(frames))
	})
}

// netEtherPayload returns a frame's EtherType and payload, past any 802.1Q
// tags.
func netEtherPayload(frame []byte) (uint16, []byte) {
	if len(frame) < netEtherHeaderLen {
		return 0, nil
	}

	typ := binary.BigEndian.Uint16(frame[netEtherTypeOffset:])
	rest := frame[netEtherHeaderLen:]

	for typ == netEtherTypeVLAN && len(rest) >= netVLANTagLen {
		typ = binary.BigEndian.Uint16(rest[netVLANTagLen/2:])
		rest = rest[netVLANTagLen:]
	}

	return typ, rest
}

// netCountK1K2Echo counts ICMP echo requests and replies between k1 and k2
// on LAN, either way.
func netCountK1K2Echo(frames [][]byte) int {
	var (
		ipA = net.ParseIP(netK1LAN).To4()
		ipB = net.ParseIP(netK2LAN).To4()
		n   int
	)

	for _, frame := range frames {
		typ, p := netEtherPayload(frame)
		if typ != netEtherTypeIPv4 || len(p) < netIPv4MinLen || p[netIPv4ProtoOffset] != netIPProtoICMP {
			continue
		}

		ihl := int(p[0]&netIPv4IHLMask) * netIPv4WordLen
		if len(p) <= ihl {
			continue
		}

		src := net.IP(p[netIPv4SrcOffset : netIPv4SrcOffset+netIPv4AddrLen])
		dst := net.IP(p[netIPv4DstOffset : netIPv4DstOffset+netIPv4AddrLen])
		pair := src.Equal(ipA) && dst.Equal(ipB) || src.Equal(ipB) && dst.Equal(ipA)

		if pair && (p[ihl] == netICMPEchoRequest || p[ihl] == netICMPEchoReply) {
			n++
		}
	}

	return n
}

// netCountARPRequests counts ARP requests asking for target.
func netCountARPRequests(frames [][]byte, target string) int {
	var (
		ip = net.ParseIP(target).To4()
		n  int
	)

	for _, frame := range frames {
		typ, p := netEtherPayload(frame)
		if typ != netEtherTypeARP || len(p) < netARPLen {
			continue
		}

		if binary.BigEndian.Uint16(p[netARPOpOffset:]) == netARPOpRequest &&
			net.IP(p[netARPTPAOffset:netARPTPAOffset+netIPv4AddrLen]).Equal(ip) {
			n++
		}
	}

	return n
}

// ---------------------------------------------------------------------------
// REST and the netflow websocket

// netWantStatus calls the REST API and checks the status and that the body
// contains text.
func netWantStatus(t *testing.T, method, path string, want int, text string) {
	t.Helper()

	if code, data := api(t, method, path, nil); code != want || !strings.Contains(string(data), text) {
		t.Errorf("%s %s = %d %q, want %d containing %q", method, path, code, data, want, text)
	}
}

// netOpenFlowWS connects to an experiment's netflow websocket and records the
// records phenix sends until the socket closes or the test ends.
func netOpenFlowWS(t *testing.T, exp string) *netFlowWS {
	t.Helper()

	addr := "ws://" + cfg.uiAddr + "/api/v1/experiments/" + exp + "/netflow/ws"

	conn, _, err := websocket.DefaultDialer.Dial(addr, nil)
	if err != nil {
		t.Fatalf("connecting to %s: %v", addr, err)
	}

	w := &netFlowWS{closed: make(chan struct{})}

	go func() {
		defer close(w.closed)

		for {
			// Reading also answers phenix's pings, which it needs every 10 s.
			_, data, err := conn.ReadMessage()
			if err != nil {
				w.mu.Lock()
				w.err = err
				w.mu.Unlock()

				return
			}

			var f netFlowFrame
			if json.Unmarshal(data, &f) == nil {
				w.mu.Lock()
				w.got = append(w.got, f)
				w.mu.Unlock()
			}
		}
	}()

	t.Cleanup(func() {
		_ = conn.Close()
		<-w.closed
	})

	return w
}

func (w *netFlowWS) frames() []netFlowFrame {
	w.mu.Lock()
	defer w.mu.Unlock()

	return slices.Clone(w.got)
}

// waitClosed checks that phenix closes the websocket within timeout with the
// normal close frame it sends when the netflow stops.
func (w *netFlowWS) waitClosed(t *testing.T, timeout time.Duration) {
	t.Helper()

	select {
	case <-w.closed:
	case <-time.After(timeout):
		t.Errorf("the netflow websocket was still open %v after the netflow stopped", timeout)

		return
	}

	w.mu.Lock()
	err := w.err
	w.mu.Unlock()

	var ce *websocket.CloseError
	if !errors.As(err, &ce) || ce.Code != websocket.CloseNormalClosure || ce.Text != netFlowStopped {
		t.Errorf("the netflow websocket ended with %v, want a normal close saying %q", err, netFlowStopped)
	}
}
