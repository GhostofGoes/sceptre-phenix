//go:build e2e

// End-to-end tests for the SCORCH runs that need no guest OS (the scorch-host
// shard). One experiment of three hollow VMs carries every run: the apps
// repository's pipe, qos, mm, pcap, hoststats, erspan, cc (reset and a
// filtered exec), disruption (baseline) and collector components, and the
// phenix scorch app's loops, counts, replacements, background components,
// run-level Filebeat, guards and `trigger running`. Each run is checked where
// it lands: minimega's tables, the tc qdiscs on the VM taps, Open vSwitch, the
// files and archive under the experiment's files directory, the status files
// the Python components write, and the pipelines phenix ui reports.

package e2e

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	scorchHostName   = "e2e-schost"
	scorchHostTypo   = "e2e-schtypo"
	scorchHostNoSuch = "e2e-schnosuch"

	scorchHostBridge = "phenix"

	// scorchHostPhenixBase is phenix's base directory; an experiment's files
	// directory, where SCORCH writes, is <base>/images/<exp>/files.
	scorchHostPhenixBase = "/phenix"

	// scorchHostBin is where the test installs its own SCORCH components and
	// the Filebeat stand-in, on the PATH of phenix and phenix ui.
	scorchHostBin = "/usr/local/bin"

	// scorchHostNetNSDir is where `ip netns add` creates named namespaces;
	// the SCORCH tap component puts its tap in a namespace of the tap's name.
	scorchHostNetNSDir  = "/var/run/netns"
	scorchHostTapSuffix = "-tapcomp"

	// The test's own components (phenix-scorch-component-<type>).
	scorchHostEcho  = "e2eschecho"
	scorchHostSleep = "e2eschsleep"
	scorchHostFail  = "e2eschfail"

	// scorchHostRunTimeout bounds one SCORCH run; the longest pauses 15 s.
	scorchHostRunTimeout = 5 * time.Minute

	// scorchHostSettle bounds how long a background component takes to act on
	// its SIGTERM and write its files.
	scorchHostSettle = 20 * time.Second

	// The ERSPAN receiver: a network namespace behind a veth pair.
	scorchHostRxNS   = "schersrx"
	scorchHostRxHost = "schers-h"
	scorchHostRxPeer = "schers-n"

	// The erspan component's port and mirror names, and a mirror the test
	// adds to show erspan leaves other mirrors alone.
	scorchHostERSPANPort   = "erspan1"
	scorchHostERSPANMirror = "m0"
	scorchHostSentinel     = "e2e-sentinel"
	scorchHostSentinelPort = "schmirout"

	scorchHostBackground = "background"
	scorchHostSuccess    = "success"

	// scorchHostPcapfSnapLen is pcapf's snaplen in the scenario. ARP frames,
	// all pcapf keeps, are shorter.
	scorchHostPcapfSnapLen = 100
)

// Run indexes of the scorch-host scenario.
const (
	scorchHostRunTap = iota
	scorchHostRunCore
	scorchHostRunBackground
	scorchHostRunPcapDup
	scorchHostRunInterference
	scorchHostRunERSPAN
	scorchHostRunLoops
	scorchHostRunCollector
	scorchHostRunFilebeat
	scorchHostRunFilter
	scorchHostRunIdle
	scorchHostRunOrphan
	scorchHostRunBackgroundFailure
	scorchHostRunERSPANFailure
	scorchHostRunForeground
	scorchHostRunCCFilter

	scorchHostRuns // the number of runs
)

// scorchHostSpareIface is h1's second interface, on VLAN SPARE.
const scorchHostSpareIface = `
      - name: eth1
        type: ethernet
        proto: static
        vlan: SPARE
        address: 10.11.0.1
        mask: 24`

// scorchHostScenario is the scorch-host scenario; {{fbdir}} is where the
// run-level Filebeat config sends its output. Runs are listed in the order of
// the scorchHostRun* indexes.
const scorchHostScenario = `apiVersion: phenix.sandia.gov/v2
kind: Scenario
metadata:
  name: {{name}}
  annotations:
    topology: {{name}}
spec:
  apps:
  - name: scorch
    metadata:
      components:
      - name: tapgen
        type: tap
        metadata: {bridge: phenix, vlan: LAN, ip: 10.10.0.210/24}
      - name: wait5
        type: pause
        metadata: {duration: 5s}
      - name: wait15
        type: pause
        metadata: {duration: 15s}
      - name: settle
        type: pause
        metadata: {duration: 3s}
      - name: ccreset
        type: cc
        metadata:
          configure: [{type: reset}]
      - name: pcap1
        type: pcap
        metadata:
          create_merged_pcap: true
          convertToJSON: true
          vms: [{hostname: h2, interface: 0}, {hostname: h3, interface: eth0}]
      - name: mmops
        type: mm
        metadata:
          start: [{type: start_capture, capture: {bridge: phenix, filename: bridge.pcap}}]
          stop: [{type: stop_capture, capture: {bridge: phenix}}]
          vms:
          - hostname: h1
            start:
            - {type: stop}
            - {type: disconnect, disconnect: {interface: 1}}
            - {type: start_capture, capture: {interface: 0, filename: h1-mm.pcap}}
            stop:
            - {type: stop_capture}
            - {type: connect, connect: {interface: 1, vlan: SPARE, bridge: phenix}}
            - {type: start}
      - name: qos1
        type: qos
        metadata:
          vms:
          - {hostname: h1, interface: 0, delay: 100ms, loss: 10}
          - {hostname: h2, interface: eth0, rate: "10 mbit"}
      - name: pipe1
        type: pipe
        metadata: {pipe: p1, mode: round-robin, log: true, data: hello}
      - name: hs1
        type: hoststats
        background: true
        metadata: {pollPeriod: 1}
      - name: hslow
        type: hoststats
        background: true
        metadata: {pollPeriod: 0.2, flushPeriod: 3600}
      - name: hs30
        type: hoststats
        background: true
        metadata: {pollPeriod: 30}
      - name: hsfg
        type: hoststats
        metadata: {pollPeriod: 1}
      - name: sleeper
        type: e2eschsleep
        background: true
        metadata: {role: graceful}
      - name: sleeper2
        type: e2eschsleep
        background: true
        metadata: {role: orphan}
      - name: bgfail
        type: e2eschfail
        background: true
        metadata: {role: failure}
      - name: pcapdup
        type: pcap
        metadata:
          vms: [{hostname: h1, interface: 0}, {hostname: h1, interface: 1}]
      - name: mmh2
        type: mm
        metadata:
          vms:
          - hostname: h2
            start: [{type: start_capture, capture: {interface: 0, filename: h2-mm.pcap}}]
            stop: [{type: stop_capture}]
      - name: pcaph1
        type: pcap
        metadata:
          vms: [{hostname: h1, interface: 0}]
      - name: pcapf
        type: pcap
        metadata:
          filter: arp
          snaplen: 100
          vms: [{hostname: h3, interface: 0}]
      - name: ers1
        type: erspan
        metadata:
          local_bridge: phenix
          local_ip: 192.0.2.1
          remote_ip: 192.0.2.2
          session_key: 100
          excluded_vlans: [SPARE]
      - name: ersbad
        type: erspan
        metadata:
          local_bridge: e2e-nobr
          local_ip: 192.0.2.1
          remote_ip: 192.0.2.2
      - name: ccfilt
        type: cc
        metadata:
          start: [{type: exec, filter: "hostname=h1", once: true, args: "true"}]
      - name: echo1
        type: e2eschecho
        metadata: {greeting: "@G@", delay: "@D@"}
      - name: echo2
        type: e2eschecho
        metadata: {greeting: inner}
      - name: dis0
        type: disruption
        metadata: {current_disruption: baseline, run_duration: 2, permutation: 0}
      - name: col0
        type: collector
        metadata: {collect_iperf: false, collect_miniccc: false, generate_csv: false}
      - name: hsfb
        type: hoststats
        background: true
        metadata:
          pollPeriod: 1
          filebeat.inputs:
          - type: filestream
            id: hs
            enabled: true
            paths: [host_stats.jsonl]
            parsers:
            - ndjson: {target: hoststats}
      - name: mmfilter
        type: mm
        metadata:
          start: [{type: start_capture, capture: {bridge: phenix, filename: f.pcap, filter: "udp or arp"}}]
          stop: [{type: stop_capture, capture: {bridge: phenix}}]
          vms:
          - hostname: h2
            start: [{type: start_capture, capture: {interface: 0, filename: h2-f.pcap, filter: "udp or arp"}}]
            stop: [{type: stop_capture}]
      runs:
      - name: tap-probe
        start: [tapgen, wait5]
        stop: [tapgen]
      - name: host-core
        configure: [ccreset]
        start: [pcap1, mmops, qos1, pipe1, hs1, wait15]
        stop: [hs1, qos1, mmops, pcap1]
        cleanup: [pipe1]
      - name: background
        start: [sleeper, hslow, hs30, wait5]
        stop: [hslow, hs30, sleeper]
      - name: pcap-same-vm
        start: [pcapdup, wait5]
        stop: [pcapdup]
      - name: capture-interference
        start: [mmh2, pcaph1, wait5]
        stop: [pcaph1, mmh2]
      - name: erspan
        configure: [ers1]
        start: [tapgen, wait15]
        stop: [tapgen]
        cleanup: [ers1]
      - name: loops
        count: 2
        replace:
          "@G@": [hello]
          "@D@": {type: int, uniform: {minimum: 3, maximum: 3.4}}
        start: [echo1]
        stop: [echo1]
        loop:
          count: 3
          start: [echo2]
      - name: collector
        start: [dis0]
        stop: [col0]
      - name: filebeat
        filebeat:
          enabled: true
          config:
            output.file: {path: "{{fbdir}}", filename: scorch}
        start: [hsfb, wait5]
        stop: [hsfb, settle]
      - name: bpf-filter
        start: [tapgen, mmfilter, pcapf, wait15]
        stop: [pcapf, mmfilter, tapgen]
      - name: idle
        start: [wait15]
      - name: orphan
        start: [sleeper2, settle]
      - name: background-failure
        start: [bgfail, wait5]
        stop: [bgfail]
      - name: erspan-local-failure
        configure: [ersbad]
        cleanup: [ersbad]
      - name: hoststats-foreground
        start: [hsfg]
      - name: cc-filter
        start: [ccfilt]
`

// scorchHostTypoScenario names a component its scorch app never defines.
const scorchHostTypoScenario = `apiVersion: phenix.sandia.gov/v2
kind: Scenario
metadata:
  name: {{name}}
  annotations:
    topology: {{name}}
spec:
  apps:
  - name: scorch
    metadata:
      components:
      - name: wait1
        type: pause
        metadata: {duration: 1s}
      runs:
      - name: typo
        start: [wait1, nosuchcmp]
`

// scorchHostEchoScript records every call it gets, in call order, and the
// experiment it got on stdin.
const scorchHostEchoScript = `#!/bin/bash
# phenix-scorch-component-e2eschecho: records each call of a SCORCH run.
set -eu
d="$PHENIX_FILES_DIR/scorch/run-$3/e2eschecho"
mkdir -p "$d"
cat > "$d/$1-$2-$3-$4-$5.stdin.json"
echo "$*" >> "$d/calls.log"
echo "e2eschecho $1 $2 ok"
`

// scorchHostSleepScript is a start stage that runs until SIGTERM and takes
// 6 s to act on it (within phenix's 10 s kill delay), so a run archived
// before it exits misses its last line, and never while it writes it.
const scorchHostSleepScript = `#!/bin/bash
# phenix-scorch-component-e2eschsleep: a start stage that runs until SIGTERM.
d="$PHENIX_FILES_DIR/scorch/run-$3/e2eschsleep"
k="$1-$2-$3-$4-$5"
mkdir -p "$d"
cat > "$d/$k.stdin.json"
echo "begin $(date +%s.%N)" >> "$d/$k.state"
if [ "$1" = start ]; then
	trap 'sleep 6; echo "sigterm $(date +%s.%N)" >> "$d/$k.state"; exit 0' TERM
	while :; do sleep 0.2; done
fi
echo "end" >> "$d/$k.state"
`

// scorchHostFailScript fails its start stage at once, quietly, after leaving
// a marker that it ran.
const scorchHostFailScript = `#!/bin/bash
# phenix-scorch-component-e2eschfail: fails its start stage.
cat > /dev/null
if [ "$1" = start ]; then
	d="$PHENIX_FILES_DIR/scorch/run-$3/e2eschfail"
	mkdir -p "$d"
	touch "$d/$1-$2.failed"
	exit 3
fi
`

// scorchHostEnv is what every scorch-host case needs to know about the
// shared experiment.
type scorchHostEnv struct {
	exp   string
	vlans map[string]int // status.vlans
	head  string         // minimega's host name
}

// TestScorchHost runs every scorch-host case on one experiment of three
// hollow VMs, one SCORCH run per case (SCH-01..SCH-14). The trigger case runs
// last, so a tap it leaks cannot hold the address the other tap runs use.
func TestScorchHost(t *testing.T) {
	scorchHostRequire(t)
	hollowImages(t)
	scorchHostInstall(t, scorchHostEcho, scorchHostEchoScript)
	scorchHostInstall(t, scorchHostSleep, scorchHostSleepScript)
	scorchHostInstall(t, scorchHostFail, scorchHostFailScript)
	scorchHostCleanTaps(t)
	scorchHostKeepFiles(t, scorchHostName)

	t.Cleanup(func() {
		_, _ = hostCmd("pkill", "-KILL", "-f", "phenix-scorch-component-"+scorchHostSleep+" ")
	})

	fbdir := filepath.Join(cfg.workDir, "scorch-host-filebeat")
	scenario := strings.ReplaceAll(scorchHostScenario, "{{fbdir}}", fbdir)

	exp := newExperiment(t, scorchHostName, scorchHostTopology(), scenario)
	exp.start()
	exp.waitRunning(2*time.Minute, "h1", "h2", "h3")

	env := scorchHostEnv{exp: exp.name, vlans: exp.doc().Status.VLANs, head: headnode(t)}

	cases := []struct {
		name  string
		check func(*testing.T, scorchHostEnv)
	}{
		{"SCH-01..05_cc_reset_captures_vm_ops_qos_pipe_hoststats", scorchHostCheckCore},
		{"cc_exec_filter", scorchHostCheckCCFilter},
		{"SCH-07a_pcap_two_interfaces_of_one_vm", scorchHostCheckPcapDup},
		{"SCH-07b_pcap_leaves_other_captures_alone", scorchHostCheckInterference},
		{"SCH-08_capture_filters", scorchHostCheckFilter},
		{"SCH-09_erspan", scorchHostCheckERSPAN},
		{"SCH-09b_erspan_local_failure", scorchHostCheckERSPANFailure},
		{"SCH-10_loops_counts_replacements", scorchHostCheckLoops},
		{"SCH-11_collector", scorchHostCheckCollector},
		{"SCH-12_run_level_filebeat", scorchHostCheckFilebeat},
		{"SCH-05b_hoststats_refuses_foreground", scorchHostCheckForeground},
		{"SCH-06_background_sigterm", scorchHostCheckGraceful},
		{"background_never_stopped", scorchHostCheckOrphan},
		{"background_failure", scorchHostCheckBackgroundFailure},
		{"SCH-14_run_guards", scorchHostCheckGuards},
		{"SCH-13_trigger_running", scorchHostCheckTrigger},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Cleanup(func() { scorchHostRestore(t, env.exp) })
			c.check(t, env)
		})
	}
}

// TestScorchHostGuards checks how phenix refuses SCORCH runs it cannot run
// (SCH-14): `phenix experiment scorch` of an experiment that does not exist,
// and the create of an experiment whose run names a component the scenario
// never defines.
func TestScorchHostGuards(t *testing.T) {
	hollowImages(t)

	t.Run("missing_experiment", func(t *testing.T) {
		out := phenixFails(t, "experiment", "scorch", scorchHostNoSuch)

		wantContains(t, "phenix experiment scorch "+scorchHostNoSuch, out,
			fmt.Sprintf("Unable to start Scorch run 0 for %s experiment", scorchHostNoSuch))
		wantLacks(t, "phenix experiment scorch "+scorchHostNoSuch, out, "panic", "invalid memory address")
	})

	t.Run("undefined_component", scorchHostCheckTypo)
}

// ---------------------------------------------------------------------------
// SCH-01..SCH-05: run 1 over REST

// scorchHostCoreSnap is what minimega and the host report during run 1's
// pause.
type scorchHostCoreSnap struct {
	vms      map[string]map[string]string
	captures []map[string]string
	pipes    []map[string]string
	qdiscs   map[string]string // VM -> tc qdisc of its first tap
	filter   string
	hs1      string
}

// scorchHostCheckCore runs run 1 through phenix ui. Its configure stage
// resets cc (SCH-01); its start stage pauses h1, unplugs h1's second NIC and
// captures h1:0 and the bridge with mm, captures h2:0 and h3:0 with pcap
// (SCH-02), shapes h1 and h2 with qos (SCH-03), writes to a pipe (SCH-04) and
// backgrounds hoststats (SCH-05) before a 15 s pause; stop and cleanup undo
// it all.
func scorchHostCheckCore(t *testing.T, env scorchHostEnv) {
	t.Helper()

	name := env.exp

	// Something for the cc component's reset to clear. No client ever
	// connects to a hollow VM, so the command stays queued.
	mm(t, name, "cc exec true")
	mm(t, name, "cc filter hostname=e2e-nosuch")

	if rows := mmTable(t, name, "cc commands"); len(rows) == 0 {
		t.Fatal("cc exec queued no command to reset")
	}

	// No guest ever answers it either, so leave a response to clear.
	seed := scorchHostResponseSeed(name)
	if err := os.MkdirAll(filepath.Dir(seed), 0o750); err != nil {
		t.Fatalf("seeding a cc response for the reset to clear: %v", err)
	}

	if err := os.WriteFile(seed, []byte("e2e\n"), 0o600); err != nil {
		t.Fatalf("seeding a cc response for the reset to clear: %v", err)
	}

	t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(filepath.Dir(seed))) })

	scorchHostPost(t, name, scorchHostRunCore)

	snap := scorchHostWaitCorePause(t, name)
	scorchHostCheckCorePause(t, env, snap)

	// phenix ui lists every run's pipeline and the one running.
	names, running := scorchHostPipelines(t, name)
	if len(names) != scorchHostRuns || names[scorchHostRunCore] != "host-core" || running != scorchHostRunCore {
		t.Errorf("during run 1, GET /scorch/pipelines lists runs %v with run %d running; want %d runs, run 1 host-core running",
			names, running, scorchHostRuns)
	}

	nodes := scorchHostWaitREST(t, name, scorchHostRunCore)

	if st := scorchHostStartStatus(nodes, "hs1"); st != scorchHostSuccess {
		t.Errorf("after run 1, pipeline node hs1 (start) is %q, want %s once its stop stage ended it", st, scorchHostSuccess)
	}

	if _, running := scorchHostPipelines(t, name); running != -1 {
		t.Errorf("after run 1, GET /scorch/pipelines reports run %d running, want -1", running)
	}

	scorchHostCheckCoreAfter(t, env, snap)
	scorchHostCheckCoreFiles(t, env)
}

func scorchHostWaitCorePause(t *testing.T, name string) scorchHostCoreSnap {
	t.Helper()

	var snap scorchHostCoreSnap

	ok, detail := scorchHostPoll(2*time.Minute, 500*time.Millisecond, func() (bool, string) {
		// hs1 starts last, so once it is backgrounded every component before
		// it is done: read it first, then the state they set.
		snap.hs1 = scorchHostStartStatus(scorchHostPipeline(t, name, scorchHostRunCore), "hs1")
		snap.pipes = scorchHostPipes(t, name)
		snap.captures = mmTable(t, name, "capture")
		snap.vms = vmInfo(t, name)

		h1, h2 := snap.vms["h1"], snap.vms["h2"]
		ready := h1["state"] == "PAUSED" && len(snap.captures) == 4 && h1["qos"] != "[]" && h2["qos"] != "[]" &&
			len(snap.pipes) == 1 && snap.hs1 == scorchHostBackground

		return ready, fmt.Sprintf("h1 %s, captures %v, qos h1 %s h2 %s, pipes %v, hs1 %q",
			h1["state"], snap.captures, h1["qos"], h2["qos"], snap.pipes, snap.hs1)
	})
	if !ok {
		t.Fatalf("run 1 never reached its pause with every component started: %s\nphenix ui log:\n%s",
			detail, scorchHostUILog(name))
	}

	snap.qdiscs = make(map[string]string)

	for _, vm := range []string{"h1", "h2"} {
		snap.qdiscs[vm] = scorchHostQdisc(t, scorchHostFirst(snap.vms[vm]["tap"]))
	}

	snap.filter = strings.TrimSpace(mmText(t, name, "capture pcap filter"))

	return snap
}

func scorchHostCheckCorePause(t *testing.T, env scorchHostEnv, snap scorchHostCoreSnap) {
	t.Helper()

	cmpDir := func(cmp, file string) string {
		return filepath.Join(scorchHostCmpDir(env.exp, scorchHostRunCore, cmp), file)
	}

	// SCH-02: mm paused h1, unplugged its second NIC and captures h1:0 and the
	// bridge; pcap captures h2:0 and h3:0 (h3's interface given by name).
	if vlans := scorchHostList(snap.vms["h1"]["vlan"]); len(vlans) != 2 || vlans[1] != "disconnected" {
		t.Errorf("during run 1, h1's VLANs are %v, want its second interface disconnected", vlans)
	}

	want := map[string]string{
		"h1:0": cmpDir("mmops", "h1-mm.pcap"),
		"h2:0": cmpDir("pcap1", "h2-0.pcap"),
		"h3:0": cmpDir("pcap1", "h3-0.pcap"),
		"":     cmpDir("mmops", "bridge.pcap"),
	}

	for iface, path := range want {
		if !slices.ContainsFunc(snap.captures, func(row map[string]string) bool {
			return row["interface"] == iface && row["path"] == path && row["type"] == "pcap" && row["bridge"] == scorchHostBridge
		}) {
			t.Errorf("during run 1, minimega's captures lack a pcap of %q to %s: %v", iface, path, snap.captures)
		}
	}

	// Neither component has a filter, so each sets the blank one; whether
	// they clear a filter of their own is seen in SCH-08 (pcap) and SCG-02
	// (mm), where that component sets the filter last.
	if snap.filter != "" {
		t.Errorf("during run 1, minimega's capture pcap filter is %q, want it cleared", snap.filter)
	}

	// SCH-03: netem on h1's tap, tbf on h2's, as minimega's vm info reports.
	wantContains(t, "tc qdisc of h1's tap during run 1", snap.qdiscs["h1"], "netem", "delay 100ms", "loss 10%")
	wantContains(t, "tc qdisc of h2's tap during run 1", snap.qdiscs["h2"], "tbf", "rate 10Mbit")
	wantContains(t, "h1's vm info qos during run 1", snap.vms["h1"]["qos"], "0: ", "delay 100ms", "loss 10")
	wantContains(t, "h2's vm info qos during run 1", snap.vms["h2"]["qos"], "0: ", "rate 10mbit")

	// SCH-04: the pipe has the mode the component set and the last value it
	// wrote; with no readers minimega still records it.
	if p := snap.pipes[0]; p["name"] != env.exp+"//p1" || p["mode"] != "round-robin" || p["previous"] != "hello" {
		t.Errorf("during run 1, minimega's pipe is %v, want %s//p1, round-robin, previous hello", p, env.exp)
	}
}

func scorchHostCheckCoreAfter(t *testing.T, env scorchHostEnv, snap scorchHostCoreSnap) {
	t.Helper()

	name := env.exp
	info := vmInfo(t, name)
	h1 := info["h1"]

	if h1["state"] != "RUNNING" {
		t.Errorf("after run 1, h1 is %s, want RUNNING again", h1["state"])
	}

	vlans, bridges := scorchHostList(h1["vlan"]), scorchHostList(h1["bridge"])
	if len(vlans) != 2 || !strings.Contains(vlans[1], "SPARE") || len(bridges) != 2 || bridges[1] != scorchHostBridge {
		t.Errorf("after run 1, h1's VLANs are %v on bridges %v, want its second interface back on SPARE on %s",
			vlans, bridges, scorchHostBridge)
	}

	if rows := mmTable(t, name, "capture"); len(rows) != 0 {
		t.Errorf("after run 1, minimega still captures: %v", rows)
	}

	for _, vm := range []string{"h1", "h2"} {
		if q := info[vm]["qos"]; q != "[]" {
			t.Errorf("after run 1, %s's vm info qos is %s, want none", vm, q)
		}

		tap := scorchHostFirst(snap.vms[vm]["tap"])
		wantLacks(t, fmt.Sprintf("tc qdisc of %s's tap after run 1", vm), scorchHostQdisc(t, tap), "netem", "tbf")
	}

	if pipes := scorchHostPipes(t, name); len(pipes) != 0 {
		t.Errorf("after run 1's cleanup, minimega still has pipes %v", pipes)
	}

	// SCH-01: the reset cleared the queued command, the filter and the
	// responses directory.
	if rows := mmTable(t, name, "cc commands"); len(rows) != 0 {
		t.Errorf("after run 1's cc reset, cc commands still lists %v", rows)
	}

	if rows := mmTable(t, name, "cc filter"); len(rows) != 0 {
		t.Errorf("after run 1's cc reset, the cc filter is still %v", rows)
	}

	responses := filepath.Join(cfg.imagesDir, name, "miniccc_responses")
	if entries, err := os.ReadDir(responses); err == nil && len(entries) != 0 {
		t.Errorf("after run 1's cc reset, %s still holds %d entries", responses, len(entries))
	}

	if _, err := os.Stat(scorchHostResponseSeed(name)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("after run 1's cc reset, the seeded cc response %s is still there (stat: %v)", scorchHostResponseSeed(name), err)
	}
}

// scorchHostResponseSeed is a cc response file SCH-01 leaves for the cc
// component's reset to clear, laid out as minimega keeps responses: by
// command ID, then client UUID.
func scorchHostResponseSeed(exp string) string {
	return filepath.Join(cfg.imagesDir, exp, "miniccc_responses", "999", "e2e-seed", "stdout")
}

func scorchHostCheckCoreFiles(t *testing.T, env scorchHostEnv) {
	t.Helper()

	name, run := env.exp, scorchHostRunCore

	if st := scorchHostWantStage(t, name, run, "ccreset", "configure"); st.Return != "" && st.Return != nil {
		t.Errorf("ccreset configure returned %v, want nothing", st.Return)
	}

	for _, s := range [][2]string{
		{"pcap1", "start"}, {"pcap1", "stop"}, {"mmops", "start"}, {"mmops", "stop"},
		{"qos1", "start"}, {"qos1", "stop"}, {"pipe1", "start"}, {"pipe1", "cleanup"}, {"hs1", "stop"},
	} {
		scorchHostWantStage(t, name, run, s[0], s[1])
	}

	pcapDir := scorchHostCmpDir(name, run, "pcap1")

	for _, f := range []string{"raw/h2-0.pcap", "raw/h3-0.pcap", "merged.pcap"} {
		scorchHostWantPcap(t, filepath.Join(pcapDir, f))
	}

	for _, f := range []string{"raw/h2-0.jsonl", "raw/h3-0.jsonl", "merged.jsonl"} {
		if _, err := os.Stat(filepath.Join(pcapDir, f)); err != nil {
			t.Errorf("pcap1's JSON conversion: %v", err)
		}
	}

	scorchHostWantPcapMeta(t, filepath.Join(pcapDir, "pcap_metadata.json"), "h2-0.pcap", "h3-0.pcap", "merged.pcap")

	mmDir := scorchHostCmpDir(name, run, "mmops")
	scorchHostWantPcap(t, filepath.Join(mmDir, "h1-mm.pcap"))
	scorchHostWantPcap(t, filepath.Join(mmDir, "bridge.pcap"))

	var applied map[string]map[string]any

	scorchHostReadJSON(t, filepath.Join(scorchHostCmpDir(name, run, "qos1"), "qos_values_applied.json"), &applied)

	wantQoS := map[string]map[string]any{
		"h1": {"interface": 0.0, "loss": 10.0, "delay": "100ms"},
		"h2": {"interface": 0.0, "rate": "10 mbit"},
	}
	if !reflect.DeepEqual(applied, wantQoS) {
		t.Errorf("qos1's qos_values_applied.json is %v, want %v", applied, wantQoS)
	}

	// SCH-05: hoststats flushed a line per poll for the whole pause, and its
	// start stage, ended by SIGTERM at stop, wrote its status file.
	hsDir := scorchHostCmpDir(name, run, "hs1")
	scorchHostWaitStage(t, hsDir, "start", "Stopped user component: hs1")
	scorchHostCheckHostStats(t, filepath.Join(hsDir, "host_stats.jsonl"), env.head, 10)

	scorchHostCheckRun(t, name, run, "host-core",
		"pcap1/loop-0-count-0/merged.pcap",
		"pcap1/loop-0-count-0/pcap_metadata.json",
		"mmops/loop-0-count-0/h1-mm.pcap",
		"mmops/loop-0-count-0/bridge.pcap",
		"qos1/loop-0-count-0/qos_values_applied.json",
		"hs1/loop-0-count-0/host_stats.jsonl",
	)
}

// scorchHostCheckCCFilter runs a cc component whose top-level exec targets h1
// by a filter: the command is queued with that filter, and the filter
// should not stay set in the namespace, where every later cc command would
// inherit it. No client ever connects to a hollow VM, so nothing runs.
func scorchHostCheckCCFilter(t *testing.T, env scorchHostEnv) {
	t.Helper()

	name, run := env.exp, scorchHostRunCCFilter

	t.Cleanup(func() { _, _ = mmRun(name, "clear cc commands") })

	scorchHostRunOK(t, name, run)
	scorchHostWantStage(t, name, run, "ccfilt", "start")

	rows := mmTable(t, name, "cc commands")
	if !slices.ContainsFunc(rows, func(row map[string]string) bool {
		return row["command"] == "[true]" && row["filter"] == "hostname=h1" && row["once"] == "true"
	}) {
		t.Errorf("after run %d, cc commands lacks the component's exec-once of true for hostname=h1: %v", run, rows)
	}

	if rows := mmTable(t, name, "cc filter"); len(rows) != 0 {
		t.Errorf("after run %d, the namespace's cc filter is still %v, which every later cc command inherits", run, rows)
	}

	scorchHostCheckRun(t, name, run, "cc-filter")
}

// ---------------------------------------------------------------------------
// SCH-07, SCH-08: captures

// scorchHostCheckPcapDup captures both interfaces of one VM with one pcap
// component (SCH-07a): stop deletes each capture by interface.
func scorchHostCheckPcapDup(t *testing.T, env scorchHostEnv) {
	t.Helper()

	name, run := env.exp, scorchHostRunPcapDup

	scorchHostRunOK(t, name, run)

	scorchHostWantStage(t, name, run, "pcapdup", "start")
	scorchHostWantStage(t, name, run, "pcapdup", "stop")

	dir := scorchHostCmpDir(name, run, "pcapdup")
	scorchHostWantPcap(t, filepath.Join(dir, "h1-0.pcap"))
	scorchHostWantPcap(t, filepath.Join(dir, "h1-1.pcap"))
	scorchHostWantPcapMeta(t, filepath.Join(dir, "pcap_metadata.json"), "h1-0.pcap", "h1-1.pcap")

	if rows := mmTable(t, name, "capture"); len(rows) != 0 {
		t.Errorf("after run %d, minimega still captures: %v", run, rows)
	}

	scorchHostCheckRun(t, name, run, "pcap-same-vm",
		"pcapdup/loop-0-count-0/h1-0.pcap", "pcapdup/loop-0-count-0/h1-1.pcap", "pcapdup/loop-0-count-0/pcap_metadata.json")
}

// scorchHostCheckInterference starts a pcap capture while an mm capture of
// another VM runs (SCH-07b): pcap clears only its own captures, so mm's
// capture survives and stops cleanly.
func scorchHostCheckInterference(t *testing.T, env scorchHostEnv) {
	t.Helper()

	name, run := env.exp, scorchHostRunInterference

	proc := scorchHostStart(t, "experiment", "scorch", name, "--run", strconv.Itoa(run))

	var rows []map[string]string

	eventually(t, time.Minute, 250*time.Millisecond, "pcaph1's capture of h1:0", func() (bool, string) {
		rows = mmTable(t, name, "capture")

		return scorchHostHasCapture(rows, "h1:0") || proc.finished(), fmt.Sprintf("captures %v", rows)
	})

	if !scorchHostHasCapture(rows, "h1:0") {
		out, err := proc.wait(t, time.Minute)
		t.Fatalf("run %d finished before pcaph1 captured h1:0 (%v):\n%s", run, err, out)
	}

	if !scorchHostHasCapture(rows, "h2:0") {
		t.Errorf("once pcaph1 started, mmh2's capture of h2:0 is gone: %v", rows)
	}

	if out, err := proc.wait(t, scorchHostRunTimeout); err != nil {
		t.Fatalf("phenix experiment scorch -r %d: %v\n%s", run, err, out)
	}

	for _, s := range [][2]string{{"mmh2", "start"}, {"mmh2", "stop"}, {"pcaph1", "start"}, {"pcaph1", "stop"}} {
		scorchHostWantStage(t, name, run, s[0], s[1])
	}

	scorchHostWantPcap(t, filepath.Join(scorchHostCmpDir(name, run, "mmh2"), "h2-mm.pcap"))
	scorchHostWantPcap(t, filepath.Join(scorchHostCmpDir(name, run, "pcaph1"), "h1-0.pcap"))

	if rows := mmTable(t, name, "capture"); len(rows) != 0 {
		t.Errorf("after run %d, minimega still captures: %v", run, rows)
	}

	scorchHostCheckRun(t, name, run, "capture-interference",
		"mmh2/loop-0-count-0/h2-mm.pcap", "pcaph1/loop-0-count-0/h1-0.pcap")
}

// scorchHostCheckFilter starts mm captures of the bridge and of h2:0 with
// the two-word BPF filter "udp or arp" (SCH-08), then a pcap
// capture of h3:0 with its own filter "arp" and snaplen, sends
// UDP, ICMP and ARP from a SCORCH tap, and checks that each capture kept its
// own filter, that pcapf's capture used its snaplen, and that the namespace's
// filter and snaplen were cleared afterwards.
func scorchHostCheckFilter(t *testing.T, env scorchHostEnv) {
	t.Helper()

	name, run := env.exp, scorchHostRunFilter

	before := scorchHostTaps()
	proc := scorchHostStart(t, "experiment", "scorch", name, "--run", strconv.Itoa(run))
	tap := scorchHostWaitTap(t, proc, before)

	var rows []map[string]string

	eventually(t, time.Minute, 250*time.Millisecond, "mmfilter's and pcapf's captures", func() (bool, string) {
		rows = mmTable(t, name, "capture")

		return len(rows) == 3 || proc.finished(), fmt.Sprintf("captures %v", rows)
	})

	if proc.finished() {
		out, err := proc.wait(t, time.Minute)
		t.Fatalf("run %d finished before mmfilter's and pcapf's captures ran (%v):\n%s", run, err, out)
	}

	pcapDir := scorchHostCmpDir(name, run, "pcapf")
	h3Pcap := filepath.Join(pcapDir, "h3-0.pcap")

	if !slices.ContainsFunc(rows, func(row map[string]string) bool {
		return row["interface"] == "h3:0" && row["path"] == h3Pcap
	}) {
		t.Errorf("during run %d, minimega's captures lack pcapf's capture of h3:0 to %s: %v", run, h3Pcap, rows)
	}

	// pcapf sets the namespace's filter after mmfilter, so this shows pcap
	// clearing its own once its capture started (SCG-02 shows mm's), which
	// it does just after the capture appears, so allow it a moment.
	if ok, f := scorchHostPoll(3*time.Second, 250*time.Millisecond, func() (bool, string) {
		got := strings.TrimSpace(mmText(t, name, "capture pcap filter"))

		return got == "", got
	}); !ok {
		t.Errorf("while mmfilter's and pcapf's captures run, minimega's capture pcap filter is %q, want it cleared", f)
	}

	// pcapf also sets the namespace's snaplen, and resets it to minimega's
	// default 0 with its filter.
	if ok, s := scorchHostPoll(3*time.Second, 250*time.Millisecond, func() (bool, string) {
		got := strings.TrimSpace(mmText(t, name, "capture pcap snaplen"))

		return got == "0", got
	}); !ok {
		t.Errorf("while mmfilter's and pcapf's captures run, minimega's capture pcap snaplen is %q, want 0 (the default)", s)
	}

	scorchHostTraffic(t, tap, scorchHostFirst(vmInfo(t, name)["h2"]["mac"]))

	if out, err := proc.wait(t, scorchHostRunTimeout); err != nil {
		t.Fatalf("phenix experiment scorch -r %d: %v\n%s", run, err, out)
	}

	scorchHostWantStage(t, name, run, "mmfilter", "start")
	scorchHostWantStage(t, name, run, "mmfilter", "stop")

	dir := scorchHostCmpDir(name, run, "mmfilter")
	vmPcap, bridgePcap := filepath.Join(dir, "h2-f.pcap"), filepath.Join(dir, "f.pcap")

	scorchHostWantPcap(t, vmPcap)
	scorchHostWantPcap(t, bridgePcap)

	udp, arp, icmp := scorchHostPackets(t, vmPcap, "udp"), scorchHostPackets(t, vmPcap, "arp"), scorchHostPackets(t, vmPcap, "icmp")
	if udp < 5 || arp < 1 || icmp != 0 {
		t.Errorf("h2's capture filtered with \"udp or arp\" holds %d UDP, %d ARP and %d ICMP packets, want >= 5, >= 1 and 0",
			udp, arp, icmp)
	}

	// The bridge capture sees the same traffic through a mirror; whether a
	// VLAN tag hides it from the filter or not, no ICMP may pass.
	if n := scorchHostPackets(t, bridgePcap, "icmp"); n != 0 {
		t.Errorf("the bridge capture filtered with \"udp or arp\" holds %d ICMP packets, want 0", n)
	}

	t.Logf("bridge capture: %d UDP, %d ARP packets", scorchHostPackets(t, bridgePcap, "udp"), scorchHostPackets(t, bridgePcap, "arp"))

	// pcapf captured h3:0 with its own filter, not mmfilter's: the broadcast
	// ARP reaches h3's tap and passes "arp"; no UDP or ICMP does.
	scorchHostWantStage(t, name, run, "pcapf", "start")
	scorchHostWantStage(t, name, run, "pcapf", "stop")
	scorchHostWantPcap(t, h3Pcap)
	scorchHostWantPcapMeta(t, filepath.Join(pcapDir, "pcap_metadata.json"), "h3-0.pcap")

	if n := scorchHostSnapLen(t, h3Pcap); n != scorchHostPcapfSnapLen {
		t.Errorf("%s has snaplen %d in its header, want pcapf's %d", h3Pcap, n, scorchHostPcapfSnapLen)
	}

	udp, arp, icmp = scorchHostPackets(t, h3Pcap, "udp"), scorchHostPackets(t, h3Pcap, "arp"), scorchHostPackets(t, h3Pcap, "icmp")
	if udp != 0 || arp < 1 || icmp != 0 {
		t.Errorf("h3's capture filtered with \"arp\" holds %d UDP, %d ARP and %d ICMP packets, want 0, >= 1 and 0", udp, arp, icmp)
	}

	if f := strings.TrimSpace(mmText(t, name, "capture pcap filter")); f != "" {
		t.Errorf("after run %d, minimega's capture pcap filter is %q, want it cleared", run, f)
	}

	if s := strings.TrimSpace(mmText(t, name, "capture pcap snaplen")); s != "0" {
		t.Errorf("after run %d, minimega's capture pcap snaplen is %q, want 0 (the default)", run, s)
	}

	if rows := mmTable(t, name, "capture"); len(rows) != 0 {
		t.Errorf("after run %d, minimega still captures: %v", run, rows)
	}

	if left := scorchHostTapsLeft(before, 15*time.Second); len(left) != 0 {
		t.Errorf("after run %d, its tap is still there: %v", run, left)
	}

	scorchHostCheckRun(t, name, run, "bpf-filter", "mmfilter/loop-0-count-0/f.pcap", "mmfilter/loop-0-count-0/h2-f.pcap",
		"pcapf/loop-0-count-0/h3-0.pcap")
}

// scorchHostTraffic sends, from a SCORCH tap's namespace, six UDP datagrams
// and three ICMP echo requests to h2 (whose MAC is pinned, since a hollow VM
// answers no ARP), and ARP requests for an address nobody has.
func scorchHostTraffic(t *testing.T, tap, mac string) {
	t.Helper()

	inTap := func(args ...string) (string, error) {
		return hostCmd("ip", append([]string{"netns", "exec", tap}, args...)...)
	}

	if out, err := inTap("ip", "neigh", "replace", "10.10.0.2", "lladdr", mac, "dev", tap, "nud", "permanent"); err != nil {
		t.Fatalf("pinning h2's MAC %s in tap %s: %v: %s", mac, tap, err, out)
	}

	if out, err := inTap("bash", "-c", "for i in 1 2 3 4 5 6; do echo e2e > /dev/udp/10.10.0.2/9999; done"); err != nil {
		t.Fatalf("sending UDP from tap %s: %v: %s", tap, err, out)
	}

	// Neither gets an answer.
	_, _ = inTap("ping", "-c", "3", "-i", "0.2", "-W", "1", "10.10.0.2")
	_, _ = inTap("ping", "-c", "2", "-W", "1", "10.10.0.99")
}

// ---------------------------------------------------------------------------
// SCH-09: erspan

// scorchHostCheckERSPAN mirrors LAN to an ERSPAN tunnel whose far end is a
// network namespace on the host (SCH-09): erspan adds its own
// port and mirror, leaves other mirrors alone, and removes only its own.
func scorchHostCheckERSPAN(t *testing.T, env scorchHostEnv) {
	t.Helper()

	name, run := env.exp, scorchHostRunERSPAN

	scorchHostReceiver(t)
	scorchHostSentinelMirror(t)

	t.Cleanup(func() {
		_, _ = hostCmd("ovs-vsctl", "--", "--id=@m", "get", "Mirror", scorchHostERSPANMirror,
			"--", "remove", "Bridge", scorchHostBridge, "mirrors", "@m")
		_, _ = hostCmd("ovs-vsctl", "--if-exists", "del-port", scorchHostBridge, scorchHostERSPANPort)
	})

	before := scorchHostTaps()
	proc := scorchHostStart(t, "experiment", "scorch", name, "--run", strconv.Itoa(run))
	tap := scorchHostWaitTap(t, proc, before)

	// Configure ran before start created the tap.
	scorchHostCheckERSPANPort(t, env)

	if n := scorchHostERSPANTraffic(t, tap); n < 3 {
		t.Errorf("the ERSPAN receiver saw %d GRE packets while the tap sent ARP requests on LAN, want >= 3", n)
	}

	if out, err := proc.wait(t, scorchHostRunTimeout); err != nil {
		t.Fatalf("phenix experiment scorch -r %d: %v\n%s", run, err, out)
	}

	mirrors := scorchHostMirrors(t)
	if _, ok := mirrors[scorchHostERSPANMirror]; ok {
		t.Errorf("after erspan's cleanup, its mirror %s is still on the bridge: %v", scorchHostERSPANMirror, mirrors)
	}

	if _, ok := mirrors[scorchHostSentinel]; !ok {
		t.Errorf("erspan's cleanup removed mirror %s it did not create: %v", scorchHostSentinel, mirrors)
	}

	if ports := scorchHostOVSPorts(t); slices.Contains(ports, scorchHostERSPANPort) {
		t.Errorf("after erspan's cleanup, port %s is still on bridge %s", scorchHostERSPANPort, scorchHostBridge)
	}

	scorchHostWantStage(t, name, run, "ers1", "configure")
	scorchHostWantStage(t, name, run, "ers1", "cleanup")
	scorchHostCheckERSPANLog(t, filepath.Join(scorchHostCmpDir(name, run, "ers1"), "erspan_metadata_configure.json"))

	scorchHostCheckRun(t, name, run, "erspan",
		"ers1/loop-0-count-0/erspan_metadata_configure.json", "ers1/loop-0-count-0/erspan_metadata_cleanup.json")
}

// scorchHostCheckERSPANFailure configures erspan on a bridge that does not
// exist (SCH-09b): the local ovs-vsctl fails, so the run fails
// naming the component, and nothing is left on Open vSwitch.
func scorchHostCheckERSPANFailure(t *testing.T, env scorchHostEnv) {
	t.Helper()

	name, run := env.exp, scorchHostRunERSPANFailure

	out, err := scorchHostRun(name, run)
	if err == nil {
		t.Errorf("phenix experiment scorch -r %d configured erspan on a missing bridge without failing\n%s", run, out)
	}

	wantContains(t, fmt.Sprintf("phenix experiment scorch -r %d", run), out, "configure component ersbad")

	st, stErr := scorchHostReadStage(scorchHostCmpDir(name, run, "ersbad"), "configure")
	if stErr != nil {
		t.Fatalf("component ersbad: %v\n%s", stErr, out)
	}

	if ret := fmt.Sprint(st.Return); !strings.HasPrefix(ret, "Error occurred") || !strings.Contains(ret, "local command") {
		t.Errorf("ersbad's configure returned %q, want the error of its local ovs-vsctl command", ret)
	}

	ifaces, err := hostCmd("ovs-vsctl", "--bare", "--columns=name", "find", "Interface", "name="+scorchHostERSPANPort)
	if err != nil || ifaces != "" {
		t.Errorf("after a failed erspan configure, OVS has interface %s: %q (%v)", scorchHostERSPANPort, ifaces, err)
	}

	// Only erspan's own mirror: minimega's bridge captures add mirrors too.
	mirrors := scorchHostMirrors(t)
	if _, ok := mirrors[scorchHostERSPANMirror]; ok {
		t.Errorf("after a failed erspan configure, OVS has erspan's mirror %s: %v", scorchHostERSPANMirror, mirrors)
	}

	scorchHostCheckRun(t, name, run, "erspan-local-failure")
}

func scorchHostCheckERSPANPort(t *testing.T, env scorchHostEnv) {
	t.Helper()

	get := func(column string) string {
		out, err := hostCmd("ovs-vsctl", "get", "Interface", scorchHostERSPANPort, column)
		if err != nil {
			t.Errorf("ovs-vsctl get Interface %s %s: %v: %s", scorchHostERSPANPort, column, err, out)
		}

		return out
	}

	if typ := get("type"); typ != "erspan" {
		t.Errorf("OVS interface %s has type %q, want erspan", scorchHostERSPANPort, typ)
	}

	wantContains(t, "OVS interface "+scorchHostERSPANPort+" options", get("options"),
		`remote_ip="192.0.2.2"`, `key="100"`, `erspan_ver="1"`)

	if e := get("error"); e != "[]" {
		t.Errorf("OVS could not set up %s: %s", scorchHostERSPANPort, e)
	}

	lan, spare := scorchHostVLAN(env.vlans, "LAN"), scorchHostVLAN(env.vlans, "SPARE")
	mirrors := scorchHostMirrors(t)

	m, ok := mirrors[scorchHostERSPANMirror]
	if !ok {
		t.Fatalf("after erspan's configure, bridge %s has no mirror %s: %v", scorchHostBridge, scorchHostERSPANMirror, mirrors)
	}

	if !m.selectAll || !slices.Equal(m.selectVLAN, []int{lan}) || slices.Contains(m.selectVLAN, spare) {
		t.Errorf("mirror %s selects all %t, VLANs %v; want all of LAN (%d) only, SPARE (%d) excluded",
			scorchHostERSPANMirror, m.selectAll, m.selectVLAN, lan, spare)
	}

	if _, ok := mirrors[scorchHostSentinel]; !ok {
		t.Errorf("erspan's configure replaced the bridge's other mirrors: %v", mirrors)
	}
}

// scorchHostCheckERSPANLog checks that every local command erspan's
// configure ran succeeded without complaint.
func scorchHostCheckERSPANLog(t *testing.T, path string) {
	t.Helper()

	var entries []struct {
		Event string `json:"event"`
		Data  struct {
			Cmd    string   `json:"cmd"`
			Stderr []string `json:"stderr"`
		} `json:"data"`
	}

	scorchHostReadJSON(t, path, &entries)

	local := 0

	for _, e := range entries {
		if e.Event != "config_local" {
			continue
		}

		local++

		if len(e.Data.Stderr) != 0 {
			t.Errorf("erspan's local command %q wrote to stderr: %v", e.Data.Cmd, e.Data.Stderr)
		}
	}

	if local != 2 {
		t.Errorf("%s records %d local commands, want 2 (port and mirror): %+v", path, local, entries)
	}
}

// scorchHostReceiver makes the ERSPAN tunnel's far end: 192.0.2.2 in its own
// network namespace, reached from the host's 192.0.2.1 over a veth pair.
func scorchHostReceiver(t *testing.T) {
	t.Helper()

	drop := func() {
		_, _ = hostCmd("ip", "netns", "delete", scorchHostRxNS)
		_, _ = hostCmd("ip", "link", "delete", scorchHostRxHost)
	}

	drop()
	t.Cleanup(drop)

	for _, args := range [][]string{
		{"netns", "add", scorchHostRxNS},
		{"link", "add", scorchHostRxHost, "type", "veth", "peer", "name", scorchHostRxPeer},
		{"link", "set", scorchHostRxPeer, "netns", scorchHostRxNS},
		{"addr", "add", "192.0.2.1/30", "dev", scorchHostRxHost},
		{"link", "set", scorchHostRxHost, "up"},
		{"-n", scorchHostRxNS, "addr", "add", "192.0.2.2/30", "dev", scorchHostRxPeer},
		{"-n", scorchHostRxNS, "link", "set", scorchHostRxPeer, "up"},
		{"-n", scorchHostRxNS, "link", "set", "lo", "up"},
	} {
		if out, err := hostCmd("ip", args...); err != nil {
			t.Fatalf("ip %s: %v: %s", strings.Join(args, " "), err, out)
		}
	}
}

// scorchHostSentinelMirror adds a mirror of the test's own to the bridge, so
// the erspan case can tell whether erspan touches mirrors it did not create.
func scorchHostSentinelMirror(t *testing.T) {
	t.Helper()

	t.Cleanup(func() {
		_, _ = hostCmd("ovs-vsctl", "--", "--id=@m", "get", "Mirror", scorchHostSentinel,
			"--", "remove", "Bridge", scorchHostBridge, "mirrors", "@m")
		_, _ = hostCmd("ovs-vsctl", "--if-exists", "del-port", scorchHostBridge, scorchHostSentinelPort)
	})

	for _, args := range [][]string{
		{"add-port", scorchHostBridge, scorchHostSentinelPort, "--", "set", "Interface", scorchHostSentinelPort, "type=internal"},
		{
			"--", "--id=@p", "get", "Port", scorchHostSentinelPort,
			"--", "--id=@m", "create", "Mirror", "name=" + scorchHostSentinel, "select_all=true", "output-port=@p",
			"--", "add", "Bridge", scorchHostBridge, "mirrors", "@m",
		},
	} {
		if out, err := hostCmd("ovs-vsctl", args...); err != nil {
			t.Fatalf("ovs-vsctl %s: %v: %s", strings.Join(args, " "), err, out)
		}
	}
}

// scorchHostERSPANTraffic captures GRE at the receiver while the tap sends
// ARP requests for an address nobody has; each is broadcast on LAN, so the
// mirror sends it through the tunnel. It returns the GRE packets seen.
func scorchHostERSPANTraffic(t *testing.T, tap string) int {
	t.Helper()

	var out bytes.Buffer

	rx := exec.CommandContext(t.Context(), "ip", "netns", "exec", scorchHostRxNS, //nolint:gosec // test harness
		"tshark", "-n", "-i", scorchHostRxPeer, "-c", "3", "-a", "duration:12", "-f", "ip proto 47",
		"-T", "fields", "-e", "frame.number")
	rx.Stdout = &out

	if err := rx.Start(); err != nil {
		t.Fatalf("starting the ERSPAN receiver's capture: %v", err)
	}

	done := make(chan error, 1)

	go func() { done <- rx.Wait() }()

	for {
		select {
		case err := <-done:
			if err != nil {
				t.Logf("the ERSPAN receiver's capture ended: %v", err)
			}

			return len(strings.Fields(out.String()))
		default:
		}

		if _, err := hostCmd("ip", "netns", "exec", tap, "ping", "-c", "2", "-W", "1", "10.10.0.99"); err != nil {
			// No answer is expected; this only keeps a vanished tap from
			// spinning the loop.
			time.Sleep(200 * time.Millisecond)
		}
	}
}

// ---------------------------------------------------------------------------
// SCH-10..SCH-12

// scorchHostCheckLoops runs a run with count 2, a nested loop of count 3 and
// replacements (SCH-10): the user component sees every call in order, with
// the stage, name, run, loop and count arguments phenix documents, and a
// replaced value keeps its type.
func scorchHostCheckLoops(t *testing.T, env scorchHostEnv) {
	t.Helper()

	name, run := env.exp, scorchHostRunLoops

	scorchHostRunOK(t, name, run)

	dir := filepath.Join(scorchHostRunDir(name, run), scorchHostEcho)
	calls := strings.Split(strings.TrimSpace(readFile(t, filepath.Join(dir, "calls.log"))), "\n")

	want := make([]string, 0, 10)

	for count := range 2 {
		want = append(want, fmt.Sprintf("start echo1 %d 0 %d", run, count))

		for loop := range 3 {
			want = append(want, fmt.Sprintf("start echo2 %d 1 %d", run, loop))
		}

		want = append(want, fmt.Sprintf("stop echo1 %d 0 %d", run, count))
	}

	if !slices.Equal(calls, want) {
		t.Errorf("the component's calls were\n%s\nwant\n%s", strings.Join(calls, "\n"), strings.Join(want, "\n"))
	}

	for count := range 2 {
		meta := scorchHostStdinMeta(t, filepath.Join(dir, fmt.Sprintf("start-echo1-%d-0-%d.stdin.json", run, count)), "echo1")

		// "@D@" is a whole value, so it is replaced by the generated integer
		// itself, not by its text.
		if meta["greeting"] != "hello" || meta["delay"] != json.Number("3") || len(meta) != 2 {
			t.Errorf("count %d: echo1's metadata on stdin is %v, want greeting hello and delay the number 3", count, meta)
		}
	}

	if meta := scorchHostStdinMeta(
		t,
		filepath.Join(dir, fmt.Sprintf("start-echo2-%d-1-2.stdin.json", run)),
		"echo2",
	); meta["greeting"] != "inner" {
		t.Errorf("echo2's metadata on stdin is %v, want greeting inner", meta)
	}

	scorchHostCheckRun(t, name, run, "loops", scorchHostEcho+"/calls.log",
		fmt.Sprintf("%s/start-echo2-%d-1-2.stdin.json", scorchHostEcho, run))
}

// scorchHostCheckCollector runs a baseline disruption and the collector in
// a run without a loop, in a scenario defining components of several types
// the run does not use (SCH-11).
func scorchHostCheckCollector(t *testing.T, env scorchHostEnv) {
	t.Helper()

	name, run := env.exp, scorchHostRunCollector

	scorchHostRunOK(t, name, run)

	scorchHostWantStage(t, name, run, "dis0", "start")
	scorchHostWantStage(t, name, run, "col0", "stop")

	results := filepath.Join(scorchHostCmpDir(name, run, "col0"), "experiment_results")

	var rec struct {
		Experiment struct {
			Disruption     string  `json:"disruption"`
			Duration       float64 `json:"duration"`
			DurationActual float64 `json:"duration_actual"`
			Permutation    int     `json:"permutation"`
		} `json:"experiment"`
		Sceptre struct {
			Experiment string `json:"experiment"`
			Topology   string `json:"topology"`
			Scenario   string `json:"scenario"`
			Minimega   string `json:"minimega_version"`
			Phenix     string `json:"phenix_version"`
		} `json:"sceptre"`
		Scorch struct {
			Run   int `json:"run"`
			Loop  int `json:"loop"`
			Count int `json:"count"`
		} `json:"scorch"`
		QoS        map[string]any    `json:"qos"`
		Provider   map[string]any    `json:"provider"`
		AllHosts   map[string]string `json:"all_hosts"`
		Pcap       any               `json:"pcap_metadata"`
		Disruption map[string]any    `json:"disruption"`
	}

	scorchHostReadJSON(t, filepath.Join(results, "experiment_record.json"), &rec)

	if e := rec.Experiment; e.Disruption != "baseline" || e.Duration != 2 || e.DurationActual < 2 || e.Permutation != 0 {
		t.Errorf("experiment record's experiment is %+v, want a baseline of 2 s that lasted at least that", e)
	}

	if s := rec.Sceptre; s.Experiment != name || s.Topology != name || s.Scenario != name || s.Phenix == "" {
		t.Errorf("experiment record's sceptre is %+v, want experiment, topology and scenario %s", s, name)
	}

	if mmVersion := mmText(t, "", "version"); rec.Sceptre.Minimega != mmVersion {
		t.Errorf("experiment record's minimega_version is %q, want minimega's %q", rec.Sceptre.Minimega, mmVersion)
	}

	if s := rec.Scorch; s.Run != run || s.Loop != 0 || s.Count != 0 {
		t.Errorf("experiment record's scorch is %+v, want run %d, loop 0, count 0", s, run)
	}

	// The run uses no qos, pcap or provider component, although the scenario
	// defines several; only h1 has more than one interface.
	if rec.QoS == nil || len(rec.QoS) != 0 || rec.Provider == nil || len(rec.Provider) != 0 || rec.Pcap != nil {
		t.Errorf("experiment record has qos %v, provider %v, pcap_metadata %v; want {}, {} and null for a run without them",
			rec.QoS, rec.Provider, rec.Pcap)
	}

	if want := map[string]string{"h1": "10.10.0.1"}; !reflect.DeepEqual(rec.AllHosts, want) {
		t.Errorf("experiment record's all_hosts is %v, want %v", rec.AllHosts, want)
	}

	if _, ok := rec.Disruption["baseline"]; !ok {
		t.Errorf("experiment record's disruption is %v, want a baseline entry", rec.Disruption)
	}

	for _, f := range []string{"metadata/topology.yaml", "metadata/scenario.yaml", "metadata/experiment.yaml",
		"disruption/disruption_start_time.txt", "disruption/disruption_stop_time.txt"} {
		if strings.TrimSpace(readFile(t, filepath.Join(results, f))) == "" {
			t.Errorf("collector's %s is empty", f)
		}
	}

	scorchHostCheckRun(t, name, run, "collector",
		"col0/loop-0-count-0/experiment_results/experiment_record.json", "dis0/loop-0-count-0/disruption_start_time.txt")
}

// scorchHostCheckFilebeat runs a run whose own filebeat config is enabled
// while the scenario-wide one is not (SCH-12): phenix writes the
// run's config and starts Filebeat with it; a stand-in records how phenix
// starts and stops it.
func scorchHostCheckFilebeat(t *testing.T, env scorchHostEnv) {
	t.Helper()

	name, run := env.exp, scorchHostRunFilebeat
	args := scorchHostFakeFilebeat(t)

	scorchHostRunOK(t, name, run)

	runDir := scorchHostRunDir(name, run)
	cfgPath := filepath.Join(runDir, "filebeat", "filebeat.yml")

	var fb map[string]any
	if err := yaml.Unmarshal([]byte(readFile(t, cfgPath)), &fb); err != nil {
		t.Fatalf("%s does not parse: %v", cfgPath, err)
	}

	fields, _ := fb["fields"].(map[string]any)
	if fields["scorch.experiment"] != name || fields["scorch.run_id"] != run || fields["scorch.run_name"] != "filebeat" {
		t.Errorf("filebeat.yml fields are %v, want scorch.experiment %s, scorch.run_id %d, scorch.run_name filebeat", fields, name, run)
	}

	inputs, _ := fb["filebeat.inputs"].([]any)
	wantPath := filepath.Join(runDir, "hsfb") + "/loop-*-count-*/host_stats.jsonl"

	if len(inputs) != 1 {
		t.Errorf("filebeat.yml has inputs %v, want hsfb's one", inputs)
	} else if in, _ := inputs[0].(map[string]any); !reflect.DeepEqual(in["paths"], []any{wantPath}) {
		t.Errorf("filebeat.yml input paths are %v, want [%s]", in["paths"], wantPath)
	}

	if out, _ := fb["output.file"].(map[string]any); out["path"] != filepath.Join(cfg.workDir, "scorch-host-filebeat") {
		t.Errorf("filebeat.yml output.file is %v, want the run's own output config", out)
	}

	scorchHostCheckHostStats(t, filepath.Join(scorchHostCmpDir(name, run, "hsfb"), "host_stats.jsonl"), env.head, 3)

	// phenix starts Filebeat for a run that enables it in its own config, and
	// interrupts it once the run is done (the stand-in serves no harvester
	// metrics, so phenix stops waiting for them at once).
	data, err := os.ReadFile(args) //nolint:gosec // test's own file
	if err != nil || !strings.HasPrefix(string(data), "-c\n"+cfgPath+"\n--path.data\n") {
		t.Errorf("phenix did not start filebeat with -c %s --path.data for a run with filebeat enabled (%v): %q",
			cfgPath, err, data)
	}

	if !strings.HasSuffix(string(data), "\ninterrupted\n") {
		t.Errorf("phenix did not interrupt filebeat at the end of the run: %q", data)
	}

	if pids, err := hostCmd("pgrep", "-f", filepath.Join(scorchHostBin, "filebeat")); err == nil {
		t.Errorf("filebeat still runs after the run: pids %s", pids)
	}

	scorchHostCheckRun(t, name, run, "filebeat", "filebeat/filebeat.yml", "hsfb/loop-0-count-0/host_stats.jsonl")
}

// scorchHostFakeFilebeat installs a filebeat stand-in that records its
// arguments, runs until phenix interrupts it and records that; it returns
// where the record goes.
func scorchHostFakeFilebeat(t *testing.T) string {
	t.Helper()

	path := filepath.Join(scorchHostBin, "filebeat")
	if _, err := os.Stat(path); err == nil {
		t.Fatalf("%s already exists; the scorch-host shard installs its own filebeat stand-in there", path)
	}

	args := filepath.Join(cfg.workDir, "scorch-host-filebeat.args")
	_ = os.Remove(args)

	script := "#!/bin/bash\nargs='" + args + "'\nprintf '%s\\n' \"$@\" > \"$args\"\n" +
		"trap 'echo interrupted >> \"$args\"; exit 0' INT TERM\nwhile :; do sleep 0.2; done\n"

	if err := os.WriteFile(path, []byte(script), 0o755); err != nil { //nolint:gosec // executable stand-in
		t.Fatal(err)
	}

	t.Cleanup(func() {
		_ = os.Remove(path)
		_, _ = hostCmd("pkill", "-KILL", "-f", path)
	})

	return args
}

// scorchHostCheckForeground runs hoststats without background: true
// (SCH-05b): its start stage waits for a SIGTERM phenix only
// sends to background components, so it must refuse at once instead of
// blocking the pipeline.
func scorchHostCheckForeground(t *testing.T, env scorchHostEnv) {
	t.Helper()

	name, run := env.exp, scorchHostRunForeground

	proc := scorchHostStart(t, "experiment", "scorch", name, "--run", strconv.Itoa(run))

	// The refusal costs one component start; a hoststats that blocks instead
	// fails the wait.
	out, err := proc.wait(t, time.Minute)
	if err == nil {
		t.Errorf("phenix experiment scorch -r %d with a foreground hoststats succeeded, want a failure\n%s", run, out)
	}

	wantContains(t, fmt.Sprintf("phenix experiment scorch -r %d", run), out, "start component hsfg")

	st, stErr := scorchHostReadStage(scorchHostCmpDir(name, run, "hsfg"), "start")
	if stErr != nil {
		t.Fatalf("component hsfg: %v\n%s", stErr, out)
	}

	if ret := fmt.Sprint(st.Return); !strings.HasPrefix(ret, "Error occurred") || !strings.Contains(ret, "must set background: true") {
		t.Errorf("hsfg's start returned %q, want the refusal of a foreground hoststats", ret)
	}

	scorchHostCheckRun(t, name, run, "hoststats-foreground")
}

// ---------------------------------------------------------------------------
// SCH-06 and the other background cases

// scorchHostCheckGraceful runs three background components over REST (SCH-06):
// phenix reports them backgrounded during the run, and its stop stage ends
// each with SIGTERM, leaving time to act on it. Each stop stage waits for its
// backgrounded start to exit (SIGKILL 10 s after SIGTERM, up to 30 s in all)
// before the run goes on, so the run is archived only after the sleeper's 6 s
// of work after SIGTERM, and the archive holds its last line.
func scorchHostCheckGraceful(t *testing.T, env scorchHostEnv) {
	t.Helper()

	name, run := env.exp, scorchHostRunBackground
	backgrounded := []string{"sleeper", "hslow", "hs30"}

	scorchHostPost(t, name, run)

	ok, detail := scorchHostPoll(time.Minute, 250*time.Millisecond, func() (bool, string) {
		nodes := scorchHostPipeline(t, name, run)
		statuses := make([]string, 0, len(backgrounded))

		for _, c := range backgrounded {
			statuses = append(statuses, scorchHostStartStatus(nodes, c))
		}

		return !slices.ContainsFunc(statuses, func(s string) bool { return s != scorchHostBackground }),
			fmt.Sprintf("%v are %q", backgrounded, statuses)
	})
	if !ok {
		t.Errorf("during run %d, the pipeline never showed every component backgrounded: %s", run, detail)
	}

	nodes := scorchHostWaitREST(t, name, run)
	ended := time.Now()

	for _, c := range backgrounded {
		if st := scorchHostStartStatus(nodes, c); st != scorchHostSuccess {
			t.Errorf("after run %d, pipeline node %s (start) is %q, want %s", run, c, st, scorchHostSuccess)
		}
	}

	sleepDir := filepath.Join(scorchHostRunDir(name, run), scorchHostSleep)
	state := filepath.Join(sleepDir, fmt.Sprintf("start-sleeper-%d-0-0.state", run))

	eventually(t, scorchHostSettle, 500*time.Millisecond, "the sleeper to act on its SIGTERM", func() (bool, string) {
		data, _ := os.ReadFile(state) //nolint:gosec // product output

		return strings.Contains(string(data), "\nsigterm "), string(data)
	})

	wantContains(
		t,
		"the sleeper's stop stage",
		readFile(t, filepath.Join(sleepDir, fmt.Sprintf("stop-sleeper-%d-0-0.state", run))),
		"end",
	)

	// hslow flushes only once at start and once when stopped: its last lines
	// and its status file come from the work it does after SIGTERM.
	hsDir := scorchHostCmpDir(name, run, "hslow")
	scorchHostWaitStage(t, hsDir, "start", "Stopped user component: hslow")
	scorchHostCheckHostStats(t, filepath.Join(hsDir, "host_stats.jsonl"), env.head, 4)
	scorchHostWantStage(t, name, run, "hslow", "stop")

	stateEntry := fmt.Sprintf("%s/start-sleeper-%d-0-0.state", scorchHostSleep, run)
	files := scorchHostCheckRun(t, name, run, "background", stateEntry, "hslow/loop-0-count-0/host_stats.jsonl")

	if s := string(files[stateEntry]); !strings.Contains(s, "\nsigterm ") {
		t.Errorf("run %d's archive holds the sleeper's state from before it acted on SIGTERM: %q", run, s)
	}

	// hs30 got SIGTERM about 7 s into its 30 s poll period, and its
	// stop stage waited for its start stage to exit (phenix kills it 10 s
	// after SIGTERM) before the sleeper's stop, which takes 6 s, ran. Its
	// start stage should have ended cleanly by the end of the run, long
	// before the poll period would have ended on its own.
	hs30Dir := scorchHostCmpDir(name, run, "hs30")

	stopped, detail := scorchHostPoll(time.Until(ended.Add(12*time.Second)), 500*time.Millisecond, func() (bool, string) {
		st, err := scorchHostReadStage(hs30Dir, "start")
		if err != nil {
			return false, err.Error()
		}

		return slices.ContainsFunc(st.Logs, func(l string) bool { return strings.Contains(l, "Stopped user component: hs30") }),
			fmt.Sprintf("return %v, logs %v", st.Return, st.Logs)
	})
	if !stopped {
		t.Errorf("hs30 (pollPeriod 30) did not end its start stage within phenix's kill delay after SIGTERM: %s", detail)
	}
}

// scorchHostCheckOrphan runs a background component that no stop or cleanup
// stage names: the run should not leave it running.
func scorchHostCheckOrphan(t *testing.T, env scorchHostEnv) {
	t.Helper()

	name, run := env.exp, scorchHostRunOrphan
	pattern := "phenix-scorch-component-" + scorchHostSleep + " start sleeper2 "

	t.Cleanup(func() { _, _ = hostCmd("pkill", "-KILL", "-f", pattern) })

	scorchHostRunOK(t, name, run)

	// The run's 3 s pause lets the component start; the run then ends it, and
	// waits for it to exit, before it returns.
	stateEntry := fmt.Sprintf("%s/start-sleeper2-%d-0-0.state", scorchHostSleep, run)
	state := readFile(t, filepath.Join(scorchHostRunDir(name, run), stateEntry))

	if !strings.Contains(state, "begin ") || !strings.Contains(state, "\nsigterm ") {
		t.Errorf("after run %d, sleeper2's state is %q, want it begun and ended with SIGTERM", run, state)
	}

	if out, err := hostCmd("pgrep", "-f", pattern); err == nil {
		t.Errorf("run %d ended but its background component sleeper2 still runs (pids %s)", run, out)
	}

	files := scorchHostCheckRun(t, name, run, "orphan", stateEntry)
	if s := string(files[stateEntry]); !strings.Contains(s, "\nsigterm ") {
		t.Errorf("run %d's archive holds sleeper2's state from before it acted on SIGTERM: %q", run, s)
	}
}

// scorchHostCheckBackgroundFailure runs a background component whose start
// stage fails at once: the run should fail and say which component.
func scorchHostCheckBackgroundFailure(t *testing.T, env scorchHostEnv) {
	t.Helper()

	name, run := env.exp, scorchHostRunBackgroundFailure

	out, err := scorchHostRun(name, run)

	marker := filepath.Join(scorchHostRunDir(name, run), scorchHostFail, "start-bgfail.failed")
	if _, statErr := os.Stat(marker); statErr != nil {
		t.Fatalf("the failing component's start stage did not run: %v\n%s", statErr, out)
	}

	if err == nil || !strings.Contains(out, "bgfail") {
		t.Errorf(
			"phenix experiment scorch -r %d with a failed background component: err %v, want a failure naming bgfail\n%s",
			run,
			err,
			out,
		)
	}

	if running, has := scorchHostState(t, name); running || has {
		t.Errorf("after run %d, scorch running %t, status.apps.scorch present %t; want neither", run, running, has)
	}
}

// ---------------------------------------------------------------------------
// SCH-13, SCH-14

// scorchHostCheckGuards checks the refusals of phenix experiment scorch on a
// running experiment (SCH-14): an unknown run ID, a second run while one
// runs, over the CLI and REST; and that `trigger running` skips SCORCH while a
// run is active. Neither may start run 0 (and its tap).
func scorchHostCheckGuards(t *testing.T, env scorchHostEnv) {
	t.Helper()

	name := env.exp

	out := phenixFails(t, "experiment", "scorch", name, "--run", "99")
	wantContains(t, "phenix experiment scorch -r 99", out, "invalid Scorch run ID")

	if running, has := scorchHostState(t, name); running || has {
		t.Errorf("after a refused run, scorch running %t, status.apps.scorch present %t; want neither", running, has)
	}

	run0 := scorchHostRunInfo(name, scorchHostRunTap)
	before := scorchHostTaps()
	proc := scorchHostStart(t, "experiment", "scorch", name, "--run", strconv.Itoa(scorchHostRunIdle))

	eventually(t, time.Minute, 250*time.Millisecond, "run 10 to mark SCORCH running", func() (bool, string) {
		running, _ := scorchHostState(t, name)

		return running || proc.finished(), fmt.Sprintf("running %t", running)
	})

	if proc.finished() {
		out, err := proc.wait(t, time.Minute)
		t.Fatalf("run %d finished before the guards were checked (%v):\n%s", scorchHostRunIdle, err, out)
	}

	out = phenixFails(t, "experiment", "scorch", name, "--run", strconv.Itoa(scorchHostRunTap))
	wantContains(t, "a second phenix experiment scorch", out, "already running")

	// phenix ui refuses the same run in the background, after answering; the
	// answer should be the refusal.
	path := fmt.Sprintf("/experiments/%s/scorch/pipelines/%d", name, scorchHostRunTap)
	code, body := api(t, http.MethodPost, path, nil)
	if code < 400 || !strings.Contains(string(body), "already running") {
		t.Errorf("POST %s while run %d runs: status %d, %q; want a refusal saying SCORCH is already running",
			path, scorchHostRunIdle, code, body)
	}

	// trigger is synchronous: had it (or the refused CLI and REST runs) run
	// SCORCH run 0, its tap would exist and run 0's directory would be new by
	// the time it returns.
	if out, err := phenixRun("experiment", "trigger", "running", name, "scorch"); err != nil {
		t.Errorf("phenix experiment trigger running during a SCORCH run: %v, want it to skip SCORCH\n%s", err, out)
	}

	if taps := scorchHostNewTaps(before); len(taps) != 0 {
		t.Errorf("a refused run or trigger running during a SCORCH run started run 0 anyway: taps %v", taps)
		scorchHostDropTaps(t, name, taps)
	}

	if now := scorchHostRunInfo(name, scorchHostRunTap); now != run0 {
		t.Errorf("a refused run or trigger running during a SCORCH run rewrote run 0's directory: %q, then %q", run0, now)
	}

	if out, err := proc.wait(t, scorchHostRunTimeout); err != nil {
		t.Fatalf("phenix experiment scorch -r %d: %v\n%s", scorchHostRunIdle, err, out)
	}

	scorchHostCheckRun(t, name, scorchHostRunIdle, "idle")
}

// scorchHostCheckTrigger runs run 0, whose tap component needs the status
// SCORCH records for a run, first through phenix experiment scorch and then
// through `trigger running` (SCH-13), which must run run 0 again the same
// way: a new run info file and archive.
func scorchHostCheckTrigger(t *testing.T, env scorchHostEnv) {
	t.Helper()

	name, run := env.exp, scorchHostRunTap

	before := scorchHostTaps()
	proc := scorchHostStart(t, "experiment", "scorch", name, "--run", strconv.Itoa(run))
	scorchHostWaitTap(t, proc, before)

	if out, err := proc.wait(t, scorchHostRunTimeout); err != nil {
		t.Fatalf("phenix experiment scorch -r %d: %v\n%s", run, err, out)
	}

	if left := scorchHostTapsLeft(before, 15*time.Second); len(left) != 0 {
		t.Errorf("run %d through phenix experiment scorch left its tap: %v", run, left)
		scorchHostDropTaps(t, name, left)
	}

	scorchHostCheckRun(t, name, run, "tap-probe")

	// The running stage skips an app silently, so tell the triggered run from
	// the files the CLI run just left.
	info := scorchHostLatest(scorchHostInfoPattern(name, run))
	archive := scorchHostLatest(scorchHostArchivePattern(name, run))

	before = scorchHostTaps()
	out, err := phenixRun("experiment", "trigger", "running", name, "scorch")
	t.Logf("phenix experiment trigger running %s scorch: %v\n%s", name, err, out)

	if err != nil {
		t.Errorf("phenix experiment trigger running %s scorch: %v, want it to run SCORCH run %d", name, err, run)
	}

	left := scorchHostTapsLeft(before, 15*time.Second)
	if len(left) != 0 {
		t.Errorf("trigger running left SCORCH run %d's tap behind: %v", run, left)
		scorchHostDropTaps(t, name, left)
	}

	if running, has := scorchHostState(t, name); running || has {
		t.Errorf("after trigger running, scorch running %t, status.apps.scorch present %t; want neither", running, has)
	}

	if err == nil {
		if now := scorchHostLatest(scorchHostInfoPattern(name, run)); now == info {
			t.Errorf("trigger running did not run SCORCH run %d: its run info file is still %s", run, info)
		}

		if now := scorchHostLatest(scorchHostArchivePattern(name, run)); now == archive {
			t.Errorf("trigger running did not run SCORCH run %d: its latest archive is still %s", run, archive)
		}

		scorchHostCheckRun(t, name, run, "tap-probe")
	}
}

// scorchHostCheckTypo checks that phenix refuses to create an experiment
// whose scorch app has a run naming a component the scenario never defines,
// and names the component.
func scorchHostCheckTypo(t *testing.T) {
	t.Helper()

	name := scorchHostTypo

	topo := strings.ReplaceAll(topology(hollowNode("t1", "LAN", "10.20.0.1", "")), "{{name}}", name)
	scenario := strings.ReplaceAll(scorchHostTypoScenario, "{{name}}", name)

	scorchHostKeepFiles(t, name)

	phenix(t, "config", "create", writeFile(t, name+"-topology.yaml", topo))
	t.Cleanup(func() { _, _ = phenixRun("config", "delete", "topology/"+name) })

	phenix(t, "config", "create", writeFile(t, name+"-scenario.yaml", scenario))
	t.Cleanup(func() { _, _ = phenixRun("config", "delete", "scenario/"+name) })

	// phenix validates the runs when it configures the scorch app.
	out, err := phenixRun("experiment", "create", name, "--topology", name, "--scenario", name)
	if err == nil {
		exp := &experiment{t: t, name: name}
		t.Cleanup(exp.teardown)

		t.Fatalf("phenix experiment create with an undefined component succeeded, want it refused naming nosuchcmp\n%s", out)
	}

	wantContains(t, "phenix experiment create with an undefined component", out, "nosuchcmp")
}

// ---------------------------------------------------------------------------
// setup

func scorchHostTopology() string {
	return topology(
		hollowNode("h1", "LAN", "10.10.0.1", scorchHostSpareIface),
		hollowNode("h2", "LAN", "10.10.0.2", ""),
		hollowNode("h3", "LAN", "10.10.0.3", ""),
	)
}

// scorchHostRequire fails the test unless the apps components and the host
// tools the shard drives are installed.
func scorchHostRequire(t *testing.T) {
	t.Helper()

	commands := make([]string, 0, 19)

	for _, c := range []string{"cc", "collector", "disruption", "erspan", "hoststats", "mm", "pcap", "pipe", "qos"} {
		commands = append(commands, "phenix-scorch-component-"+c)
	}

	// The collector shells out to phenix; pcap needs the Wireshark tools.
	commands = append(commands, "phenix", "tshark", "capinfos", "editcap", "mergecap", "ovs-vsctl", "tc", "ip", "pgrep", "pkill")

	requireApps(t, "scorch-host", commands...)
}

// scorchHostInstall installs a SCORCH user component of type typ on the PATH
// of phenix and phenix ui, and removes it when the test ends.
func scorchHostInstall(t *testing.T, typ, script string) {
	t.Helper()

	path := filepath.Join(scorchHostBin, "phenix-scorch-component-"+typ)

	if err := os.WriteFile(path, []byte(script), 0o755); err != nil { //nolint:gosec // executable component
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = os.Remove(path) })
}

// scorchHostRestore puts back the namespace state a failed run can leave
// behind, so it cannot affect the next case.
func scorchHostRestore(t *testing.T, exp string) {
	t.Helper()

	// A run a failed case started through phenix ui may still be going.
	if ok, detail := scorchHostPoll(scorchHostRunTimeout, time.Second, func() (bool, string) {
		running, _, err := scorchHostReadState(exp)

		return err == nil && !running, fmt.Sprint(err)
	}); !ok {
		t.Logf("restore: SCORCH is still marked running in %s: %s", exp, detail)
	}

	// h1 first: clearing qos looks up the bridge of every interface, and a
	// disconnected one has none.
	if resp, err := mmRun(exp, "vm info"); err != nil {
		t.Logf("restore: %v", err)
	} else {
		for _, row := range mmRows(resp) {
			if row["name"] != "h1" {
				continue
			}

			if row["state"] == "PAUSED" {
				t.Logf("restore: resuming h1")

				_, _ = mmRun(exp, "vm start h1")
			}

			if vlans := scorchHostList(row["vlan"]); len(vlans) > 1 && vlans[1] == "disconnected" {
				t.Logf("restore: reconnecting h1's second interface")

				_, _ = mmRun(exp, "vm net connect h1 1 SPARE "+scorchHostBridge)
			}
		}
	}

	// minimega: a namespaced subcommand is rebuilt quoting only values with
	// whitespace (pkg/minicli/input.go, trie.go) and compiled again
	// (cmd/minimega/cli.go), so `capture pcap filter ''` loses its empty value
	// and becomes a query. A blank filter survives and matches every packet.
	for _, c := range []string{"clear capture", "capture pcap filter ' '", "capture pcap snaplen 0", "clear qos all all", "clear cc filter"} {
		if _, err := mmRun(exp, c); err != nil {
			t.Logf("restore: %v", err)
		}
	}
}

// ---------------------------------------------------------------------------
// running SCORCH

// scorchHostRun runs one SCORCH run through the CLI and returns its output.
func scorchHostRun(exp string, run int) (string, error) {
	return phenixRun("experiment", "scorch", exp, "--run", strconv.Itoa(run))
}

// scorchHostRunOK runs one SCORCH run through the CLI and fails the test if
// it fails.
func scorchHostRunOK(t *testing.T, exp string, run int) string {
	t.Helper()

	out, err := scorchHostRun(exp, run)
	if err != nil {
		t.Fatalf("phenix experiment scorch %s -r %d: %v\n%s", exp, run, err, out)
	}

	return out
}

// scorchHostStart runs the phenix CLI in the background; it is killed when
// the test ends if it still runs.
func scorchHostStart(t *testing.T, args ...string) *phenixProc {
	t.Helper()

	p := &phenixProc{done: make(chan struct{})}

	// Not canceled when the test ends: a SCORCH run killed midway stays
	// marked running and every later run is refused.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), phenixTimeout)

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
	}()

	// A test that fails midway lets the run finish, then interrupts it, which
	// phenix handles by running the run's stop and cleanup stages.
	t.Cleanup(func() {
		defer cancel()

		select {
		case <-p.done:
			return
		case <-time.After(scorchHostRunTimeout):
		}

		_ = cmd.Process.Signal(os.Interrupt)

		select {
		case <-p.done:
		case <-time.After(time.Minute):
			_ = cmd.Process.Kill()
			<-p.done
		}
	})

	return p
}

// scorchHostNode is one node of a SCORCH pipeline phenix ui reports.
type scorchHostNode struct {
	Name   string `json:"name"`
	Stage  string `json:"stage"`
	Status string `json:"status"`
}

// scorchHostPipeline returns loop 0 of a run's pipeline as phenix ui
// reports it, or nil while it has none.
func scorchHostPipeline(t *testing.T, exp string, run int) []scorchHostNode {
	t.Helper()

	code, data := api(t, http.MethodGet, fmt.Sprintf("/experiments/%s/scorch/pipelines/%d/0", exp, run), nil)
	if code != http.StatusOK {
		return nil
	}

	var body struct {
		Pipeline []scorchHostNode `json:"pipeline"`
	}

	if json.Unmarshal(data, &body) != nil {
		return nil
	}

	return body.Pipeline
}

// scorchHostPipelines returns the run names GET /scorch/pipelines lists, in
// run order, and the run it reports running (-1 for none).
func scorchHostPipelines(t *testing.T, exp string) ([]string, int) {
	t.Helper()

	var body struct {
		Pipelines []struct {
			Name string `json:"name"`
		} `json:"pipelines"`
		Running int `json:"running"`
	}

	apiOK(t, http.MethodGet, "/experiments/"+exp+"/scorch/pipelines", nil, &body)

	names := make([]string, 0, len(body.Pipelines))

	for _, p := range body.Pipelines {
		names = append(names, p.Name)
	}

	return names, body.Running
}

// scorchHostStartStatus is the status of a component's start-stage node.
func scorchHostStartStatus(nodes []scorchHostNode, name string) string {
	for _, n := range nodes {
		if n.Stage == "start" && n.Name == name {
			return n.Status
		}
	}

	return ""
}

// scorchHostPost starts a run through phenix ui, after removing what an
// earlier run left in its directory.
func scorchHostPost(t *testing.T, exp string, run int) {
	t.Helper()

	if err := os.RemoveAll(scorchHostRunDir(exp, run)); err != nil {
		t.Fatal(err)
	}

	path := fmt.Sprintf("/experiments/%s/scorch/pipelines/%d", exp, run)

	if code, body := api(t, http.MethodPost, path, nil); code != http.StatusNoContent {
		t.Fatalf("POST %s: status %d, want 204: %s", path, code, body)
	}
}

// scorchHostWaitREST waits until a run started through phenix ui has written
// its run info and cleared its running flag, fails the test for every
// pipeline node that failed, and returns the pipeline.
func scorchHostWaitREST(t *testing.T, exp string, run int) []scorchHostNode {
	t.Helper()

	ok, detail := scorchHostPoll(scorchHostRunTimeout, time.Second, func() (bool, string) {
		matches, _ := filepath.Glob(scorchHostInfoPattern(exp, run))
		running, _ := scorchHostState(t, exp)

		return len(matches) == 1 && !running, fmt.Sprintf("run info %v, scorch running %t", matches, running)
	})
	if !ok {
		t.Fatalf("run %d through phenix ui did not finish: %s\nphenix ui log:\n%s", run, detail, scorchHostUILog(exp))
	}

	nodes := scorchHostPipeline(t, exp, run)

	for _, n := range nodes {
		if n.Status == "failure" {
			t.Errorf("run %d: pipeline node %s (%s) failed; phenix ui log:\n%s", run, n.Name, n.Stage, scorchHostUILog(exp))
		}
	}

	return nodes
}

// scorchHostState reports status.appRunningStageStatus.scorch and whether
// status.apps.scorch is set.
func scorchHostState(t *testing.T, exp string) (bool, bool) {
	t.Helper()

	running, has, err := scorchHostReadState(exp)
	if err != nil {
		t.Fatal(err)
	}

	return running, has
}

func scorchHostReadState(exp string) (bool, bool, error) {
	out, err := phenixRun("config", "get", "experiment/"+exp, "--output", "json")
	if err != nil {
		return false, false, fmt.Errorf("phenix config get experiment/%s: %w\n%s", exp, err, out)
	}

	var doc struct {
		Status struct {
			Running map[string]bool `json:"appRunningStageStatus"`
			Apps    map[string]any  `json:"apps"`
		} `json:"status"`
	}

	if err := json.Unmarshal([]byte(jsonPart(out)), &doc); err != nil {
		return false, false, fmt.Errorf("decoding experiment %s: %w\n%s", exp, err, out)
	}

	return doc.Status.Running["scorch"], doc.Status.Apps["scorch"] != nil, nil
}

// scorchHostUILog returns the last lines of phenix ui's log that name exp.
func scorchHostUILog(exp string) string {
	data, err := os.ReadFile(filepath.Join(cfg.workDir, "phenix-ui.log"))
	if err != nil {
		return err.Error()
	}

	var lines []string

	for l := range strings.SplitSeq(string(data), "\n") {
		if strings.Contains(l, exp) {
			lines = append(lines, l)
		}
	}

	if len(lines) > 40 {
		lines = lines[len(lines)-40:]
	}

	return strings.Join(lines, "\n")
}

// scorchHostPoll is eventually without failing: it reports whether cond
// held within timeout, and its last detail.
func scorchHostPoll(timeout, interval time.Duration, cond func() (bool, string)) (bool, string) {
	deadline := time.Now().Add(timeout)

	for {
		ok, detail := cond()
		if ok || time.Now().After(deadline) {
			return ok, detail
		}

		time.Sleep(interval)
	}
}

// ---------------------------------------------------------------------------
// run output

func scorchHostFilesDir(exp string) string {
	return filepath.Join(scorchHostPhenixBase, "images", exp, "files")
}

func scorchHostRunDir(exp string, run int) string {
	return filepath.Join(scorchHostFilesDir(exp), "scorch", fmt.Sprintf("run-%d", run))
}

// scorchHostCmpDir is a component's output directory in loop 0, count 0.
func scorchHostCmpDir(exp string, run int, cmp string) string {
	return filepath.Join(scorchHostRunDir(exp, run), cmp, "loop-0-count-0")
}

func scorchHostInfoPattern(exp string, run int) string {
	return filepath.Join(scorchHostRunDir(exp, run), fmt.Sprintf("%s-scorch-run-%d-*.json", exp, run))
}

// scorchHostArchivePattern matches the archives SCORCH writes of a run.
func scorchHostArchivePattern(exp string, run int) string {
	return filepath.Join(scorchHostFilesDir(exp), fmt.Sprintf("scorch-run-%d_*.tgz", run))
}

// scorchHostRunInfo names a run's info file with its modification time, or
// is empty when the run has none.
func scorchHostRunInfo(exp string, run int) string {
	path := scorchHostLatest(scorchHostInfoPattern(exp, run))
	if path == "" {
		return ""
	}

	fi, err := os.Stat(path)
	if err != nil {
		return ""
	}

	return path + "@" + fi.ModTime().String()
}

// scorchHostLatest returns the last file matching pattern, in name order
// (SCORCH names its files with sortable timestamps).
func scorchHostLatest(pattern string) string {
	matches, _ := filepath.Glob(pattern)
	if len(matches) == 0 {
		return ""
	}

	slices.Sort(matches)

	return matches[len(matches)-1]
}

// scorchHostCheckRun checks what every SCORCH run leaves: its run
// info, an archive of its directory holding the given entries, its status
// cleared, and the experiment's namespace still there. It returns the
// archive's files.
func scorchHostCheckRun(t *testing.T, exp string, run int, name string, entries ...string) map[string][]byte {
	t.Helper()

	infos, _ := filepath.Glob(scorchHostInfoPattern(exp, run))
	if len(infos) != 1 {
		t.Errorf("run %d has run info files %v, want one", run, infos)
	} else {
		var info struct {
			Experiment struct {
				Name string `json:"name"`
			} `json:"experiment"`
			Run struct {
				Name  string `json:"name"`
				Index int    `json:"index"`
			} `json:"run"`
			Start    string `json:"start"`
			End      string `json:"end"`
			Minimega string `json:"minimega_version"`
		}

		scorchHostReadJSON(t, infos[0], &info)

		if info.Experiment.Name != exp || info.Run.Index != run || info.Run.Name != name {
			t.Errorf("run info %s names experiment %q run %d %q, want %s run %d %q",
				infos[0], info.Experiment.Name, info.Run.Index, info.Run.Name, exp, run, name)
		}

		if mmVersion := mmText(t, "", "version"); info.Minimega != mmVersion {
			t.Errorf("run info minimega_version is %q, want minimega's %q", info.Minimega, mmVersion)
		}

		start, err1 := time.Parse(time.RFC3339, info.Start)
		end, err2 := time.Parse(time.RFC3339, info.End)

		if err1 != nil || err2 != nil || end.Before(start) {
			t.Errorf("run info start %q and end %q are not an RFC 3339 interval", info.Start, info.End)
		}

		entries = append(entries, filepath.Base(infos[0]))
	}

	var files map[string][]byte

	archive := scorchHostLatest(scorchHostArchivePattern(exp, run))
	if archive == "" {
		t.Errorf("run %d left no archive in %s", run, scorchHostFilesDir(exp))
	} else {
		files = scorchHostArchive(t, archive)

		for _, e := range entries {
			if _, ok := files[e]; !ok {
				t.Errorf("run %d's archive %s has no %s", run, archive, e)
			}
		}
	}

	if running, has := scorchHostState(t, exp); running || has {
		t.Errorf("after run %d, scorch running %t, status.apps.scorch present %t; want neither", run, running, has)
	}

	if _, ok := mmNamespaces(t)[exp]; !ok {
		t.Errorf("after run %d, minimega no longer has namespace %s", run, exp)
	}

	return files
}

// scorchHostArchive returns the files of a gzipped tar archive by name.
func scorchHostArchive(t *testing.T, path string) map[string][]byte {
	t.Helper()

	f, err := os.Open(path) //nolint:gosec // product output
	if err != nil {
		t.Errorf("opening archive: %v", err)

		return nil
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Errorf("%s is not gzipped: %v", path, err)

		return nil
	}

	var (
		tr    = tar.NewReader(gz)
		files = make(map[string][]byte)
	)

	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return files
		}

		if err != nil {
			t.Errorf("reading %s: %v", path, err)

			return files
		}

		data, err := io.ReadAll(tr)
		if err != nil {
			t.Errorf("reading %s from %s: %v", h.Name, path, err)

			return files
		}

		files[h.Name] = data
	}
}

// scorchHostStage is the status file a Python component writes per stage.
type scorchHostStage struct {
	Experiment string   `json:"experiment_name"`
	Run        int      `json:"scorch_run_index"`
	Component  string   `json:"component"`
	Stage      string   `json:"stage"`
	Return     any      `json:"return"`
	Logs       []string `json:"logs"`
}

// scorchHostReadStage decodes the newest status file for stage in a
// component's output directory.
func scorchHostReadStage(dir, stage string) (scorchHostStage, error) {
	var st scorchHostStage

	path := scorchHostLatest(filepath.Join(dir, stage+"-*.json"))
	if path == "" {
		return st, fmt.Errorf("no %s status file in %s", stage, dir)
	}

	data, err := os.ReadFile(path) //nolint:gosec // product output
	if err != nil {
		return st, err
	}

	if err := json.Unmarshal(data, &st); err != nil {
		return st, fmt.Errorf("decoding %s: %w", path, err)
	}

	return st, nil
}

// scorchHostWantStage checks that a Python component wrote a status file
// for stage that names it and the run, and does not report an error.
func scorchHostWantStage(t *testing.T, exp string, run int, cmp, stage string) scorchHostStage {
	t.Helper()

	st, err := scorchHostReadStage(scorchHostCmpDir(exp, run, cmp), stage)
	if err != nil {
		t.Errorf("component %s: %v", cmp, err)

		return st
	}

	if st.Experiment != exp || st.Run != run || st.Component != cmp || st.Stage != stage {
		t.Errorf("component %s's %s status file names %s run %d, %s, %s", cmp, stage, st.Experiment, st.Run, st.Component, st.Stage)
	}

	if ret := fmt.Sprint(st.Return); strings.HasPrefix(ret, "Error occurred") {
		t.Errorf("component %s's %s stage failed: %s\n%s", cmp, stage, ret, strings.Join(st.Logs, "\n"))
	}

	return st
}

// scorchHostWaitStage waits for a backgrounded component's start stage, ended
// by SIGTERM, to write its status file with the given log line.
func scorchHostWaitStage(t *testing.T, dir, stage, logLine string) {
	t.Helper()

	eventually(t, scorchHostSettle, 500*time.Millisecond, fmt.Sprintf("%s's %s status file", dir, stage), func() (bool, string) {
		st, err := scorchHostReadStage(dir, stage)
		if err != nil {
			return false, err.Error()
		}

		return slices.ContainsFunc(st.Logs, func(l string) bool { return strings.Contains(l, logLine) }),
			fmt.Sprintf("return %v, logs %v", st.Return, st.Logs)
	})
}

// scorchHostHostStat is one line of hoststats' host_stats.jsonl.
type scorchHostHostStat struct {
	ComputeName string   `json:"compute_name"`
	Load1       *float64 `json:"Load_1"`
	Load5       *float64 `json:"Load_5"`
	Load15      *float64 `json:"Load_15"`
	Timestamp   int64    `json:"timestamp"`
	VMList      []string `json:"vm_list"`
}

// scorchHostCheckHostStats checks that hoststats wrote at least minLines
// samples of this host with the experiment's three VMs.
func scorchHostCheckHostStats(t *testing.T, path, head string, minLines int) {
	t.Helper()

	var lines []string

	for l := range strings.SplitSeq(readFile(t, path), "\n") {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}

	if len(lines) < minLines {
		t.Errorf("%s has %d samples, want at least %d", path, len(lines), minLines)
	}

	for i, l := range lines {
		var s scorchHostHostStat

		if err := json.Unmarshal([]byte(l), &s); err != nil {
			t.Errorf("%s line %d does not parse: %v: %s", path, i+1, err, l)

			return
		}

		if s.ComputeName != head || s.Load1 == nil || s.Load5 == nil || s.Load15 == nil || s.Timestamp == 0 ||
			!slices.Equal(s.VMList, []string{"h1", "h2", "h3"}) {
			t.Errorf("%s line %d is %s, want host %s, its loads, a timestamp and VMs [h1 h2 h3]", path, i+1, l, head)

			return
		}
	}
}

// scorchHostStdinMeta returns the metadata of component cmp in the
// experiment a user component got on stdin, keeping numbers as [json.Number].
func scorchHostStdinMeta(t *testing.T, path, cmp string) map[string]any {
	t.Helper()

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

	dec := json.NewDecoder(strings.NewReader(readFile(t, path)))
	dec.UseNumber()

	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("%s is not JSON: %v", path, err)
	}

	for _, a := range doc.Spec.Scenario.Apps {
		for _, c := range a.Metadata.Components {
			if a.Name == "scorch" && c.Name == cmp {
				return c.Metadata
			}
		}
	}

	t.Errorf("%s has no scorch component %s", path, cmp)

	return nil
}

// scorchHostWantPcap checks that capinfos reads a file as a pcap.
func scorchHostWantPcap(t *testing.T, path string) {
	t.Helper()

	out, err := exec.Command("capinfos", "-T", "-M", path).Output() //nolint:gosec // test harness
	if err != nil {
		t.Errorf("capinfos %s: %v", path, err)

		return
	}

	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != 2 {
		t.Errorf("capinfos %s printed %q, want a header and a row", path, out)

		return
	}

	header, row := strings.Split(lines[0], "\t"), strings.Split(lines[1], "\t")

	if i := slices.Index(header, "File type"); i < 0 || i >= len(row) || !strings.Contains(row[i], "pcap") {
		t.Errorf("capinfos does not read %s as a pcap:\n%s", path, out)
	}
}

// scorchHostWantPcapMeta checks that pcap_metadata.json has a valid pcap
// entry for each name.
func scorchHostWantPcapMeta(t *testing.T, path string, names ...string) {
	t.Helper()

	// capinfos fields, as csv.DictReader read them: strings, or null for a
	// field the row lacks.
	var meta map[string]map[string]any

	scorchHostReadJSON(t, path, &meta)

	for _, n := range names {
		fileType, _ := meta[n]["File type"].(string)
		if !strings.Contains(fileType, "pcap") {
			t.Errorf("%s has no pcap entry for %s: %v", path, n, meta[n])
		}
	}
}

// scorchHostSnapLen reads the snapshot length in a classic pcap file's
// header; minimega writes the namespace's snaplen there, or 1600 for 0.
func scorchHostSnapLen(t *testing.T, path string) uint32 {
	t.Helper()

	f, err := os.Open(path) //nolint:gosec // test harness
	if err != nil {
		t.Errorf("opening %s: %v", path, err)

		return 0
	}

	defer f.Close()

	hdr := make([]byte, 24)
	if _, err := io.ReadFull(f, hdr); err != nil {
		t.Errorf("reading the pcap header of %s: %v", path, err)

		return 0
	}

	// The magic is 0xa1b2c3d4 (microseconds) or 0xa1b23c4d (nanoseconds)
	// in the writer's byte order.
	switch binary.LittleEndian.Uint32(hdr) {
	case 0xa1b2c3d4, 0xa1b23c4d:
		return binary.LittleEndian.Uint32(hdr[16:])
	case 0xd4c3b2a1, 0x4d3cb2a1:
		return binary.BigEndian.Uint32(hdr[16:])
	}

	t.Errorf("%s is not a classic pcap file: its magic is % x", path, hdr[:4])

	return 0
}

// scorchHostPackets counts the packets of a capture that match a Wireshark
// display filter.
func scorchHostPackets(t *testing.T, path, filter string) int {
	t.Helper()

	cmd := exec.Command("tshark", "-n", "-r", path, "-Y", filter, "-T", "fields", "-e", "frame.number") //nolint:gosec // test harness

	out, err := cmd.Output()
	if err != nil {
		t.Errorf("tshark -r %s -Y %s: %v", path, filter, err)

		return -1
	}

	return len(strings.Fields(string(out)))
}

func scorchHostReadJSON(t *testing.T, path string, v any) {
	t.Helper()

	if err := json.Unmarshal([]byte(readFile(t, path)), v); err != nil {
		t.Fatalf("decoding %s: %v", path, err)
	}
}

// ---------------------------------------------------------------------------
// minimega, host and Open vSwitch

// scorchHostList splits a minimega list column such as "[a, b]".
func scorchHostList(s string) []string {
	s = strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(s), "["), "]")
	if strings.TrimSpace(s) == "" {
		return nil
	}

	parts := strings.Split(s, ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}

	return parts
}

// scorchHostFirst is the first element of a minimega list column.
func scorchHostFirst(s string) string {
	if l := scorchHostList(s); len(l) > 0 {
		return l[0]
	}

	return ""
}

// scorchHostVLAN looks a VLAN alias up in status.vlans, ignoring case.
func scorchHostVLAN(vlans map[string]int, alias string) int {
	for k, v := range vlans {
		if strings.EqualFold(k, alias) {
			return v
		}
	}

	return 0
}

// scorchHostPipes lists the namespace's minimega pipes.
func scorchHostPipes(t *testing.T, exp string) []map[string]string {
	t.Helper()

	return mmTable(t, exp, "pipe")
}

func scorchHostHasCapture(rows []map[string]string, iface string) bool {
	return slices.ContainsFunc(rows, func(row map[string]string) bool { return row["interface"] == iface })
}

// scorchHostQdisc returns the tc qdiscs of a host interface.
func scorchHostQdisc(t *testing.T, dev string) string {
	t.Helper()

	if dev == "" {
		t.Error("no tap to read the qdisc of")

		return ""
	}

	out, err := hostCmd("tc", "qdisc", "show", "dev", dev)
	if err != nil {
		t.Errorf("tc qdisc show dev %s: %v: %s", dev, err, out)
	}

	return out
}

// scorchHostTaps lists the SCORCH tap component's taps on the host: their
// network namespaces and Open vSwitch ports.
func scorchHostTaps() map[string]bool {
	taps := make(map[string]bool)

	if entries, err := os.ReadDir(scorchHostNetNSDir); err == nil {
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), scorchHostTapSuffix) {
				taps[e.Name()] = true
			}
		}
	}

	if ports, err := hostCmd("ovs-vsctl", "list-ports", scorchHostBridge); err == nil {
		for p := range strings.FieldsSeq(ports) {
			if strings.HasSuffix(p, scorchHostTapSuffix) {
				taps[p] = true
			}
		}
	}

	return taps
}

func scorchHostNewTaps(before map[string]bool) []string {
	var taps []string

	for tap := range scorchHostTaps() {
		if !before[tap] {
			taps = append(taps, tap)
		}
	}

	slices.Sort(taps)

	return taps
}

// scorchHostWaitTap waits for a running SCORCH run's tap component to
// create its tap and move it, with its address, into its network namespace,
// and returns the tap's name (its namespace's too). The OVS port appears
// before the namespace and the address do.
func scorchHostWaitTap(t *testing.T, proc *phenixProc, before map[string]bool) string {
	t.Helper()

	var tap string

	eventually(t, time.Minute, 250*time.Millisecond, "the SCORCH tap component's host tap", func() (bool, string) {
		if taps := scorchHostNewTaps(before); len(taps) == 1 {
			addr, err := hostCmd("ip", "-n", taps[0], "-4", "-o", "addr", "show", "dev", taps[0])
			if err == nil && strings.Contains(addr, "inet ") {
				tap = taps[0]

				return true, ""
			}

			return proc.finished(), fmt.Sprintf("tap %s has no address in its namespace yet: %v %s", taps[0], err, addr)
		}

		return proc.finished(), "no new " + scorchHostTapSuffix + " tap"
	})

	if tap == "" {
		out, err := proc.wait(t, time.Minute)
		t.Fatalf("the SCORCH run finished before its tap appeared (%v):\n%s", err, out)
	}

	return tap
}

// scorchHostTapsLeft waits up to timeout for the taps not in before to go,
// and returns those still there.
func scorchHostTapsLeft(before map[string]bool, timeout time.Duration) []string {
	var left []string

	scorchHostPoll(timeout, 500*time.Millisecond, func() (bool, string) {
		left = scorchHostNewTaps(before)

		return len(left) == 0, ""
	})

	return left
}

// scorchHostDropTaps removes taps a SCORCH run leaked, while the
// experiment's namespace still exists.
func scorchHostDropTaps(t *testing.T, exp string, taps []string) {
	t.Helper()

	for _, tap := range taps {
		if _, err := mmRun(exp, "tap delete "+tap); err != nil {
			t.Logf("removing leaked tap %s: %v", tap, err)
		}

		_, _ = hostCmd("ip", "netns", "delete", tap)
		_, _ = hostCmd("ovs-vsctl", "--if-exists", "del-port", scorchHostBridge, tap)
	}
}

// scorchHostCleanTaps removes, when the test ends, every SCORCH tap the
// test left on the host. Register it before the experiment, so it runs after
// the experiment's teardown (it does not touch minimega, whose namespace is
// gone by then).
func scorchHostCleanTaps(t *testing.T) {
	t.Helper()

	before := scorchHostTaps()

	t.Cleanup(func() {
		for _, tap := range scorchHostNewTaps(before) {
			t.Logf("cleanup: removing host tap %s left behind", tap)

			_, _ = hostCmd("ip", "netns", "delete", tap)
			_, _ = hostCmd("ovs-vsctl", "--if-exists", "del-port", scorchHostBridge, tap)
		}
	})
}

// scorchHostKeepFiles removes, when the test ends, the files directory
// SCORCH filled for an experiment, after copying it into the work directory
// (which the workflow keeps as logs) if the test failed. Register it before
// the experiment, so it runs after the experiment's teardown.
func scorchHostKeepFiles(t *testing.T, exp string) {
	t.Helper()

	t.Cleanup(func() {
		dir := scorchHostFilesDir(exp)

		if t.Failed() {
			dst := filepath.Join(cfg.workDir, t.Name(), exp+"-files")
			_ = os.MkdirAll(filepath.Dir(dst), 0o750)

			if out, err := hostCmd("cp", "-a", dir, dst); err != nil {
				t.Logf("keeping %s: %v: %s", dir, err, out)
			} else {
				t.Logf("kept %s in %s", dir, dst)
			}
		}

		_ = os.RemoveAll(dir)
	})
}

// scorchHostOVSPorts lists the ports of the experiment bridge.
func scorchHostOVSPorts(t *testing.T) []string {
	t.Helper()

	out, err := hostCmd("ovs-vsctl", "list-ports", scorchHostBridge)
	if err != nil {
		t.Fatalf("ovs-vsctl list-ports %s: %v: %s", scorchHostBridge, err, out)
	}

	return strings.Fields(out)
}

// scorchHostMirror is one row of Open vSwitch's Mirror table.
type scorchHostMirror struct {
	selectAll  bool
	selectVLAN []int
}

// scorchHostMirrors returns Open vSwitch's mirrors by name.
func scorchHostMirrors(t *testing.T) map[string]scorchHostMirror {
	t.Helper()

	out, err := hostCmd("ovs-vsctl", "--format=json", "--columns=name,select_all,select_vlan", "list", "Mirror")
	if err != nil {
		t.Fatalf("ovs-vsctl list Mirror: %v: %s", err, out)
	}

	var table struct {
		Data     [][]any  `json:"data"`
		Headings []string `json:"headings"`
	}

	if err := json.Unmarshal([]byte(out), &table); err != nil {
		t.Fatalf("decoding ovs-vsctl list Mirror: %v\n%s", err, out)
	}

	mirrors := make(map[string]scorchHostMirror, len(table.Data))

	for _, row := range table.Data {
		var (
			name string
			m    scorchHostMirror
		)

		for i, h := range table.Headings {
			if i >= len(row) {
				continue
			}

			switch h {
			case "name":
				name, _ = row[i].(string)
			case "select_all":
				m.selectAll, _ = row[i].(bool)
			case "select_vlan":
				m.selectVLAN = scorchHostOVSInts(row[i])
			}
		}

		mirrors[name] = m
	}

	return mirrors
}

// scorchHostOVSInts flattens an OVSDB JSON integer or set of integers.
func scorchHostOVSInts(v any) []int {
	switch x := v.(type) {
	case float64:
		return []int{int(x)}
	case []any:
		if len(x) != 2 || x[0] != "set" {
			return nil
		}

		elems, _ := x[1].([]any)
		ints := make([]int, 0, len(elems))

		for _, e := range elems {
			if f, ok := e.(float64); ok {
				ints = append(ints, int(f))
			}
		}

		slices.Sort(ints)

		return ints
	}

	return nil
}
