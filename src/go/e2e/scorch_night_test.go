//go:build e2e

// End-to-end tests for the sceptre-phenix-apps SCORCH components the nightly
// shard (scorch-night) runs: snort and ettercap side by side, the disruption
// component's dos mode, ssh, trafficgen, art (with goart missing, and against
// a stand-in for goart's command line), providerdata and rtds against a fake
// bennu provider in a guest plus fake RSCAD automation and Elasticsearch
// endpoints this test serves on the host, and the collector's miniccc logs.
// Three tools guests (cfg.tools) boot once and every SCORCH run of one
// scenario runs on them in turn; one run goes through phenix ui's REST routes,
// the others through `phenix experiment scorch`. Each check reads what the run
// leaves where it lands: the component output and status files, the run
// record and archive, the pipeline phenix ui reports, the guests over C2, and
// the requests the host-side fakes received.
//
// Left out: the kafka component needs a Kafka broker, and none can run on the
// runner without downloading Kafka (the runner has no Kafka and phenix's
// module has no Go broker); run-level Filebeat needs a filebeat binary, and no
// pinned one is installed on the runner. The scorch-host shard covers
// Filebeat's configuration with a stand-in binary.

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

const (
	// scorchNightName names the experiment, its topology and its scenario.
	scorchNightName = "e2e-scn"

	// scorchNightFixtures holds the host-side files the components copy into
	// the guests (snort's configuration, trafficgen's scripts).
	scorchNightFixtures = "/phenix/e2e-scorch-night"

	// scorchNightTap is the host tap that puts the host on VLAN LAN, so the
	// ssh component, which connects from the host, reaches g1.
	scorchNightTap     = "e2escnssh"
	scorchNightTapAddr = "10.10.0.254"

	// Addresses of the guests on VLAN LAN.
	scorchNightG1 = "10.10.0.1"
	scorchNightG2 = "10.10.0.2"
	scorchNightG3 = "10.10.0.3"

	// scorchNightRunTimeout bounds one SCORCH run; the longest (provider)
	// takes about two minutes.
	scorchNightRunTimeout = 10 * time.Minute

	// scorchNightStopTimeout bounds how long a canceled run takes to run its
	// stop and cleanup stages and exit.
	scorchNightStopTimeout = 3 * time.Minute

	// scorchNightDoSDuration is dos1's run_duration in seconds: the start
	// stage lasts at least this long, however soon the attacker script ends.
	scorchNightDoSDuration = 70.0

	// scorchNightDocRate is how many documents a second the fake
	// Elasticsearch reports its indices growing by. rtds wants 1440 a second
	// (8 PMUs, 6 points, 30 updates a second), providerdata at least 100.
	scorchNightDocRate = 2000.0

	// trafficgen sends for tg1's duration at each client's rate; the checks
	// want at least half of what was sent to have reached the server.
	scorchNightMinBackground = 100
	scorchNightMinMalware    = 50
)

// The scenario's SCORCH runs, by index.
const (
	scorchNightRunMITM = iota
	scorchNightRunDoS
	scorchNightRunRemote
	scorchNightRunSSHFail
	scorchNightRunArtMissing
	scorchNightRunArtStub
	scorchNightRunProvider
	scorchNightRunCollector
)

// scorchNightScenario is the scenario the shard runs. {{fixtures}} is the
// host fixture directory and {{services}} the base URL of the fake RSCAD
// automation and Elasticsearch endpoints.
//
// Run 0 runs snort in g1 and ettercap in g3 through a pause; run 1 runs the
// disruption component's dos mode with a stand-in attacker script; run 2 runs
// ssh against g1 and trafficgen from g2 and g3 to g1; run 3 runs ssh commands
// that fail; run 4 runs art where goart is missing; run 5 runs art against a
// stand-in for goart's command line, with every option the README documents;
// run 6 runs providerdata and rtds against the fake provider; run 7 collects
// the guests' miniccc logs.
const scorchNightScenario = `apiVersion: phenix.sandia.gov/v2
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
      - name: snort1
        type: snort
        metadata:
          hostname: g1
          sniffInterface: eth0
          waitDuration: 5
          configs:
          - src: {{fixtures}}/snort.conf
            dst: /etc/snort/snort.conf
      - name: ett1
        type: ettercap
        metadata:
          vms:
          - hostname: g3
            iface: eth0
            targets: /10.10.0.1// /10.10.0.2//
      - name: wait45
        type: pause
        metadata:
          duration: 45s
      - name: dos1
        type: disruption
        metadata:
          current_disruption: dos
          run_duration: 70
          permutation: 1
          dos:
            start_delay: 2
            attack_duration: 3
            attacker:
              hostname: g3
              interface: eth0
              script_path: /usr/local/bin/e2e-dos
              results_path: /tmp/e2e-dos.json
            targets:
            - hostname: g1
              interface: eth0
      - name: ssh1
        type: ssh
        metadata:
          ip: 10.10.0.1
          user: root
          cmds:
          - hostname
          - test -f /etc/hostname
          files:
          - /etc/hostname
          - /etc/os-release
      - name: ssh2
        type: ssh
        metadata:
          ip: 10.10.0.1
          user: root
          cmds:
          - hostname
          - "false"
          files:
          - /etc/hostname
          - /nonexistent/e2e-file
      - name: tg1
        type: trafficgen
        metadata:
          scripts:
            trafficServer: {{fixtures}}/traffic-server.py
            backgroundGen: {{fixtures}}/background-gen.py
            malwareGen: {{fixtures}}/malware-gen.py
          targets:
          - hostname: g1
            interface: eth0
            duration: 10
            backgroundClient:
              hostname: g2
              rate: 20
              probability: 0.5
            malwareClient:
              hostname: g3
              rate: 10
              probability: 0.75
      - name: art0
        type: art
        metadata:
          technique: T1082
          testIndex: 0
          vms:
          - hostname: g1
      - name: art1
        type: art
        metadata:
          technique: T1082
          testIndex: 0
          goartPath: /opt/e2e-art/goart
          outputPath: /var/tmp/e2e-art
          execWaitSeconds: 1
          abortOnError: true
          env:
            E2E_ENV: night
          validator: grep -q Linux
          vms:
          - hostname: g1
            inputs:
              e2e_input: hello
      - name: pd1
        type: providerdata
        metadata:
          hostname: g1
          csv_files:
            path: /root/provider_data
            export: true
          elasticsearch:
            verify: true
            server: {{services}}
            index: e2e-provider
      - name: rt1
        type: rtds
        metadata:
          hostname: g1
          case_name: e2e-case
          csv_files:
            path: /root/provider_data
            export: true
          rscad_automation:
            enabled: true
            url: {{services}}/rscad
          elasticsearch:
            verify: true
            server: {{services}}
            index: e2e-rtds
            acceptable_time_drift: 100
      - name: dis5
        type: disruption
        metadata:
          current_disruption: baseline
          run_duration: 5
          permutation: 0
      - name: col5
        type: collector
        metadata:
          collect_iperf: false
          collect_miniccc: true
          generate_csv: false
      runs:
      - name: mitm
        configure: [snort1]
        start: [snort1, ett1, wait45]
        stop: [ett1, snort1]
        cleanup: [snort1]
      - name: dos
        configure: [dos1]
        start: [dos1]
        cleanup: [dos1]
      - name: remote
        configure: [ssh1, tg1]
        start: [tg1]
        stop: [tg1]
        cleanup: [tg1, ssh1]
      - name: ssh-fail
        start: [ssh2]
      - name: art-missing
        start: [art0]
      - name: art-stub
        start: [art1]
      - name: provider
        configure: [pd1, rt1]
        start: [pd1, rt1]
        stop: [pd1, rt1]
        cleanup: [rt1]
      - name: collector
        start: [dis5]
        stop: [col5]
`

// scorchNightSnortConf is the snort configuration snort1 copies to g1: one
// rule that alerts on every ICMP packet, fast alerts, packet logging and
// performance statistics in the files the component collects.
const scorchNightSnortConf = `config logdir: /var/log/snort
ipvar HOME_NET any
ipvar EXTERNAL_NET any
output alert_fast: alert
output log_tcpdump: snort.log
preprocessor perfmonitor: time 5 file /var/log/snort/snort.stats pktcnt 1
alert icmp any any -> any any (msg:"E2E ICMP"; sid:1000001; rev:1;)
`

// scorchNightTrafficServer is the UDP sink trafficgen starts on its target:
// it counts the datagrams each generator sends.
const scorchNightTrafficServer = `"""UDP sink for the trafficgen component: counts datagrams per sender."""
import json
import os
import socket

COUNTS = "/tmp/traffic-server.count"

counts = {}
sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
sock.bind(("0.0.0.0", 9999))

while True:
    data, (ip, _) = sock.recvfrom(2048)
    key = ip + " " + data.decode(errors="replace").strip()
    counts[key] = counts.get(key, 0) + 1
    with open(COUNTS + ".tmp", "w") as f:
        json.dump(counts, f)
    os.replace(COUNTS + ".tmp", COUNTS)
`

// scorchNightTrafficGen is the generator trafficgen starts on a client, both
// as the background and as the malware generator: it records its arguments
// and sends UDP datagrams carrying its own name to the target's port 9999.
const scorchNightTrafficGen = `"""UDP generator for the trafficgen component."""
import argparse
import json
import socket
import sys
import time
from pathlib import Path

name = Path(sys.argv[0]).stem
parser = argparse.ArgumentParser()
parser.add_argument("--ip", required=True)
parser.add_argument("--rate", type=float, required=True)
parser.add_argument("--duration", type=float, required=True)
parser.add_argument("--probability", type=float, required=True)
args = parser.parse_args()

with open("/tmp/" + name + ".args", "w") as f:
    json.dump(vars(args), f)

sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
end = time.monotonic() + args.duration
while time.monotonic() < end:
    try:
        sock.sendto(name.encode(), (args.ip, 9999))
    except OSError:
        pass
    time.sleep(1.0 / args.rate)
`

// scorchNightG1Setup prepares g1 as the SSH target, the fake bennu provider
// and the art target. %s is the public key the ssh component logs in with.
//
// The fake provider is what providerdata and rtds look for: the provider's
// configuration under /etc/sceptre, a pybennu-power-solver that "start"
// daemonizes as a process of that name writing CSV data and "stop" ends, and
// an ntpq reporting a synchronized peer (the image's own NTP state is logged
// first; it has no NTP server to reach in the experiment). The goart stand-in
// records its arguments and writes the result document art reads.
const scorchNightG1Setup = `set -e
mkdir -p /root/.ssh
chmod 700 /root/.ssh
cat > /root/.ssh/authorized_keys <<'EOF'
%s
EOF
chmod 600 /root/.ssh/authorized_keys

mkdir -p /etc/sceptre /usr/local/lib/e2e /opt/e2e-art /var/tmp/e2e-art
cat > /etc/sceptre/config.ini <<'EOF'
[power-solver-service]
solver-type = Dummy
config-file = /etc/sceptre/provider.yaml
EOF
cat > /etc/sceptre/provider.yaml <<'EOF'
pmu:
  pmus: []
EOF
cat > /etc/sceptre/rtds_config.yaml <<'EOF'
rtds:
  case: e2e-case
EOF

cat > /usr/local/lib/e2e/provider-loop <<'EOF'
while :; do
	echo "$(date +%%s),60.0" >> /root/provider_data/bus.csv
	sleep 1
done
EOF
cat > /usr/local/bin/pybennu-power-solver <<'EOF'
#!/bin/bash
echo "$*" >> /tmp/e2e-provider.calls
case " $* " in
*" start "*)
	mkdir -p /root/provider_data
	setsid -f bash -c 'exec -a pybennu-power-solver /bin/bash /usr/local/lib/e2e/provider-loop' </dev/null >/dev/null 2>&1
	;;
*" stop "*)
	pkill -f '^pybennu-power-solver' || true
	;;
esac
exit 0
EOF
chmod 755 /usr/local/bin/pybennu-power-solver
rm -f /tmp/e2e-provider.calls

echo "image_ntpq=$( (timeout 20 ntpq -p 2>&1 || true) | tr '\n' '|')"
cat > /usr/local/bin/ntpq <<'EOF'
#!/bin/bash
echo "     remote           refid      st t when poll reach   delay   offset  jitter"
echo "=============================================================================="
echo "*10.10.0.254     .LOCL.          10 u   12   64  377    0.512    0.031   0.008"
EOF
chmod 755 /usr/local/bin/ntpq

cat > /opt/e2e-art/goart <<'EOF'
#!/bin/bash
printf '%%s\n' "$@" > /var/tmp/e2e-art/argv
out=
while [ $# -gt 0 ]; do
	if [ "$1" = -o ]; then out=$2; shift; fi
	shift
done
[ -n "$out" ] || { echo "goart stand-in: no -o" >&2; exit 2; }
mkdir -p "$(dirname "$out")"
python3 - "$out" "$(uname -a)" <<'PY'
import json, sys
with open(sys.argv[1], "w") as f:
    json.dump({"Technique": "T1082", "Executor": {"ExecutedCommand": {"command": "uname -a", "results": sys.argv[2]}}}, f)
PY
EOF
chmod 755 /opt/e2e-art/goart

command -v snort >/dev/null && echo snort=yes || echo snort=no
command -v python3 >/dev/null && echo python3=yes || echo python3=no
ss -ltn | grep -q ':22 ' && echo sshd=yes || echo sshd=no
echo ready=yes
`

// scorchNightG3Setup prepares g3 as the dos attacker. The attacker script is a
// stand-in that sends nothing: it takes the arguments the disruption
// component passes, waits and runs for the times it is given, writes a
// results file and prints a line, which is the contract the component relies
// on.
const scorchNightG3Setup = `set -e
cat > /usr/local/bin/e2e-dos <<'EOF'
#!/bin/bash
if [ "$1" = --help ]; then
	echo "usage: e2e-dos --wait-for S --run-for S --target-ips IP... --interface IF --results-file F"
	exit 0
fi
echo "$*" > /tmp/e2e-dos.args
wait_for=0 run_for=0 iface= results= targets=
while [ $# -gt 0 ]; do
	case "$1" in
	--wait-for) wait_for=$2; shift 2 ;;
	--run-for) run_for=$2; shift 2 ;;
	--interface) iface=$2; shift 2 ;;
	--results-file) results=$2; shift 2 ;;
	--target-ips)
		shift
		while [ $# -gt 0 ] && [ "${1#--}" = "$1" ]; do targets="$targets $1"; shift; done
		;;
	*) echo "e2e-dos: unknown argument $1" >&2; exit 2 ;;
	esac
done
sleep "$wait_for"
sleep "$run_for"
printf '{"wait_for": "%s", "run_for": "%s", "targets": "%s", "interface": "%s"}\n' \
	"$wait_for" "$run_for" "${targets# }" "$iface" > "$results"
echo "e2e-dos finished: waited ${wait_for}s, ran ${run_for}s"
EOF
chmod 755 /usr/local/bin/e2e-dos
rm -f /tmp/e2e-dos.args /tmp/e2e-dos.json
command -v ettercap >/dev/null && echo ettercap=yes || echo ettercap=no
command -v hping3 >/dev/null && echo hping3=yes || echo hping3=no
command -v python3 >/dev/null && echo python3=yes || echo python3=no
echo ready=yes
`

// scorchNightEnv is what the checks share: the environment of the SCORCH CLI
// runs and the host-side fakes.
type scorchNightEnv struct {
	// env is the SCORCH CLI's environment. Components inherit it: HOME
	// holds the SSH key the ssh component logs in with (paramiko looks in
	// ~/.ssh), the collector runs `phenix config get` against this test's
	// store, and a cc file send that never lands fails in a minute.
	env []string

	// pub is the public half of that key, authorized for root in g1.
	pub string

	// services are the fake RSCAD automation and Elasticsearch endpoints.
	services *scorchNightServices
}

// TestScorchNightComponents boots three tools guests once and runs every
// SCORCH run of the scenario on them (SCN-01..SCN-06).
func TestScorchNightComponents(t *testing.T) {
	scorchNightRequire(t)
	requireImage(t, cfg.tools)

	n := scorchNightPrepare(t)

	exp := newExperiment(t, scorchNightName, scorchNightTopology(), n.scenarioYAML())

	began := time.Now()

	exp.start()
	waitCCClients(t, scorchNightName, "g1", "g2", "g3")
	t.Logf("the three guests registered with C2 %v after start", time.Since(began).Round(time.Second))

	scorchNightHostTap(t)
	scorchNightSetupGuests(t, n)

	t.Run("SCN-01 snort alerts while ettercap runs in another guest", n.checkMITM)
	t.Run("SCN-02 disruption dos waits out the attacker script's delay and run", n.checkDoS)
	t.Run("SCN-03 ssh commands and file fetches, trafficgen clients and server", n.checkRemote)
	t.Run("SCN-03b art fails clearly when goart is missing (REST)", scorchNightCheckArtMissing)
	t.Run("SCN-03c art honors goartPath, outputPath, env, inputs and the validator", n.checkArtStub)
	t.Run("SCN-04 ssh fails the stage when its remote work fails", n.checkSSHFail)
	t.Run("SCN-05 providerdata and rtds against a fake provider, RSCAD and Elasticsearch", n.checkProvider)
	t.Run("SCN-06 collector copies each guest's miniccc log", n.checkCollector)
}

// ---------------------------------------------------------------------------
// SCN-01: snort and ettercap

func (n *scorchNightEnv) checkMITM(t *testing.T) {
	const run = scorchNightRunMITM

	// g1 pings g2 through the whole run: the ICMP snort1's rule alerts on.
	ping := scorchShell(t, scorchNightName, "g1", "timeout 300 ping -q -i 0.5 "+scorchNightG2+" >/dev/null 2>&1 & echo started=yes")
	if ping["started"] != "yes" {
		t.Fatalf("starting the ping from g1 to g2: %v", ping)
	}

	p := n.start(t, run)

	// ett1 writes its start status file when its start stage ends; then only
	// the pause runs, so the guest checks below race no component's cc
	// filter.
	ettDir := scorchNightDir(run, "ett1")

	eventually(t, 4*time.Minute, time.Second, "ett1's start stage to finish", func() (bool, string) {
		if p.finished() {
			return true, ""
		}

		matches, _ := filepath.Glob(filepath.Join(ettDir, "start-*.json"))

		return len(matches) > 0, "no start status file in " + ettDir
	})

	if p.finished() {
		out, err := p.wait(t, time.Second)
		t.Fatalf("SCORCH run %d ended before its pause: %v\n%s", run, err, out)
	}

	// These checks set and clear the namespace's cc filter, so they must end
	// before wait45 does and ett1's and snort1's stops set their own.
	// eventually checks its budget only between tries, so each cc command
	// starts only within 25 s of the pause; one takes about 5 s.
	probeEnd := time.Now().Add(25 * time.Second)

	// The guards below end the last try early, so they carry the last full
	// probe result into the failure message.
	last := "none"

	eventually(t, 25*time.Second, 2*time.Second, "snort in g1 and ettercap in g3 during the pause", func() (bool, string) {
		if time.Now().After(probeEnd) {
			return false, "no time left in the pause for another cc command; last full probe: " + last
		}

		g1 := scorchShell(t, scorchNightName, "g1", "pgrep -x snort >/dev/null && echo snort=yes || echo snort=no")

		if time.Now().After(probeEnd) {
			return false, fmt.Sprintf("g1 %v, and no time left in the pause to ask g3; last full probe: %s", g1, last)
		}

		g3 := scorchShell(t, scorchNightName, "g3", "pgrep -x ettercap >/dev/null && echo ettercap=yes || echo ettercap=no")
		last = fmt.Sprintf("g1 %v, g3 %v", g1, g3)

		return g1["snort"] == "yes" && g3["ettercap"] == "yes", last
	})

	out, err := p.wait(t, scorchNightRunTimeout)

	if err != nil {
		t.Errorf("SCORCH run %d (mitm): %v\n%s", run, err, out)
	}

	scorchNightWantStages(t, run, "snort1/configure", "snort1/start", "ett1/start", "ett1/stop", "snort1/cleanup")

	scorchNightWantStage(t, run, "snort1", "stop")

	snortDir := scorchNightDir(run, "snort1")

	if alert := scorchRead(t, filepath.Join(snortDir, "alert")); !strings.Contains(alert, "E2E ICMP") {
		t.Errorf("snort1's alert file has no E2E ICMP alert:\n%s", alert)
	}

	if logs, _ := filepath.Glob(filepath.Join(snortDir, "snort.log*")); len(logs) == 0 {
		t.Errorf("snort1 collected no snort.log* packet log into %s", snortDir)
	}

	if stats := filepath.Join(snortDir, "snort-stats.jsonl"); scorchNightExists(stats) {
		if rows := scorchJSONLines(t, stats); len(rows) == 0 {
			t.Errorf("%s has no rows", stats)
		}
	} else {
		t.Errorf("snort1 wrote no snort-stats.jsonl into %s", snortDir)
	}

	scorchNightWantRun(t, run, "mitm", "snort1/loop-0-count-0/alert", "ett1/loop-0-count-0/stop-")

	// The stop stages kill both tools; snort may take a while to exit on
	// SIGINT. Once ettercap is gone the guests reach each other directly
	// again, which the later runs rely on.
	eventually(t, 2*time.Minute, 2*time.Second, "snort and ettercap to exit, and g1 to reach g2", func() (bool, string) {
		g1 := scorchShell(t, scorchNightName, "g1", "pgrep -x snort >/dev/null && echo snort=yes || echo snort=no; "+
			"ping -c 1 -W 2 "+scorchNightG2+" >/dev/null 2>&1 && echo reach=yes || echo reach=no")
		g3 := scorchShell(t, scorchNightName, "g3", "pgrep -x ettercap >/dev/null && echo ettercap=yes || echo ettercap=no")

		return g1["snort"] == "no" && g1["reach"] == "yes" && g3["ettercap"] == "no", fmt.Sprintf("g1 %v, g3 %v", g1, g3)
	})
}

// ---------------------------------------------------------------------------
// SCN-02: disruption dos

func (n *scorchNightEnv) checkDoS(t *testing.T) {
	const run = scorchNightRunDoS

	// The component waits start_delay + run_for + 60 s for the attacker script,
	// which here needs its own 2 s delay plus 3 s run; waiting only run_for
	// (3 s) would cut it short.
	if out, err := n.run(t, run); err != nil {
		t.Errorf("SCORCH run %d (dos): %v\n%s", run, err, out)
	}

	scorchNightWantStages(t, run, "dos1/configure", "dos1/cleanup")

	start := scorchNightWantStage(t, run, "dos1", "start")
	if !start.logged("Finished running DoS command") {
		t.Errorf("dos1's start stage did not log that the attacker script finished: %v", start.Logs)
	}

	dir := scorchNightDir(run, "dos1")

	began := scorchNightReadTime(t, filepath.Join(dir, "disruption_start_time.txt"))
	ended := scorchNightReadTime(t, filepath.Join(dir, "disruption_stop_time.txt"))

	if !began.IsZero() && !ended.IsZero() {
		if took := ended.Sub(began).Seconds(); took < scorchNightDoSDuration-0.5 {
			t.Errorf("dos1 disrupted for %.1f s, want its run_duration of %.0f s", took, scorchNightDoSDuration)
		}
	}

	// The component copies the attacker's results file and the script.
	var results map[string]string
	if scorchReadJSON(t, filepath.Join(dir, "e2e-dos.json"), &results) {
		want := map[string]string{"wait_for": "2.0", "run_for": "3.0", "targets": scorchNightG1, "interface": "eth0"}
		for k, v := range want {
			if results[k] != v {
				t.Errorf("attacker results %s = %q, want %q: %v", k, results[k], v, results)
			}
		}
	}

	scorchWantFile(t, filepath.Join(dir, "e2e-dos"))

	scorchNightWantRun(t, run, "dos", "dos1/loop-0-count-0/e2e-dos.json", "dos1/loop-0-count-0/disruption_stop_time.txt")

	// The cleanup stage deletes the results file; the guest runs commands in
	// order, so it has done that before this check runs.
	g3 := scorchShell(t, scorchNightName, "g3", "echo args=$(cat /tmp/e2e-dos.args); "+
		"test -e /tmp/e2e-dos.json && echo results=yes || echo results=no; "+
		"pgrep -x e2e-dos >/dev/null && echo running=yes || echo running=no")

	wantArgs := "--wait-for 2.0 --run-for 3.0 --target-ips " + scorchNightG1 + " --interface eth0 --results-file /tmp/e2e-dos.json"
	if g3["args"] != wantArgs {
		t.Errorf("the attacker script ran with %q, want %q", g3["args"], wantArgs)
	}

	if g3["results"] != "no" || g3["running"] != "no" {
		t.Errorf("g3 after dos1's cleanup: %v, want the results file deleted and no attacker script running", g3)
	}
}

// ---------------------------------------------------------------------------
// SCN-03: ssh and trafficgen

func (n *scorchNightEnv) checkRemote(t *testing.T) {
	const run = scorchNightRunRemote

	scorchNightWantSSH(t)

	if out, err := n.run(t, run); err != nil {
		t.Errorf("SCORCH run %d (remote): %v\n%s", run, err, out)
	}

	scorchNightWantStages(t, run, "ssh1/configure", "ssh1/cleanup",
		"tg1/configure", "tg1/start", "tg1/stop", "tg1/cleanup")

	for _, stage := range []string{"configure", "cleanup"} {
		scorchNightCheckSSHStage(t, run, "ssh1", stage, "hostname && test -f /etc/hostname", 0)

		dir := filepath.Join(scorchNightDir(run, "ssh1"), stage)

		if got := strings.TrimSpace(scorchRead(t, filepath.Join(dir, "hostname"))); got != "g1" {
			t.Errorf("ssh1 %s fetched g1's /etc/hostname as %q, want g1", stage, got)
		}

		scorchWantFile(t, filepath.Join(dir, "os-release"))
	}

	scorchNightCheckTrafficGen(t)

	scorchNightWantRun(t, run, "remote", "ssh1/loop-0-count-0/configure/hostname", "tg1/loop-0-count-0/stop-")
}

// scorchNightCheckTrafficGen checks what tg1 left in the guests: the server's
// counts from both generators, each generator's own arguments, the scripts
// deleted and the server stopped.
func scorchNightCheckTrafficGen(t *testing.T) {
	t.Helper()

	// The stop and cleanup stages queue their pkill and rm; each guest runs
	// its commands in order, so they ran before these checks.
	g1 := scorchShell(t, scorchNightName, "g1", "echo counts=$(cat /tmp/traffic-server.count); "+
		"test -e /traffic-server.py && echo script=yes || echo script=no; "+
		"pgrep -f 'traffic-server[.]py' >/dev/null && echo running=yes || echo running=no")

	var counts map[string]int
	if err := json.Unmarshal([]byte(g1["counts"]), &counts); err != nil {
		t.Errorf("g1's traffic server counts %q: %v", g1["counts"], err)
	}

	if got := counts[scorchNightG2+" background-gen"]; got < scorchNightMinBackground {
		t.Errorf("the traffic server counted %d datagrams from the background generator in g2, want at least %d: %v",
			got, scorchNightMinBackground, counts)
	}

	if got := counts[scorchNightG3+" malware-gen"]; got < scorchNightMinMalware {
		t.Errorf("the traffic server counted %d datagrams from the malware generator in g3, want at least %d: %v",
			got, scorchNightMinMalware, counts)
	}

	if g1["script"] != "no" || g1["running"] != "no" {
		t.Errorf("g1 after tg1's stop and cleanup: %v, want the server stopped and /traffic-server.py deleted", g1)
	}

	// Each generator gets its own client's settings (the malware one from
	// malwareClient, not backgroundClient).
	for vm, want := range map[string]struct {
		script string
		args   map[string]any
	}{
		"g2": {"background-gen", map[string]any{"ip": scorchNightG1, "rate": 20.0, "duration": 10.0, "probability": 0.5}},
		"g3": {"malware-gen", map[string]any{"ip": scorchNightG1, "rate": 10.0, "duration": 10.0, "probability": 0.75}},
	} {
		got := scorchShell(t, scorchNightName, vm, "echo args=$(cat /tmp/"+want.script+".args); "+
			"test -e /"+want.script+".py && echo script=yes || echo script=no")

		var args map[string]any
		if err := json.Unmarshal([]byte(got["args"]), &args); err != nil {
			t.Errorf("%s's %s arguments %q: %v", vm, want.script, got["args"], err)
		} else if !scorchNightSameJSON(args, want.args) {
			t.Errorf("%s ran %s with %v, want %v", vm, want.script, args, want.args)
		}

		if got["script"] != "no" {
			t.Errorf("tg1's cleanup left /%s.py in %s", want.script, vm)
		}
	}
}

// ---------------------------------------------------------------------------
// SCN-04: ssh failures

func (n *scorchNightEnv) checkSSHFail(t *testing.T) {
	const run = scorchNightRunSSHFail

	scorchNightWantSSH(t)

	if out, err := n.run(t, run); err == nil {
		t.Errorf("SCORCH run %d (ssh-fail) succeeded, want it to fail on the remote false and the missing file\n%s", run, out)
	}

	st := scorchNightStage(t, run, "ssh2", "start")

	for _, want := range []string{
		"command 'hostname && false' on " + scorchNightG1 + " failed with exit status 1",
		"fetching '/nonexistent/e2e-file' from " + scorchNightG1 + " failed",
	} {
		if !strings.Contains(st.failure(), want) {
			t.Errorf("ssh2's start stage returned %v, want an error containing %q", st.Return, want)
		}
	}

	// The other file is still fetched and the metadata still written.
	if got := strings.TrimSpace(scorchRead(t, filepath.Join(scorchNightDir(run, "ssh2"), "start", "hostname"))); got != "g1" {
		t.Errorf("ssh2 fetched g1's /etc/hostname as %q, want g1", got)
	}

	scorchNightCheckSSHStage(t, run, "ssh2", "start", "hostname && false", 1)

	scorchNightWantRun(t, run, "ssh-fail", "ssh2/loop-0-count-0/ssh_metadata_start.json")
}

// scorchNightCheckSSHStage checks the metadata an ssh component saved for a
// stage: the remote command, its output and its exit status.
func scorchNightCheckSSHStage(t *testing.T, run int, cmp, stage, cmd string, status int) {
	t.Helper()

	var entries []struct {
		Event string         `json:"event"`
		Data  map[string]any `json:"data"`
	}

	path := filepath.Join(scorchNightDir(run, cmp), "ssh_metadata_"+stage+".json")
	if !scorchReadJSON(t, path, &entries) {
		return
	}

	for _, e := range entries {
		if _, ok := e.Data["exit_status"]; !ok {
			continue
		}

		if e.Data["cmd"] != cmd || e.Data["stdout"] != "g1" || e.Data["exit_status"] != float64(status) {
			t.Errorf("%s: remote run %v, want command %q printing g1 and exiting %d", path, e.Data, cmd, status)
		}

		return
	}

	t.Errorf("%s records no remote command result: %v", path, entries)
}

// ---------------------------------------------------------------------------
// SCN-03b, SCN-03c: art

// scorchNightCheckArtMissing runs art over REST against g1, which has no goart
// at the default path. phenix ui runs it with its own environment, which art
// needs nothing from.
func scorchNightCheckArtMissing(t *testing.T) {
	const (
		run  = scorchNightRunArtMissing
		want = "goart not found or not executable at /phenix/art/goart on g1"
	)

	path := fmt.Sprintf("/experiments/%s/scorch/pipelines/%d", scorchNightName, run)

	if code, body := api(t, http.MethodPost, path, nil); code != http.StatusNoContent {
		t.Fatalf("POST %s: status %d, want 204: %s", path, code, body)
	}

	// If a check fails while the run is still going, cancel it, as the CLI
	// runs' cleanup does, so phenix tears it down and clears its running flag
	// before the next run starts.
	t.Cleanup(func() {
		if !t.Failed() || !scorchNightRunning(t) {
			return
		}

		_, _ = api(t, http.MethodDelete, path, nil)

		eventually(t, scorchNightStopTimeout, 2*time.Second, "the canceled REST run to end", func() (bool, string) {
			return !scorchNightRunning(t), "status.appRunningStageStatus.scorch is still true"
		})
	})

	eventually(t, scorchNightRunTimeout, time.Second, "the REST run to finish", func() (bool, string) {
		matches, _ := filepath.Glob(scorchNightRecordPattern(run))

		return len(matches) == 1 && !scorchNightRunning(t), fmt.Sprintf("run records %v", matches)
	})

	var body struct {
		Pipeline []struct {
			Name   string `json:"name"`
			Stage  string `json:"stage"`
			Status string `json:"status"`
		} `json:"pipeline"`
	}

	apiOK(t, http.MethodGet, path+"/0", nil, &body)

	status := ""

	for _, node := range body.Pipeline {
		if node.Name == "art0" && node.Stage == "start" {
			status = node.Status
		}
	}

	if status != "failure" {
		t.Errorf("phenix ui reports art0's start as %q, want failure: %+v", status, body.Pipeline)
	}

	var output struct {
		Output string `json:"output"`
	}

	apiOK(t, http.MethodGet, fmt.Sprintf("/experiments/%s/scorch/components/%d/0/start/art0", scorchNightName, run), nil, &output)

	if !strings.Contains(output.Output, want) {
		t.Errorf("art0's start output does not say %q:\n%s", want, output.Output)
	}

	if st := scorchNightStage(t, run, "art0", "start"); !strings.Contains(st.failure(), want) {
		t.Errorf("art0's start stage returned %v, want an error containing %q", st.Return, want)
	}

	scorchNightWantRun(t, run, "art-missing", "art0/loop-0-count-0/start-")
}

// checkArtStub runs art against the goart stand-in in g1. It checks
// the component's side of goart's contract: the binary and output paths it is
// given, the arguments it passes, the results it fetches and validates.
func (n *scorchNightEnv) checkArtStub(t *testing.T) {
	const run = scorchNightRunArtStub

	if out, err := n.run(t, run); err != nil {
		t.Errorf("SCORCH run %d (art-stub): %v\n%s", run, err, out)
	}

	st := scorchNightWantStage(t, run, "art1", "start")
	if !st.logged("results are valid") {
		t.Errorf("art1 did not log that the validator passed: %v", st.Logs)
	}

	var result struct {
		Executor struct {
			ExecutedCommand struct {
				Results string `json:"results"`
			} `json:"ExecutedCommand"`
		} `json:"Executor"`
	}

	if scorchReadJSON(t, filepath.Join(scorchNightDir(run, "art1"), "g1.json"), &result) &&
		!strings.Contains(result.Executor.ExecutedCommand.Results, "Linux") {
		t.Errorf("art1 fetched results %q, want the stand-in's uname output", result.Executor.ExecutedCommand.Results)
	}

	argv := strings.Fields(guestShell(t, scorchNightName, "g1", "cat /var/tmp/e2e-art/argv"))

	out := ""
	if i := slices.Index(argv, "-o"); i >= 0 && i+1 < len(argv) {
		out = argv[i+1]
	}

	if !strings.HasPrefix(out, "/var/tmp/e2e-art/") || !strings.HasSuffix(out, ".json") {
		t.Errorf("art1 told goart to write %q, want a JSON file under its outputPath /var/tmp/e2e-art: %v", out, argv)
	}

	want := []string{"-q", "-f", "json", "-t", "T1082", "-o", out, "-i", "0", "--env", "E2E_ENV=night", "--input", "e2e_input=hello"}
	if !slices.Equal(argv, want) {
		t.Errorf("art1 ran goart with %q, want %q", argv, want)
	}

	scorchNightWantRun(t, run, "art-stub", "art1/loop-0-count-0/g1.json")
}

// ---------------------------------------------------------------------------
// SCN-05: providerdata and rtds

func (n *scorchNightEnv) checkProvider(t *testing.T) {
	const run = scorchNightRunProvider

	before := n.services.requests()

	if out, err := n.run(t, run); err != nil {
		t.Errorf("SCORCH run %d (provider): %v\n%s", run, err, out)
	}

	scorchNightWantStages(t, run, "pd1/configure", "pd1/start", "pd1/stop",
		"rt1/configure", "rt1/start", "rt1/stop", "rt1/cleanup")

	pd := scorchNightDir(run, "pd1")
	rt := scorchNightDir(run, "rt1")

	for _, f := range []string{
		filepath.Join(pd, "config.ini"), filepath.Join(pd, "provider.yaml"),
		filepath.Join(rt, "config.ini"), filepath.Join(rt, "rtds_config.yaml"),
	} {
		scorchWantFile(t, f)
	}

	for _, dir := range []string{pd, rt} {
		csv := scorchRead(t, filepath.Join(dir, "provider_data", "bus.csv"))
		if !strings.Contains(csv, ",60.0") {
			t.Errorf("%s/provider_data/bus.csv does not hold the provider's CSV rows:\n%s", dir, csv)
		}
	}

	// providerdata starts the provider; rtds finds it running, stops it in
	// its stop stage, and finds nothing to stop at cleanup, which also
	// deletes the CSV directory.
	g1 := scorchShell(t, scorchNightName, "g1", "echo calls=$(tr '\\n' '|' </tmp/e2e-provider.calls); "+
		"pgrep -f '^pybennu-power-solver' >/dev/null && echo running=yes || echo running=no; "+
		"test -e /root/provider_data && echo csv=yes || echo csv=no")

	if want := "-d start|stop|"; g1["calls"] != want {
		t.Errorf("the fake provider was called as %q, want %q (providerdata starts it, rtds stops it once)", g1["calls"], want)
	}

	if g1["running"] != "no" || g1["csv"] != "no" {
		t.Errorf("g1 after rt1's stop and cleanup: %v, want the provider stopped and /root/provider_data deleted", g1)
	}

	reqs := n.services.requests()[len(before):]

	for _, want := range []struct {
		what, prefix, suffix string
		count                int
	}{
		{"providerdata's Elasticsearch doc count", "GET /e2e-provider-", "/_stats", 1},
		{"rtds's two Elasticsearch doc counts", "GET /e2e-rtds-", "/_stats", 2},
		{"rtds's frequency and time drift searches", "POST /e2e-rtds-", "/_search", 2},
		{"rtds starting the RSCAD case at configure", "POST /rscad/start_case", "", 1},
		{"rtds stopping the RSCAD case at stop and cleanup", "POST /rscad/stop_case", "", 2},
	} {
		got := 0

		for _, r := range reqs {
			if strings.HasPrefix(r, want.prefix) && strings.HasSuffix(r, want.suffix) {
				got++
			}
		}

		if got != want.count {
			t.Errorf("%s: %d requests %s*%s, want %d; requests: %v", want.what, got, want.prefix, want.suffix, want.count, reqs)
		}
	}

	scorchNightWantRun(t, run, "provider", "pd1/loop-0-count-0/provider_data/bus.csv", "rt1/loop-0-count-0/rtds_config.yaml")
}

// ---------------------------------------------------------------------------
// SCN-06: collector miniccc logs

func (n *scorchNightEnv) checkCollector(t *testing.T) {
	const run = scorchNightRunCollector

	// The collector reads /var/log/miniccc.log, where the miniccc unit
	// `phenix image create` writes logs to (api/image/constants.go), and
	// falls back to /miniccc.log. The unit `phenix image inject-miniexe`
	// installs, which the images recipes use, logs to the journal instead,
	// so record what the image does and then append a marker for each guest
	// to /var/log/miniccc.log, creating it where the image has none.
	for _, vm := range []string{"g1", "g2", "g3"} {
		got := scorchShell(t, scorchNightName, vm, "test -e /var/log/miniccc.log && echo image_log=yes || echo image_log=no; "+
			"test -e /miniccc.log && echo root_log=yes || echo root_log=no; "+
			"echo e2e-miniccc-marker-"+vm+" >> /var/log/miniccc.log && echo ready=yes")

		// The marker tells the two logs apart, so a /miniccc.log only needs
		// recording: a collector that read it instead would miss the marker.
		t.Logf("%s: the image's miniccc writes /var/log/miniccc.log: %s, /miniccc.log exists: %s", vm, got["image_log"], got["root_log"])

		if got["ready"] != "yes" {
			t.Fatalf("writing the miniccc log marker in %s: %v", vm, got)
		}
	}

	if out, err := n.run(t, run); err != nil {
		t.Errorf("SCORCH run %d (collector): %v\n%s", run, err, out)
	}

	scorchNightWantStages(t, run, "dis5/start", "col5/stop")

	dir := scorchNightDir(run, "col5")

	for _, vm := range []string{"g1", "g2", "g3"} {
		path := filepath.Join(dir, "miniccc", vm+"_miniccc.log")

		if log := scorchRead(t, path); !strings.Contains(log, "e2e-miniccc-marker-"+vm) {
			t.Errorf("%s does not hold %s's /var/log/miniccc.log (no marker):\n%s", path, vm, log)
		}
	}

	var record struct {
		Experiment struct {
			Disruption string  `json:"disruption"`
			Duration   float64 `json:"duration"`
		} `json:"experiment"`
		Sceptre struct {
			Experiment string `json:"experiment"`
		} `json:"sceptre"`
		Scorch struct {
			Run int `json:"run"`
		} `json:"scorch"`
	}

	if scorchReadJSON(t, filepath.Join(dir, "experiment_results", "experiment_record.json"), &record) {
		if record.Experiment.Disruption != "baseline" || record.Experiment.Duration != 5 ||
			record.Sceptre.Experiment != scorchNightName || record.Scorch.Run != run {
			t.Errorf("experiment record %+v, want a 5 s baseline of %s in run %d", record, scorchNightName, run)
		}
	}

	scorchNightWantRun(t, run, "collector", "col5/loop-0-count-0/miniccc/g1_miniccc.log",
		"col5/loop-0-count-0/experiment_results/experiment_record.json")
}

// ---------------------------------------------------------------------------
// setup

// scorchNightRequire fails unless the E2E workflow installed the apps and the
// host tools this shard uses.
func scorchNightRequire(t *testing.T) {
	t.Helper()

	components := []string{"art", "collector", "disruption", "ettercap", "providerdata", "rtds", "snort", "ssh", "trafficgen"}
	commands := make([]string, 0, len(components)+3)
	commands = append(commands, "phenix", "ssh-keygen", "ip")

	for _, c := range components {
		commands = append(commands, "phenix-scorch-component-"+c)
	}

	requireApps(t, "scorch-night", commands...)
}

// scorchNightPrepare writes the host fixtures, starts the host-side fakes and
// creates the SSH key, and removes the fixtures and the experiment's SCORCH
// output when the test ends, keeping a copy of the output in the work
// directory when the test failed.
func scorchNightPrepare(t *testing.T) *scorchNightEnv {
	t.Helper()

	if err := os.MkdirAll(scorchNightFixtures, 0o755); err != nil { //nolint:gosec // read by phenix and minimega
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = os.RemoveAll(scorchNightFixtures) })

	for name, content := range map[string]string{
		"snort.conf":        scorchNightSnortConf,
		"traffic-server.py": scorchNightTrafficServer,
		"background-gen.py": scorchNightTrafficGen,
		"malware-gen.py":    scorchNightTrafficGen,
	} {
		if err := os.WriteFile(filepath.Join(scorchNightFixtures, name), []byte(content), 0o644); err != nil { //nolint:gosec // guest file
			t.Fatal(err)
		}
	}

	// Registered before newExperiment's teardown, so it runs after it.
	t.Cleanup(func() {
		files := filepath.Join(cfg.imagesDir, scorchNightName, "files")

		if t.Failed() {
			keep := filepath.Join(cfg.workDir, t.Name(), "scorch-files")

			if err := os.MkdirAll(filepath.Dir(keep), 0o750); err == nil {
				if err := run("cp", "-a", files, keep); err != nil {
					t.Logf("keeping the SCORCH output: %v", err)
				}
			}
		}

		_ = os.RemoveAll(filepath.Join(cfg.imagesDir, scorchNightName))
	})

	home := t.TempDir()
	key := filepath.Join(home, ".ssh", "id_ed25519")

	if err := os.MkdirAll(filepath.Dir(key), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := run("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "e2e-scorch-night", "-f", key); err != nil {
		t.Fatal(err)
	}

	pub, err := os.ReadFile(key + ".pub") //nolint:gosec // test key
	if err != nil {
		t.Fatal(err)
	}

	return &scorchNightEnv{
		env: append(os.Environ(),
			"HOME="+home,
			"PHENIX_STORE_ENDPOINT="+cfg.store,
			"PHENIX_CC_SEND_GRACE=60",
		),
		pub:      strings.TrimSpace(string(pub)),
		services: scorchNightServe(t),
	}
}

func (n *scorchNightEnv) scenarioYAML() string {
	return strings.NewReplacer("{{fixtures}}", scorchNightFixtures, "{{services}}", n.services.url).Replace(scorchNightScenario)
}

// scorchNightTopology is three tools guests on VLAN LAN.
func scorchNightTopology() string {
	return topology(
		node("g1", cfg.tools, "LAN", scorchNightG1),
		node("g2", cfg.tools, "LAN", scorchNightG2),
		node("g3", cfg.tools, "LAN", scorchNightG3),
	)
}

// scorchNightHostTap puts the host on VLAN LAN with a minimega host tap in the
// experiment's namespace: the ssh component connects to g1 from the host's
// own network namespace, and no SCORCH tap reaches it there (the tap
// component always uses a network namespace of its own).
func scorchNightHostTap(t *testing.T) {
	t.Helper()

	bridge := scorchNightFirst(vmInfo(t, scorchNightName)["g1"]["bridge"])
	if bridge == "" {
		bridge = "phenix"
	}

	mm(t, scorchNightName, fmt.Sprintf("tap create LAN bridge %s ip %s/24 %s", bridge, scorchNightTapAddr, scorchNightTap))

	// Registered after newExperiment's teardown, so it runs before it.
	t.Cleanup(func() {
		if _, err := mmRun(scorchNightName, "tap delete "+scorchNightTap); err != nil {
			t.Logf("deleting host tap %s: %v", scorchNightTap, err)
		}
	})

	out, err := exec.Command("ip", "-4", "-o", "addr", "show", "dev", scorchNightTap).CombinedOutput()
	if err != nil || !strings.Contains(string(out), scorchNightTapAddr+"/24") {
		t.Fatalf("host tap %s does not have %s/24: %v\n%s", scorchNightTap, scorchNightTapAddr, err, out)
	}
}

// scorchNightSetupGuests installs the guest-side fixtures and checks that the
// tools the components run are in the image.
func scorchNightSetupGuests(t *testing.T, n *scorchNightEnv) {
	t.Helper()

	g1 := scorchShell(t, scorchNightName, "g1", fmt.Sprintf(scorchNightG1Setup, n.pub))
	t.Logf("g1: the image's own ntpq -p: %s", g1["image_ntpq"])

	g3 := scorchShell(t, scorchNightName, "g3", scorchNightG3Setup)
	g2 := scorchShell(t, scorchNightName, "g2", "command -v python3 >/dev/null && echo python3=yes || echo python3=no; echo ready=yes")

	for _, check := range []struct {
		vm   string
		got  map[string]string
		want []string
	}{
		{"g1", g1, []string{"ready", "snort", "python3", "sshd"}},
		{"g2", g2, []string{"ready", "python3"}},
		{"g3", g3, []string{"ready", "ettercap", "hping3", "python3"}},
	} {
		// ready means every fixture was written (the scripts stop at the first
		// failure); a missing tool fails only the checks that use it, so it
		// is reported without stopping the others.
		if check.got["ready"] != "yes" {
			t.Fatalf("setting up %s: the setup script failed: %v", check.vm, check.got)
		}

		for _, k := range check.want[1:] {
			if check.got[k] != "yes" {
				t.Errorf("setting up %s: %s=%q, want yes: %v", check.vm, k, check.got[k], check.got)
			}
		}
	}
}

// scorchNightWantSSH waits until the host reaches g1's SSH port through the
// host tap.
func scorchNightWantSSH(t *testing.T) {
	t.Helper()

	addr := net.JoinHostPort(scorchNightG1, "22")

	eventually(t, time.Minute, 2*time.Second, "the host to reach "+addr, func() (bool, string) {
		conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
		if err != nil {
			return false, err.Error()
		}

		_ = conn.Close()

		return true, ""
	})
}

// ---------------------------------------------------------------------------
// host-side fakes: RSCAD automation and Elasticsearch

// scorchNightServices serves, on the host, what rtds and providerdata reach
// from the host: RSCAD's GUI automation (POST /rscad/start_case and
// /rscad/stop_case) and Elasticsearch (GET / for the client's info call,
// GET /<index>/_stats with a doc count growing at scorchNightDocRate, and
// POST /<index>/_search with a 60 Hz measurement and a 5 ms time drift). It
// records every request.
type scorchNightServices struct {
	url   string
	began time.Time

	mu   sync.Mutex
	reqs []string
}

func scorchNightServe(t *testing.T) *scorchNightServices {
	t.Helper()

	var lc net.ListenConfig

	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	s := &scorchNightServices{url: "http://" + ln.Addr().String(), began: time.Now()}
	srv := &http.Server{Handler: http.HandlerFunc(s.serve), ReadHeaderTimeout: 10 * time.Second}

	go func() { _ = srv.Serve(ln) }()

	t.Cleanup(func() { _ = srv.Close() })

	return s
}

func (s *scorchNightServices) serve(w http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)

	s.mu.Lock()
	s.reqs = append(s.reqs, r.Method+" "+r.URL.Path)
	s.mu.Unlock()

	var (
		path  = r.URL.Path
		index = strings.Split(strings.TrimPrefix(path, "/"), "/")[0]
		body  any
	)

	switch {
	case path == "/rscad/start_case" || path == "/rscad/stop_case":
		body = map[string]any{
			"success":    true,
			"status":     strings.TrimSuffix(strings.TrimPrefix(path, "/rscad/"), "_case"),
			"case_file":  "C:/RSCAD/e2e-case.dtp",
			"case_title": "e2e",
		}
	case strings.HasSuffix(path, "/_stats"):
		count := 1000 + int(time.Since(s.began).Seconds()*scorchNightDocRate)
		body = map[string]any{
			"indices": map[string]any{index: map[string]any{"total": map[string]any{"docs": map[string]any{"count": count}}}},
		}
	case strings.HasSuffix(path, "/_search"):
		body = map[string]any{
			"took":      1,
			"timed_out": false,
			"hits": map[string]any{
				"total": map[string]any{"value": 1, "relation": "eq"},
				"hits": []any{map[string]any{
					"_index":  index,
					"_id":     "e2e",
					"_source": map[string]any{"measurement": map[string]any{"frequency": 60.0}},
				}},
			},
			"aggregations": map[string]any{"timestamp_diff": map[string]any{"value": 5.0}},
		}
	case path == "/":
		body = map[string]any{
			"name":         "e2e",
			"cluster_name": "e2e",
			"cluster_uuid": "e2e",
			"version":      map[string]any{"number": "8.15.0", "build_flavor": "default"},
			"tagline":      "You Know, for Search",
		}
	default:
		http.NotFound(w, r)

		return
	}

	w.Header().Set("Content-Type", "application/json")
	// The Elasticsearch client refuses servers that do not name the product.
	w.Header().Set("X-Elastic-Product", "Elasticsearch")
	_ = json.NewEncoder(w).Encode(body)
}

// requests returns the requests served so far, as "METHOD /path".
func (s *scorchNightServices) requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return slices.Clone(s.reqs)
}

// ---------------------------------------------------------------------------
// running SCORCH

func (n *scorchNightEnv) command(ctx context.Context, run int) *exec.Cmd {
	cmd := exec.CommandContext(ctx, cfg.phenix, //nolint:gosec // test harness
		phenixArgs("experiment", "scorch", scorchNightName, "--run", strconv.Itoa(run))...)
	cmd.Env = n.env

	// On a timeout, cancel the run the way an operator does (SIGTERM), so
	// phenix runs its stop and cleanup stages and clears
	// status.appRunningStageStatus.scorch. A SIGKILL would leave that flag set
	// and phenix would refuse every later run as already running.
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = scorchNightStopTimeout

	return cmd
}

// run runs a SCORCH run with the CLI and waits for it.
func (n *scorchNightEnv) run(t *testing.T, run int) (string, error) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), scorchNightRunTimeout)
	defer cancel()

	out, err := n.command(ctx, run).CombinedOutput()

	return string(out), err
}

// start starts a SCORCH run with the CLI. If the test ends first, the cleanup
// sends the CLI SIGTERM, which cancels the run and still runs its stop and
// cleanup stages.
func (n *scorchNightEnv) start(t *testing.T, run int) *scorchProc {
	t.Helper()

	p := &scorchProc{phenixProc: phenixProc{done: make(chan struct{})}, cmd: n.command(context.Background(), run)}
	p.cmd.Stdout = p
	p.cmd.Stderr = p

	if err := startChild(p.cmd); err != nil {
		t.Fatalf("starting SCORCH run %d: %v", run, err)
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
		case <-time.After(scorchNightStopTimeout):
			_ = p.cmd.Process.Kill()

			<-p.done
		}
	})

	return p
}

// ---------------------------------------------------------------------------
// SCORCH output

func scorchNightFilesDir() string {
	return filepath.Join(cfg.imagesDir, scorchNightName, "files")
}

func scorchNightRunDir(run int) string {
	return filepath.Join(scorchNightFilesDir(), "scorch", "run-"+strconv.Itoa(run))
}

// scorchNightDir is where a component writes its output in a run (loop 0,
// count 0).
func scorchNightDir(run int, cmp string) string {
	return filepath.Join(scorchNightRunDir(run), cmp, "loop-0-count-0")
}

func scorchNightRecordPattern(run int) string {
	return filepath.Join(scorchNightRunDir(run), fmt.Sprintf("%s-scorch-run-%d-*.json", scorchNightName, run))
}

// scorchNightStage reads the status file a component wrote for a stage of a
// run, checking that it names that run and stage.
func scorchNightStage(t *testing.T, run int, cmp, stage string) scorchStatus {
	t.Helper()

	var st scorchStatus

	matches, _ := filepath.Glob(filepath.Join(scorchNightDir(run, cmp), stage+"-*.json"))
	if len(matches) != 1 {
		t.Errorf("run %d: want one %s status file from %s, have %v", run, stage, cmp, matches)

		return st
	}

	if !scorchReadJSON(t, matches[0], &st) {
		return st
	}

	if st.Experiment != scorchNightName || st.Run != run || st.Component != cmp || st.Stage != stage || st.Loop != 0 || st.Count != 0 {
		t.Errorf("%s names experiment %q run %d component %q stage %q loop %d count %d",
			matches[0], st.Experiment, st.Run, st.Component, st.Stage, st.Loop, st.Count)
	}

	return st
}

// scorchNightWantStage checks that a component ran a stage of a run and
// succeeded.
func scorchNightWantStage(t *testing.T, run int, cmp, stage string) scorchStatus {
	t.Helper()

	st := scorchNightStage(t, run, cmp, stage)
	if msg := st.failure(); msg != "" {
		t.Errorf("run %d: %s %s failed: %s", run, cmp, stage, msg)
	}

	return st
}

// scorchNightWantStages calls scorchNightWantStage for each "component/stage".
func scorchNightWantStages(t *testing.T, run int, stages ...string) {
	t.Helper()

	for _, s := range stages {
		cmp, stage, _ := strings.Cut(s, "/")
		scorchNightWantStage(t, run, cmp, stage)
	}
}

// scorchNightWantRun checks what phenix leaves for every run: the run record,
// an archive of the run directory with entries starting with each of entries,
// the experiment's SCORCH status cleared, and the namespace still there.
func scorchNightWantRun(t *testing.T, run int, name string, entries ...string) {
	t.Helper()

	var rec struct {
		Experiment struct {
			Name string `json:"name"`
		} `json:"experiment"`
		Run struct {
			Name  string `json:"name"`
			Index int    `json:"index"`
		} `json:"run"`
		Minimega string `json:"minimega_version"`
	}

	matches, _ := filepath.Glob(scorchNightRecordPattern(run))
	if len(matches) != 1 {
		t.Errorf("run %d: want one run record matching %s, have %v", run, scorchNightRecordPattern(run), matches)
	} else if scorchReadJSON(t, matches[0], &rec) {
		if rec.Experiment.Name != scorchNightName || rec.Run.Index != run || rec.Run.Name != name {
			t.Errorf("run record %+v, want experiment %s run %d named %s", rec, scorchNightName, run, name)
		}

		if v := mmText(t, "", "version"); rec.Minimega != v {
			t.Errorf("run record minimega_version %q, want minimega's %q", rec.Minimega, v)
		}

		entries = append(entries, filepath.Base(matches[0]))
	}

	// Every run adds an archive and none is removed; the newest is this run's.
	archives, _ := filepath.Glob(filepath.Join(scorchNightFilesDir(), fmt.Sprintf("scorch-run-%d_*.tgz", run)))
	slices.Sort(archives)

	if len(archives) == 0 {
		t.Errorf("run %d left no archive in %s", run, scorchNightFilesDir())
	} else {
		listed := scorchArchive(t, archives[len(archives)-1])

		for _, want := range entries {
			if !slices.ContainsFunc(listed, func(e string) bool { return strings.HasPrefix(e, want) }) {
				t.Errorf("run %d archive has no %s*: %v", run, want, listed)
			}
		}
	}

	if scorchNightRunning(t) {
		t.Errorf("status.appRunningStageStatus.scorch is still true after run %d", run)
	}

	if _, ok := mmNamespaces(t)[scorchNightName]; !ok {
		t.Errorf("minimega namespace %s is gone after run %d", scorchNightName, run)
	}
}

// scorchNightRunning reports status.appRunningStageStatus.scorch, and fails
// the test when status.apps.scorch outlived a run.
func scorchNightRunning(t *testing.T) bool {
	t.Helper()

	out := phenix(t, "config", "get", "experiment/"+scorchNightName, "--output", "json")

	var c struct {
		Status struct {
			Running map[string]any `json:"appRunningStageStatus"`
			Apps    map[string]any `json:"apps"`
		} `json:"status"`
	}

	if err := json.Unmarshal([]byte(jsonPart(out)), &c); err != nil {
		t.Fatalf("decoding experiment %s: %v\n%s", scorchNightName, err, out)
	}

	if running, _ := c.Status.Running["scorch"].(bool); running {
		return true
	}

	if v, ok := c.Status.Apps["scorch"]; ok && v != nil {
		t.Errorf("status.apps.scorch = %v after the run, want it removed", v)
	}

	return false
}

// ---------------------------------------------------------------------------
// files and guests

func scorchNightExists(path string) bool {
	_, err := os.Stat(path)

	return err == nil
}

// scorchNightReadTime reads a timestamp file the disruption component writes
// (Python isoformat, UTC offset included).
func scorchNightReadTime(t *testing.T, path string) time.Time {
	t.Helper()

	s := strings.TrimSpace(scorchRead(t, path))
	if s == "" {
		return time.Time{}
	}

	ts, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Errorf("%s holds %q, not a timestamp: %v", path, s, err)
	}

	return ts
}

// scorchNightSameJSON reports whether two decoded JSON objects are equal.
func scorchNightSameJSON(a, b map[string]any) bool {
	ja, errA := json.Marshal(a)
	jb, errB := json.Marshal(b)

	return errA == nil && errB == nil && bytes.Equal(ja, jb)
}

// scorchNightFirst is the first element of a minimega list column.
func scorchNightFirst(s string) string {
	s = strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(s), "["), "]")
	first, _, _ := strings.Cut(s, ",")

	return strings.TrimSpace(first)
}
