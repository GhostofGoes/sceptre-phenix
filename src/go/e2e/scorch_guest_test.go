//go:build e2e

// End-to-end tests for the sceptre-phenix-apps SCORCH components that act in
// real guests (shard scorch-guest): the cc component's exec, background, send,
// recv and reset against miniccc; pcap, mm and tcpdump capturing real traffic;
// qos as the guests see it; the iperf, vmstats, hoststats, disruption and
// collector pipeline; the cc filters components leave behind; and ending a run
// early over REST, with SIGTERM and by stopping the experiment. Three tools
// guests (cfg.tools) boot once and every SCORCH run of one scenario runs on
// them in turn. Each check reads what the run leaves where it lands: the
// component output and status files, the run record and archive, minimega's
// capture, qos, cc and tag state, tc on the host taps, and the guests over C2.

package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	// scorchGuestName names the experiment, its topology and its scenario.
	scorchGuestName = "e2e-scg"

	// scorchGuestPhenixBase is phenix's base directory, PHENIX_DIR for the
	// components; the cc component resolves relative send paths against it.
	scorchGuestPhenixBase = "/phenix"

	// scorchGuestFixtures is the fixture directory under the phenix base
	// directory; cc1's send step names it relative to that directory.
	scorchGuestFixtures = "e2e-scorch-guest"

	// scorchGuestHello is the content of the file cc1 sends to g1.
	scorchGuestHello = "e2e-hello"

	// scorchGuestServer is g1's address: the ping target and iperf server.
	scorchGuestServer = "10.10.0.1"

	// scorchGuestSendGrace bounds each cc file send of the Python components
	// (PHENIX_CC_SEND_GRACE, 300 s by default), so a send that never lands
	// fails its step within a minute.
	scorchGuestSendGrace = "60"

	// scorchGuestRunTimeout bounds a short SCORCH run, and
	// scorchGuestPipelineTimeout the collection pipeline of run 3.
	scorchGuestRunTimeout      = 5 * time.Minute
	scorchGuestPipelineTimeout = 10 * time.Minute

	// scorchGuestStopTimeout bounds how long a canceled run takes to run its
	// stop and cleanup stages and exit.
	scorchGuestStopTimeout = 3 * time.Minute

	// scorchGuestDisruption is dis1's run_duration in seconds; the collector
	// records it as the experiment duration.
	scorchGuestDisruption = 30.0
)

// The scenario's SCORCH runs, by index.
const (
	scorchGuestRunCC = iota
	scorchGuestRunCapture
	scorchGuestRunNetem
	scorchGuestRunPipeline
	scorchGuestRunCancel
)

// Lower and upper bounds the checks hold the components' results to.
const (
	// g2 pings g1 five times a second through all of run 1, whose captures
	// span at least its 20 s pause, and g1's tap sees requests and replies.
	scorchGuestMinICMP = 40

	// tcpdump starts in g2 a few seconds into run 1 and runs through the
	// pause.
	scorchGuestMinDumpICMP = 20

	// g3 sends g1 a UDP datagram twice a second through all of run 1.
	scorchGuestMinUDP = 5

	// qos1 delays everything g1's tap sends g1 by 200 ms, so every echo
	// request takes at least that long; without qos the LAN is local.
	scorchGuestMinDelayedRTT = 195.0
	scorchGuestMaxPlainRTT   = 50.0

	// ping45 pings g1 from g2 once a second for 45 s inside pcap3's capture.
	scorchGuestMinPings = 20

	// vmstat samples once a second from vm1's start through iperf's stop,
	// well over a minute; hoststats polls every 2 s for as long.
	scorchGuestMinVMStats   = 25
	scorchGuestMinHostStats = 10

	// iperf reports one interval a second for its 10 s run.
	scorchGuestMinRTTLines = 8

	// scorchGuestStaleFilter is the capture filter left before the pipeline
	// run; no packet of the run matches it.
	scorchGuestStaleFilter = "host 192.0.2.99"

	// scorchGuestMMCapSnapLen is mmcap's snaplen in the scenario; g3's
	// datagrams to g1 are shorter.
	scorchGuestMMCapSnapLen = 96
)

// scorchGuestScenario is the scenario the shard runs. Run 0 drives the cc
// component; run 1 captures the traffic the test sends with an unfiltered mm
// bridge capture, then with pcap, mm and tcpdump, each filtered; run 2 delays
// g1 with qos; run 3 is the HARMONIE-style collection pipeline, with no loop
// (the collector must cope with the null loop phenix stores) and with qos4
// and pcap4 defined after the qos and pcap components it uses (the collector
// must collect from the run's own); run 4 sets up qos and a capture and
// pauses long enough to be canceled.
const scorchGuestScenario = `apiVersion: phenix.sandia.gov/v2
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
      - name: cc1
        type: cc
        metadata:
          configure:
          - type: exec
            filter: hostname=g1
            once: true
            args: touch /tmp/e2e-cc-top
          cleanup:
          - type: reset
          vms:
          - hostname: g1
            start:
            - type: send
              args: {{fixtures}}/hello.txt:/tmp/hello.txt
            - type: recv
              args: /etc/hostname
            - type: exec
              args: hostname
              wait: true
              validator: grep -qx g1
            - type: background
              args: sleep 600
      - name: pcap1
        type: pcap
        metadata:
          filter: icmp or arp
          create_merged_pcap: true
          convertToJSON: true
          vms:
          - hostname: g1
            interface: 0
          - hostname: g2
            interface: eth0
      - name: mmplain
        type: mm
        metadata:
          start:
          - type: start_capture
            capture:
              bridge: phenix
              filename: bridge.pcap
          stop:
          - type: stop_capture
            capture:
              bridge: phenix
      - name: mmcap
        type: mm
        metadata:
          vms:
          - hostname: g3
            start:
            - type: start_capture
              capture:
                interface: 0
                filename: g3.pcap
                filter: udp
                snaplen: 96
            stop:
            - type: stop_capture
              capture:
                convert: true
      - name: tcp1
        type: tcpdump
        metadata:
          vms:
          - hostname: g2
            iface: eth0
            filter: icmp
      - name: wait20
        type: pause
        metadata:
          duration: 20s
      - name: qos1
        type: qos
        metadata:
          vms:
          - hostname: g1
            interface: eth0
            delay: 200ms
      - name: wait30
        type: pause
        metadata:
          duration: 30s
      - name: vm1
        type: vmstats
        metadata:
          pollPeriod: 1
      - name: hs1
        type: hoststats
        background: true
        metadata:
          pollPeriod: 2
      - name: pcap3
        type: pcap
        metadata:
          create_merged_pcap: true
          vms:
          - hostname: g1
            interface: 0
          - hostname: g2
            interface: 0
      - name: qos3
        type: qos
        metadata:
          vms:
          - hostname: g2
            interface: 0
            delay: 10ms
      - name: ping45
        type: cc
        metadata:
          start:
          - type: exec
            filter: hostname=g2
            once: true
            args: "bash -c 'ping -c 45 -i 1 10.10.0.1 >/dev/null 2>&1 &'"
      - name: ip1
        type: iperf
        metadata:
          run_duration: 10
          bandwidth: 2000000
          iperf_paths:
            linux: /usr/bin/iperf3
            windows: "C:/iperf3/iperf3.exe"
          servers:
          - hostname: g1
            port_range_start: 5201
            clients:
            - hostname: g2
            - hostname: g3
      - name: dis1
        type: disruption
        metadata:
          current_disruption: baseline
          run_duration: 30
          permutation: 0
      - name: col1
        type: collector
        metadata:
          collect_iperf: true
          collect_miniccc: false
          generate_csv: false
      - name: qos4
        type: qos
        metadata:
          vms:
          - hostname: g1
            interface: 0
            delay: 100ms
      - name: pcap4
        type: pcap
        metadata:
          vms:
          - hostname: g1
            interface: 0
      - name: wait120
        type: pause
        metadata:
          duration: 120s
      runs:
      - name: cc
        configure: [cc1]
        start: [cc1]
        cleanup: [cc1]
      - name: capture
        start: [mmplain, pcap1, mmcap, tcp1, wait20]
        stop: [tcp1, mmcap, pcap1, mmplain]
      - name: netem
        start: [qos1, wait30]
        stop: [qos1]
      - name: pipeline
        configure: [ip1, dis1]
        start: [vm1, hs1, pcap3, qos3, ping45, ip1, dis1]
        stop: [ip1, vm1, qos3, pcap3, hs1, col1]
        cleanup: [ip1, dis1]
      - name: cancel
        start: [qos4, pcap4, wait120]
        stop: [pcap4, qos4]
`

// TestScorchGuestComponents boots three tools guests once and runs every
// SCORCH run of the scenario on them (SCG-01..SCG-07). SCG-07 stops the
// experiment, so it runs last.
//
// The scorch-host shard checks, on hollow VMs, the cc filter a stage-level cc
// exec leaves behind; this shard checks tcpdump's, which needs a guest.
func TestScorchGuestComponents(t *testing.T) {
	scorchGuestRequire(t)
	requireImage(t, cfg.tools)

	// Components inherit phenix's environment (util/shell/exec.go): the
	// collector runs `phenix config get` itself and must read this test's
	// store, and a cc file send that never lands should fail in a minute.
	t.Setenv("PHENIX_STORE_ENDPOINT", cfg.store)
	t.Setenv("PHENIX_CC_SEND_GRACE", scorchGuestSendGrace)

	scorchGuestPrepare(t)

	exp := newExperiment(t, scorchGuestName, scorchGuestTopology(), scorchGuestScenarioYAML())

	began := time.Now()

	exp.start()
	waitCCClients(t, scorchGuestName, scorchGuestVMs()...)
	t.Logf("the three guests registered with C2 %v after start", time.Since(began).Round(time.Second))

	t.Run("SCG-01 cc component exec, background, send, recv and reset", scorchGuestCheckCC)
	t.Run("SCG-02 pcap, mm and tcpdump capture real traffic", scorchGuestCheckCapture)
	t.Run("SCG-03 qos delay shows in the guests' round trips", scorchGuestCheckNetem)
	t.Run("SCG-04 iperf, vmstats, hoststats, disruption and collector pipeline", scorchGuestCheckPipeline)
	t.Run("SCG-05 a REST cancel still runs stop", scorchGuestCheckRESTCancel)
	t.Run("SCG-06 SIGTERM to the CLI cancels the run and still runs stop", scorchGuestCheckCLICancel)
	t.Run("SCG-07 stopping the experiment ends a REST run", scorchGuestCheckExperimentStop)
}

// ---------------------------------------------------------------------------
// SCG-01: the cc component

func scorchGuestCheckCC(t *testing.T) {
	const run = scorchGuestRunCC

	scorchGuestReset(t)

	// The per-VM exec and background steps wait on `cc commands` rows, which
	// they read by header.
	if out, err := scorchGuestRun(run); err != nil {
		t.Errorf("SCORCH run %d (cc): %v\n%s", run, err, out)
	}

	// The cleanup stage's reset clears the filter the configure exec set.
	scorchGuestWantNoCCFilter(t, run)

	scorchGuestWantStage(t, run, "cc1", "configure")
	start := scorchGuestWantStage(t, run, "cc1", "start")
	scorchGuestWantStage(t, run, "cc1", "cleanup")

	for _, msg := range []string{"results are valid", "command 'sleep 600' backgrounded in VM g1"} {
		if !start.logged(msg) {
			t.Errorf("cc1's start stage did not log %q: %v", msg, start.Logs)
		}
	}

	if got := strings.TrimSpace(scorchRead(t, filepath.Join(scorchGuestDir(run, "cc1"), "hostname"))); got != "g1" {
		t.Errorf("cc1 received g1's /etc/hostname as %q, want g1", got)
	}

	// The cleanup stage's reset deletes every command. Responses still in
	// flight from the guests may land afterwards, so only list them.
	if rows := mmTable(t, scorchGuestName, "cc commands"); len(rows) != 0 {
		t.Errorf("cc commands after cc1's reset: %v, want none", rows)
	}

	if entries, err := os.ReadDir(filepath.Join(cfg.imagesDir, scorchGuestName, "miniccc_responses")); err == nil {
		t.Logf("%d C2 responses on disk after cc1's reset", len(entries))
	}

	scorchGuestWantRun(t, run, "cc", "cc1/loop-0-count-0/hostname", "cc1/loop-0-count-0/start-")

	// The check also ends the background step's sleep, which would otherwise
	// outlive this check by ten minutes.
	g1 := scorchShell(t, scorchGuestName, "g1", "echo hello=$(cat /tmp/hello.txt); "+
		"test -e /tmp/e2e-cc-top && echo top=yes || echo top=no; "+
		"pgrep -fx 'sleep 600' >/dev/null && echo bg=yes || echo bg=no; pkill -fx 'sleep 600'; echo done=yes")

	want := map[string]string{"hello": scorchGuestHello, "top": "yes", "bg": "yes"}
	for k, v := range want {
		if g1[k] != v {
			t.Errorf("g1 after run %d: %s=%q, want %q (sent file, filtered top-level exec, background step): %v", run, k, g1[k], v, g1)
		}
	}

	if g2 := scorchShell(t, scorchGuestName, "g2", "test -e /tmp/e2e-cc-top && echo top=yes || echo top=no"); g2["top"] != "no" {
		t.Errorf("cc1's configure exec with filter hostname=g1 also ran in g2: %v", g2)
	}
}

// ---------------------------------------------------------------------------
// SCG-02: packet captures of real traffic

func scorchGuestCheckCapture(t *testing.T) {
	const run = scorchGuestRunCapture

	scorchGuestReset(t)

	// mmplain's bridge capture, the run's first, sets no filter, so it holds
	// the run's traffic only if mm clears this one.
	scorchGuestLeaveFilter(t, run)

	// The traffic starts before the run and outlasts its captures, so no
	// harness cc command races the cc filters the components set: g2 pings
	// g1 (ICMP, which pcap1's filter keeps) and g3 sends g1 UDP (which it
	// drops on g1's tap, while mmcap's udp-filtered capture of g3 keeps it).
	scorchShell(t, scorchGuestName, "g2", "timeout 90 ping -q -i 0.2 "+scorchGuestServer+" >/dev/null 2>&1 & echo started=yes")
	scorchShell(t, scorchGuestName, "g3", "timeout 90 bash -c 'while true; do echo x >/dev/udp/"+scorchGuestServer+
		"/9999; sleep 0.5; done' >/dev/null 2>&1 & echo started=yes")

	// The checks below stop the traffic; if one ends this check first, stop it
	// here so it does not reach the next check's guests.
	trafficStopped := false

	t.Cleanup(func() {
		if !trafficStopped {
			for _, vm := range []string{"g2", "g3"} {
				scorchShell(t, scorchGuestName, vm, "pkill -x timeout; echo stopped=yes")
			}
		}
	})

	var (
		pcapDir   = scorchGuestDir(run, "pcap1")
		mmDir     = scorchGuestDir(run, "mmcap")
		bridgeDir = scorchGuestDir(run, "mmplain")
		tcpDir    = scorchGuestDir(run, "tcp1")
		want      = map[string]string{
			"pcap phenix": filepath.Join(bridgeDir, "bridge.pcap"),
			"g1:0":        filepath.Join(pcapDir, "g1-0.pcap"),
			"g2:0":        filepath.Join(pcapDir, "g2-0.pcap"),
			"g3:0":        filepath.Join(mmDir, "g3.pcap"),
		}
		caps map[string]string
	)

	proc := scorchGuestStart(t, run)

	eventually(t, 2*time.Minute, time.Second, "run 1's bridge capture and three VM captures", func() (bool, string) {
		caps = scorchGuestCaptures(t)

		return len(caps) == len(want) || proc.finished(), fmt.Sprintf("captures %v", caps)
	})

	if !reflect.DeepEqual(caps, want) {
		t.Errorf("minimega captures during run %d = %v, want %v", run, caps, want)
	}

	// mmcap sets the namespace's filter last, after pcap1 cleared its own, so a
	// filter here is mm's 'udp' left behind once its capture runs. mm clears it
	// just after the capture appears, so allow it a moment. SCH-08 shows pcap
	// clearing its own filter.
	if ok, f := scorchHostPoll(3*time.Second, 250*time.Millisecond, func() (bool, string) {
		got := strings.TrimSpace(mmText(t, scorchGuestName, "capture pcap filter"))

		return got == "", got
	}); !ok {
		t.Errorf("the namespace's capture filter is %q with every capture started, want it cleared", f)
	}

	// mmcap's capture also sets the namespace's snaplen; mm resets it to
	// minimega's default 0 with the filter.
	if ok, s := scorchHostPoll(3*time.Second, 250*time.Millisecond, func() (bool, string) {
		got := strings.TrimSpace(mmText(t, scorchGuestName, "capture pcap snaplen"))

		return got == "0", got
	}); !ok {
		t.Errorf("the namespace's capture snaplen is %q with every capture started, want 0 (the default)", s)
	}

	if out, err := proc.wait(t, scorchGuestRunTimeout); err != nil {
		t.Errorf("SCORCH run %d (capture): %v\n%s", run, err, out)
	}

	// Checked before any harness cc command, which sets and clears its own.
	scorchGuestWantNoCCFilter(t, run)

	scorchGuestWantStages(t, run, "mmplain/start", "mmplain/stop", "pcap1/start", "pcap1/stop", "mmcap/start", "mmcap/stop",
		"tcp1/start", "tcp1/stop")

	if caps := scorchGuestCaptures(t); len(caps) != 0 {
		t.Errorf("minimega captures after run %d: %v, want none", run, caps)
	}

	if f := strings.TrimSpace(mmText(t, scorchGuestName, "capture pcap filter")); f != "" {
		t.Errorf("the namespace's capture filter after run %d is %q, want it cleared", run, f)
	}

	if s := strings.TrimSpace(mmText(t, scorchGuestName, "capture pcap snaplen")); s != "0" {
		t.Errorf("the namespace's capture snaplen after run %d is %q, want 0 (the default)", run, s)
	}

	// The bridge mirror sees g2's pings to g1 on their way; the leftover
	// filter would have dropped every one.
	bridgePcap := filepath.Join(bridgeDir, "bridge.pcap")
	if n := scorchGuestPackets(t, bridgePcap, "icmp"); n < scorchGuestMinICMP {
		t.Errorf("%s holds %d ICMP packets, want at least %d from g2's pings, which mmplain's capture keeps only if it"+
			" cleared the leftover filter %q", bridgePcap, n, scorchGuestMinICMP, scorchGuestStaleFilter)
	}

	scorchGuestCheckPcap1(t, pcapDir)
	scorchGuestCheckMMCap(t, filepath.Join(mmDir, "g3.pcap"))
	scorchGuestCheckTCPDump(t, filepath.Join(tcpDir, "g2-eth0.pcap"))

	g2 := scorchShell(
		t,
		scorchGuestName,
		"g2",
		"test -e /dump-eth0.pcap && echo dump=yes || echo dump=no; pkill -x timeout; echo stopped=yes",
	)
	if g2["dump"] != "no" {
		t.Errorf("tcpdump's capture /dump-eth0.pcap is still in g2 after tcp1 stopped: %v", g2)
	}

	scorchShell(t, scorchGuestName, "g3", "pkill -x timeout; echo stopped=yes")

	trafficStopped = true

	scorchGuestWantRun(t, run, "capture", "mmplain/loop-0-count-0/bridge.pcap",
		"pcap1/loop-0-count-0/merged.pcap", "mmcap/loop-0-count-0/g3.pcap", "tcp1/loop-0-count-0/g2-eth0.pcap")
}

// scorchGuestCheckPcap1 checks pcap1's filtered captures of g1 and g2, their
// merge, their JSON conversion and their metadata.
func scorchGuestCheckPcap1(t *testing.T, dir string) {
	t.Helper()

	var (
		g1     = filepath.Join(dir, "raw", "g1-0.pcap")
		g2     = filepath.Join(dir, "raw", "g2-0.pcap")
		merged = filepath.Join(dir, "merged.pcap")
	)

	if n := scorchGuestPackets(t, g1, "icmp"); n < scorchGuestMinICMP {
		t.Errorf("%s holds %d ICMP packets, want at least %d from g2's pings", g1, n, scorchGuestMinICMP)
	}

	// g3's UDP reaches g1's tap throughout; pcap1's own filter drops it.
	for _, f := range []string{g1, g2, merged} {
		if n := scorchGuestPackets(t, f, "not (icmp or arp)"); n != 0 {
			t.Errorf("%s holds %d packets its 'icmp or arp' filter should have dropped", f, n)
		}
	}

	for _, f := range []string{"raw/g1-0.jsonl", "raw/g2-0.jsonl", "merged.jsonl"} {
		scorchWantFile(t, filepath.Join(dir, f))
	}

	scorchGuestWantPcapMetadata(t, dir, "g1-0.pcap", "g2-0.pcap", "merged.pcap")
}

// scorchGuestCheckMMCap checks the mm component's capture of g3 and its JSON
// conversion.
func scorchGuestCheckMMCap(t *testing.T, file string) {
	t.Helper()

	// mm sets its own capture's 'udp' filter. ICMP errors quote the datagrams
	// they answer, so count only real UDP; g1's ICMP errors and the ARP on g3's
	// tap are what the filter drops.
	if n := scorchGuestPackets(t, file, "udp.dstport == 9999 && !icmp"); n < scorchGuestMinUDP {
		t.Errorf("%s holds %d of g3's UDP datagrams to g1, want at least %d", file, n, scorchGuestMinUDP)
	}

	if n := scorchGuestPackets(t, file, "!udp || icmp"); n != 0 {
		t.Errorf("%s holds %d packets mmcap's 'udp' filter should have dropped", file, n)
	}

	if n := scorchHostSnapLen(t, file); n != scorchGuestMMCapSnapLen {
		t.Errorf("%s has snaplen %d in its header, want mmcap's %d", file, n, scorchGuestMMCapSnapLen)
	}

	scorchWantFile(t, file+".jsonl")
}

// scorchGuestCheckTCPDump checks the capture the tcpdump component took inside
// g2 and fetched.
func scorchGuestCheckTCPDump(t *testing.T, file string) {
	t.Helper()

	if n := scorchGuestPackets(t, file, "icmp"); n < scorchGuestMinDumpICMP {
		t.Errorf("%s holds %d ICMP packets, want at least %d", file, n, scorchGuestMinDumpICMP)
	}

	if n := scorchGuestPackets(t, file, "not icmp"); n != 0 {
		t.Errorf("%s holds %d packets tcpdump's icmp filter should have dropped", file, n)
	}
}

// ---------------------------------------------------------------------------
// SCG-03: qos as the guests see it

func scorchGuestCheckNetem(t *testing.T) {
	const run = scorchGuestRunNetem

	scorchGuestReset(t)

	proc := scorchGuestStart(t, run)

	eventually(t, 2*time.Minute, 500*time.Millisecond, "qos1 to delay g1's interface", func() (bool, string) {
		q := scorchGuestQoS(t, "g1")

		return strings.Contains(q, "delay 200ms") || proc.finished(), "qos " + q
	})

	if proc.finished() {
		out, err := proc.wait(t, time.Second)
		t.Fatalf("SCORCH run %d ended before qos1 delayed g1 (%v):\n%s", run, err, out)
	}

	if qd := scorchGuestQdisc(t, "g1"); !strings.Contains(qd, "netem") || !strings.Contains(qd, "delay 200") {
		t.Errorf("g1's tap qdisc while qos1 applies: %q, want netem delay 200ms", qd)
	}

	if lo, _ := scorchGuestRTT(t); lo < scorchGuestMinDelayedRTT {
		t.Errorf("g2 -> g1 minimum round trip %.1f ms while qos1 delays g1, want at least %.0f ms", lo, scorchGuestMinDelayedRTT)
	}

	if out, err := proc.wait(t, scorchGuestRunTimeout); err != nil {
		t.Errorf("SCORCH run %d (netem): %v\n%s", run, err, out)
	}

	scorchGuestWantStages(t, run, "qos1/start", "qos1/stop")

	if q := scorchGuestQoS(t, "g1"); strings.Contains(q, "delay") {
		t.Errorf("g1's qos after qos1 stopped: %q, want none", q)
	}

	if qd := scorchGuestQdisc(t, "g1"); strings.Contains(qd, "netem") {
		t.Errorf("g1's tap qdisc after qos1 stopped: %q, want no netem", qd)
	}

	if _, hi := scorchGuestRTT(t); hi >= scorchGuestMaxPlainRTT {
		t.Errorf("g2 -> g1 maximum round trip %.1f ms after qos1 stopped, want under %.0f ms", hi, scorchGuestMaxPlainRTT)
	}

	scorchGuestWantQoSApplied(t, run, "qos1", "g1", "200ms")
	scorchGuestWantRun(t, run, "netem", "qos1/loop-0-count-0/qos_values_applied.json")
}

// ---------------------------------------------------------------------------
// SCG-04: the HARMONIE-style collection pipeline

func scorchGuestCheckPipeline(t *testing.T) {
	const run = scorchGuestRunPipeline

	scorchGuestReset(t)

	// pcap3 sets no filter, so its captures hold the run's traffic only if
	// it clears this one.
	scorchGuestLeaveFilter(t, run)

	proc := scorchGuestStart(t, run)

	// iperf tags the guests in configure and untags them in cleanup.
	sawTags := false

	eventually(t, scorchGuestPipelineTimeout, 2*time.Second, "the iperf tag on every guest during run 3", func() (bool, string) {
		info := vmInfo(t, scorchGuestName)
		tagged := 0

		for _, vm := range scorchGuestVMs() {
			if scorchGuestTags(t, info[vm])["iperf"] == "1" {
				tagged++
			}
		}

		sawTags = sawTags || tagged == len(scorchGuestVMs())

		return sawTags || proc.finished(), fmt.Sprintf("%d guests tagged", tagged)
	})

	if !sawTags {
		t.Error("the iperf component never tagged the guests iperf=1 during the run")
	}

	out, err := proc.wait(t, scorchGuestPipelineTimeout)

	if err != nil {
		t.Errorf("SCORCH run %d (pipeline): %v\n%s", run, err, out)
	}

	// vmstats, ping45 and iperf aim cc filters; ip1's cleanup clears the last.
	scorchGuestWantNoCCFilter(t, run)

	t.Run("component stages", scorchGuestCheckPipelineStages)
	t.Run("vmstats", scorchGuestCheckVMStats)
	t.Run("hoststats in the background", scorchGuestCheckHostStats)
	t.Run("pcap and the cc ping", scorchGuestCheckPcap3)
	t.Run("qos", scorchGuestCheckQoS3)
	t.Run("iperf", scorchGuestCheckIperf)
	t.Run("collector record", scorchGuestCheckCollector)
	t.Run("tags and guest leftovers", scorchGuestCheckPipelineLeftovers)

	scorchGuestWantRun(t, run, "pipeline",
		"col1/loop-0-count-0/experiment_results/experiment_record.json", "vm1/loop-0-count-0/vm_stats.jsonl")
}

func scorchGuestCheckPipelineStages(t *testing.T) {
	const run = scorchGuestRunPipeline

	scorchGuestWantStages(t, run,
		"ip1/configure", "ip1/start", "ip1/cleanup",
		"dis1/configure", "dis1/start", "dis1/cleanup",
		"vm1/start", "vm1/stop",
		"pcap3/start", "pcap3/stop",
		"qos3/start", "qos3/stop",
		"ping45/start",
		"hs1/stop",
		"col1/stop",
	)

	scorchGuestWantStage(t, run, "ip1", "stop")

	for _, f := range []string{"disruption_start_time.txt", "disruption_stop_time.txt"} {
		scorchWantFile(t, filepath.Join(scorchGuestDir(run, "dis1"), f))
	}
}

func scorchGuestCheckVMStats(t *testing.T) {
	var (
		rows = scorchJSONLines(t, filepath.Join(scorchGuestDir(scorchGuestRunPipeline, "vm1"), "vm_stats.jsonl"))
		per  = make(map[string]int)
	)

	for i, row := range rows {
		for _, k := range []string{"us", "sy", "id", "free"} {
			if _, ok := row[k]; !ok {
				t.Fatalf("vm_stats.jsonl row %d lacks %q: %v", i, k, row)
			}
		}

		name, _ := row["vm_name"].(string)
		per[name]++
	}

	for _, vm := range scorchGuestVMs() {
		if per[vm] < scorchGuestMinVMStats {
			t.Errorf("vm_stats.jsonl has %d rows for %s, want at least %d: %v", per[vm], vm, scorchGuestMinVMStats, per)
		}
	}
}

func scorchGuestCheckHostStats(t *testing.T) {
	const run = scorchGuestRunPipeline

	dir := scorchGuestDir(run, "hs1")

	// The stop stage cancels the backgrounded start with SIGTERM before
	// SIGKILL, so start flushes and writes its own status. The stop stage waits
	// for that, so the file should be there when the run ends; the poll only
	// allows for a slow write.
	eventually(t, time.Minute, time.Second, "hs1's start status file", func() (bool, string) {
		m, _ := filepath.Glob(filepath.Join(dir, "start-*.json"))

		return len(m) == 1, fmt.Sprintf("have %v", m)
	})

	if start := scorchGuestWantStage(t, run, "hs1", "start"); !start.logged("Stopped user component: hs1") {
		t.Errorf("hs1's start stage did not log its SIGTERM wrap-up: %v", start.Logs)
	}

	rows := scorchJSONLines(t, filepath.Join(dir, "host_stats.jsonl"))
	if len(rows) < scorchGuestMinHostStats {
		t.Errorf("host_stats.jsonl has %d rows, want at least %d", len(rows), scorchGuestMinHostStats)
	}

	head := headnode(t)
	vms := []any{"g1", "g2", "g3"}

	for i, row := range rows {
		_, load1 := row["Load_1"].(float64)
		_, load15 := row["Load_15"].(float64)

		if row["compute_name"] != head || !load1 || !load15 || !reflect.DeepEqual(row["vm_list"], vms) {
			t.Errorf("host_stats.jsonl row %d = %v, want compute_name %s, numeric loads and vm_list %v", i, row, head, vms)

			break
		}
	}
}

func scorchGuestCheckPcap3(t *testing.T) {
	dir := scorchGuestDir(scorchGuestRunPipeline, "pcap3")

	// ping45 is a top-level cc exec filtered to hostname=g2; its echo
	// requests are the only ones in this run.
	g1 := filepath.Join(dir, "raw", "g1-0.pcap")
	if n := scorchGuestPackets(t, g1, "icmp.type == 8 && ip.src == 10.10.0.2"); n < scorchGuestMinPings {
		t.Errorf("%s holds %d echo requests from g2, want at least %d from ping45", g1, n, scorchGuestMinPings)
	}

	// pcap3 sets no filter, so its captures keep iperf's TCP only if it
	// cleared the filter left before the run, which matches none of it.
	merged := filepath.Join(dir, "merged.pcap")
	if n := scorchGuestPackets(t, merged, "tcp.port == 5201 || tcp.port == 5202"); n <= 0 {
		t.Errorf("%s holds %d iperf TCP packets, want some", merged, n)
	}

	scorchGuestWantPcapMetadata(t, dir, "g1-0.pcap", "g2-0.pcap", "merged.pcap")
}

func scorchGuestCheckQoS3(t *testing.T) {
	scorchGuestWantQoSApplied(t, scorchGuestRunPipeline, "qos3", "g2", "10ms")

	if q := scorchGuestQoS(t, "g2"); strings.Contains(q, "delay") {
		t.Errorf("g2's qos after qos3 stopped: %q, want none", q)
	}

	if qd := scorchGuestQdisc(t, "g2"); strings.Contains(qd, "netem") {
		t.Errorf("g2's tap qdisc after qos3 stopped: %q, want no netem", qd)
	}
}

// scorchGuestIperfResult is the part of an iperf3 --json result the checks read.
type scorchGuestIperfResult struct {
	End struct {
		SumSent struct {
			Bytes float64 `json:"bytes"`
		} `json:"sum_sent"`
	} `json:"end"`
	Error string `json:"error"`
}

func scorchGuestCheckIperf(t *testing.T) {
	dir := scorchGuestDir(scorchGuestRunPipeline, "ip1")

	scorchWantFile(t, filepath.Join(dir, "iperf_mapping.json"))

	// The first file the stop stage fetches: g2's client result.
	var res scorchGuestIperfResult
	if scorchReadJSON(t, filepath.Join(dir, "iperf_client-data_client-g2_server-g1.json"), &res) &&
		(res.End.SumSent.Bytes <= 0 || res.Error != "") {
		t.Errorf("g2's iperf client sent %.0f bytes with error %q, want bytes and no error", res.End.SumSent.Bytes, res.Error)
	}

	// Each server writes its --json result with --logfile to the path the stop
	// stage fetches after the client's. The stage gives up at the first file
	// it cannot fetch, so one missing result also loses every later one.
	for _, f := range []string{
		"iperf_server-data_client-g2_server-g1.json",
		"iperf_client-data_client-g3_server-g1.json",
		"iperf_server-data_client-g3_server-g1.json",
	} {
		scorchWantFile(t, filepath.Join(dir, f))
	}

	for _, f := range []string{"rtt_histogram_g2-g1.txt", "rtt_histogram_g3-g1.txt"} {
		if n := scorchGuestLines(t, filepath.Join(dir, f)); n < scorchGuestMinRTTLines {
			t.Errorf("%s has %d round trip samples, want at least %d", f, n, scorchGuestMinRTTLines)
		}
	}
}

// scorchGuestRecord is the part of the collector's experiment_record.json the
// checks read.
type scorchGuestRecord struct {
	Experiment struct {
		Disruption     string  `json:"disruption"`
		Permutation    int     `json:"permutation"`
		Duration       float64 `json:"duration"`
		DurationActual float64 `json:"duration_actual"`
	} `json:"experiment"`
	Sceptre struct {
		Experiment string `json:"experiment"`
		Topology   string `json:"topology"`
		Scenario   string `json:"scenario"`
		Minimega   string `json:"minimega_version"`
	} `json:"sceptre"`
	Scorch struct {
		Run   int `json:"run"`
		Loop  int `json:"loop"`
		Count int `json:"count"`
	} `json:"scorch"`
	Disruption   map[string]any               `json:"disruption"`
	QoS          map[string]map[string]any    `json:"qos"`
	Provider     map[string]any               `json:"provider"`
	AllHosts     map[string]any               `json:"all_hosts"`
	PcapMetadata map[string]map[string]string `json:"pcap_metadata"`
}

func scorchGuestCheckCollector(t *testing.T) {
	const run = scorchGuestRunPipeline

	// With no loop in the run, and components the run does not use, the
	// collector still starts and writes the record.
	results := filepath.Join(scorchGuestDir(run, "col1"), "experiment_results")

	var rec scorchGuestRecord
	if !scorchReadJSON(t, filepath.Join(results, "experiment_record.json"), &rec) {
		return
	}

	if e := rec.Experiment; e.Disruption != "baseline" || e.Permutation != 0 || e.Duration != scorchGuestDisruption ||
		e.DurationActual < scorchGuestDisruption {
		t.Errorf("record experiment = %+v, want baseline, permutation 0, duration %.0f and at least that long", e, scorchGuestDisruption)
	}

	if s := rec.Sceptre; s.Experiment != scorchGuestName || s.Topology != scorchGuestName || s.Scenario != scorchGuestName ||
		s.Minimega != mmText(t, "", "version") {
		t.Errorf("record sceptre = %+v, want experiment, topology and scenario %s and minimega's version", s, scorchGuestName)
	}

	if rec.Scorch.Run != run || rec.Scorch.Loop != 0 || rec.Scorch.Count != 0 {
		t.Errorf("record scorch = %+v, want run %d loop 0 count 0", rec.Scorch, run)
	}

	if _, ok := rec.Disruption["baseline"]; !ok || len(rec.Provider) != 0 || len(rec.AllHosts) != 0 {
		t.Errorf("record disruption %v, provider %v, all_hosts %v, want baseline only and no provider or multi-homed hosts",
			rec.Disruption, rec.Provider, rec.AllHosts)
	}

	// qos and pcap come from the run's own qos3 and pcap3, not from qos4 and
	// pcap4, defined later for run 4.
	if want := map[string]map[string]any{"g2": {"interface": 0.0, "delay": "10ms"}}; !reflect.DeepEqual(rec.QoS, want) {
		t.Errorf("record qos = %v, want qos3's %v", rec.QoS, want)
	}

	for _, k := range []string{"g1-0.pcap", "g2-0.pcap", "merged.pcap"} {
		if rec.PcapMetadata[k] == nil {
			t.Errorf("record pcap_metadata lacks pcap3's %s: %v", k, rec.PcapMetadata)
		}
	}

	for _, f := range []string{
		"metadata/topology.yaml", "metadata/scenario.yaml", "metadata/experiment.yaml",
		"metadata/vm_stats.jsonl", "metadata/host_stats.jsonl", "metadata/qos_values_applied.json",
		"iperf/iperf_mapping.json", "iperf/iperf_client-data_client-g2_server-g1.json",
		"disruption/disruption_start_time.txt", "disruption/disruption_stop_time.txt",
		"pcaps/merged.pcap", "pcaps/pcap_metadata.json",
	} {
		scorchWantFile(t, filepath.Join(results, f))
	}

	// The collector's own `phenix config get` read this experiment.
	if doc := scorchRead(t, filepath.Join(results, "metadata", "experiment.yaml")); !strings.Contains(doc, scorchGuestName) {
		t.Errorf("the collected experiment.yaml does not name %s:\n%s", scorchGuestName, doc)
	}
}

func scorchGuestCheckPipelineLeftovers(t *testing.T) {
	info := vmInfo(t, scorchGuestName)

	for _, vm := range scorchGuestVMs() {
		tags := scorchGuestTags(t, info[vm])

		if _, ok := tags["iperf"]; ok {
			t.Errorf("%s keeps the iperf tag after ip1's cleanup: %v", vm, tags)
		}

		if _, ok := tags["vmstat"]; ok {
			t.Errorf("%s keeps the vmstat tag after vm1 stopped: %v", vm, tags)
		}
	}

	// miniccc runs the components' commands, and this script, in its own
	// working directory.
	g1 := scorchShell(t, scorchGuestName, "g1", "echo cwd=$(pwd); test -e 1 && echo stray=yes || echo stray=no; "+
		"test -e /vmstat.out && echo vmstat=yes || echo vmstat=no; echo iperf=$(ls /iperf_* 2>/dev/null | wc -l)")

	if g1["vmstat"] != "no" || g1["iperf"] != "0" {
		t.Errorf("g1 keeps vmstat or iperf output after the run's stop and cleanup: %v", g1)
	}

	if g1["stray"] != "no" {
		t.Errorf("iperf's 2>1 left a file named 1 in miniccc's working directory %s in g1", g1["cwd"])
	}
}

// ---------------------------------------------------------------------------
// SCG-05, SCG-06: canceling a run

func scorchGuestCheckRESTCancel(t *testing.T) {
	const run = scorchGuestRunCancel

	scorchGuestReset(t)

	path := fmt.Sprintf("/experiments/%s/scorch/pipelines/%d", scorchGuestName, run)

	if code, body := api(t, http.MethodPost, path, nil); code != http.StatusNoContent {
		t.Fatalf("POST %s: status %d, want 204: %s", path, code, body)
	}

	t.Cleanup(func() {
		if scorchGuestRunning(t) {
			_, _ = api(t, http.MethodDelete, path, nil)

			eventually(t, scorchGuestStopTimeout, time.Second, "the run to stop", func() (bool, string) {
				return !scorchGuestRunning(t), "SCORCH still running"
			})
		}
	})

	eventually(t, 2*time.Minute, time.Second, "run 4 to reach its 120 s pause", func() (bool, string) {
		nodes := scorchGuestPipeline(t)

		return scorchGuestNodeStatus(nodes, "start", "wait120") == "running", fmt.Sprintf("pipeline %+v", nodes)
	})

	scorchGuestCheckCancelApplied(t)

	if code, body := api(t, http.MethodDelete, path, nil); code != http.StatusNoContent {
		t.Errorf("DELETE %s: status %d, want 204: %s", path, code, body)
	}

	eventually(t, scorchGuestStopTimeout, time.Second, "the canceled run to finish", func() (bool, string) {
		return !scorchGuestRunning(t), "SCORCH still running"
	})

	// The stop stage runs after a cancel.
	scorchGuestCheckCancelTornDown(t)

	nodes := scorchGuestPipeline(t)

	for _, n := range nodes {
		if n.Status == "running" || n.Status == "background" {
			t.Errorf("pipeline node %s (%s) is still %s after the canceled run finished", n.Name, n.Stage, n.Status)
		}
	}

	for _, want := range []scorchGuestNode{
		{Stage: "start", Name: "qos4", Status: "success"},
		{Stage: "start", Name: "pcap4", Status: "success"},
		{Stage: "start", Name: "wait120", Status: "failure"},
		{Stage: "stop", Name: "pcap4", Status: "success"},
		{Stage: "stop", Name: "qos4", Status: "success"},
	} {
		if got := scorchGuestNodeStatus(nodes, want.Stage, want.Name); got != want.Status {
			t.Errorf("pipeline node %s (%s) is %q after the cancel, want %q: %+v", want.Name, want.Stage, got, want.Status, nodes)
		}
	}

	scorchGuestWantRun(t, run, "cancel", "pcap4/loop-0-count-0/stop-", "qos4/loop-0-count-0/stop-")
}

func scorchGuestCheckCLICancel(t *testing.T) {
	const run = scorchGuestRunCancel

	scorchGuestReset(t)

	began := time.Now()
	proc := scorchGuestStart(t, run)

	// pcap4 writes its start status file once its start stage is done, and
	// wait120 runs next. The run directory still holds SCG-05's until this
	// run wipes it, so only a file written since this run began counts.
	eventually(t, 2*time.Minute, time.Second, "run 4's start stage to reach its pause", func() (bool, string) {
		m, _ := filepath.Glob(filepath.Join(scorchGuestDir(run, "pcap4"), "start-*.json"))
		if len(m) == 1 {
			if fi, err := os.Stat(m[0]); err == nil && !fi.ModTime().Before(began) {
				return true, ""
			}
		}

		return proc.finished(), fmt.Sprintf("have %v", m)
	})

	if proc.finished() {
		out, err := proc.wait(t, time.Second)
		t.Fatalf("SCORCH run %d ended before its pause (%v):\n%s", run, err, out)
	}

	scorchGuestCheckCancelApplied(t)
	proc.terminate(t)

	// A run canceled by a signal is not an error for the CLI while its stop
	// and cleanup stages succeed (cmd/experiment.go).
	if out, err := proc.wait(t, scorchGuestStopTimeout); err != nil {
		t.Errorf("phenix experiment scorch after SIGTERM: %v, want exit 0\n%s", err, out)
	}

	scorchGuestCheckCancelTornDown(t)
	scorchGuestWantRun(t, run, "cancel", "pcap4/loop-0-count-0/stop-", "qos4/loop-0-count-0/stop-")
}

// scorchGuestCheckCancelApplied checks what run 4's start stage sets up
// before its pause: qos4's delay and pcap4's capture of g1.
func scorchGuestCheckCancelApplied(t *testing.T) {
	t.Helper()

	if q := scorchGuestQoS(t, "g1"); !strings.Contains(q, "delay 100ms") {
		t.Errorf("g1's qos during run 4's pause: %q, want qos4's delay 100ms", q)
	}

	want := map[string]string{"g1:0": filepath.Join(scorchGuestDir(scorchGuestRunCancel, "pcap4"), "g1-0.pcap")}
	if caps := scorchGuestCaptures(t); !reflect.DeepEqual(caps, want) {
		t.Errorf("minimega captures during run 4's pause: %v, want %v", caps, want)
	}
}

// scorchGuestCheckCancelTornDown checks that the stop stage of a canceled run
// 4 released what its start stage set up, and that SCORCH is idle again.
func scorchGuestCheckCancelTornDown(t *testing.T) {
	t.Helper()

	const run = scorchGuestRunCancel

	if q := scorchGuestQoS(t, "g1"); strings.Contains(q, "delay") {
		t.Errorf("g1's qos after the canceled run: %q, want none", q)
	}

	if qd := scorchGuestQdisc(t, "g1"); strings.Contains(qd, "netem") {
		t.Errorf("g1's tap qdisc after the canceled run: %q, want no netem", qd)
	}

	if caps := scorchGuestCaptures(t); len(caps) != 0 {
		t.Errorf("minimega captures after the canceled run: %v, want none", caps)
	}

	scorchGuestWantStages(t, run, "qos4/start", "pcap4/start", "pcap4/stop", "qos4/stop")
	scorchGuestWantPcapMetadata(t, scorchGuestDir(run, "pcap4"), "g1-0.pcap")
	scorchGuestWantIdle(t)
}

// ---------------------------------------------------------------------------
// SCG-07: stopping the experiment during a REST run

// scorchGuestCheckExperimentStop stops the experiment over REST while run 4
// pauses: the stop must end the run, which must still run its stop stage and
// clear its status, without taking phenix ui down or leaving minimega a
// namespace for the stopped experiment.
func scorchGuestCheckExperimentStop(t *testing.T) {
	const run = scorchGuestRunCancel

	path := fmt.Sprintf("/experiments/%s/scorch/pipelines/%d", scorchGuestName, run)

	if code, body := api(t, http.MethodPost, path, nil); code != http.StatusNoContent {
		t.Fatalf("POST %s: status %d, want 204: %s", path, code, body)
	}

	t.Cleanup(func() {
		if scorchGuestRunning(t) {
			_, _ = api(t, http.MethodDelete, path, nil)
		}

		// A namespaced minimega command re-creates a namespace that was
		// cleared; do not leave one behind for the stopped experiment.
		if _, ok := mmNamespaces(t)[scorchGuestName]; ok {
			if _, err := mmRun("", "clear namespace "+scorchGuestName); err != nil {
				t.Logf("clearing the re-created namespace %s: %v", scorchGuestName, err)
			}
		}
	})

	eventually(t, 2*time.Minute, time.Second, "run 4 to reach its 120 s pause", func() (bool, string) {
		nodes := scorchGuestPipeline(t)

		return scorchGuestNodeStatus(nodes, "start", "wait120") == "running", fmt.Sprintf("pipeline %+v", nodes)
	})

	stopPath := "/experiments/" + scorchGuestName + "/stop"
	apiOK(t, http.MethodPost, stopPath, nil, nil)

	// Read at once, for the failure message below: the stop waited for the
	// run's stop stage before it cleared the namespace, so a namespace here
	// was created again during or right after the stop.
	_, early := mmNamespaces(t)[scorchGuestName]

	eventually(t, scorchGuestStopTimeout, time.Second, "the run the experiment stop ended to finish", func() (bool, string) {
		return !scorchGuestRunning(t), "SCORCH still running"
	})

	resp, err := httpClient.Get("http://" + cfg.uiAddr + "/version")
	if err != nil {
		t.Fatalf("phenix ui after the experiment stopped during a SCORCH run: %v", err)
	}

	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("phenix ui /version after the experiment stopped during a SCORCH run: status %d", resp.StatusCode)
	}

	nodes := scorchGuestPipeline(t)

	// The stop canceled the pause instead of waiting it out.
	if got := scorchGuestNodeStatus(nodes, "start", "wait120"); got != "failure" {
		t.Errorf("pipeline node wait120 (start) is %q after the experiment stopped, want failure (canceled): %+v", got, nodes)
	}

	for _, n := range nodes {
		if n.Status == "running" || n.Status == "background" {
			t.Errorf("pipeline node %s (%s) is still %s after the run the experiment stop ended", n.Name, n.Stage, n.Status)
		}
	}

	scorchGuestWantIdle(t)

	if st, _ := scorchGuestStatusMap(t)["startTime"].(string); st != "" {
		t.Errorf("status.startTime = %q after the REST stop, want it cleared", st)
	}

	eventually(t, time.Minute, time.Second, "the guests' QEMU processes to exit", func() (bool, string) {
		n := qemuProcesses(t)

		return n == 0, fmt.Sprintf("%d QEMU processes", n)
	})

	// The stop cancels the run, and waits for its stop stage (pcap4, qos4),
	// before it kills the VMs and clears the namespace, so that stage acts on
	// VMs minimega still has and nothing creates the namespace again.
	scorchGuestWantStages(t, run, "pcap4/stop", "qos4/stop")

	if _, ok := mmNamespaces(t)[scorchGuestName]; ok {
		t.Errorf(
			"minimega namespace %s exists after the experiment stopped and its SCORCH run ended (it existed right after the stop returned: %t)",
			scorchGuestName,
			early,
		)
	}
}

// scorchGuestWantNoCCFilter checks that a run left no cc filter on the
// namespace for the commands after it.
func scorchGuestWantNoCCFilter(t *testing.T, runID int) {
	t.Helper()

	if rows := mmTable(t, scorchGuestName, "cc filter"); len(rows) != 0 {
		t.Errorf("cc filter after run %d: %v, want none", runID, rows)
	}
}

// ---------------------------------------------------------------------------
// setup

// scorchGuestRequire fails unless the E2E workflow installed the apps and the
// host tools the components shell out to.
func scorchGuestRequire(t *testing.T) {
	t.Helper()

	var (
		components = []string{"cc", "collector", "disruption", "hoststats", "iperf", "mm", "pcap", "qos", "tcpdump", "vmstats"}
		// pcap post-processes captures with the Wireshark tools, the checks
		// read qdiscs with tc, and the collector runs `phenix config get`.
		tools    = []string{"tshark", "capinfos", "editcap", "mergecap", "tc", "phenix"}
		commands = make([]string, 0, len(components)+len(tools))
	)

	commands = append(commands, tools...)

	for _, c := range components {
		commands = append(commands, "phenix-scorch-component-"+c)
	}

	requireApps(t, "scorch-guest", commands...)
}

// scorchGuestPrepare writes the fixture cc1 sends, and removes it and the
// experiment's SCORCH output when the test ends, keeping a copy of the output
// in the work directory when the test failed.
func scorchGuestPrepare(t *testing.T) {
	t.Helper()

	dir := filepath.Join(scorchGuestPhenixBase, scorchGuestFixtures)

	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // read by phenix and minimega
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	if err := os.WriteFile(filepath.Join(dir, "hello.txt"), []byte(scorchGuestHello+"\n"), 0o644); err != nil { //nolint:gosec // guest file
		t.Fatal(err)
	}

	// Registered before newExperiment's teardown, so it runs after it.
	t.Cleanup(func() {
		if t.Failed() {
			keep := filepath.Join(cfg.workDir, t.Name(), "scorch-files")

			if err := os.MkdirAll(filepath.Dir(keep), 0o750); err == nil {
				if err := run("cp", "-a", scorchGuestFilesDir(), keep); err != nil {
					t.Logf("keeping the SCORCH output: %v", err)
				}
			}
		}

		_ = os.RemoveAll(filepath.Join(scorchGuestPhenixBase, "images", scorchGuestName))
	})
}

// scorchGuestVMs are the topology's guests.
func scorchGuestVMs() []string {
	return []string{"g1", "g2", "g3"}
}

// scorchGuestTopology is three tools guests on VLAN LAN: g1 is the server, g2
// the client and g3 a second client.
func scorchGuestTopology() string {
	return topology(
		node("g1", cfg.tools, "LAN", scorchGuestServer),
		node("g2", cfg.tools, "LAN", "10.10.0.2"),
		node("g3", cfg.tools, "LAN", "10.10.0.3"),
	)
}

func scorchGuestScenarioYAML() string {
	return strings.ReplaceAll(scorchGuestScenario, "{{fixtures}}", scorchGuestFixtures)
}

// scorchGuestReset clears, once the calling check ends, the namespace state a
// failed run could leave behind for the next check: captures, the capture
// filter and snaplen, qos and the cc filter. Each check asserts that state
// itself first.
func scorchGuestReset(t *testing.T) {
	t.Helper()

	t.Cleanup(func() {
		// minimega: a namespaced subcommand is rebuilt quoting only values
		// with whitespace (pkg/minicli/input.go, trie.go) and compiled again
		// (cmd/minimega/cli.go), so `capture pcap filter ''` loses its empty
		// value and becomes a query. A blank filter survives and matches
		// every packet.
		for _, c := range []string{
			"clear capture", "capture pcap filter ' '", "capture pcap snaplen 0", "clear qos all all", "clear cc filter",
		} {
			if _, err := mmRun(scorchGuestName, c); err != nil {
				t.Logf("resetting the namespace with %q: %v", c, err)
			}
		}
	})
}

// scorchGuestLeaveFilter leaves, before a run, a capture filter that no
// packet of the run matches, as an earlier capture could.
func scorchGuestLeaveFilter(t *testing.T, run int) {
	t.Helper()

	if _, err := mmRun(scorchGuestName, "capture pcap filter '"+scorchGuestStaleFilter+"'"); err != nil {
		t.Errorf("leaving a capture filter for run %d: %v", run, err)
	} else if f := strings.TrimSpace(mmText(t, scorchGuestName, "capture pcap filter")); f != scorchGuestStaleFilter {
		t.Errorf("the namespace's capture filter before run %d is %q, want %q", run, f, scorchGuestStaleFilter)
	}
}

// ---------------------------------------------------------------------------
// running SCORCH

// scorchGuestRun runs a SCORCH run with the CLI and waits for it.
func scorchGuestRun(runID int) (string, error) {
	return phenixRun("experiment", "scorch", scorchGuestName, "--run", strconv.Itoa(runID))
}

// scorchGuestStart starts a SCORCH run with the CLI. If the test ends first,
// the cleanup sends the CLI SIGTERM, which cancels the run and still runs its
// stop and cleanup stages, so a failed check leaves no QoS, capture or SCORCH
// status behind for the next one.
func scorchGuestStart(t *testing.T, runID int) *scorchProc {
	t.Helper()

	p := &scorchProc{phenixProc: phenixProc{done: make(chan struct{})}}

	p.cmd = exec.Command(
		cfg.phenix,
		phenixArgs("experiment", "scorch", scorchGuestName, "--run", strconv.Itoa(runID))...) //nolint:gosec // test harness
	p.cmd.Stdout = p
	p.cmd.Stderr = p

	if err := startChild(p.cmd); err != nil {
		t.Fatalf("starting SCORCH run %d: %v", runID, err)
	}

	go func() {
		err := p.cmd.Wait()

		p.mu.Lock()
		p.err = err
		p.mu.Unlock()

		close(p.done)
	}()

	t.Cleanup(func() {
		if p.finished() {
			return
		}

		_ = p.cmd.Process.Signal(syscall.SIGTERM)

		select {
		case <-p.done:
		case <-time.After(scorchGuestStopTimeout):
			_ = p.cmd.Process.Kill()

			<-p.done
		}
	})

	return p
}

// scorchGuestNode is one node of a SCORCH pipeline phenix ui reports.
type scorchGuestNode struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Stage  string `json:"stage"`
}

// scorchGuestPipeline returns the nodes of run 4's pipeline from phenix ui.
func scorchGuestPipeline(t *testing.T) []scorchGuestNode {
	t.Helper()

	var body struct {
		Pipeline []scorchGuestNode `json:"pipeline"`
	}

	apiOK(t, http.MethodGet, fmt.Sprintf("/experiments/%s/scorch/pipelines/%d/0", scorchGuestName, scorchGuestRunCancel), nil, &body)

	return body.Pipeline
}

func scorchGuestNodeStatus(nodes []scorchGuestNode, stage, name string) string {
	for _, n := range nodes {
		if n.Stage == stage && n.Name == name {
			return n.Status
		}
	}

	return ""
}

// ---------------------------------------------------------------------------
// SCORCH output

func scorchGuestFilesDir() string {
	return filepath.Join(scorchGuestPhenixBase, "images", scorchGuestName, "files")
}

func scorchGuestRunDir(runID int) string {
	return filepath.Join(scorchGuestFilesDir(), "scorch", "run-"+strconv.Itoa(runID))
}

// scorchGuestDir is where a component writes its output in a run (loop 0,
// count 0).
func scorchGuestDir(runID int, cmp string) string {
	return filepath.Join(scorchGuestRunDir(runID), cmp, "loop-0-count-0")
}

// scorchGuestWantStage checks that a component ran a stage of a run, wrote a
// status file naming that run and stage, and succeeded.
func scorchGuestWantStage(t *testing.T, runID int, cmp, stage string) scorchStatus {
	t.Helper()

	var st scorchStatus

	matches, _ := filepath.Glob(filepath.Join(scorchGuestDir(runID, cmp), stage+"-*.json"))
	if len(matches) != 1 {
		t.Errorf("run %d: want one %s status file from %s, have %v", runID, stage, cmp, matches)

		return st
	}

	if !scorchReadJSON(t, matches[0], &st) {
		return st
	}

	if st.Experiment != scorchGuestName || st.Run != runID || st.Component != cmp || st.Stage != stage || st.Loop != 0 || st.Count != 0 {
		t.Errorf("%s names experiment %q run %d component %q stage %q loop %d count %d", matches[0],
			st.Experiment, st.Run, st.Component, st.Stage, st.Loop, st.Count)
	}

	if msg := st.failure(); msg != "" {
		t.Errorf("run %d: %s %s failed: %s", runID, cmp, stage, msg)
	}

	return st
}

// scorchGuestWantStages calls scorchGuestWantStage for each "component/stage".
func scorchGuestWantStages(t *testing.T, runID int, stages ...string) {
	t.Helper()

	for _, s := range stages {
		cmp, stage, _ := strings.Cut(s, "/")
		scorchGuestWantStage(t, runID, cmp, stage)
	}
}

// scorchGuestRunRecord is the record phenix writes for every SCORCH run.
type scorchGuestRunRecord struct {
	Experiment struct {
		Name string `json:"name"`
	} `json:"experiment"`
	Run struct {
		Name  string `json:"name"`
		Index int    `json:"index"`
	} `json:"run"`
	Minimega string `json:"minimega_version"`
}

// scorchGuestWantRun checks what phenix leaves for every run: the run record,
// an archive of the run directory with entries starting with each of entries,
// the experiment's SCORCH status cleared, and the namespace still there.
func scorchGuestWantRun(t *testing.T, runID int, name string, entries ...string) {
	t.Helper()

	var rec scorchGuestRunRecord

	recPath := scorchGuestOne(t, filepath.Join(scorchGuestRunDir(runID), fmt.Sprintf("%s-scorch-run-%d-*.json", scorchGuestName, runID)))
	if recPath != "" && scorchReadJSON(t, recPath, &rec) {
		if rec.Experiment.Name != scorchGuestName || rec.Run.Index != runID || rec.Run.Name != name {
			t.Errorf("run record %+v, want experiment %s run %d named %s", rec, scorchGuestName, runID, name)
		}

		if v := mmText(t, "", "version"); rec.Minimega != v {
			t.Errorf("run record minimega_version %q, want minimega's %q", rec.Minimega, v)
		}

		entries = append(entries, filepath.Base(recPath))
	}

	// Every run adds an archive and none is removed; the newest is this run's.
	archives, _ := filepath.Glob(filepath.Join(scorchGuestFilesDir(), fmt.Sprintf("scorch-run-%d_*.tgz", runID)))
	slices.Sort(archives)

	if len(archives) == 0 {
		t.Errorf("run %d left no archive in %s", runID, scorchGuestFilesDir())
	} else {
		archive := archives[len(archives)-1]

		listed := scorchArchive(t, archive)

		for _, want := range entries {
			if !slices.ContainsFunc(listed, func(e string) bool { return strings.HasPrefix(e, want) }) {
				t.Errorf("run %d archive has no %s*: %v", runID, want, listed)
			}
		}
	}

	scorchGuestWantIdle(t)

	if _, ok := mmNamespaces(t)[scorchGuestName]; !ok {
		t.Errorf("minimega namespace %s is gone after run %d", scorchGuestName, runID)
	}

	t.Logf("run %d left %d cc commands in the namespace", runID, len(mmTable(t, scorchGuestName, "cc commands")))
}

// scorchGuestOne returns the one file matching pattern, or "" after failing
// the test.
func scorchGuestOne(t *testing.T, pattern string) string {
	t.Helper()

	matches, err := filepath.Glob(pattern)
	if err != nil || len(matches) != 1 {
		t.Errorf("want one file matching %s, have %v (%v)", pattern, matches, err)

		return ""
	}

	return matches[0]
}

// scorchGuestStatusMap returns the experiment's status as phenix stores it.
func scorchGuestStatusMap(t *testing.T) map[string]any {
	t.Helper()

	// stdout alone: phenix can print a warning to stderr around the document.
	out, err := phenixStdout("config", "get", "experiment/"+scorchGuestName, "--output", "json")
	if err != nil {
		t.Fatalf("phenix config get experiment/%s: %v\n%s", scorchGuestName, err, out)
	}

	var c struct {
		Status map[string]any `json:"status"`
	}

	if err := json.Unmarshal([]byte(jsonPart(out)), &c); err != nil {
		t.Fatalf("decoding experiment %s: %v\n%s", scorchGuestName, err, out)
	}

	return c.Status
}

// scorchGuestRunning reports status.appRunningStageStatus.scorch.
func scorchGuestRunning(t *testing.T) bool {
	t.Helper()

	running, _ := scorchGuestStatusMap(t)["appRunningStageStatus"].(map[string]any)
	scorch, _ := running["scorch"].(bool)

	return scorch
}

// scorchGuestWantIdle checks that no SCORCH run is recorded as running.
func scorchGuestWantIdle(t *testing.T) {
	t.Helper()

	status := scorchGuestStatusMap(t)

	running, _ := status["appRunningStageStatus"].(map[string]any)
	if r, _ := running["scorch"].(bool); r {
		t.Error("status.appRunningStageStatus.scorch is still true")
	}

	apps, _ := status["apps"].(map[string]any)
	if v, ok := apps["scorch"]; ok && v != nil {
		t.Errorf("status.apps.scorch = %v, want it removed", v)
	}
}

// scorchGuestWantQoSApplied checks the record a qos component writes of what
// it applied to one VM's first interface.
func scorchGuestWantQoSApplied(t *testing.T, runID int, cmp, vm, delay string) {
	t.Helper()

	var applied map[string]map[string]any

	if scorchReadJSON(t, filepath.Join(scorchGuestDir(runID, cmp), "qos_values_applied.json"), &applied) {
		if want := map[string]map[string]any{vm: {"interface": 0.0, "delay": delay}}; !reflect.DeepEqual(applied, want) {
			t.Errorf("%s qos_values_applied.json = %v, want %v", cmp, applied, want)
		}
	}
}

// scorchGuestWantPcapMetadata checks a pcap component's pcap_metadata.json:
// capinfos must have seen every named file as a pcap.
func scorchGuestWantPcapMetadata(t *testing.T, dir string, files ...string) {
	t.Helper()

	var meta map[string]map[string]string

	if !scorchReadJSON(t, filepath.Join(dir, "pcap_metadata.json"), &meta) {
		return
	}

	for _, f := range files {
		if !strings.Contains(meta[f]["File type"], "pcap") {
			t.Errorf("%s pcap_metadata.json has %s as %v, want a pcap file", dir, f, meta[f])
		}
	}
}

// ---------------------------------------------------------------------------
// files, captures, minimega and guests

// scorchGuestLines counts the non-empty lines of a file, or returns -1 after
// failing the test when it cannot be read.
func scorchGuestLines(t *testing.T, path string) int {
	t.Helper()

	data, err := os.ReadFile(path) //nolint:gosec // product output
	if err != nil {
		t.Errorf("reading %s: %v", path, err)

		return -1
	}

	return len(strings.Fields(string(data)))
}

// scorchGuestPackets counts the packets of a capture that match a Wireshark
// display filter, or returns -1 after failing the test when tshark fails.
func scorchGuestPackets(t *testing.T, file, filter string) int {
	t.Helper()

	var stderr bytes.Buffer

	cmd := exec.Command("tshark", "-n", "-r", file, "-Y", filter, "-T", "fields", "-e", "frame.number") //nolint:gosec // test harness
	cmd.Stderr = &stderr

	out, err := cmd.Output()
	if err != nil {
		t.Errorf("tshark -r %s -Y %q: %v\n%s", file, filter, err, stderr.String())

		return -1
	}

	return len(strings.Fields(string(out)))
}

// scorchGuestCaptures returns minimega's captures in the namespace, VM
// captures keyed by "vm:interface" and others by "type bridge", with the file
// each writes to.
func scorchGuestCaptures(t *testing.T) map[string]string {
	t.Helper()

	caps := make(map[string]string)

	for _, row := range mmTable(t, scorchGuestName, "capture") {
		key := row["interface"]
		if key == "" {
			key = row["type"] + " " + row["bridge"]
		}

		caps[key] = row["path"]
	}

	return caps
}

// scorchGuestQoS is minimega's qos column for a VM ("[]" without qos).
func scorchGuestQoS(t *testing.T, vm string) string {
	t.Helper()

	return vmInfo(t, scorchGuestName)[vm]["qos"]
}

// scorchGuestQdisc returns tc's qdiscs of a VM's first tap on the host, where
// minimega applies qos.
func scorchGuestQdisc(t *testing.T, vm string) string {
	t.Helper()

	taps := strings.Trim(vmInfo(t, scorchGuestName)[vm]["tap"], "[]")
	tap, _, _ := strings.Cut(taps, ",")

	out, err := exec.Command("tc", "qdisc", "show", "dev", strings.TrimSpace(tap)).CombinedOutput() //nolint:gosec // test harness
	if err != nil {
		t.Errorf("tc qdisc show dev %s (%s's tap): %v\n%s", tap, vm, err, out)
	}

	return string(out)
}

// scorchGuestTags decodes a vm info row's tags.
func scorchGuestTags(t *testing.T, row map[string]string) map[string]string {
	t.Helper()

	tags := make(map[string]string)

	if s := row["tags"]; s != "" && s != "null" {
		if err := json.Unmarshal([]byte(s), &tags); err != nil {
			t.Errorf("vm info tags %q of %s: %v", s, row["name"], err)
		}
	}

	return tags
}

// scorchGuestRTT pings g1 from g2 five times and returns the lowest and the
// highest round trip in milliseconds.
func scorchGuestRTT(t *testing.T) (float64, float64) {
	t.Helper()

	out := guestShell(t, scorchGuestName, "g2", "ping -n -c 5 -i 0.5 -W 3 "+scorchGuestServer+" | tail -1")

	m := regexp.MustCompile(`= ([0-9.]+)/([0-9.]+)/([0-9.]+)/`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("ping from g2 to g1 printed no round trip summary: %q", out)
	}

	lo, errLo := strconv.ParseFloat(m[1], 64)
	hi, errHi := strconv.ParseFloat(m[3], 64)

	if errLo != nil || errHi != nil {
		t.Fatalf("ping summary %q: %v %v", out, errLo, errHi)
	}

	return lo, hi
}
