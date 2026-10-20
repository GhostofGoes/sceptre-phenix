//go:build e2e

package e2e

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Runtime tier for the domain apps of sceptre-phenix-apps, on the images they
// are written for: ot-sim and helics
// (phenix-app-ot-sim, phenix-app-helics) and wind-turbine on ot-sim.qc2 with
// HELICS brokers on bennu.qc2, and sceptre's Linux provider and field devices
// on bennu.qc2. Each experiment boots at most five 2048 MB guests and checks
// the data the apps configure as it flows between the guests, with oracles
// that run inside the guests: OT-sim's CPU REST API, raw Modbus reads, the
// HELICS root broker's web API and pybennu-probe.
//
// Not covered here: caldera (TestImagesSmoke checks its caldera.qc2 server),
// ignition and sceptre's Windows device types (Windows images and licensed
// SCADA software), and the Node-RED parts of ot-sim and wind-turbine (the
// ot-sim recipe installs no Node-RED).

const (
	domrtOTSimImage = "ot-sim.qc2"
	domrtBennuImage = "bennu.qc2"

	// domrtBoot bounds how long up to five domain guests take to register
	// with C2 when they boot together on the runner's four vCPUs.
	domrtBoot = 6 * time.Minute

	// domrtProbeCall bounds one in-guest probe run, which must answer well
	// within the two minutes ccExec waits for a C2 response.
	domrtProbeCall = 60 * time.Second

	// domrtCase300SHA256 is PYPOWER 5.1.19's case300.py, which pybennu pins
	// and which is byte-identical to sceptre-phenix-topologies'
	// soap-legacy/injects/case300.py.
	domrtCase300SHA256 = "5790108c91a2a5b99cb0dde5272dabdc2c6b846db72ea5e807b2080aa0fb757c"
)

// ---------------------------------------------------------------------------
// ot-sim, helics and wind-turbine

// TestDomainRuntimeOtsim boots the ot-sim and helics apps' field devices with
// a HELICS federation that runs through a sub-broker, then the wind-turbine
// app's turbine, one experiment after the other.
func TestDomainRuntimeOtsim(t *testing.T) {
	requireApps(t, "domain runtime", "phenix-app-ot-sim", "phenix-app-helics", "phenix-app-wind-turbine")
	requireImage(t, domrtOTSimImage)
	requireImage(t, domrtBennuImage)

	t.Run("HELICS federation through a sub-broker", domrtOTSimHELICS)
	t.Run("wind turbine from the topologies repository", domrtWindTurbine)
}

// domrtOTSimTopology has the HELICS root broker on broker, a sub-broker and
// the test's simulator federate on sub (whose hand-written helics/federate
// annotation names its broker as host|iface), and the
// ot-sim field devices: rtu-1 (fd-server), fep-1 (fep) and client-1
// (fd-client). The first %s is the simulator script, the second its startup
// script, which sorts after the helics app's 90-helics-broker.sh.
const domrtOTSimTopology = `apiVersion: phenix.sandia.gov/v1
kind: Topology
metadata:
  name: {{name}}
spec:
  nodes:
  - type: VirtualMachine
    general:
      hostname: broker
    hardware:
      vcpus: 1
      memory: 2048
      os_type: linux
      drives:
      - image: bennu.qc2
    network:
      interfaces:
      - name: eth0
        type: ethernet
        proto: static
        vlan: OT
        address: 10.20.0.5
        mask: 24
  - type: VirtualMachine
    general:
      hostname: sub
    annotations:
      helics/federate:
      - broker: sub|eth0
        fed-count: 1
    hardware:
      vcpus: 1
      memory: 2048
      os_type: linux
      drives:
      - image: bennu.qc2
    network:
      interfaces:
      - name: eth0
        type: ethernet
        proto: static
        vlan: OT
        address: 10.20.0.12
        mask: 24
    injections:
    - src: %s
      dst: /root/e2e-helics-pub.py
    - src: %s
      dst: /etc/phenix/startup/95-e2e-pub.sh
  - type: VirtualMachine
    general:
      hostname: rtu-1
    hardware:
      vcpus: 1
      memory: 2048
      os_type: linux
      drives:
      - image: ot-sim.qc2
    network:
      interfaces:
      - name: eth0
        type: ethernet
        proto: static
        vlan: OT
        address: 10.20.0.11
        mask: 24
  - type: VirtualMachine
    general:
      hostname: fep-1
    hardware:
      vcpus: 1
      memory: 2048
      os_type: linux
      drives:
      - image: ot-sim.qc2
    network:
      interfaces:
      - name: eth0
        type: ethernet
        proto: static
        vlan: OT
        address: 10.20.0.21
        mask: 24
  - type: VirtualMachine
    general:
      hostname: client-1
    hardware:
      vcpus: 1
      memory: 2048
      os_type: linux
      drives:
      - image: ot-sim.qc2
    network:
      interfaces:
      - name: eth0
        type: ethernet
        proto: static
        vlan: OT
        address: 10.20.0.31
        mask: 24
`

// domrtOTSimScenario runs ot-sim before helics, as the helics app requires:
// it reads the helics/federate annotations ot-sim adds. rtu-1 serves bus-1
// and breaker-1 over Modbus and bus-2 over DNP3; fep-1 polls it with both and
// re-serves everything over both; client-1 polls fep-1. helics.scan-rate
// gives client-1's DNP3 master the 5 s scan rate fep-1's gets by default:
// without it an fd-client scans at ot-sim's default of 30 s, and the wait in
// domrtCheckClientDNP3 is sized for 5 s scans.
const domrtOTSimScenario = `apiVersion: phenix.sandia.gov/v2
kind: Scenario
metadata:
  name: {{name}}
  annotations:
    topology: {{name}}
spec:
  apps:
  - name: ot-sim
    metadata:
      helics:
        broker:
          hostname: broker|eth0
        federate: OpenDSS
        scan-rate: 5
    hosts:
    - hostname: rtu-1
      metadata:
        type: fd-server
        modbus:
        - name: bus-1
          type: bus
        - name: breaker-1
          type: breaker
        dnp3:
        - name: bus-2
          type: bus
    - hostname: fep-1
      metadata:
        type: fep
        upstream:
        - rtu-1
        downstream:
        - dnp3
        - modbus
    - hostname: client-1
      metadata:
        type: fd-client
        connected_rtus:
        - fep-1
  - name: helics
    metadata:
      broker:
        root: broker|eth0
`

// domrtSimAddr is where the simulator federate on sub joins: the sub-broker
// the helics app starts there, on its default port.
const domrtSimAddr = "10.20.0.12:24000"

// domrtPublisher is the test's stand-in for the power simulator: a HELICS
// federate named OpenDSS (the ot-sim app's default federate) that publishes
// the values rtu-1 subscribes to and, like a simulator, applies the breaker
// command rtu-1 sends to its OpenDSS/updates endpoint and publishes the new
// breaker state. breaker-1.voltage is above 655.35 and breaker-1.power is
// negative, which an unsigned 16-bit register at scaling 2 cannot carry.
const domrtPublisher = `import json
import socket
import sys
import time
import traceback

LOG = sys.argv[3] if len(sys.argv) > 3 else "/var/log/e2e-pub.log"


def log(msg):
    with open(LOG, "a") as f:
        f.write("%.1f %s\n" % (time.time(), msg))


def wait_port(addr, timeout):
    host, port = addr.rsplit(":", 1)
    deadline = time.time() + timeout
    while time.time() < deadline:
        try:
            socket.create_connection((host, int(port)), 2).close()
            return True
        except OSError:
            time.sleep(1)
    return False


def main():
    broker = sys.argv[1]
    name = sys.argv[2]

    import helics as h

    log("helics " + h.helicsGetVersion())
    log("broker %s reachable: %s" % (broker, wait_port(broker, 120)))

    values = {
        "bus-1.voltage": 0.93,
        "bus-2.voltage": 0.95,
        "breaker-1.voltage": 12470.0,
        "breaker-1.current": 2.5,
        "breaker-1.freq": 60.0,
        "breaker-1.power": -3.0,
        "breaker-1.status": False,
        "breaker-1.controls": False,
    }

    fi = h.helicsCreateFederateInfo()
    h.helicsFederateInfoSetCoreTypeFromString(fi, "zmq")
    h.helicsFederateInfoSetCoreInitString(fi, "--federates=1 --broker=" + broker)
    fed = h.helicsCreateCombinationFederate(name, fi)

    pubs = {}
    for key, value in values.items():
        if isinstance(value, bool):
            typ = h.helics_data_type_boolean
        else:
            typ = h.helics_data_type_double
        pubs[key] = h.helicsFederateRegisterPublication(fed, key, typ, "")

    ep = h.helicsFederateRegisterEndpoint(fed, "updates", "")

    log("entering execution")
    h.helicsFederateEnterExecutingMode(fed)
    log("executing")

    granted = 0.0
    while True:
        granted = h.helicsFederateRequestTime(fed, granted + 1)

        while h.helicsEndpointHasMessage(ep):
            msg = h.helicsEndpointGetMessage(ep)
            data = h.helicsMessageGetString(msg)
            log("update from %s: %s" % (h.helicsMessageGetSource(msg), data))

            for point in json.loads(data):
                if point.get("tag") == "breaker-1.controls":
                    closed = bool(point.get("value"))
                    values["breaker-1.controls"] = closed
                    values["breaker-1.status"] = closed

        for key, pub in pubs.items():
            value = values[key]
            if isinstance(value, bool):
                h.helicsPublicationPublishBoolean(pub, value)
            else:
                h.helicsPublicationPublishDouble(pub, value)

        if int(granted) % 30 == 1:
            log("granted %s published %s" % (granted, json.dumps(values, sort_keys=True)))

        time.sleep(1)


try:
    main()
except Exception:
    log(traceback.format_exc())
    raise
`

// domrtPublisherStart starts the simulator from phenix-start.sh, in the
// background so the startup scripts after it still run.
const domrtPublisherStart = `#!/bin/bash
nohup python3 /root/e2e-helics-pub.py ` + domrtSimAddr + ` OpenDSS >>/var/log/e2e-pub.log 2>&1 &
`

func domrtOTSimHELICS(t *testing.T) {
	const name = "e2e-dr-ots"

	var (
		qemuBefore = qemuProcesses(t)
		pub        = writeFile(t, "e2e-helics-pub.py", domrtPublisher)
		pubStart   = writeFile(t, "95-e2e-pub.sh", domrtPublisherStart)
		guests     = []string{"broker", "sub", "rtu-1", "fep-1", "client-1"}
	)

	e := newExperiment(t, name, fmt.Sprintf(domrtOTSimTopology, pub, pubStart), domrtOTSimScenario)

	diag := domrtDiagnoseOnFailure(t, name, map[string]string{
		"broker":   domrtDiagHELICS,
		"sub":      domrtDiagHELICS + "; tail -n 40 /var/log/e2e-pub.log",
		"rtu-1":    domrtDiagOTSim,
		"fep-1":    domrtDiagOTSim,
		"client-1": domrtDiagOTSim,
	})

	start := time.Now()

	e.start()

	script := e.mmScript()

	t.Run("the apps labelled and annotated the federation", func(t *testing.T) {
		doc := domrtStored(t, name)

		for host, want := range map[string]map[string]string{
			"broker":   {"group": "helics", "helics": "broker"},
			"sub":      {"group": "helics", "helics": "broker"},
			"rtu-1":    {"group": "helics", "helics": "federate"},
			"fep-1":    {},
			"client-1": {},
		} {
			domrtWantLabels(t, name, doc, host, want)
		}

		ann := doc.node(t, "rtu-1").Annotations["helics/federate"]
		if got, want := domrtJSON(ann), `[{"broker":"10.20.0.5","fed-count":1}]`; got != want {
			t.Errorf("ot-sim annotated rtu-1 with helics/federate %s, want %s (its I/O module joins the root broker)", got, want)
		}
	})

	t.Run("the helics app counts each broker's federates", func(t *testing.T) {
		// The root counts every federate and its one sub-broker; the sub-broker
		// its one federate, whose annotation names it as sub|eth0.
		root := domrtWords(t, domrtInjectSrc(t, script, "broker", "/etc/phenix/startup/90-helics-broker.sh"))
		for _, want := range []string{"--name broker", "--subbrokers 1", "-f2", "--web"} {
			if !strings.Contains(root, want) {
				t.Errorf("the root broker script on broker has no %q: %s", want, root)
			}
		}

		sub := domrtWords(t, domrtInjectSrc(t, script, "sub", "/etc/phenix/startup/90-helics-broker.sh"))
		for _, want := range []string{"--name sub", "--broker 10.20.0.5", "--local_interface " + domrtSimAddr, "-f1"} {
			if !strings.Contains(sub, want) {
				t.Errorf("the sub-broker script on sub has no %q: %s", want, sub)
			}
		}
	})

	waitCCClientsWithin(t, name, domrtBoot, guests...)
	t.Logf("the five guests registered with C2 %v after start", time.Since(start).Round(time.Second))

	t.Run("each guest finished its startup scripts and runs its service", func(t *testing.T) {
		for _, vm := range []string{"broker", "sub"} {
			src := domrtInjectSrc(t, script, vm, "/etc/phenix/startup/90-helics-broker.sh")
			checks := [][2]string{
				{"phenix", domrtPhenixDone},
				{"broker", "pgrep -f 'helics_broker --name " + vm + "' >/dev/null && echo running"},
				{"script", "sha256sum < /etc/phenix/startup/90-helics-broker.sh | cut -d' ' -f1"},
			}

			if vm == "sub" {
				checks = append(checks,
					[2]string{"simulator", "pgrep -f e2e-helics-pub.py >/dev/null && echo running"},
				)
			}

			facts := domrtFacts(t, name, vm, checks)

			domrtWantFact(t, vm, facts, "phenix", "active", "phenix.service, which runs the startup scripts")
			domrtWantFact(t, vm, facts, "broker", "running", "a helics_broker named "+vm)
			domrtWantFact(t, vm, facts, "script", domrtSHA256(t, src), "the sha256 of the injected broker script")

			if vm == "sub" {
				domrtWantFact(t, vm, facts, "simulator", "running", "the simulator federate")
			}
		}

		for _, vm := range []string{"rtu-1", "fep-1", "client-1"} {
			domrtCheckOTSimGuest(t, name, script, vm)
		}
	})

	// helicsOK says whether the simulator's values reached rtu-1 over HELICS
	// and fep-1 over Modbus, which the breaker command needs to come back.
	var helicsOK bool

	t.Run("HELICS values reach the RTU and flow over Modbus and DNP3 to the FEP and the client", func(t *testing.T) {
		results := domrtWantProbe(t, name, "sub", 4*time.Minute,
			domrtModules("rtu-1 modules", "10.20.0.11", "backplane", "dnp3", "modbus", "i/o"),
			domrtModules("fep-1 modules", "10.20.0.21", "backplane", "dnp3", "modbus"),
			domrtModules("client-1 modules", "10.20.0.31", "backplane", "dnp3", "modbus"),

			// rtu-1's I/O module subscribes to OpenDSS/<tag> through the root.
			domrtQuery("rtu-1 bus-1.voltage", "10.20.0.11", "bus-1.voltage", 0.93, 1e-6),
			domrtQuery("rtu-1 bus-2.voltage", "10.20.0.11", "bus-2.voltage", 0.95, 1e-6),
			domrtQuery("rtu-1 breaker-1.voltage", "10.20.0.11", "breaker-1.voltage", 12470, 1e-6),
			domrtQuery("rtu-1 breaker-1.power", "10.20.0.11", "breaker-1.power", -3, 1e-6),
			domrtQuery("rtu-1 breaker-1.status", "10.20.0.11", "breaker-1.status", 0, 0),

			// fep-1 polls rtu-1 over Modbus (signed 32-bit registers, scaling 2)
			// and over DNP3 (a DNP3 master on a FEP).
			domrtQuery("fep-1 bus-1.voltage", "10.20.0.21", "bus-1.voltage", 0.93, 0.005),
			domrtQuery("fep-1 breaker-1.voltage", "10.20.0.21", "breaker-1.voltage", 12470, 0.005),
			domrtQuery("fep-1 breaker-1.power", "10.20.0.21", "breaker-1.power", -3, 0.005),
			domrtQuery("fep-1 bus-2.voltage", "10.20.0.21", "bus-2.voltage", 0.95, 0.001),
			domrtQuery("fep-1 breaker-1.status", "10.20.0.21", "breaker-1.status", 0, 0),

			// client-1 polls fep-1 over DNP3 and Modbus, which both feed this
			// tag; the DNP3 master on an fd-client is checked by itself
			// below.
			domrtQuery("client-1 bus-2.voltage", "10.20.0.31", "bus-2.voltage", 0.95, 0.001),
		)

		helicsOK = results["rtu-1 bus-1.voltage"].OK && results["fep-1 bus-1.voltage"].OK

		facts := domrtFacts(t, name, "fep-1", [][2]string{
			{"listening", "ss -ltnH | awk '{print $4}' | sort | tr '\\n' ' '"},
		})

		for _, want := range []string{"10.20.0.21:20000", "10.20.0.21:502"} {
			if !strings.Contains(facts["listening"], want) {
				t.Errorf("fep-1 does not listen on %s (its downstream outstations): %s", want, facts["listening"])
			}
		}

		domrtCheckClientDNP3(t, name, script)
	})

	t.Run("the root broker federates the RTU and the simulator through the sub-broker", func(t *testing.T) {
		facts := domrtFacts(t, name, "sub", [][2]string{
			{"executing", "grep -c executing /var/log/e2e-pub.log"},
		})

		domrtWantCount(t, "sub", facts, "executing", "log line saying the simulator entered HELICS execution mode")

		// The root broker's web API lists every federate in the federation,
		// including those that joined through a sub-broker. The values above
		// already crossed the federation, so it has formed by now.
		domrtWantProbe(t, name, "sub", 90*time.Second, domrtCheck{
			Name: "federates", Kind: "contains", URL: "http://10.20.0.5:8080/broker/federates",
			Contains: []string{`"rtu-1"`, `"OpenDSS"`},
			What:     "the root broker's web API to list federates rtu-1 and OpenDSS",
		})
	})

	t.Run("a breaker command from the FEP reaches the simulator and the new state comes back", func(t *testing.T) {
		if !helicsOK {
			t.Fatal("not checked: the simulator's values never reached rtu-1 and fep-1 (see the subtest above), " +
				"so no breaker state can come back")
		}

		// fep-1 writes rtu-1's coil over Modbus; rtu-1's I/O module sends the
		// update to OpenDSS/updates; the simulator closes the breaker and
		// publishes it; fep-1 polls the new status.
		domrtWantProbe(t, name, "sub", 3*time.Minute,
			domrtWrite("command", "10.20.0.21", "breaker-1.controls", "1"),
			domrtQuery("rtu-1 breaker-1.controls", "10.20.0.11", "breaker-1.controls", 1, 0),
			domrtQuery("rtu-1 breaker-1.status", "10.20.0.11", "breaker-1.status", 1, 0),
			domrtQuery("fep-1 breaker-1.status", "10.20.0.21", "breaker-1.status", 1, 0),
		)

		// The update carries the value rtu-1's Modbus module put on its bus:
		// OT-sim encodes the coil's float64 1 as the JSON number 1, and the
		// I/O module passes it on as Python's json reads it.
		facts := domrtFacts(t, name, "sub", [][2]string{
			{"update", `grep -cE 'update from rtu-1/updates: \[\{"tag": "breaker-1.controls", "value": 1(\.0)?\}\]' /var/log/e2e-pub.log`},
		})

		domrtWantCount(t, "sub", facts, "update", "simulator log line with the breaker-1.controls update rtu-1/updates sent")
	})

	t.Run("the client reads the FEP's Modbus server", func(t *testing.T) { domrtCheckClientModbus(t, name, script) })

	diag.beforeStop(t)

	t.Run("stop removes the guests", func(t *testing.T) { domrtStop(t, e, qemuBefore) })
}

// domrtCheckClientDNP3 checks client-1's DNP3 master by itself. OT-sim's CPU
// keeps the last value of a tag from any module, and client-1's Modbus client
// sets the same tags, so the master shows only in its own session and log:
// its id is dnp3-master (OT-sim dnp3 main.cpp), and it logs each tag it sets
// from a scan. The wait for a scan is sized for the scenario's 5 s scan
// rate, so the rendered master must have it.
func domrtCheckClientDNP3(t *testing.T, name, script string) {
	t.Helper()

	var rates []string

	for _, dnp3 := range domrtParseXML(t, domrtInjectSrc(t, script, "client-1", "/etc/ot-sim/config.xml")).all("dnp3") {
		if dnp3.Attrs["mode"] == "client" {
			rates = append(rates, dnp3.child("master").text("scan-rate"))
		}
	}

	if !slices.Equal(rates, []string{"5"}) {
		t.Errorf("client-1's rendered DNP3 masters have the scan rates %q, want one with the scenario's helics.scan-rate 5", rates)
	}

	facts := domrtFacts(t, name, "client-1", [][2]string{
		{"dnp3", domrtWaitFor("ss -tnH state established '( dport = :20000 )' | wc -l")},
		{"scan", domrtWaitFor(`journalctl -u ot-sim --no-pager | grep -cE '\[dnp3-master\] setting tag bus-2\.voltage to 0\.9(5|49)'`)},
	})

	domrtWantCount(t, "client-1", facts, "dnp3", "established DNP3 session to fep-1's outstation")
	domrtWantCount(t, "client-1", facts, "scan", "DNP3 master log line setting bus-2.voltage to 0.95 from fep-1")
}

// domrtCheckClientModbus checks what client-1's Modbus client reads from
// fep-1. fep-1's Modbus client of rtu-1 has its own name, since OT-sim's
// Modbus server ignores statuses from its own sender's name and serves 0 for
// them, and fep-1's Modbus server serves the DNP3-sourced bus-2.voltage too.
// client-1's DNP3 master feeds the same CPU tags, so the probe reads
// fep-1's registers as client-1's Modbus client does (int32, scaling 2),
// including the breaker state the command closed, at the addresses fep-1's
// rendered config gives them. The client itself shows in what only it
// publishes and logs: OT-sim's Modbus client sets <name>.connected to 1 when
// a read of its last poll succeeded and logs a line for each read that
// failed, but keeps its session either way. Which tags it reads into which
// registers is the render tier's to check (domainWantClientOfServers).
func domrtCheckClientModbus(t *testing.T, name, script string) {
	t.Helper()

	const fep = "10.20.0.21"

	mbClient := domrtModbusClientName(t,
		domrtParseXML(t, domrtInjectSrc(t, script, "client-1", "/etc/ot-sim/config.xml")), fep+":502")

	served := domrtModbusServed(t, domrtParseXML(t, domrtInjectSrc(t, script, "fep-1", "/etc/ot-sim/config.xml")), fep+":502")

	// The probe reads each tag where the config serves it, so a layout change
	// that serves every tag correctly passes; the register type only picks
	// the function code and decoding the probe reads it with.
	at := func(tag, typ string) int {
		reg, ok := served[tag]
		if !ok {
			t.Fatalf("fep-1's rendered Modbus server at %s:502 serves no %s: %v", fep, tag, served)
		}

		if reg.Type != typ {
			t.Fatalf("fep-1's rendered Modbus server at %s:502 serves %s as a %q register, but the probe reads it as %q",
				fep, tag, reg.Type, typ)
		}

		return reg.Addr
	}

	domrtWantProbe(t, name, "sub", 90*time.Second,
		domrtModbusInput("fep-1 Modbus bus-2.voltage", fep, at("bus-2.voltage", "input"), "bus-2.voltage", 95, 1),
		domrtModbusInput("fep-1 Modbus bus-1.voltage", fep, at("bus-1.voltage", "input"), "bus-1.voltage", 93, 1),
		domrtModbusInput("fep-1 Modbus breaker-1.power", fep, at("breaker-1.power", "input"), "breaker-1.power", -300, 1),
		domrtModbusDiscrete("fep-1 Modbus breaker-1.status", fep, at("breaker-1.status", "discrete"), "breaker-1.status", 1),
		domrtQuery("client-1 bus-1.voltage", "10.20.0.31", "bus-1.voltage", 0.93, 0.005),
		domrtQuery("client-1 breaker-1.power", "10.20.0.31", "breaker-1.power", -3, 0.005),
		domrtQuery("client-1 breaker-1.status", "10.20.0.31", "breaker-1.status", 1, 0),
		domrtQuery("client-1 "+mbClient+".connected", "10.20.0.31", mbClient+".connected", 1, 0),
	)

	// Reads that failed before fep-1 served stay in the journal, so count
	// only the last 30 s, six polls; a passing failure is waited out.
	facts := domrtFacts(t, name, "client-1", [][2]string{
		{"modbus", domrtWaitFor("ss -tnH state established '( dport = :502 )' | wc -l")},
		{"errors", domrtWaitNone("journalctl -u ot-sim --no-pager --since=-30s | grep -cF '[" + mbClient + "] [ERROR]'")},
	})

	domrtWantCount(t, "client-1", facts, "modbus", "established Modbus session to fep-1's server")
	domrtWantFact(t, "client-1", facts, "errors", "0",
		"the number of "+mbClient+" error lines in ot-sim's journal over the last 30 s")
}

// domrtWindTopology is sceptre-phenix-topologies'
// renewables/wind/turbine/phenix-configs/topology.yml, reduced to fit a
// runner: no grafana (the scenario's Elasticsearch ground truth is dropped
// with it), two blades instead of three, every node 1 vCPU and 2048 MB (the
// original sets none, so phenix would give the OT-sim guests 512 MB), and the
// public 1.1.1.0/24 turbine VLAN renumbered to 10.41.0.0/24.
const domrtWindTopology = `apiVersion: phenix.sandia.gov/v1
kind: Topology
metadata:
  name: {{name}}
spec:
  nodes:
  - general:
      hostname: wtg-1-main-controller
    hardware:
      drives:
      - image: ot-sim.qc2
      memory: 2048
      os_type: linux
      vcpus: 1
    network:
      interfaces:
      - address: 192.168.100.1
        gateway: 192.168.100.254
        mask: 24
        name: IF0
        proto: static
        type: ethernet
        vlan: ot
      - address: 10.41.0.254
        mask: 24
        name: IF1
        proto: static
        type: ethernet
        vlan: turbine
    type: VirtualMachine
  - general:
      hostname: wtg-1-yaw-controller
    hardware:
      drives:
      - image: ot-sim.qc2
      memory: 2048
      os_type: linux
      vcpus: 1
    network:
      interfaces:
      - address: 10.41.0.11
        gateway: 10.41.0.254
        mask: 24
        name: IF0
        proto: static
        type: ethernet
        vlan: turbine
    type: VirtualMachine
  - general:
      hostname: wtg-1-signal-converter
    hardware:
      drives:
      - image: ot-sim.qc2
      memory: 2048
      os_type: linux
      vcpus: 1
    network:
      interfaces:
      - address: 10.41.0.21
        gateway: 10.41.0.254
        mask: 24
        name: IF0
        proto: static
        type: ethernet
        vlan: turbine
    type: VirtualMachine
  - general:
      hostname: wtg-1-blade-1
    hardware:
      drives:
      - image: ot-sim.qc2
      memory: 2048
      os_type: linux
      vcpus: 1
    network:
      interfaces:
      - address: 10.41.0.31
        gateway: 10.41.0.254
        mask: 24
        name: IF0
        proto: static
        type: ethernet
        vlan: turbine
    type: VirtualMachine
  - general:
      hostname: wtg-1-blade-2
    hardware:
      drives:
      - image: ot-sim.qc2
      memory: 2048
      os_type: linux
      vcpus: 1
    network:
      interfaces:
      - address: 10.41.0.32
        gateway: 10.41.0.254
        mask: 24
        name: IF0
        proto: static
        type: ethernet
        vlan: turbine
    type: VirtualMachine
`

// domrtWindScenario is the same repository's phenix-configs/scenario.yml with
// the ground-truth module (Elasticsearch on grafana) removed, the third blade
// removed, replayData pointing at the test's weather file (%s; the
// repository's weather.csv is a Git LFS pointer) and the yaw controller
// starting at 100 degrees, so its movement toward the setpoint can be timed.
const domrtWindScenario = `apiVersion: phenix.sandia.gov/v2
kind: Scenario
metadata:
  name: {{name}}
  annotations:
    topology: {{name}}
spec:
  apps:
  - name: wind-turbine
    metadata:
      templates:
        default:
          main-controller:
            turbine:
              type: E-126/4200
              hubHeight: 135
              roughnessLength: 0.15
            logic:
              speedTag: speed.high
              directionTag: dir.high
              directionError: 0.04
            weather:
              columns:
              - name: wind_speed
                tags:
                - name: speed.high
                  height: 58.2
                - name: speed.med
                  height: 36.6
                - name: speed.low
                  height: 15.0
              - name: temperature
                tags:
                - name: temp.high
                  height: 58.2
                - name: temp.low
                  height: 3.0
              - name: pressure
                tags:
                - name: pressure
                  height: 0.0
          anemometer:
            weather:
              replayData: %s
              columns:
              - name: Windspeed 58.2m
                tag: speed.high
              - name: Windspeed 36.6m
                tag: speed.med
              - name: Windspeed 15.0m
                tag: speed.low
              - name: Wind Direction 58.2m
                tag: dir.high
              - name: Wind Direction 36.6m
                tag: dir.med
              - name: Wind Direction 15.0m
                tag: dir.low
              - name: Temperature 58.2m
                tag: temp.high
              - name: Temperature 3.0m
                tag: temp.low
              - name: Barometric Pressure
                tag: pressure
          yaw-controller:
            yaw:
              initialPosition: 100
              degreePerSecond: 0.1
    hosts:
    - hostname: "(.*-(.*))-main-controller"
      metadata:
        type: main-controller
        template: default
        controllers:
          anemometer: "$1-signal-converter"
          yaw: "$1-yaw-controller"
          blades:
          - "$1-blade-1"
          - "$1-blade-2"
`

// The weather the file replays, constant so every reading is exact. The
// anemometer's Modbus registers are unsigned 16-bit at scaling 2 and OT-sim
// truncates value*100 to an integer, so every value stays within 0..655.35
// and is a binary fraction (288.15 would read back as 288.14).
const (
	domrtWindSpeed = 10.0   // m/s at 58.2 m
	domrtWindDir   = 90.0   // degrees at 58.2 m
	domrtWindTemp  = 288.5  // K at 58.2 m
	domrtWindPress = 101.25 // barometric pressure

	// domrtYawTarget is where the main controller's logic points the
	// turbine: into the wind, 180 degrees from where it comes from.
	domrtYawTarget = domrtWindDir + 180

	// The E-126/4200 power curve in windpowerlib 0.2.2 starts at 3 m/s and
	// ends at 25 m/s; with the logarithmic wind profile (roughness 0.15 m)
	// 10 m/s at 58.2 m is 11.41 m/s at the 135 m hub, which the curve turns
	// into 3.80 MW.
	domrtCutIn     = 3.0
	domrtCutOut    = 25.0
	domrtTurbineMW = 3.80
)

// domrtWeatherCSV is a replay file in the columns the scenario maps, one row
// per second for an hour, so the anemometer never wraps during the test.
func domrtWeatherCSV() string {
	var b strings.Builder

	b.WriteString("Windspeed 58.2m,Windspeed 36.6m,Windspeed 15.0m," +
		"Wind Direction 58.2m,Wind Direction 36.6m,Wind Direction 15.0m," +
		"Temperature 58.2m,Temperature 3.0m,Barometric Pressure\n")

	row := fmt.Sprintf("%g,9.0,8.0,%g,%g,%g,%g,289.5,%g\n",
		domrtWindSpeed, domrtWindDir, domrtWindDir, domrtWindDir, domrtWindTemp, domrtWindPress)

	b.WriteString(strings.Repeat(row, 3600))

	return b.String()
}

func domrtWindTurbine(t *testing.T) {
	const (
		name   = "e2e-dr-wind"
		main   = "wtg-1-main-controller"
		mainIP = "192.168.100.1"
		yawIP  = "10.41.0.11"
		convIP = "10.41.0.21"
	)

	var (
		qemuBefore = qemuProcesses(t)
		weather    = writeFile(t, "weather-const.csv", domrtWeatherCSV())
		guests     = []string{main, "wtg-1-yaw-controller", "wtg-1-signal-converter", "wtg-1-blade-1", "wtg-1-blade-2"}
		blades     = map[string]string{"wtg-1-blade-1": "10.41.0.31", "wtg-1-blade-2": "10.41.0.32"}
	)

	e := newExperiment(t, name, domrtWindTopology, fmt.Sprintf(domrtWindScenario, weather))

	scripts := make(map[string]string, len(guests))
	for _, vm := range guests {
		scripts[vm] = domrtDiagOTSim
	}

	diag := domrtDiagnoseOnFailure(t, name, scripts)

	start := time.Now()

	e.start()

	script := e.mmScript()

	waitCCClientsWithin(t, name, domrtBoot, guests...)
	t.Logf("the five guests registered with C2 %v after start", time.Since(start).Round(time.Second))

	t.Run("each guest finished its startup scripts and runs ot-sim", func(t *testing.T) {
		for _, vm := range guests {
			domrtCheckOTSimGuest(t, name, script, vm)
		}

		facts := domrtFacts(t, name, "wtg-1-signal-converter", [][2]string{
			{"weather", "sha256sum < /etc/ot-sim/data/weather.csv | cut -d' ' -f1"},
		})

		domrtWantFact(t, "wtg-1-signal-converter", facts, "weather", domrtSHA256(t, weather), "the sha256 of the replayed weather data")

		facts = domrtFacts(t, name, main, [][2]string{
			{"listening", "ss -ltnH | awk '{print $4}' | sort | tr '\\n' ' '"},
		})

		if !strings.Contains(facts["listening"], mainIP+":20000") {
			t.Errorf("the main controller does not listen on %s:20000 (its DNP3 outstation): %s", mainIP, facts["listening"])
		}
	})

	// setpointOK says whether the yaw controller got its setpoint over
	// Modbus, and runningOK whether the turbine and its blades were
	// unfeathered, which the checks after the next subtest build on.
	var setpointOK, runningOK bool

	t.Run("the turbine reads the wind, points into it and produces power", func(t *testing.T) {
		checks := []domrtCheck{
			domrtModules("main controller modules", mainIP, "backplane", "turbine-power-output", "logic", "modbus", "dnp3"),
			domrtModules("signal converter modules", convIP, "backplane", "turbine-anemometer", "modbus"),
			domrtModules("yaw controller modules", yawIP, "backplane", "modbus", "logic"),

			// The anemometer module replays the weather file.
			domrtQuery("signal converter speed.high", convIP, "speed.high", domrtWindSpeed, 1e-6),
			domrtQuery("signal converter dir.high", convIP, "dir.high", domrtWindDir, 1e-6),

			// The main controller polls the signal converter over Modbus.
			domrtQuery("main controller speed.high", mainIP, "speed.high", domrtWindSpeed, 0.01),
			domrtQuery("main controller dir.high", mainIP, "dir.high", domrtWindDir, 0.01),
			domrtQuery("main controller temp.high", mainIP, "temp.high", domrtWindTemp, 0.01),
			domrtQuery("main controller pressure", mainIP, "pressure", domrtWindPress, 0.01),

			// The power output module publishes the power curve's limits and
			// the turbine's output for the wind it reads.
			domrtQuery("main controller turbine.cut-in", mainIP, "turbine.cut-in", domrtCutIn, 1e-6),
			domrtQuery("main controller turbine.cut-out", mainIP, "turbine.cut-out", domrtCutOut, 1e-6),
			domrtQuery("main controller turbine.mw-output", mainIP, "turbine.mw-output", domrtTurbineMW, domrtTurbineMW/10),

			// The logic module unfeathers the blades and sets the yaw
			// controller's setpoint over Modbus.
			domrtQuery("main controller feathered", mainIP, "feathered", 0, 0),
			domrtQuery("main controller yaw.setpoint", mainIP, "yaw.setpoint", domrtYawTarget, 1e-6),

			// The yaw controller's CPU API never reports yaw.setpoint: its
			// Modbus server passes the main controller's holding-register
			// write on as an Update, the API records only Status messages,
			// and its logic program never assigns yaw_setpoint, so never
			// publishes it. Read holding register 40001 (scaling 2) itself.
			{
				Name: "yaw controller yaw.setpoint",
				Kind: "holding",
				Host: yawIP,
				Addr: 40001,
				Op:   "approx",
				Want: domrtYawTarget * 100,
				Tol:  1,
				What: fmt.Sprintf(
					"the yaw controller's holding register 40001 (yaw.setpoint at scaling 2), read over Modbus, to be %g",
					domrtYawTarget*100,
				),
			},
		}

		results := domrtWantProbe(t, name, main, 5*time.Minute, append(checks, domrtFeathered(blades, 0)...)...)

		setpointOK = results["yaw controller yaw.setpoint"].OK
		runningOK = results["main controller feathered"].OK

		for _, c := range domrtFeathered(blades, 0) {
			runningOK = runningOK && results[c.Name].OK
		}
	})

	t.Run("the yaw controller turns toward the setpoint at its configured rate", func(t *testing.T) {
		if !setpointOK {
			t.Fatal("not checked: the yaw controller never got the setpoint (see the subtest above), so it does not turn toward it")
		}

		// degreePerSecond 0.1: ten seconds turn it by one degree.
		results := domrtWantProbe(t, name, main, 90*time.Second, domrtCheck{
			Name: "yaw rate", Kind: "rate", URL: domrtQueryURL(yawIP, "yaw.current"), Gap: 10,
			Op: "approx", Want: 1, Tol: 0.5,
			What: "yaw.current on the yaw controller to grow by about one degree in 10 s",
		})

		if r := results["yaw rate"]; r.First != nil && r.Value != nil {
			if last := *r.First + *r.Value; *r.First <= 0 || last > domrtYawTarget {
				t.Errorf("yaw.current went from %.2f to %.2f, outside (0, %.0f]", *r.First, last, domrtYawTarget)
			}
		}
	})

	t.Run("an emergency stop feathers every blade and stops the power output", func(t *testing.T) {
		if !runningOK {
			t.Fatal("not checked: the blades were never unfeathered (see the subtest above), so feathering them would prove nothing")
		}

		checks := []domrtCheck{
			domrtWrite("emergency stop", mainIP, "turbine.emergency-stop", "1"),
			domrtQuery("main controller feathered", mainIP, "feathered", 1, 0),
			domrtQuery("main controller turbine.mw-output", mainIP, "turbine.mw-output", 0, 1e-6),
		}

		domrtWantProbe(t, name, main, 2*time.Minute, append(checks, domrtFeathered(blades, 1)...)...)
	})

	diag.beforeStop(t)

	t.Run("stop removes the guests", func(t *testing.T) { domrtStop(t, e, qemuBefore) })
}

// domrtFeathered checks each blade's feathered coil with a raw Modbus read
// (the blades' OT-sim CPU API never sees the tag: their Modbus server only
// sends coil writes on as updates, and the API records status messages).
func domrtFeathered(blades map[string]string, want float64) []domrtCheck {
	checks := make([]domrtCheck, 0, len(blades))

	for _, blade := range slices.Sorted(maps.Keys(blades)) {
		checks = append(checks, domrtCheck{
			Name: blade + " feathered coil", Kind: "coil", Host: blades[blade], Addr: 1, Op: "approx", Want: want,
			What: fmt.Sprintf("%s's feathered coil 1, read over Modbus, to be %g", blade, want),
		})
	}

	return checks
}

// domrtCheckOTSimGuest checks that an OT-sim guest finished phenix's startup
// scripts, runs the image's ot-sim.service and holds the configuration
// the app rendered for it.
func domrtCheckOTSimGuest(t *testing.T, ns, script, vm string) {
	t.Helper()

	src := domrtInjectSrc(t, script, vm, "/etc/ot-sim/config.xml")

	facts := domrtFacts(t, ns, vm, [][2]string{
		{"phenix", domrtPhenixDone},
		{"ot-sim", domrtWaitActive("ot-sim.service")},
		{"config", "sha256sum < /etc/ot-sim/config.xml | cut -d' ' -f1"},
	})

	domrtWantFact(t, vm, facts, "phenix", "active", "phenix.service, which runs the startup scripts")
	domrtWantFact(t, vm, facts, "ot-sim", "active", "the image's ot-sim.service")
	domrtWantFact(t, vm, facts, "config", domrtSHA256(t, src), "the sha256 of /etc/ot-sim/config.xml")
}

// ---------------------------------------------------------------------------
// sceptre

// TestDomainRuntimeSceptre boots the sceptre app's Linux SCEPTRE stack from
// sceptre-phenix-topologies' soap-legacy, brought in through fromScenario as
// that topology does: a PyPower provider, a DNP3 RTU, a Modbus RTU and an
// fd-client polling both. It follows provider data over UDP multicast to the
// RTUs and over DNP3 and Modbus to the client, and a generator trip from the
// client back through the DNP3 RTU to the provider.
func TestDomainRuntimeSceptre(t *testing.T) {
	const name = "e2e-dr-sceptre"

	requireApps(t, "domain runtime", "phenix-app-sceptre")
	requireImage(t, domrtBennuImage)

	var (
		qemuBefore = qemuProcesses(t)
		caseFile   = domrtCase300(t)
		guests     = []string{"provider", "rtu-1", "rtu-2", "client-1"}
	)

	// The scenario the experiment's sceptre app takes its settings from, as
	// soap-legacy's soap-scenario.yaml does with soap-sceptre.
	from := name + "-app"

	phenix(t, "config", "create", writeFile(t, from+"-scenario.yaml", fmt.Sprintf(domrtSceptreAppScenario, from, name, caseFile)))
	t.Cleanup(func() { _, _ = phenixRun("config", "delete", "scenario/"+from) })

	e := newExperiment(t, name, domrtSceptreTopology, fmt.Sprintf(domrtSceptreScenario, from))

	scripts := make(map[string]string, len(guests))
	for _, vm := range guests {
		scripts[vm] = domrtDiagSceptre
	}

	diag := domrtDiagnoseOnFailure(t, name, scripts)

	t.Run("fromScenario brought in the sceptre app's hosts", func(t *testing.T) {
		app := domrtStored(t, name).app(t, "sceptre")

		hosts := make([]string, 0, len(app.Hosts))
		for _, h := range app.Hosts {
			hosts = append(hosts, h.Hostname)
		}

		if want := []string{"provider", "rtu-1", "rtu-2", "client-1"}; !slices.Equal(hosts, want) {
			t.Errorf("the experiment's sceptre app has hosts %v, want %v from scenario %s", hosts, want, from)
		}
	})

	start := time.Now()

	e.start()

	script := e.mmScript()

	var (
		rtu1   = domrtParseXML(t, domrtInjectSrc(t, script, "rtu-1", "/etc/sceptre/config.xml"))
		rtu2   = domrtParseXML(t, domrtInjectSrc(t, script, "rtu-2", "/etc/sceptre/config.xml"))
		client = domrtParseXML(t, domrtInjectSrc(t, script, "client-1", "/etc/sceptre/config.xml"))

		// Where the client finds each provider point it is checked on, by
		// following the tag names and register addresses the app rendered.
		voltage1 = domrtSceptreMap(t, rtu1, client, "input", "bus-242.voltage")
		active1  = domrtSceptreMap(t, rtu1, client, "input", "generator-1_bus-242.active")
		trip1    = domrtSceptreMap(t, rtu1, client, "output", "generator-1_bus-242.active")
		voltage2 = domrtSceptreMap(t, rtu2, client, "input", "bus-243.voltage")
		gen2     = domrtSceptreMap(t, rtu2, client, "input", "generator-1_bus-243.mw")
	)

	waitCCClientsWithin(t, name, domrtBoot, guests...)
	t.Logf("the four guests registered with C2 %v after start", time.Since(start).Round(time.Second))

	t.Run("each guest finished its startup scripts and runs its SCEPTRE daemon", func(t *testing.T) {
		facts := domrtFacts(t, name, "provider", [][2]string{
			{"phenix", domrtPhenixDone},
			{"solver", domrtWaitFor("pgrep -fc pybennu-power-solver")},
			{"listening", domrtWaitFor("ss -ltnH 'sport = :5555' | wc -l")},
			{"config", "sha256sum < /etc/sceptre/config.ini | cut -d' ' -f1"},
			{"case", "sha256sum < /etc/sceptre/case300.py | cut -d' ' -f1"},
		})

		domrtWantFact(t, "provider", facts, "phenix", "active", "phenix.service, which runs sceptre-start.sh")
		domrtWantFact(
			t,
			"provider",
			facts,
			"config",
			domrtSHA256(t, domrtInjectSrc(t, script, "provider", "/etc/sceptre/config.ini")),
			"the sha256 of /etc/sceptre/config.ini",
		)
		domrtWantFact(t, "provider", facts, "case", domrtCase300SHA256, "the sha256 of /etc/sceptre/case300.py")
		domrtWantCount(t, "provider", facts, "solver", "pybennu-power-solver process")
		domrtWantCount(t, "provider", facts, "listening", "listener on port 5555, the provider's server endpoint")

		for vm, endpoint := range map[string]string{"rtu-1": "10.117.4.101:20000", "rtu-2": "10.117.4.102:502"} {
			facts := domrtFacts(t, name, vm, [][2]string{
				{"phenix", domrtPhenixDone},
				{"device", domrtWaitFor(domrtFieldDevice)},
				{"listening", domrtWaitFor("ss -ltnH | grep -c ' " + endpoint + " '")},
				{"watcher", "test -s /etc/sceptre/watcher.ini && echo ok"},
				{"start", "stat -c %a /etc/phenix/startup/sceptre-start.sh"},
				{"config", "sha256sum < /etc/sceptre/config.xml | cut -d' ' -f1"},
			})

			domrtWantFact(t, vm, facts, "phenix", "active", "phenix.service, which runs sceptre-start.sh")
			domrtWantFact(t, vm, facts, "watcher", "ok", "a non-empty /etc/sceptre/watcher.ini written by sceptre-start.sh")
			domrtWantFact(t, vm, facts, "start", "744", "the mode of /etc/phenix/startup/sceptre-start.sh (the app's permissions 0744)")
			domrtWantFact(
				t,
				vm,
				facts,
				"config",
				domrtSHA256(t, domrtInjectSrc(t, script, vm, "/etc/sceptre/config.xml")),
				"the sha256 of /etc/sceptre/config.xml",
			)
			domrtWantCount(t, vm, facts, "device", "bennu-field-deviced process")
			domrtWantCount(t, vm, facts, "listening", "listener on "+endpoint+", its outstation")
		}

		facts = domrtFacts(t, name, "client-1", [][2]string{
			{"phenix", domrtPhenixDone},
			{"device", domrtWaitFor(domrtFieldDevice)},
			{"dnp3", domrtWaitFor("ss -tnH state established '( dport = :20000 )' | wc -l")},
			{"commands", "ss -ltnH | awk '{print $4}' | sort | tr '\\n' ' '"},
		})

		domrtWantFact(t, "client-1", facts, "phenix", "active", "phenix.service, which runs sceptre-start.sh")
		domrtWantCount(t, "client-1", facts, "device", "bennu-field-deviced process")
		domrtWantCount(t, "client-1", facts, "dnp3", "established DNP3 session to rtu-1")
		// bennu: the Modbus TCP client dials rtu-2 once when it starts and
		// again only on a read (TcpClient.hpp), so its session is checked after
		// the Modbus reads below; opendnp3 keeps retrying, so DNP3 is up here.

		for _, command := range []string{voltage1.command, voltage2.command} {
			if addr := strings.TrimPrefix(command, "tcp://"); !strings.Contains(facts["commands"], addr) {
				t.Errorf("client-1 does not listen on its command interface %s: %s", command, facts["commands"])
			}
		}
	})

	const provider = "tcp://172.16.1.2:5555"

	// The provider's own values, read from client-1 over the MGMT VLAN. The
	// provider publishes them with up to 1% noise, so the client's readings
	// are compared within 2%.
	values := map[string]float64{}

	// generatorOK says whether the client read the generator's state through
	// rtu-1, which the trip builds on.
	var generatorOK bool

	t.Run("the provider solves the case and answers probes", func(t *testing.T) {
		results := domrtWantProbe(t, name, "client-1", 3*time.Minute,
			domrtBennuQuery("provider points", provider, "bus-242.voltage", "generator-1_bus-242.active", "bus-243.voltage"),
			domrtBennuRead("provider bus-242.voltage", provider, "bus-242.voltage", "range", 0.8, 1.2),
			domrtBennuRead("provider bus-242.gen_mw", provider, "bus-242.gen_mw", "approx", 170, 1),
			domrtBennuRead("provider generator-1_bus-242.active", provider, "generator-1_bus-242.active", "approx", 1, 0),
			domrtBennuRead("provider bus-243.voltage", provider, "bus-243.voltage", "range", 0.8, 1.2),
			domrtBennuRead("provider generator-1_bus-243.mw", provider, "generator-1_bus-243.mw", "approx", 84, 1),
		)

		for _, point := range []string{"bus-242.voltage", "bus-243.voltage", "generator-1_bus-243.mw"} {
			if r := results["provider "+point]; r.Value != nil {
				values[point] = *r.Value
			}
		}

		t.Logf("provider values: %v", values)
	})

	t.Run("provider data reaches the client over multicast and the RTUs' DNP3 and Modbus", func(t *testing.T) {
		if len(values) != 3 {
			t.Fatalf("the provider's values are unknown: %v", values)
		}

		// Modbus registers carry the infrastructure's range [-600, 1800] in
		// 16 bits, a step of 0.037.
		const modbusStep = 2400.0 / 65535

		results := domrtWantProbe(t, name, "client-1", 3*time.Minute,
			domrtBennuRead("client bus-242.voltage over DNP3", voltage1.command, voltage1.clientTag, "approx",
				values["bus-242.voltage"], 0.02*values["bus-242.voltage"]+0.001),
			domrtBennuRead("client generator-1_bus-242.active over DNP3", active1.command, active1.clientTag, "approx", 1, 0),
			domrtBennuRead("client bus-243.voltage over Modbus", voltage2.command, voltage2.clientTag, "approx",
				values["bus-243.voltage"], 0.02*values["bus-243.voltage"]+modbusStep),
			domrtBennuRead("client generator-1_bus-243.mw over Modbus", gen2.command, gen2.clientTag, "approx",
				values["generator-1_bus-243.mw"], 0.02*values["generator-1_bus-243.mw"]+modbusStep),
		)

		generatorOK = results["client generator-1_bus-242.active over DNP3"].OK

		facts := domrtFacts(t, name, "client-1", [][2]string{
			{"modbus", domrtWaitFor("ss -tnH state established '( dport = :502 )' | wc -l")},
		})
		domrtWantCount(t, "client-1", facts, "modbus", "established Modbus session to rtu-2 after the Modbus reads")
	})

	t.Run("a generator trip from the client reaches the provider through the RTU", func(t *testing.T) {
		if !generatorOK {
			t.Fatal("not checked: the client never read the generator's state through rtu-1 (see the subtest above), " +
				"so it cannot see it trip")
		}

		// The client's DNP3 command operates rtu-1's binary output, which
		// rtu-1 writes to the provider. pybennu sets the generator out of
		// service and acknowledges the write whether or not its re-solve
		// converges, so these show the trip reaching the provider and its
		// state coming back to the client, not a solved model.
		domrtWantProbe(t, name, "client-1", 3*time.Minute,
			domrtBennuWrite("trip", trip1.command, trip1.clientTag, "false"),
			domrtBennuRead("provider generator-1_bus-242.active", provider, "generator-1_bus-242.active", "approx", 0, 0),
			domrtBennuRead("client generator-1_bus-242.active over DNP3", active1.command, active1.clientTag, "approx", 0, 0),
		)

		// pybennu: pybennu 6.0.0's power solver turns case300's PV buses into
		// PQ buses (python_power.py _dict_to_pcf), so the re-solve after this
		// trip does not converge: the provider raises bus-alert.active and
		// bus-242.gen_mw keeps its old value. Asserted as it is, so a pybennu
		// fix shows here; then assert gen_mw below 1 and bus-alert.active 0.
		alert := domrtBennuRead("provider bus-alert.active", provider, "bus-alert.active", "approx", 1, 0)
		alert.What = "pybennu's bus-alert.active to be 1 (true), raised by the re-solve that does not converge"

		mw := domrtBennuRead("provider bus-242.gen_mw after the trip", provider, "bus-242.gen_mw", "approx", 170, 1)
		mw.What = "pybennu's bus-242.gen_mw to keep its 170 MW (within 1), since the re-solve does not converge"

		domrtWantProbe(t, name, "client-1", 30*time.Second, alert, mw)
	})

	diag.beforeStop(t)

	t.Run("stop removes the guests", func(t *testing.T) { domrtStop(t, e, qemuBefore) })
}

// domrtSceptreTopology is sceptre-phenix-topologies'
// soap-legacy/soap-topology.yaml reduced to the provider and two RTUs, plus
// client-1, an fd-client modelled on the RTU nodes. Every node has 2048 MB
// (the original gives the RTUs 512 MB and the provider the default).
const domrtSceptreTopology = `apiVersion: phenix.sandia.gov/v1
kind: Topology
metadata:
  name: {{name}}
spec:
  nodes:
  - general:
      hostname: provider
    hardware:
      drives:
      - image: bennu.qc2
      memory: 2048
      os_type: linux
      vcpus: 1
    network:
      interfaces:
      - address: 172.16.1.2
        gateway: 172.16.1.1
        mask: 16
        name: mgmt
        proto: static
        type: ethernet
        vlan: MGMT
    type: VirtualMachine
  - general:
      hostname: rtu-1
    hardware:
      drives:
      - image: bennu.qc2
      memory: 2048
      os_type: linux
      vcpus: 1
    network:
      interfaces:
      - address: 10.117.4.101
        mask: 24
        name: exp
        proto: static
        type: ethernet
        vlan: EXP
      - address: 172.16.1.4
        mask: 16
        name: mgmt
        proto: static
        type: ethernet
        vlan: MGMT
    type: VirtualMachine
  - general:
      hostname: rtu-2
    hardware:
      drives:
      - image: bennu.qc2
      memory: 2048
      os_type: linux
      vcpus: 1
    network:
      interfaces:
      - address: 10.117.4.102
        mask: 24
        name: exp
        proto: static
        type: ethernet
        vlan: EXP
      - address: 172.16.1.5
        mask: 16
        name: mgmt
        proto: static
        type: ethernet
        vlan: MGMT
    type: VirtualMachine
  - general:
      hostname: client-1
    hardware:
      drives:
      - image: bennu.qc2
      memory: 2048
      os_type: linux
      vcpus: 1
    network:
      interfaces:
      - address: 10.117.4.120
        mask: 24
        name: exp
        proto: static
        type: ethernet
        vlan: EXP
      - address: 172.16.1.20
        mask: 16
        name: mgmt
        proto: static
        type: ethernet
        vlan: MGMT
    type: VirtualMachine
`

// domrtSceptreAppScenario is soap-legacy/sceptre.yaml (soap-sceptre) reduced
// to the provider and rtu-1 and rtu-2, without the Ignition host (which would
// add a minute of sleep to every startup), the elk labels (no ELK box
// here) and assetDir, with the case file at %s, rtu-2 serving its devices
// over Modbus instead of DNP3, and client-1 added. The first %s is the
// scenario's name, the second the topology it is marked for.
const domrtSceptreAppScenario = `apiVersion: phenix.sandia.gov/v2
kind: Scenario
metadata:
  name: %s
  annotations:
    topology: %s
spec:
  apps:
    - name: sceptre
      hosts:
      - hostname: provider
        metadata:
          case: %s
          publish_endpoint: udp://*;239.0.0.1:40000
          simulator: PyPower
          type: provider
      - hostname: rtu-1
        metadata:
          dnp3:
          - name: bus-242
            type: bus
          - name: generator-1_bus-242
            type: generator
          - name: bus-alert
            type: bus
          infrastructure: power-transmission
          provider: provider
          type: fd-server
      - hostname: rtu-2
        metadata:
          modbus:
          - name: bus-243
            type: bus
          - name: load-1_bus-243
            type: load
          - name: generator-1_bus-243
            type: generator
          infrastructure: power-transmission
          provider: provider
          type: fd-server
      - hostname: client-1
        metadata:
          connected_rtus:
          - rtu-1
          - rtu-2
          type: fd-client
`

// domrtSceptreScenario takes the sceptre app from the scenario named %s, as
// soap-legacy/soap-scenario.yaml does.
const domrtSceptreScenario = `apiVersion: phenix.sandia.gov/v2
kind: Scenario
metadata:
  name: {{name}}
  annotations:
    topology: {{name}}
spec:
  apps:
    - name: sceptre
      fromScenario: %s
`

// domrtCase300 copies the IEEE 300-bus case the provider solves out of
// bennu.qc2's PYPOWER, where it is byte-identical to soap-legacy's
// injects/case300.py, and returns its path. The sceptre app injects it into
// the provider.
func domrtCase300(t *testing.T) string {
	t.Helper()

	var data []byte

	inspectDisk(t, filepath.Join(cfg.imagesDir, domrtBennuImage), func(root string) {
		found := make([]string, 0, 1)

		for _, pattern := range []string{
			"usr/local/lib/python3*/dist-packages/pypower/case300.py",
			"usr/lib/python3*/dist-packages/pypower/case300.py",
			"usr/lib/python3/dist-packages/pypower/case300.py",
		} {
			matches, _ := filepath.Glob(filepath.Join(root, pattern))
			found = append(found, matches...)
		}

		if len(found) == 0 {
			t.Fatalf("%s has no PYPOWER case300.py (pybennu pins PYPOWER, which ships it)", domrtBennuImage)
		}

		var err error
		if data, err = os.ReadFile(found[0]); err != nil { //nolint:gosec // path from the test's own glob
			t.Fatalf("reading %s from %s: %v", found[0], domrtBennuImage, err)
		}
	})

	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); got != domrtCase300SHA256 {
		t.Errorf("%s's case300.py has sha256 %s, want PYPOWER 5.1.19's %s", domrtBennuImage, got, domrtCase300SHA256)
	}

	// The provider's config names the file by its base name.
	return writeFile(t, "case300.py", string(data))
}

// domrtSceptrePoint is how one provider point reaches the fd-client.
type domrtSceptrePoint struct {
	point     string // the provider's name for it
	rtuTag    string // the RTU's tag
	register  string // the RTU's register element (analog-input, coil, ...)
	address   string // its address on the RTU's server
	endpoint  string // the RTU's server endpoint
	clientTag string // the client's tag for the same register
	command   string // the client's command interface for that protocol
}

// domrtSceptreMap follows a provider point through the configs the sceptre
// app rendered: the RTU's input or output section names the point's I/O id,
// its external tag names the I/O, its server serves the tag at an address,
// and the client's connection to that server reads the address into a tag of
// its own.
func domrtSceptreMap(t *testing.T, rtu, client *domrtXML, section, point string) domrtSceptrePoint {
	t.Helper()

	p := domrtSceptrePoint{point: point}
	dev := rtu.child("field-device")

	var id string

	for _, io := range dev.child(section).Children {
		if io.text("name") == point {
			id = io.text("id")
		}
	}

	for _, tag := range dev.child("tags").all("external-tag") {
		if tag.text("io") == id {
			p.rtuTag = tag.text("name")
		}
	}

	for _, server := range dev.child("comms").Children {
		for _, reg := range server.Children {
			if reg.text("tag") == p.rtuTag && p.rtuTag != "" {
				p.register, p.address, p.endpoint = reg.Name, reg.text("address"), server.text("endpoint")
			}
		}
	}

	for _, proto := range client.child("field-device").child("comms").Children {
		for _, conn := range proto.Children {
			if !strings.HasSuffix(conn.Name, "-connection") || conn.text("endpoint") != p.endpoint {
				continue
			}

			for _, reg := range conn.Children {
				if reg.Name == p.register && reg.text("address") == p.address {
					p.clientTag, p.command = reg.text("tag"), proto.text("command-interface")
				}
			}
		}
	}

	if id == "" || p.rtuTag == "" || p.address == "" || p.clientTag == "" || p.command == "" {
		t.Fatalf("cannot follow provider point %s (%s) through the rendered configs: %+v", point, section, p)
	}

	return p
}

// ---------------------------------------------------------------------------
// in-guest probes

// domrtCheck is one condition the in-guest probe (domrtProbeCode) polls for.
type domrtCheck struct {
	Name string `json:"name"`

	// Kind is query (an OT-sim CPU API tag), modules (the API's module
	// list), contains (a GET whose body has every Contains string), status
	// (a request answered with Code), coil, holding, input or discrete (a raw
	// Modbus read at Host and Addr of a coil, a holding register, a signed
	// 32-bit input register pair or a discrete input, unscaled), bennu
	// (pybennu-probe Cmd against Endpoint) or rate (two reads of the OT-sim
	// tag at URL Gap seconds apart, compared as their difference).
	Kind string `json:"kind"`

	URL      string   `json:"url,omitempty"`
	Method   string   `json:"method,omitempty"`
	Code     int      `json:"code"`
	Contains []string `json:"contains,omitempty"`
	Modules  []string `json:"modules,omitempty"`
	Host     string   `json:"host,omitempty"`
	Addr     int      `json:"addr"`
	Endpoint string   `json:"endpoint,omitempty"`
	Cmd      string   `json:"cmd,omitempty"`
	Tag      string   `json:"tag,omitempty"`
	Status   string   `json:"status,omitempty"`
	Gap      float64  `json:"gap"`

	// Op compares a value with Want: approx (within Tol) or range (Want to
	// Tol). Kinds without a value ignore it.
	Op   string  `json:"op,omitempty"`
	Want float64 `json:"want"`
	Tol  float64 `json:"tol"`

	// What says, for a failure, what the check expects.
	What string `json:"-"`
}

// domrtResult is what the probe last saw for one check.
type domrtResult struct {
	OK    bool     `json:"ok"`
	Code  int      `json:"code"`
	Value *float64 `json:"value"`
	First *float64 `json:"first"`
	Body  string   `json:"body"`
	Err   string   `json:"err"`
}

func (r domrtResult) String() string {
	var parts []string

	if r.Value != nil {
		parts = append(parts, fmt.Sprintf("value %g", *r.Value))
	}

	if r.First != nil {
		parts = append(parts, fmt.Sprintf("first reading %g", *r.First))
	}

	if r.Code != 0 {
		parts = append(parts, fmt.Sprintf("HTTP %d", r.Code))

		// The OT-sim API answers 400 for a tag no module has published yet.
		if r.Code == 400 {
			parts = append(parts, "the tag was never published")
		}
	}

	if r.Err != "" {
		parts = append(parts, "error "+r.Err)
	}

	if r.Body != "" {
		parts = append(parts, fmt.Sprintf("output %q", r.Body))
	}

	if len(parts) == 0 {
		return "the check never ran"
	}

	return strings.Join(parts, ", ")
}

// domrtAPI is the base URL of the OT-sim CPU REST API of the device at ip,
// on the port the ot-sim and wind-turbine apps configure by default.
func domrtAPI(ip string) string {
	return "http://" + net.JoinHostPort(ip, "9101") + "/api/v1"
}

// domrtCompareText says in words what a check's Op, Want and Tol accept.
func domrtCompareText(op string, want, tol float64) string {
	switch op {
	case "range":
		return fmt.Sprintf("between %g and %g", want, tol)
	default:
		return fmt.Sprintf("%g (within %g)", want, tol)
	}
}

func domrtQueryURL(ip, tag string) string {
	return domrtAPI(ip) + "/query/" + tag
}

// domrtQuery checks a tag on the OT-sim CPU API of the device at ip.
func domrtQuery(name, ip, tag string, want, tol float64) domrtCheck {
	return domrtCheck{
		Name: name, Kind: "query", URL: domrtQueryURL(ip, tag), Op: "approx", Want: want, Tol: tol,
		What: fmt.Sprintf("the OT-sim API at %s to report %s = %s", ip, tag, domrtCompareText("approx", want, tol)),
	}
}

// domrtModbusInput reads the signed 32-bit input register pair at addr of the
// Modbus server at ip, unscaled, as a client polling it would.
func domrtModbusInput(name, ip string, addr int, tag string, want, tol float64) domrtCheck {
	return domrtCheck{
		Name: name, Kind: "input", Host: ip, Addr: addr, Op: "approx", Want: want, Tol: tol,
		What: fmt.Sprintf("input registers %d-%d (%s) of the Modbus server at %s to hold %s",
			addr, addr+1, tag, ip, domrtCompareText("approx", want, tol)),
	}
}

// domrtModbusDiscrete reads the discrete input at addr of the Modbus server
// at ip.
func domrtModbusDiscrete(name, ip string, addr int, tag string, want float64) domrtCheck {
	return domrtCheck{
		Name: name, Kind: "discrete", Host: ip, Addr: addr, Op: "approx", Want: want,
		What: fmt.Sprintf("discrete input %d (%s) of the Modbus server at %s to be %g", addr, tag, ip, want),
	}
}

// domrtModules checks that the OT-sim CPU at ip runs the named modules.
func domrtModules(name, ip string, modules ...string) domrtCheck {
	return domrtCheck{
		Name: name, Kind: "modules", URL: domrtAPI(ip) + "/modules", Modules: modules,
		What: fmt.Sprintf("the OT-sim CPU at %s to have modules %v enabled", ip, modules),
	}
}

// domrtWrite writes a tag through the OT-sim CPU API of the device at ip.
func domrtWrite(name, ip, tag, value string) domrtCheck {
	return domrtCheck{
		Name: name, Kind: "status", Method: "POST", Code: 204,
		URL:  domrtAPI(ip) + "/write/" + tag + "/" + value,
		What: fmt.Sprintf("the OT-sim API at %s to accept %s = %s with HTTP 204", ip, tag, value),
	}
}

// domrtBennuQuery checks that a bennu endpoint lists the given points.
func domrtBennuQuery(name, endpoint string, points ...string) domrtCheck {
	return domrtCheck{
		Name: name, Kind: "bennu", Cmd: "query", Endpoint: endpoint, Contains: points,
		What: fmt.Sprintf("pybennu-probe query of %s to list %v", endpoint, points),
	}
}

// domrtBennuRead reads a tag from a bennu endpoint; true reads 1, false 0.
func domrtBennuRead(name, endpoint, tag, op string, want, tol float64) domrtCheck {
	return domrtCheck{
		Name: name, Kind: "bennu", Cmd: "read", Endpoint: endpoint, Tag: tag, Op: op, Want: want, Tol: tol,
		What: fmt.Sprintf("pybennu-probe to read %s from %s as %s", tag, endpoint, domrtCompareText(op, want, tol)),
	}
}

// domrtBennuWrite writes a binary tag through a bennu endpoint.
func domrtBennuWrite(name, endpoint, tag, status string) domrtCheck {
	return domrtCheck{
		Name: name, Kind: "bennu", Cmd: "write", Endpoint: endpoint, Tag: tag, Status: status,
		What: fmt.Sprintf("pybennu-probe write of %s = %s through %s to be acknowledged", tag, status, endpoint),
	}
}

// domrtProbeCode polls the checks in SPEC from inside a guest until all pass
// or the time is up, and prints every check's last result after a marker.
// It runs on Python 3.8 (ot-sim.qc2, focal) and 3.10 (bennu.qc2, jammy) with
// the standard library only.
const domrtProbeCode = `
import base64, json, os, re, socket, struct, subprocess, time, urllib.error, urllib.request

spec = json.loads(base64.b64decode(SPEC).decode())
opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))


def http(method, url):
    data = b"" if method == "POST" else None
    req = urllib.request.Request(url, data=data, method=method)
    try:
        resp = opener.open(req, timeout=4)
        try:
            return resp.getcode(), resp.read().decode("utf-8", "replace")
        finally:
            resp.close()
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode("utf-8", "replace")


def number(text):
    text = text.strip().lower()
    if text in ("true", "false"):
        return 1.0 if text == "true" else 0.0
    return float(text)


def otsim(url):
    code, body = http("GET", url)
    if code != 200:
        return code, None, body
    return code, float(json.loads(body)["value"]), body


def modbus_read(host, function, addr, count, size):
    s = socket.create_connection((host, 502), 4)
    try:
        s.settimeout(4)
        s.sendall(struct.pack(">HHHBBHH", 7, 0, 6, 1, function, addr, count))
        data = b""
        # An exception reply is 9 bytes, shorter than any data reply.
        while len(data) < 9 + size and not (len(data) >= 9 and data[7] == function | 0x80):
            chunk = s.recv(260)
            if not chunk:
                break
            data += chunk
    finally:
        s.close()
    if len(data) >= 9 and data[7] == function | 0x80:
        raise ValueError("modbus exception %d for function %d at address %d" % (data[8], function, addr))
    if len(data) < 9 + size or data[7] != function:
        raise ValueError("modbus reply %r" % data)
    return data[9:9 + size]


def coil(host, addr):
    return float(modbus_read(host, 1, addr, 1, 1)[0] & 1)


def holding(host, addr):
    return float(struct.unpack(">H", modbus_read(host, 3, addr, 1, 2))[0])


def input_int32(host, addr):
    return float(struct.unpack(">i", modbus_read(host, 4, addr, 2, 4))[0])


def discrete(host, addr):
    return float(modbus_read(host, 2, addr, 1, 1)[0] & 1)


def bennu(c):
    args = ["timeout", "10", "pybennu-probe", "-e", c["endpoint"], "-c", c["cmd"]]
    if c.get("tag"):
        args += ["-t", c["tag"]]
    if c.get("status"):
        args += ["-s", c["status"]]
    # bennu's libraries live in /usr/local/lib, which phenix.service, but
    # not miniccc.service, puts on the library path.
    env = dict(os.environ, LD_LIBRARY_PATH="/usr/local/lib:" + os.environ.get("LD_LIBRARY_PATH", ""))
    p = subprocess.run(args, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, env=env)
    # bennu servers end every reply with a NUL byte, which the probe prints
    # after the value, where float() would refuse it.
    return p.stdout.decode("utf-8", "replace").replace("\x00", "")


def compare(c, v):
    op, want, tol = c.get("op") or "approx", c.get("want", 0), c.get("tol", 0)
    if op == "approx":
        return abs(v - want) <= tol
    if op == "range":
        return want <= v <= tol
    raise ValueError("unknown op " + op)


def check(c):
    r = {"ok": False}
    try:
        kind = c["kind"]
        if kind == "coil":
            r["value"] = coil(c["host"], c["addr"])
            r["ok"] = compare(c, r["value"])
        elif kind == "holding":
            r["value"] = holding(c["host"], c["addr"])
            r["ok"] = compare(c, r["value"])
        elif kind == "input":
            r["value"] = input_int32(c["host"], c["addr"])
            r["ok"] = compare(c, r["value"])
        elif kind == "discrete":
            r["value"] = discrete(c["host"], c["addr"])
            r["ok"] = compare(c, r["value"])
        elif kind == "bennu":
            out = bennu(c)
            r["body"] = out[-300:]
            if c["cmd"] == "query":
                r["ok"] = "I: ACK" in out and all(p in out for p in c["contains"])
            elif c["cmd"] == "write":
                r["ok"] = "I: ACK" in out
            else:
                m = re.search(r"^\s*" + re.escape(c["tag"]) + r":(\S+)\s*$", out, re.M)
                if m:
                    r["value"] = number(m.group(1))
                    r["ok"] = compare(c, r["value"])
        elif kind == "rate":
            code, first, body = otsim(c["url"])
            r["code"] = code
            if first is not None:
                time.sleep(c["gap"])
                code, last, body = otsim(c["url"])
                r["code"] = code
                if last is not None:
                    r["first"] = first
                    r["value"] = last - first
                    r["ok"] = compare(c, r["value"])
            if not r["ok"]:
                r["body"] = body[:300]
        else:
            code, body = http(c.get("method") or "GET", c["url"])
            r["code"] = code
            if kind == "status":
                r["ok"] = code == c["code"]
            elif code == 200 and kind == "modules":
                mods = json.loads(body)
                r["ok"] = all(mods.get(m) == "enabled" for m in c["modules"])
            elif code == 200 and kind == "contains":
                r["ok"] = all(s in body for s in c["contains"])
            elif code == 200 and kind == "query":
                r["value"] = float(json.loads(body)["value"])
                r["ok"] = compare(c, r["value"])
            if not r["ok"]:
                r["body"] = body[:300]
    except Exception as e:
        r["err"] = repr(e)[:300]
    return r


deadline = time.time() + spec["timeout"]
results = {}
while True:
    for c in spec["checks"]:
        if time.time() > deadline:
            break
        if results.get(c["name"], {}).get("ok"):
            continue
        results[c["name"]] = check(c)
    if all(results.get(c["name"], {}).get("ok") for c in spec["checks"]) or time.time() > deadline:
        break
    time.sleep(2)

print("@@domrt@@" + json.dumps(results, sort_keys=True))
`

// domrtProbe runs checks inside guest vm, in as many probe runs as it takes
// until every check has passed or within has passed, and returns each
// check's last result. A check that passed is not run again.
func domrtProbe(t *testing.T, ns, vm string, within time.Duration, checks ...domrtCheck) map[string]domrtResult {
	t.Helper()

	var (
		deadline = time.Now().Add(within)
		results  = make(map[string]domrtResult, len(checks))
		pending  = checks
	)

	for len(pending) > 0 {
		budget := min(time.Until(deadline), domrtProbeCall)
		budget = max(budget, 5*time.Second)

		got := domrtProbeOnce(t, ns, vm, budget, pending)

		var next []domrtCheck

		for _, c := range pending {
			// A call out of time skips the checks it did not reach; keep what
			// an earlier call saw for them.
			if r, ok := got[c.Name]; ok {
				results[c.Name] = r
			}

			if !results[c.Name].OK {
				next = append(next, c)
			}
		}

		pending = next

		if time.Now().After(deadline) {
			break
		}
	}

	return results
}

func domrtProbeOnce(t *testing.T, ns, vm string, budget time.Duration, checks []domrtCheck) map[string]domrtResult {
	t.Helper()

	out := guestShell(t, ns, vm, domrtProbeScript(t, budget, checks))

	results, err := domrtProbeResults(out)
	if err != nil {
		t.Fatalf("the probe in %s: %v\n%s", vm, err, out)
	}

	return results
}

// domrtProbeScript is the guest script that runs domrtProbeCode on checks
// for at most budget.
func domrtProbeScript(t *testing.T, budget time.Duration, checks []domrtCheck) string {
	t.Helper()

	spec, err := json.Marshal(map[string]any{"timeout": budget.Seconds(), "checks": checks})
	if err != nil {
		t.Fatal(err)
	}

	return "python3 - <<'DOMRT_PY'\nSPEC = '" + base64.StdEncoding.EncodeToString(spec) + "'\n" +
		domrtProbeCode + "\nDOMRT_PY\n:\n"
}

// domrtProbeResults decodes the results line the probe printed.
func domrtProbeResults(out string) (map[string]domrtResult, error) {
	_, marked, found := strings.Cut(out, "@@domrt@@")
	if !found {
		return nil, errors.New("it printed no results")
	}

	marked, _, _ = strings.Cut(marked, "\n")

	var results map[string]domrtResult
	if err := json.Unmarshal([]byte(marked), &results); err != nil {
		return nil, fmt.Errorf("decoding its results: %w", err)
	}

	return results, nil
}

// domrtWantProbe runs checks in vm and fails the test for each one that did
// not pass within the time given.
func domrtWantProbe(t *testing.T, ns, vm string, within time.Duration, checks ...domrtCheck) map[string]domrtResult {
	t.Helper()

	results := domrtProbe(t, ns, vm, within, checks...)

	domrtWantResults(t, results, checks...)

	return results
}

func domrtWantResults(t *testing.T, results map[string]domrtResult, checks ...domrtCheck) {
	t.Helper()

	for _, c := range checks {
		if r := results[c.Name]; !r.OK {
			t.Errorf("%s: want %s, got %s", c.Name, c.What, r)
		}
	}
}

// ---------------------------------------------------------------------------
// guest facts and diagnostics

// domrtFactsBudget bounds the waiting all the checks of one domrtFacts call
// do together, which must end well within the two minutes ccExec waits for a
// C2 response (a round trip adds up to two 5 s heartbeats). The checks share
// the deadline $domrt_end.
const domrtFactsBudget = 80

// domrtFieldDevice counts bennu's field device daemons, which bennu-watcherd
// starts as /usr/bin/bennu-field-deviced. A bare pattern would also count
// bennu-watcherd itself, whose --b argument names bennu-field-deviced.firmware.
const domrtFieldDevice = "pgrep -fc '^/usr/bin/bennu-field-deviced '"

// domrtPhenixDone waits for phenix.service, the oneshot unit that runs
// /etc/phenix/startup/* one after another, to finish, and prints its state.
const domrtPhenixDone = `s=$(systemctl is-active phenix.service); ` +
	`while [ "$s" = activating ] && [ "$(date +%s)" -lt "$domrt_end" ]; do ` +
	`sleep 1; s=$(systemctl is-active phenix.service); done; echo "$s"`

// domrtWaitFor is a guest check that waits, until the shared deadline, for
// the count command to print a positive number, and prints the last count.
func domrtWaitFor(count string) string {
	return `n=$(` + count + `); while ! [ "$n" -gt 0 ] 2>/dev/null && [ "$(date +%s)" -lt "$domrt_end" ]; do ` +
		`sleep 1; n=$(` + count + `); done; echo "$n"`
}

// domrtWaitNone is a guest check that waits, until the shared deadline, for
// the count command to print 0, and prints the last count.
func domrtWaitNone(count string) string {
	return `n=$(` + count + `); while [ "$n" != 0 ] && [ "$(date +%s)" -lt "$domrt_end" ]; do ` +
		`sleep 1; n=$(` + count + `); done; echo "$n"`
}

// domrtWaitActive is a guest check that waits, until the shared deadline,
// for a systemd unit to be active, and prints its last state.
func domrtWaitActive(unit string) string {
	return `s=$(systemctl is-active ` + unit + `); while [ "$s" != active ] && [ "$(date +%s)" -lt "$domrt_end" ]; do ` +
		`sleep 1; s=$(systemctl is-active ` + unit + `); done; echo "$s"`
}

// domrtFacts runs every check in one C2 command, since each round trip costs
// a heartbeat, and returns each check's trimmed stdout by name.
func domrtFacts(t *testing.T, ns, vm string, checks [][2]string) map[string]string {
	t.Helper()

	return domrtParseFacts(guestShell(t, ns, vm, domrtFactsScript(checks)), checks)
}

// domrtFactsScript runs each check in its own subshell, with its stderr
// dropped so only its answer is compared, after a marker naming it.
func domrtFactsScript(checks [][2]string) string {
	var script strings.Builder

	fmt.Fprintf(&script, "domrt_end=$(( $(date +%%s) + %d )); ", domrtFactsBudget)

	for i, c := range checks {
		fmt.Fprintf(&script, "echo '@@domrt-fact %d@@'; ( %s ) 2>/dev/null; echo; ", i, c[1])
	}

	return script.String()
}

func domrtParseFacts(out string, checks [][2]string) map[string]string {
	facts := make(map[string]string, len(checks))

	for i, c := range checks {
		_, after, found := strings.Cut(out, fmt.Sprintf("@@domrt-fact %d@@", i))
		if !found {
			facts[c[0]] = ""

			continue
		}

		if next := strings.Index(after, "@@domrt-fact "); next >= 0 {
			after = after[:next]
		}

		facts[c[0]] = strings.TrimSpace(after)
	}

	return facts
}

func domrtWantFact(t *testing.T, vm string, facts map[string]string, key, want, what string) {
	t.Helper()

	if got := facts[key]; got != want {
		t.Errorf("%s: %s is %q, want %q", vm, what, got, want)
	}
}

// domrtWantCount checks that a counting fact found at least one.
func domrtWantCount(t *testing.T, vm string, facts map[string]string, key, what string) {
	t.Helper()

	if n, err := strconv.Atoi(facts[key]); err != nil || n < 1 {
		t.Errorf("%s: found no %s (count %q)", vm, what, facts[key])
	}
}

const (
	domrtDiagCommon = "systemctl --no-pager status phenix.service | tail -n 5; " +
		"journalctl -u phenix --no-pager | tail -n 60; ls -l /etc/phenix/startup; " +
		"ip -4 -o addr show; free -m"

	domrtDiagOTSim = domrtDiagCommon + "; systemctl --no-pager status ot-sim.service | tail -n 5; " +
		"journalctl -u ot-sim --no-pager | tail -n 120; ss -tanH"

	domrtDiagHELICS = domrtDiagCommon + "; pgrep -af helics_broker; ss -tanH; " +
		"tail -n 40 /var/log/helics-root-broker.log /var/log/helics-sub-broker.log"

	domrtDiagSceptre = domrtDiagCommon + "; pgrep -af 'bennu|pybennu'; ss -tanH; " +
		"tail -n 40 /etc/sceptre/log/* /var/log/bennu-pybennu.* 2>/dev/null"
)

// domrtDiagnostics logs, once and only after a failure, what each guest that
// registered with C2 says about its startup scripts and services.
type domrtDiagnostics struct {
	ns      string
	scripts map[string]string
	done    bool
}

// domrtDiagnoseOnFailure returns the diagnostics of experiment ns, whose
// guests run scripts. The test calls beforeStop ahead of the subtest that
// stops the experiment, since stopping removes the guests; the cleanup this
// registers covers a test that ends before it gets there. Registered after
// newExperiment, that cleanup runs before the experiment is torn down.
func domrtDiagnoseOnFailure(t *testing.T, ns string, scripts map[string]string) *domrtDiagnostics {
	t.Helper()

	d := &domrtDiagnostics{ns: ns, scripts: scripts}

	t.Cleanup(func() {
		if t.Failed() && !d.done {
			d.dump(t)
		}
	})

	return d
}

// beforeStop logs the diagnostics, in a subtest of their own, when t has
// failed so far.
func (d *domrtDiagnostics) beforeStop(t *testing.T) {
	t.Helper()

	if t.Failed() && !d.done {
		t.Run("guest diagnostics after the failure", d.dump)
	}
}

func (d *domrtDiagnostics) dump(t *testing.T) {
	t.Helper()

	d.done = true

	// minimega creates any namespace a command names, so ask its cc server
	// only while the experiment's namespace still exists.
	if _, ok := mmNamespaces(t)[d.ns]; !ok {
		t.Logf("no guest diagnostics: minimega has no namespace %s", d.ns)

		return
	}

	registered := map[string]bool{}
	for _, row := range mmTable(t, d.ns, "cc clients") {
		registered[row["hostname"]] = true
	}

	for _, vm := range slices.Sorted(maps.Keys(d.scripts)) {
		if !registered[vm] {
			t.Logf("no diagnostics from %s: it never registered with C2", vm)

			continue
		}

		t.Logf("diagnostics from %s:\n%s", vm, guestShell(t, d.ns, vm, d.scripts[vm]+"; true"))
	}
}

// ---------------------------------------------------------------------------
// experiment, script and host helpers

// domrtExpDoc is the part of a stored experiment these tests read.
type domrtExpDoc struct {
	Spec struct {
		Topology struct {
			Nodes []domrtNodeDoc `json:"nodes"`
		} `json:"topology"`
		Scenario struct {
			Apps []domrtAppDoc `json:"apps"`
		} `json:"scenario"`
	} `json:"spec"`
}

type domrtNodeDoc struct {
	General struct {
		Hostname string `json:"hostname"`
	} `json:"general"`
	Labels      map[string]string `json:"labels"`
	Annotations map[string]any    `json:"annotations"`
}

type domrtAppDoc struct {
	Name  string `json:"name"`
	Hosts []struct {
		Hostname string `json:"hostname"`
	} `json:"hosts"`
}

func domrtStored(t *testing.T, name string) domrtExpDoc {
	t.Helper()

	// Standard output alone, as experiment.doc reads it, so nothing phenix
	// writes to standard error can be taken for the document.
	out, err := phenixStdout("config", "get", "experiment/"+name, "--output", "json")
	if err != nil {
		t.Fatalf("phenix config get experiment/%s: %v\n%s", name, err, out)
	}

	var d domrtExpDoc
	if err = json.Unmarshal([]byte(jsonPart(out)), &d); err != nil {
		t.Fatalf("decoding experiment %s: %v\n%s", name, err, out)
	}

	return d
}

func (d domrtExpDoc) node(t *testing.T, hostname string) domrtNodeDoc {
	t.Helper()

	for _, n := range d.Spec.Topology.Nodes {
		if n.General.Hostname == hostname {
			return n
		}
	}

	t.Fatalf("the stored experiment has no node %s", hostname)

	return domrtNodeDoc{}
}

func (d domrtExpDoc) app(t *testing.T, name string) domrtAppDoc {
	t.Helper()

	for _, a := range d.Spec.Scenario.Apps {
		if a.Name == name {
			return a
		}
	}

	t.Fatalf("the stored experiment has no app %s", name)

	return domrtAppDoc{}
}

// domrtWantLabels checks the labels an app gave a node, in the stored
// experiment and in the tags minimega reports for its VM. Only the label keys
// in want are compared, so the labels other apps add do not matter; an empty
// want means the node has no helics labels.
func domrtWantLabels(t *testing.T, ns string, doc domrtExpDoc, host string, want map[string]string) {
	t.Helper()

	labels := doc.node(t, host).Labels

	var tags map[string]string

	if raw := vmInfo(t, ns)[host]["tags"]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &tags); err != nil {
			t.Errorf("minimega's tags for %s are not a JSON map: %q", host, raw)
		}
	}

	for _, key := range []string{"group", "helics"} {
		if labels[key] != want[key] {
			t.Errorf("%s has label %s=%q in the stored experiment, want %q", host, key, labels[key], want[key])
		}

		if tags[key] != want[key] {
			t.Errorf("minimega reports tag %s=%q on VM %s, want %q", key, tags[key], host, want[key])
		}
	}
}

// domrtInjectSrc returns the host file the rendered minimega script injects
// at dst in vm's snapshot.
func domrtInjectSrc(t *testing.T, script, vm, dst string) string {
	t.Helper()

	_, block, found := strings.Cut(script, "## VM: "+vm+" ##")
	if !found {
		t.Fatalf("the rendered minimega script has no VM %s", vm)
	}

	block, _, _ = strings.Cut(block, "## VM: ")

	m := regexp.MustCompile(`"([^"]+)":"` + regexp.QuoteMeta(dst) + `"`).FindStringSubmatch(block)
	if m == nil {
		t.Fatalf("the rendered minimega script injects nothing at %s into %s:\n%s", dst, vm, block)
	}

	return m[1]
}

func domrtSHA256(t *testing.T, path string) string {
	t.Helper()

	data, err := os.ReadFile(path) //nolint:gosec // a file the experiment injects
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}

	sum := sha256.Sum256(data)

	return hex.EncodeToString(sum[:])
}

// domrtWords reads a shell script the way bash splits it into words, so a
// template's runs of spaces do not matter.
func domrtWords(t *testing.T, path string) string {
	t.Helper()

	data, err := os.ReadFile(path) //nolint:gosec // a file the experiment injects
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}

	return strings.Join(strings.Fields(strings.ReplaceAll(string(data), "\\\n", " ")), " ")
}

func domrtJSON(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}

	return string(data)
}

// domrtStop stops the experiment and checks that its guests and namespace
// are gone.
func domrtStop(t *testing.T, e *experiment, qemuBefore int) {
	t.Helper()

	e.with(t).stop()

	eventually(t, time.Minute, time.Second, "QEMU processes to exit", func() (bool, string) {
		n := qemuProcesses(t)

		return n <= qemuBefore, fmt.Sprintf("%d running, %d before", n, qemuBefore)
	})

	if ns, ok := mmNamespaces(t)[e.name]; ok {
		t.Errorf("minimega still has namespace %s after stop: %v", e.name, ns)
	}
}

// ---------------------------------------------------------------------------
// XML

// domrtXML is an element of a rendered XML config.
type domrtXML struct {
	Name     string
	Attrs    map[string]string
	Text     string
	Children []*domrtXML
}

func domrtParseXML(t *testing.T, path string) *domrtXML {
	t.Helper()

	data, err := os.ReadFile(path) //nolint:gosec // a file the experiment injects
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}

	root := &domrtXML{}
	stack := []*domrtXML{root}
	dec := xml.NewDecoder(bytes.NewReader(data))

	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			t.Fatalf("%s is not well-formed XML: %v", path, err)
		}

		switch tok := tok.(type) {
		case xml.StartElement:
			el := &domrtXML{Name: tok.Name.Local, Attrs: make(map[string]string, len(tok.Attr))}
			for _, a := range tok.Attr {
				el.Attrs[a.Name.Local] = a.Value
			}

			top := stack[len(stack)-1]
			top.Children = append(top.Children, el)
			stack = append(stack, el)
		case xml.EndElement:
			stack = stack[:len(stack)-1]
		case xml.CharData:
			top := stack[len(stack)-1]
			top.Text += string(tok)
		}
	}

	if len(root.Children) != 1 {
		t.Fatalf("%s has %d root elements", path, len(root.Children))
	}

	return root.Children[0]
}

// child returns the first child element named name, or an empty element.
func (x *domrtXML) child(name string) *domrtXML {
	for _, c := range x.Children {
		if c.Name == name {
			return c
		}
	}

	return &domrtXML{}
}

func (x *domrtXML) all(name string) []*domrtXML {
	var out []*domrtXML

	for _, c := range x.Children {
		if c.Name == name {
			out = append(out, c)
		}
	}

	return out
}

// text returns the trimmed text of the first child element named name.
func (x *domrtXML) text(name string) string {
	return strings.TrimSpace(x.child(name).Text)
}

// domrtModbusReg is a register an OT-sim Modbus server serves.
type domrtModbusReg struct {
	Type string // coil, discrete, input or holding
	Addr int
}

// domrtModbusClientName returns the name of the OT-sim Modbus client of the
// server at endpoint in a rendered config: its module's log prefix and the
// prefix of the connected tag it publishes.
func domrtModbusClientName(t *testing.T, cfg *domrtXML, endpoint string) string {
	t.Helper()

	for _, client := range cfg.all("modbus") {
		if client.Attrs["mode"] == "client" && client.text("endpoint") == endpoint {
			if name := client.Attrs["name"]; regexp.MustCompile(`^[A-Za-z0-9._-]+$`).MatchString(name) {
				return name
			}

			t.Fatalf("the Modbus client of %s has the name %q, which the checks cannot quote", endpoint, client.Attrs["name"])
		}
	}

	t.Fatalf("the rendered config has no OT-sim Modbus client of %s", endpoint)

	return ""
}

// domrtModbusServed returns, by tag, the registers that the OT-sim Modbus
// server at endpoint serves in a rendered config.
func domrtModbusServed(t *testing.T, cfg *domrtXML, endpoint string) map[string]domrtModbusReg {
	t.Helper()

	regs := make(map[string]domrtModbusReg)

	for _, server := range cfg.all("modbus") {
		if server.Attrs["mode"] != "server" || server.text("endpoint") != endpoint {
			continue
		}

		for _, reg := range server.all("register") {
			addr, err := strconv.Atoi(reg.text("address"))
			if err != nil {
				t.Fatalf("the Modbus server at %s has a register with address %q: %v", endpoint, reg.text("address"), err)
			}

			regs[reg.text("tag")] = domrtModbusReg{Type: reg.Attrs["type"], Addr: addr}
		}
	}

	if len(regs) == 0 {
		t.Fatalf("the rendered config has no OT-sim Modbus server at %s", endpoint)
	}

	return regs
}
