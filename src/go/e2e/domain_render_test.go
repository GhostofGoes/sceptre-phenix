//go:build e2e

package e2e

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/big"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Render tier of the domain apps in sceptre-phenix-apps: ot-sim, wind-turbine,
// helics, sceptre, caldera and ignition. These apps never talk to minimega.
// Their whole contract is the experiment they hand back to phenix: topology
// mutations (added nodes, labels, annotations) and file injections, which
// phenix renders into one `disk inject` line per VM that minimega copies onto
// the VM's snapshot. Each case runs configure (experiment create) and pre-start
// (experiment start) through phenix with hollow VMs, then reads the snapshot
// disks minimega wrote and checks the files on them, parsing the generated XML,
// JSON and INI where their content matters and cross-checking configs that must
// agree across VMs (a client against the server it polls). The E2E workflow
// installs the apps and sets E2E_APPS=true for this shard.

const (
	domainOTSim       = "phenix-app-ot-sim"
	domainWindTurbine = "phenix-app-wind-turbine"
	domainHELICS      = "phenix-app-helics"
	domainSceptre     = "phenix-app-sceptre"
	domainCaldera     = "phenix-app-caldera"
	domainIgnition    = "phenix-app-ignition"

	// domainNTFSImage is the hollow Windows disk: an msdos label and one NTFS
	// partition with the marker and the profile directory a Windows image
	// has, so a directory injected under Documents nests as it would there.
	domainNTFSImage   = "e2e-domain-ntfs.qc2"
	domainNTFSProfile = "/Users/wwuser/Documents"

	// domainExperimentsDir is phenix's default experiment base directory.
	domainExperimentsDir = "/phenix/experiments"

	domainMemory       = 256
	domainBootTimeout  = time.Minute
	domainStopTimeout  = 30 * time.Second
	domainMountTimeout = 15 * time.Second
	domainPoll         = 100 * time.Millisecond

	domainOTSimConfig   = "/etc/ot-sim/config.xml"
	domainSceptreConfig = "/etc/sceptre/config.xml"
	domainStartup       = "/etc/phenix/startup/"
	domainWaitBroker    = domainStartup + "5-wait-broker.sh"
	domainHELICSBroker  = domainStartup + "90-helics-broker.sh"
	domainSceptreStart  = domainStartup + "sceptre-start.sh"
	domainHELICSLabel   = "helics/federate"

	// domainStartFailed and domainCreateFailed mark the cases whose phenix
	// command must fail.
	domainCreateFailed = "create"
	domainStartFailed  = "start"
)

// domainMD is app or host metadata, written into the scenario as JSON.
type domainMD = map[string]any

// ---------------------------------------------------------------------------
// The tests, one per app. Each case is its own experiment.

// TestDomainRenderOTSim covers DR-OTS-01..05: the configs ot-sim writes for an
// RTU (fd-server), a FEP and a client, the HELICS broker and wait scripts and
// federate annotation, app-level infrastructure, the CPU API with TLS and a
// Node-RED flow, a missing flow file, and a missing broker.
func TestDomainRenderOTSim(t *testing.T) {
	requireApps(t, "domain-render", domainOTSim)
	hollowImages(t)

	for _, rc := range []domainRender{
		domainOTSimRTUFEPClient(),
		domainOTSimInfrastructure(),
		domainOTSimTLS(),
		domainOTSimMissingFlow(),
		domainOTSimNoBroker(),
	} {
		t.Run(rc.title, func(t *testing.T) { domainRun(t, rc) })
	}
}

// TestDomainRenderHELICS covers DR-HEL-01..04: the root and sub-broker
// scripts, federate counts, labels and wait scripts for every annotation form,
// a missing root broker, and ot-sim and helics both claiming the broker
// script.
func TestDomainRenderHELICS(t *testing.T) {
	requireApps(t, "domain-render", domainHELICS, domainOTSim)
	hollowImages(t)

	for _, rc := range []domainRender{
		domainHELICSBrokers(),
		domainHELICSNoRoot(),
		domainHELICSConflict(),
	} {
		t.Run(rc.title, func(t *testing.T) { domainRun(t, rc) })
	}
}

// TestDomainRenderWindTurbine covers DR-WT-01..02 on the wind turbine
// topology and scenario of sceptre-phenix-topologies: the six OT-sim configs
// that must agree with each other, the replayed weather data, and a missing
// replay file.
func TestDomainRenderWindTurbine(t *testing.T) {
	requireApps(t, "domain-render", domainWindTurbine)
	hollowImages(t)

	for _, rc := range []domainRender{
		domainWindTurbineRender(),
		domainWindTurbineNoReplayData(),
	} {
		t.Run(rc.title, func(t *testing.T) { domainRun(t, rc) })
	}
}

// TestDomainRenderSceptre covers DR-SCE-01..06 on a reduced soap-legacy
// model: the provider, RTU, FEP and client configs (through fromScenario),
// overrides from an asset directory, the Windows device types on NTFS, the
// scenario check, and the HELICS labels.
func TestDomainRenderSceptre(t *testing.T) {
	requireApps(t, "domain-render", domainSceptre)
	hollowImages(t)
	domainNTFSImages(t)

	for _, rc := range []domainRender{
		domainSceptreSoap(),
		domainSceptreOverrides(),
		domainSceptreWindows(),
		domainSceptreNoProvider(),
		domainSceptreHELICS(),
	} {
		t.Run(rc.title, func(t *testing.T) { domainRun(t, rc) })
	}
}

// TestDomainRenderCaldera covers DR-CAL-01..02: the server nodes the app adds
// at configure (one with a gateway, one without) and what it injects into the
// servers and into a Linux and a Windows agent.
func TestDomainRenderCaldera(t *testing.T) {
	requireApps(t, "domain-render", domainCaldera)
	hollowImages(t)
	domainNTFSImages(t)

	rc := domainCalderaRender()
	t.Run(rc.title, func(t *testing.T) { domainRun(t, rc) })
}

// TestDomainRenderIgnition covers DR-IGN-01..03 on hollow NTFS gateways: the
// DNP3 device tree, Perspective and WebDev projects and client scripts, a
// gateway backup restore, and the three ways a gateway config is refused.
func TestDomainRenderIgnition(t *testing.T) {
	requireApps(t, "domain-render", domainIgnition)
	hollowImages(t)
	domainNTFSImages(t)

	for _, rc := range []domainRender{
		domainIgnitionGateway(),
		domainIgnitionRestore(),
		domainIgnitionMissingBackup(),
		domainIgnitionBackupAndRTUs(),
		domainIgnitionUnaddressedRTU(),
	} {
		t.Run(rc.title, func(t *testing.T) { domainRun(t, rc) })
	}
}

// ---------------------------------------------------------------------------
// ot-sim

func domainOTSimRTUFEPClient() domainRender {
	return domainRender{
		name:  "e2e-dom-ots",
		title: "DR-OTS-01 and 02 RTU, FEP, client and broker",
		topology: func(*domainCase) string {
			return domainTopology(
				domainLinux("power", "10.20.0.5"),
				domainLinux("rtu-1", "10.20.0.11"),
				domainLinux("fep-1", "10.20.0.21"),
				domainLinux("client-1", "10.20.0.31"),
			)
		},
		scenario: func(*domainCase) string {
			return domainScenario(domainApp{
				name: "ot-sim",
				metadata: domainMD{"helics": domainMD{
					"broker":   domainMD{"hostname": "power|eth0", "base-fed-count": 1},
					"federate": "OpenDSS",
				}},
				hosts: []domainAppHost{
					{"rtu-1", domainMD{
						"type":   "fd-server",
						"modbus": []domainMD{{"name": "line-01", "type": "line"}},
						"dnp3": domainMD{
							"interface": "eth0:20001",
							"devices":   []domainMD{{"name": "bus-01", "type": "bus"}},
						},
						"logic": domainMD{
							"period":    "1s",
							"program":   "x = 1",
							"variables": domainMD{"x": domainMD{"value": 0}},
						},
					}},
					{"fep-1", domainMD{"type": "fep", "upstream": []string{"rtu-1"}, "downstream": []string{"dnp3", "modbus"}}},
					{"client-1", domainMD{"type": "fd-client", "connected_rtus": []string{"fep-1"}}},
				},
			})
		},
		check: domainCheckOTSimRTUFEPClient,
	}
}

func domainCheckOTSimRTUFEPClient(t *testing.T, c *domainCase) {
	t.Helper()

	t.Run("rtu-1 serves DNP3 and Modbus and feeds them from HELICS", func(t *testing.T) {
		x := c.xml(t, "rtu-1", domainOTSimConfig)

		domainWantSet(t, "rtu-1 CPU modules", x.modules(), "backplane", "dnp3", "modbus", "i/o", "logic")
		domainWantText(t, x, "cpu/api/endpoint", "0.0.0.0:9101")

		dnp3 := x.one(t, "dnp3[@mode=server]")
		domainWantText(t, dnp3, "endpoint", "10.20.0.11:20001")
		domainWantText(t, dnp3, "outstation/local-address", "1024")
		domainWantText(t, dnp3, "outstation/remote-address", "1")
		domainWantRegs(t, "rtu-1 DNP3 outstation", domainOTSimRegs(dnp3), []domainReg{
			{kind: "input", typ: "analog", address: "0", tag: "bus-01.voltage"},
		})

		modbus := x.one(t, "modbus[@mode=server]")
		domainWantText(t, modbus, "endpoint", "10.20.0.11:502")

		fields := []string{
			"from_voltage", "from_current", "from_active_power", "from_reactive_power",
			"to_voltage", "to_current", "to_active_power", "to_reactive_power",
		}
		want := make([]domainReg, 0, len(fields))

		for i, field := range fields {
			// Analog points are signed 32-bit registers, two addresses wide.
			want = append(want, domainReg{
				kind: "register", typ: "input", address: strconv.Itoa(30001 + 2*i),
				tag: "line-01." + field, dataType: "int32", scaling: "-2",
			})
		}

		domainWantRegs(t, "rtu-1 Modbus server", domainOTSimRegs(modbus), want)

		io := x.one(t, "io")
		domainWantText(t, io, "broker-endpoint", "10.20.0.5")
		domainWantText(t, io, "federate-name", "rtu-1")

		if keys := io.texts("subscription/key"); !slices.Contains(keys, "OpenDSS/bus-01.voltage") {
			t.Errorf("rtu-1 I/O module subscribes %v, want OpenDSS/bus-01.voltage among them", keys)
		}

		// Every point the RTU serves is fed by a HELICS subscription.
		domainWantSet(t, "rtu-1 I/O subscription tags", io.texts("subscription/tag"),
			append(domainRegTags(domainOTSimRegs(dnp3)), domainRegTags(domainOTSimRegs(modbus))...)...)

		logic := x.one(t, "logic")
		domainWantText(t, logic, "period", "1s")
		domainWantText(t, logic, "program", "x = 1")
		domainWantText(t, logic, "variables/x", "0")
	})

	t.Run("power starts the HELICS broker and rtu-1 waits for it", func(t *testing.T) {
		broker := domainFields(c.disk(t, "power").read(t, domainHELICSBroker))
		if !strings.HasPrefix(broker, "helics_broker -f2 --ipv4 --web") {
			t.Errorf("power %s = %q, want helics_broker -f2 --ipv4 --web ... (base count 1 plus rtu-1)", domainHELICSBroker, broker)
		}

		domainWantContains(t, "rtu-1 "+domainWaitBroker, domainFields(c.disk(t, "rtu-1").read(t, domainWaitBroker)),
			"ping -c 1 -n -w 1 10.20.0.5")

		for _, vm := range []string{"power", "fep-1", "client-1"} {
			if c.disk(t, vm).has(domainWaitBroker) {
				t.Errorf("%s got %s, but only HELICS federates wait for the broker", vm, domainWaitBroker)
			}
		}

		domainWantAnnotation(t, c, "rtu-1", `[{"broker":"10.20.0.5","fed-count":1}]`)
	})

	t.Run("fep-1 polls every point rtu-1 serves and serves them again", func(t *testing.T) {
		rtu := c.xml(t, "rtu-1", domainOTSimConfig)
		fep := c.xml(t, "fep-1", domainOTSimConfig)

		domainWantSet(t, "fep-1 CPU modules", fep.modules(), "backplane", "dnp3", "modbus")

		mc := fep.one(t, "modbus[@mode=client]")
		domainWantText(t, mc, "endpoint", "10.20.0.11:502")
		domainWantPolls(t, "fep-1 Modbus client", mc, rtu.one(t, "modbus[@mode=server]"), true)

		dc := fep.one(t, "dnp3[@mode=client]")
		domainWantText(t, dc, "endpoint", "10.20.0.11:20001")
		domainWantText(t, dc, "master/local-address", "1")
		domainWantText(t, dc, "master/remote-address", "1024")
		// App-level helics gives a FEP's DNP3 master a 5 s scan rate.
		domainWantText(t, dc, "master/scan-rate", "5")
		domainWantPolls(t, "fep-1 DNP3 master", dc, rtu.one(t, "dnp3[@mode=server]"), true)

		served := append(domainRegTags(domainOTSimRegs(rtu.one(t, "dnp3[@mode=server]"))),
			domainRegTags(domainOTSimRegs(rtu.one(t, "modbus[@mode=server]")))...)

		ds := fep.one(t, "dnp3[@mode=server]")
		domainWantText(t, ds, "endpoint", "10.20.0.21:20000")
		domainWantSet(t, "fep-1 DNP3 outstation tags", domainRegTags(domainOTSimRegs(ds)), served...)

		ms := fep.one(t, "modbus[@mode=server]")
		domainWantText(t, ms, "endpoint", "10.20.0.21:502")
		domainWantSet(t, "fep-1 Modbus server tags", domainRegTags(domainOTSimRegs(ms)), served...)

		domainWantAnalogRegisters(t, "fep-1 Modbus server", domainOTSimRegs(ms))
	})

	t.Run("client-1 polls fep-1 with a DNP3 master", func(t *testing.T) {
		client := c.xml(t, "client-1", domainOTSimConfig)
		fep := c.xml(t, "fep-1", domainOTSimConfig)

		dc := client.one(t, "dnp3[@mode=client]")
		domainWantText(t, dc, "endpoint", "10.20.0.21:20000")
		domainWantText(t, dc, "master/local-address", "1")
		domainWantText(t, dc, "master/remote-address", "1024")
		// An fd-client takes a scan rate only from an explicit helics.scan-rate;
		// without one, ot-sim's own default (30 s) applies.
		domainWantNone(t, "client-1 DNP3 master", dc, "master/scan-rate")

		domainWantClientOfServers(t, "client-1", client, fep)
	})

	t.Run("the rendered script injects each config as /etc/ot-sim/config.xml", func(t *testing.T) {
		base := c.baseDir(t)

		for _, vm := range []string{"rtu-1", "fep-1", "client-1"} {
			domainWantPair(t, c, vm, filepath.Join(base, "ot-sim", vm+".xml"), domainOTSimConfig)
		}

		domainWantPair(t, c, "power", filepath.Join(base, "ot-sim", "power-helics-broker.sh"), domainHELICSBroker)
	})
}

func domainOTSimInfrastructure() domainRender {
	return domainRender{
		name:  "e2e-dom-otsinf",
		title: "DR-OTS-03 app-level infrastructure and API, FEP without app HELICS",
		topology: func(*domainCase) string {
			return domainTopology(
				domainLinux("rtu-1", "10.20.0.11"),
				domainLinux("fep-1", "10.20.0.21"),
				domainLinux("client-1", "10.20.0.31"),
			)
		},
		scenario: func(*domainCase) string {
			return domainScenario(domainApp{
				name: "ot-sim",
				metadata: domainMD{
					"infrastructure": "power-3-phase-distribution",
					"cpu-module":     domainMD{"api": domainMD{"endpoint": "0.0.0.0:9102"}},
				},
				hosts: []domainAppHost{
					{"rtu-1", domainMD{
						"type":   "fd-server",
						"modbus": []domainMD{{"name": "bus-01", "type": "bus"}},
						"dnp3":   []domainMD{{"name": "bus-02", "type": "bus"}},
						"helics": domainMD{"broker": domainMD{"address": "10.20.0.5"}},
					}},
					{"fep-1", domainMD{"type": "fep", "upstream": []string{"rtu-1"}}},
					// A host-level null api-endpoint disables the CPU API.
					{"client-1", domainMD{
						"type":           "fd-client",
						"connected_rtus": []string{"fep-1"},
						"cpu-module":     domainMD{"api-endpoint": nil},
					}},
				},
			})
		},
		check: domainCheckOTSimInfrastructure,
	}
}

func domainCheckOTSimInfrastructure(t *testing.T, c *domainCase) {
	t.Helper()

	t.Run("rtu-1 maps its points from the app-level infrastructure", func(t *testing.T) {
		x := c.xml(t, "rtu-1", domainOTSimConfig)

		phases := func(device string) []string {
			fields := []string{"Va-mag", "Vb-mag", "Vc-mag", "Va-ang", "Vb-ang", "Vc-ang"}
			tags := make([]string, 0, len(fields))

			for _, field := range fields {
				tags = append(tags, device+"."+field)
			}

			return tags
		}

		modbus := domainRegTags(domainOTSimRegs(x.one(t, "modbus[@mode=server]")))
		dnp3 := domainRegTags(domainOTSimRegs(x.one(t, "dnp3[@mode=server]")))

		domainWantSet(t, "rtu-1 Modbus register tags", modbus, phases("bus-01")...)
		domainWantSet(t, "rtu-1 DNP3 point tags", dnp3, phases("bus-02")...)
		domainWantSet(t, "rtu-1 I/O subscription tags", x.texts("io/subscription/tag"), append(modbus, dnp3...)...)
		domainWantText(t, x, "io/broker-endpoint", "10.20.0.5")

		domainWantContains(t, "rtu-1 "+domainWaitBroker, domainFields(c.disk(t, "rtu-1").read(t, domainWaitBroker)),
			"ping -c 1 -n -w 1 10.20.0.5")
		domainWantAnnotation(t, c, "rtu-1", `[{"broker":"10.20.0.5","fed-count":1}]`)
	})

	t.Run("an app-level API endpoint without TLS gets no TLS files", func(t *testing.T) {
		for _, vm := range []string{"rtu-1", "fep-1"} {
			x := c.xml(t, vm, domainOTSimConfig)
			domainWantText(t, x, "cpu/api/endpoint", "0.0.0.0:9102")

			domainWantNone(t, vm+" TLS settings", x, "cpu/api/tls-key", "cpu/api/tls-certificate", "cpu/api/ca-certificate")

			if certs := c.disk(t, vm).under("/etc/ot-sim/certs"); len(certs) > 0 {
				t.Errorf("%s has %d files under /etc/ot-sim/certs, but no TLS file was configured: %v", vm, len(certs), domainHead(certs))
			}
		}

		domainWantNone(t, "client-1 CPU API (api-endpoint is null)", c.xml(t, "client-1", domainOTSimConfig), "cpu/api")
	})

	t.Run("fep-1 builds a DNP3 master without app-level HELICS settings", func(t *testing.T) {
		rtu := c.xml(t, "rtu-1", domainOTSimConfig)
		fep := c.xml(t, "fep-1", domainOTSimConfig)

		dc := fep.one(t, "dnp3[@mode=client]")
		domainWantText(t, dc, "endpoint", "10.20.0.11:20000")
		domainWantText(t, dc, "master/remote-address", "1024")
		// Without app-level helics a FEP's master gets no scan rate, so
		// ot-sim's own default (30 s) applies.
		domainWantNone(t, "fep-1 DNP3 master", dc, "master/scan-rate")
		domainWantPolls(t, "fep-1 DNP3 master", dc, rtu.one(t, "dnp3[@mode=server]"), true)
		domainWantPolls(t, "fep-1 Modbus client", fep.one(t, "modbus[@mode=client]"), rtu.one(t, "modbus[@mode=server]"), true)

		// The downstream side defaults to DNP3 only.
		domainWantText(t, fep, "dnp3[@mode=server]/endpoint", "10.20.0.21:20000")
		domainWantNone(t, "fep-1 Modbus server", fep, "modbus[@mode=server]")
	})

	t.Run("client-1 polls only servers fep-1 runs", func(t *testing.T) {
		client := c.xml(t, "client-1", domainOTSimConfig)
		domainWantText(t, client, "dnp3[@mode=client]/master/remote-address", "1024")
		domainWantNone(t, "client-1 DNP3 master", client, "dnp3[@mode=client]/master/scan-rate")

		domainWantClientOfServers(t, "client-1", client, c.xml(t, "fep-1", domainOTSimConfig))
	})
}

func domainOTSimTLS() domainRender {
	return domainRender{
		name:  "e2e-dom-otstls",
		title: "DR-OTS-04 CPU API with TLS and a Node-RED flow",
		topology: func(*domainCase) string {
			return domainTopology(domainLinux("rtu-1", "10.20.0.11"))
		},
		scenario: func(c *domainCase) string {
			key, crt := domainTLSPair(c.t)

			return domainScenario(domainApp{
				name: "ot-sim",
				metadata: domainMD{
					"helics": domainMD{"broker": domainMD{"address": "10.20.0.5"}},
					"cpu-module": domainMD{"api": domainMD{
						"endpoint":        "0.0.0.0:9443",
						"tls-key":         c.file(c.t, "api.key", key),
						"tls-certificate": c.file(c.t, "api.crt", crt),
					}},
				},
				hosts: []domainAppHost{{"rtu-1", domainMD{
					"type":   "fd-server",
					"modbus": []domainMD{{"name": "bus-01", "type": "bus"}},
					"node-red": domainMD{
						"flow":     c.file(c.t, "flow.json", `[{"id":"e2e","type":"tab","label":"e2e"}]`+"\n"),
						"endpoint": domainMD{"port": 1880},
						"auth":     domainMD{"editor": domainMD{"user": "a", "pass": "b"}},
					},
				}}},
			})
		},
		check: domainCheckOTSimTLS,
	}
}

func domainCheckOTSimTLS(t *testing.T, c *domainCase) {
	t.Helper()

	t.Run("the TLS pair and the flow are on the disk", func(t *testing.T) {
		d := c.disk(t, "rtu-1")

		domainWantSame(t, d, "/etc/ot-sim/certs/api.key", c.path("api.key"))
		domainWantSame(t, d, "/etc/ot-sim/certs/api.crt", c.path("api.crt"))
		domainWantSame(t, d, "/etc/node-red.json", c.path("flow.json"))

		if d.has("/etc/ot-sim/certs/api.ca.crt") {
			t.Error("rtu-1 has /etc/ot-sim/certs/api.ca.crt, but no ca-certificate was configured")
		}
	})

	t.Run("the config points the API and Node-RED at them", func(t *testing.T) {
		x := c.xml(t, "rtu-1", domainOTSimConfig)

		domainWantText(t, x, "cpu/api/endpoint", "0.0.0.0:9443")
		domainWantText(t, x, "cpu/api/tls-key", "/etc/ot-sim/certs/api.key")
		domainWantText(t, x, "cpu/api/tls-certificate", "/etc/ot-sim/certs/api.crt")

		domainWantNone(t, "rtu-1 CA certificate", x, "cpu/api/ca-certificate")

		domainWantSet(t, "rtu-1 CPU modules", x.modules(), "backplane", "modbus", "i/o", "node-red")

		nr := x.one(t, "node-red")
		domainWantText(t, nr, "flow-path", "/etc/node-red.json")
		domainWantAttr(t, nr.one(t, "endpoint"), "port", "1880")
		domainWantAttr(t, nr.one(t, "endpoint"), "host", "0.0.0.0")
		domainWantAttr(t, nr.one(t, "authentication/editor"), "username", "a")
		domainWantAttr(t, nr.one(t, "authentication/editor"), "password", "b")
	})
}

func domainOTSimMissingFlow() domainRender {
	return domainRender{
		name:  "e2e-dom-otsnf",
		title: "DR-OTS-04 a missing Node-RED flow fails the start",
		fails: domainStartFailed,
		topology: func(*domainCase) string {
			return domainTopology(domainLinux("rtu-1", "10.20.0.11"))
		},
		scenario: func(c *domainCase) string {
			return domainScenario(domainApp{
				name:     "ot-sim",
				metadata: domainMD{"helics": domainMD{"broker": domainMD{"address": "10.20.0.5"}}},
				hosts: []domainAppHost{{"rtu-1", domainMD{
					"type":     "fd-server",
					"modbus":   []domainMD{{"name": "bus-01", "type": "bus"}},
					"node-red": domainMD{"flow": c.path("absent.json")},
				}}},
			})
		},
		check: func(t *testing.T, c *domainCase) {
			t.Helper()

			domainWantContains(t, "experiment start output", c.startOut, c.path("absent.json"))
		},
	}
}

func domainOTSimNoBroker() domainRender {
	return domainRender{
		name:  "e2e-dom-otsnob",
		title: "DR-OTS-05 an RTU with devices and no HELICS broker is refused",
		fails: domainStartFailed,
		topology: func(*domainCase) string {
			return domainTopology(domainLinux("rtu-1", "10.20.0.11"))
		},
		scenario: func(*domainCase) string {
			return domainScenario(domainApp{
				name:  "ot-sim",
				hosts: []domainAppHost{{"rtu-1", domainMD{"type": "fd-server", "modbus": []domainMD{{"name": "bus-01", "type": "bus"}}}}},
			})
		},
		check: func(t *testing.T, c *domainCase) {
			t.Helper()

			domainWantContains(t, "experiment start output", c.startOut, "user app ot-sim")

			if !domainHasLine(c.startOut, "rtu-1", "broker") {
				t.Errorf("no line of the start output names rtu-1 and its missing HELICS broker:\n%s", domainTail(c.startOut))
			}
		},
	}
}

// ---------------------------------------------------------------------------
// helics

func domainHELICSBrokers() domainRender {
	federate := func(name, address string, dnb bool, annotation string) domainHost {
		h := domainLinux(name, address)
		h.dnb = dnb
		h.annotations = `{"helics/federate": ` + annotation + `}`

		return h
	}

	return domainRender{
		name:  "e2e-dom-hel",
		title: "DR-HEL-01 and 02 root broker, sub-broker, labels and waits",
		topology: func(*domainCase) string {
			return domainTopology(
				domainLinux("power", "10.20.0.5"),
				domainLinux("sub", "10.20.0.12"),
				federate("fed-1", "10.20.0.11", false, `[{"broker": "10.20.0.12:24000", "fed-count": 2}]`),
				federate("fed-2", "10.20.0.13", false, `[{"broker": "10.20.0.5", "fed-count": 1}]`),
				federate("fed-3", "10.20.0.14", true, `[{"broker": "10.20.0.5", "fed-count": 1}]`),
				// The sub-broker named without a port, and as host|iface.
				federate("fed-4", "10.20.0.15", false, `[{"broker": "10.20.0.12", "fed-count": 1}]`),
				federate("fed-5", "10.20.0.16", false, `[{"broker": "sub|eth0", "fed-count": 1, "broker-wait": false}]`),
			)
		},
		scenario: func(*domainCase) string {
			return domainScenario(domainApp{
				name:     "helics",
				metadata: domainMD{"broker": domainMD{"root": "power|eth0", "log-dir": "/var/log/e2e"}},
			})
		},
		check:   domainCheckHELICSBrokers,
		mmCheck: domainCheckHELICSTags,
	}
}

// domainHELICSLabels are the labels the helics app gives each node of the
// broker case; fed-3 does not boot and gets none.
func domainHELICSLabels() map[string]map[string]string {
	broker := map[string]string{"group": "helics", "helics": "broker"}
	federate := map[string]string{"group": "helics", "helics": "federate"}

	return map[string]map[string]string{
		"power": broker, "sub": broker,
		"fed-1": federate, "fed-2": federate, "fed-4": federate, "fed-5": federate,
	}
}

func domainCheckHELICSBrokers(t *testing.T, c *domainCase) {
	t.Helper()

	t.Run("labels mark the brokers and the booting federates", func(t *testing.T) {
		want := domainHELICSLabels()

		for _, n := range c.doc(t).Spec.Topology.Nodes {
			got := n.Labels
			if len(got) == 0 {
				got = nil
			}

			if w := want[n.General.Hostname]; fmt.Sprint(got) != fmt.Sprint(w) {
				t.Errorf("%s labels = %v, want %v", n.General.Hostname, got, w)
			}
		}
	})

	t.Run("the root broker counts every federate and its sub-broker", func(t *testing.T) {
		script := domainFields(c.disk(t, "power").read(t, domainHELICSBroker))

		if n := strings.Count(script, "helics_broker "); n != 1 {
			t.Errorf("power starts %d brokers, want 1: %q", n, script)
		}

		// fed-1 (2), fed-2 (1), fed-4 and fed-5 (1 each); fed-3 does not boot.
		for _, want := range []string{"--name power", "--subbrokers 1 --web", "-f5", "--logfile /var/log/e2e/helics-root-broker.log"} {
			domainWantContains(t, "power "+domainHELICSBroker, script, want)
		}
	})

	t.Run("every annotation form of the sub-broker lands on one sub-broker", func(t *testing.T) {
		script := domainFields(c.disk(t, "sub").read(t, domainHELICSBroker))

		if n := strings.Count(script, "helics_broker "); n != 1 {
			t.Errorf("sub starts %d brokers, want 1 for 10.20.0.12:24000, 10.20.0.12 and sub|eth0: %q", n, script)
		}

		for _, want := range []string{
			"--name sub", "--broker 10.20.0.5 --local_interface 10.20.0.12:24000", "-f4",
			"--logfile /var/log/e2e/helics-sub-broker.log",
		} {
			domainWantContains(t, "sub "+domainHELICSBroker, script, want)
		}
	})

	t.Run("federates wait for the root broker unless told not to", func(t *testing.T) {
		for _, vm := range []string{"fed-1", "fed-2", "fed-4"} {
			domainWantContains(t, vm+" "+domainWaitBroker, domainFields(c.disk(t, vm).read(t, domainWaitBroker)),
				"ping -c 1 -n -w 1 10.20.0.5")
		}

		for _, vm := range []string{"fed-5", "power", "sub"} {
			if c.disk(t, vm).has(domainWaitBroker) {
				t.Errorf("%s got %s (fed-5 sets broker-wait: false; brokers are not federates)", vm, domainWaitBroker)
			}
		}
	})

	t.Run("the do-not-boot federate is not launched or injected", func(t *testing.T) {
		block := c.block(t, "fed-3")

		if !slices.Contains(block, "## DoNotBoot: true ##") {
			t.Errorf("fed-3 block lacks the DoNotBoot marker: %q", block)
		}

		for _, l := range block {
			if strings.HasPrefix(l, "disk ") || strings.HasPrefix(l, "vm launch") {
				t.Errorf("fed-3 block has %q", l)
			}
		}
	})
}

// domainCheckHELICSTags checks the labels as minimega tags on the VMs.
func domainCheckHELICSTags(t *testing.T, c *domainCase) {
	t.Helper()

	t.Run("minimega tags the VMs with the labels", func(t *testing.T) {
		info := vmInfo(t, c.name)

		for vm, want := range domainHELICSLabels() {
			var got map[string]string
			if err := json.Unmarshal([]byte(info[vm]["tags"]), &got); err != nil {
				t.Errorf("%s vm info tags %q: %v", vm, info[vm]["tags"], err)

				continue
			}

			if fmt.Sprint(got) != fmt.Sprint(want) {
				t.Errorf("%s minimega tags = %v, want %v", vm, got, want)
			}
		}

		if _, ok := info["fed-3"]; ok {
			t.Error("minimega lists the do-not-boot fed-3")
		}
	})
}

func domainHELICSNoRoot() domainRender {
	return domainRender{
		name:  "e2e-dom-helnr",
		title: "DR-HEL-03 a root broker is required",
		fails: domainStartFailed,
		topology: func(*domainCase) string {
			return domainTopology(domainLinux("power", "10.20.0.5"))
		},
		scenario: func(*domainCase) string {
			return domainScenario(domainApp{name: "helics", metadata: domainMD{}})
		},
		check: func(t *testing.T, c *domainCase) {
			t.Helper()

			domainWantContains(t, "experiment start output", c.startOut, "no root broker provided, but required")
		},
	}
}

func domainHELICSConflict() domainRender {
	return domainRender{
		name:  "e2e-dom-helcf",
		title: "DR-HEL-04 ot-sim and helics both writing the broker script is refused",
		fails: domainStartFailed,
		topology: func(*domainCase) string {
			return domainTopology(domainLinux("power", "10.20.0.5"), domainLinux("rtu-1", "10.20.0.11"))
		},
		scenario: func(*domainCase) string {
			return domainScenario(
				domainApp{
					name:     "ot-sim",
					metadata: domainMD{"helics": domainMD{"broker": domainMD{"hostname": "power|eth0", "base-fed-count": 1}}},
					hosts: []domainAppHost{{"rtu-1", domainMD{
						"type":   "fd-server",
						"modbus": []domainMD{{"name": "bus-01", "type": "bus"}},
					}}},
				},
				domainApp{name: "helics", metadata: domainMD{"broker": domainMD{"root": "power|eth0"}}},
			)
		},
		check: func(t *testing.T, c *domainCase) {
			t.Helper()

			domainWantContains(t, "experiment start output", c.startOut,
				"ot-sim: helics.broker for 'power' sets base-fed-count or dynamic, but the helics app also writes "+
					"/etc/phenix/startup/90-helics-broker.sh for 'power'")

			if strings.Contains(c.startOut, "destination appears twice") {
				t.Error("the conflict reached minimega's disk inject instead of failing in the app")
			}
		},
	}
}

// ---------------------------------------------------------------------------
// wind-turbine

// domainWindTopology is renewables/wind/turbine/phenix-configs/topology.yml
// from sceptre-phenix-topologies with grafana dropped, hollow disks, and the
// turbine VLAN renumbered from the public 1.1.1.0/24 to 10.41.0.0/24.
const domainWindTopology = `apiVersion: phenix.sandia.gov/v1
kind: Topology
metadata:
  name: {{name}}
spec:
  nodes:
  - general:
      hostname: wtg-1-main-controller
    hardware:
      drives:
      - image: e2e-ext4.qc2
      memory: 256
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
      - image: e2e-ext4.qc2
      memory: 256
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
      - image: e2e-ext4.qc2
      memory: 256
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
      - image: e2e-ext4.qc2
      memory: 256
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
      - image: e2e-ext4.qc2
      memory: 256
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
  - general:
      hostname: wtg-1-blade-3
    hardware:
      drives:
      - image: e2e-ext4.qc2
      memory: 256
      os_type: linux
      vcpus: 1
    network:
      interfaces:
      - address: 10.41.0.33
        gateway: 10.41.0.254
        mask: 24
        name: IF0
        proto: static
        type: ethernet
        vlan: turbine
    type: VirtualMachine
`

// domainWindScenario is renewables/wind/turbine/phenix-configs/scenario.yml
// with the Elasticsearch ground truth removed, the replay data pointed at the
// test's fixture ({{weather}}) and the yaw controller starting at 100 degrees.
const domainWindScenario = `apiVersion: phenix.sandia.gov/v2
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
              replayData: {{weather}}
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
          - "$1-blade-3"
`

// domainWindColumns are the anemometer's CSV columns and the tags the
// scenario maps them to, in order.
func domainWindColumns() [][2]string {
	return [][2]string{
		{"Windspeed 58.2m", "speed.high"}, {"Windspeed 36.6m", "speed.med"}, {"Windspeed 15.0m", "speed.low"},
		{"Wind Direction 58.2m", "dir.high"}, {"Wind Direction 36.6m", "dir.med"}, {"Wind Direction 15.0m", "dir.low"},
		{"Temperature 58.2m", "temp.high"}, {"Temperature 3.0m", "temp.low"}, {"Barometric Pressure", "pressure"},
	}
}

// domainWeather is a constant replay file. The real one is a Git LFS object,
// and every value stays within what a Modbus register at scaling 2 holds.
func domainWeather() string {
	var b strings.Builder

	for i, col := range domainWindColumns() {
		if i > 0 {
			b.WriteString(",")
		}

		b.WriteString(col[0])
	}

	b.WriteString("\n")

	for range 120 {
		b.WriteString("10.0,9.0,8.0,90.0,90.0,90.0,288.15,289.15,101.3\n")
	}

	return b.String()
}

func domainWindTurbineRender() domainRender {
	return domainRender{
		name:     "e2e-dom-wt",
		title:    "DR-WT-01 the wind turbine topology renders six agreeing configs",
		topology: func(*domainCase) string { return domainWindTopology },
		scenario: func(c *domainCase) string {
			return strings.ReplaceAll(domainWindScenario, "{{weather}}", c.file(c.t, "weather-const.csv", domainWeather()))
		},
		check: domainCheckWindTurbine,
	}
}

func domainWindHosts() []string {
	return []string{
		"wtg-1-main-controller", "wtg-1-yaw-controller", "wtg-1-signal-converter",
		"wtg-1-blade-1", "wtg-1-blade-2", "wtg-1-blade-3",
	}
}

func domainCheckWindTurbine(t *testing.T, c *domainCase) {
	t.Helper()

	t.Run("every turbine VM gets its config from the ot-sim directory", func(t *testing.T) {
		base := c.baseDir(t)

		for _, vm := range domainWindHosts() {
			domainWantPair(t, c, vm, filepath.Join(base, "ot-sim", vm+".xml"), domainOTSimConfig)
			c.xml(t, vm, domainOTSimConfig)
		}
	})

	t.Run("the main controller polls each turbine controller as it serves", func(t *testing.T) {
		main := c.xml(t, "wtg-1-main-controller", domainOTSimConfig)

		domainWantSet(t, "main controller CPU modules", main.modules(),
			"backplane", "turbine-power-output", "logic", "modbus", "dnp3")

		for peer, endpoint := range map[string]string{
			"wtg-1-signal-converter": "10.41.0.21:502",
			"wtg-1-yaw-controller":   "10.41.0.11:502",
			"wtg-1-blade-1":          "10.41.0.31:502",
			"wtg-1-blade-2":          "10.41.0.32:502",
			"wtg-1-blade-3":          "10.41.0.33:502",
		} {
			client := main.one(t, "modbus[@name="+peer+"][@mode=client]")
			domainWantText(t, client, "endpoint", endpoint)

			server := c.xml(t, peer, domainOTSimConfig).one(t, "modbus[@mode=server]")
			domainWantText(t, server, "endpoint", endpoint)
			domainWantPolls(t, "main controller Modbus client of "+peer, client, server, true)
		}
	})

	t.Run("the main controller serves DNP3 and models the turbine", func(t *testing.T) {
		main := c.xml(t, "wtg-1-main-controller", domainOTSimConfig)

		dnp3 := main.one(t, "dnp3[@mode=server]")
		domainWantText(t, dnp3, "endpoint", "192.168.100.1:20000")
		domainWantText(t, dnp3, "outstation/local-address", "1024")

		po := main.one(t, "wind-turbine/power-output")
		domainWantText(t, po, "turbine-type", "E-126/4200")
		domainWantText(t, po, "hub-height", "135")
		domainWantText(t, po, "roughness-length", "0.15")

		domainWantAttr(t, main.one(t, "logic/variables/current_yaw"), "tag", "yaw.current")
		domainWantAttr(t, main.one(t, "logic/variables/yaw_setpoint"), "tag", "yaw.setpoint")
	})

	t.Run("the yaw controller starts at the scenario's position and rate", func(t *testing.T) {
		yaw := c.xml(t, "wtg-1-yaw-controller", domainOTSimConfig)

		domainWantText(t, yaw, "logic/variables/current_yaw", "100")
		domainWantContains(t, "yaw controller logic", yaw.text(t, "logic/program"), "current_yaw + (dir * 0.1)")
	})

	t.Run("the anemometer replays the fixture", func(t *testing.T) {
		domainWantSame(t, c.disk(t, "wtg-1-signal-converter"), "/etc/ot-sim/data/weather.csv", c.path("weather-const.csv"))

		x := c.xml(t, "wtg-1-signal-converter", domainOTSimConfig)
		domainWantText(t, x, "wind-turbine/anemometer/data-path", "/etc/ot-sim/data/weather.csv")

		cols := x.find("wind-turbine/anemometer/weather-data/column")

		got := make([][2]string, 0, len(cols))
		for _, col := range cols {
			got = append(got, [2]string{col.attr("name"), strings.TrimSpace(col.chars)})
		}

		if fmt.Sprint(got) != fmt.Sprint(domainWindColumns()) {
			t.Errorf("anemometer columns = %v, want %v", got, domainWindColumns())
		}
	})
}

func domainWindTurbineNoReplayData() domainRender {
	return domainRender{
		name:     "e2e-dom-wtnod",
		title:    "DR-WT-02 the default replay data is missing and fails the start",
		fails:    domainStartFailed,
		topology: func(*domainCase) string { return domainWindTopology },
		scenario: func(c *domainCase) string {
			if _, err := os.Stat("/phenix/injects/weather.csv"); err == nil {
				c.t.Fatal("/phenix/injects/weather.csv exists on this host, so the app's default replay data is not missing")
			}

			return strings.Replace(domainWindScenario, "              replayData: {{weather}}\n", "", 1)
		},
		check: func(t *testing.T, c *domainCase) {
			t.Helper()

			domainWantContains(t, "experiment start output", c.startOut, "/phenix/injects/weather.csv")
		},
	}
}

// ---------------------------------------------------------------------------
// sceptre

// domainSoapTopology is the provider, rtu-1 and rtu-2 of soap-legacy's
// soap-topology.yaml (sceptre-phenix-topologies) on hollow disks, plus a FEP
// and a client on the same networks.
const domainSoapTopology = `apiVersion: phenix.sandia.gov/v1
kind: Topology
metadata:
  name: {{name}}
spec:
  nodes:
  - general:
      hostname: provider
    hardware:
      drives:
      - image: e2e-ext4.qc2
      memory: 256
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
      - image: e2e-ext4.qc2
      memory: 256
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
      - image: e2e-ext4.qc2
      memory: 256
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
{{extra}}`

// domainSoapNode is one more soap-style node with an EXP and a MGMT address.
func domainSoapNode(hostname, image, osType, exp, mgmt string) string {
	return fmt.Sprintf(`  - general:
      hostname: %s
    hardware:
      drives:
      - image: %s
      memory: %d
      os_type: %s
      vcpus: 1
    network:
      interfaces:
      - address: %s
        mask: 24
        name: exp
        proto: static
        type: ethernet
        vlan: EXP
      - address: %s
        mask: 16
        name: mgmt
        proto: static
        type: ethernet
        vlan: MGMT
    type: VirtualMachine
`, hostname, image, domainMemory, osType, exp, mgmt)
}

// domainSoapHosts are the provider and rtu-1 of soap-legacy's sceptre.yaml,
// with rtu-2 serving Modbus instead of DNP3, a FEP in front of rtu-1 and a
// client polling both RTUs. {{case}} is the case file.
const domainSoapHosts = `      - hostname: provider
        metadata:
          case: {{case}}
          labels:
          - elk
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
          labels:
          - elk
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
          labels:
          - elk
          provider: provider
          type: fd-server
      - hostname: fep-1
        metadata:
          type: fep
          provider: provider
          infrastructure: power-transmission
          connected_rtus:
          - rtu-1
      - hostname: client-1
        metadata:
          type: fd-client
          connected_rtus:
          - rtu-1
          - rtu-2
`

// domainCase300 stands in for soap-legacy's injects/case300.py: the app only
// copies the case file, PyPower reads it in the guest.
const domainCase300 = `# e2e stand-in for a PYPOWER case file
from numpy import array

def case300():
    ppc = {"version": "2"}
    ppc["baseMVA"] = 100.0
    ppc["bus"] = array([[242, 1, 0, 0, 0, 0, 1, 1.0, 0, 345, 1, 1.1, 0.9]])
    return ppc
`

func domainSceptreSoap() domainRender {
	return domainRender{
		name:  "e2e-dom-sce",
		title: "DR-SCE-01 and 03 provider, RTUs, FEP and client through fromScenario",
		topology: func(*domainCase) string {
			return strings.Replace(domainSoapTopology, "{{extra}}",
				domainSoapNode("fep-1", ext4Image, "linux", "10.117.4.110", "172.16.1.10")+
					domainSoapNode("client-1", ext4Image, "linux", "10.117.4.120", "172.16.1.20"), 1)
		},
		before: func(t *testing.T, c *domainCase) {
			t.Helper()

			// The app's hosts live in their own scenario, which the
			// experiment's scenario pulls in with fromScenario; it has to
			// name the topology it was written for.
			c.config(t, "scenario", "e2e-dom-scesc", `apiVersion: phenix.sandia.gov/v2
kind: Scenario
metadata:
  name: e2e-dom-scesc
  annotations:
    topology: e2e-dom-sce
spec:
  apps:
  - name: sceptre
    hosts:
`+strings.ReplaceAll(domainSoapHosts, "{{case}}", c.file(t, "case300.py", domainCase300)))
		},
		scenario: func(*domainCase) string {
			return domainScenario(domainApp{name: "sceptre", fromScenario: "e2e-dom-scesc"})
		},
		check: domainCheckSceptreSoap,
	}
}

func domainCheckSceptreSoap(t *testing.T, c *domainCase) {
	t.Helper()

	t.Run("the provider runs PyPower on the case file", func(t *testing.T) {
		d := c.disk(t, "provider")

		ini := domainINI(d.read(t, "/etc/sceptre/config.ini"))["power-solver-service"]
		for key, want := range map[string]string{
			"solver-type":      "PyPower",
			"debug":            "False",
			"server-endpoint":  "tcp://172.16.1.2:5555",
			"publish-endpoint": "udp://*;239.0.0.1:40000",
			"case-file":        "/etc/sceptre/case300.py",
		} {
			if ini[key] != want {
				t.Errorf("provider config.ini %s = %q, want %q", key, ini[key], want)
			}
		}

		domainWantSame(t, d, "/etc/sceptre/case300.py", c.path("case300.py"))

		start := d.read(t, domainSceptreStart)
		domainWantContains(t, "provider sceptre-start.sh", start, "pybennu-power-solver start -c /etc/sceptre/config.ini -e pybennu -d")
		domainWantContains(t, "provider sceptre-start.sh", start, "ListenAddress 172.16.1.2")
	})

	t.Run("rtu-1 serves DNP3 fed by the provider", func(t *testing.T) {
		x := c.xml(t, "rtu-1", domainSceptreConfig)
		fd := x.one(t, "field-device")

		domainWantText(t, fd, "name", "rtu-1")
		domainWantText(t, fd, "comms/dnp3-server/endpoint", "tcp://10.117.4.101:20000")
		domainWantText(t, fd, "comms/dnp3-server/address", "10")
		domainWantText(t, fd, "input/endpoint", "udp://172.16.1.4;239.0.0.1:40000")
		domainWantText(t, fd, "output/endpoint", "tcp://172.16.1.2:5555")
		domainWantSceptreTags(t, "rtu-1", fd, "dnp3-server")

		// The provider's bus-242.voltage reaches the first DNP3 analog input.
		if got := domainSceptrePoint(fd, "dnp3-server", "bus-242.voltage"); got != "analog-input@0" {
			t.Errorf("rtu-1 serves bus-242.voltage as %q, want analog-input@0", got)
		}

		d := c.disk(t, "rtu-1")
		start := d.read(t, domainSceptreStart)
		domainWantContains(t, "rtu-1 sceptre-start.sh", start, "bennu-watcherd restart")
		domainWantContains(t, "rtu-1 sceptre-start.sh", start, "ListenAddress 10.117.4.101")
		domainWantContains(t, "rtu-1 sceptre-start.sh", start, "ListenAddress 172.16.1.4")

		// phenix sets the source, which the app wrote in the experiment's
		// base directory, to the injection's permissions; minimega copies it
		// without -p, so the daemon's umask (022) applies.
		if m := d.mode(t, domainSceptreStart); m != 0o744 {
			t.Errorf("rtu-1 %s mode = %v, want 0744", domainSceptreStart, m)
		}
	})

	t.Run("rtu-2 serves Modbus fed by the provider", func(t *testing.T) {
		fd := c.xml(t, "rtu-2", domainSceptreConfig).one(t, "field-device")

		domainWantText(t, fd, "comms/modbus-server/endpoint", "tcp://10.117.4.102:502")
		domainWantText(t, fd, "input/endpoint", "udp://172.16.1.5;239.0.0.1:40000")
		domainWantSceptreTags(t, "rtu-2", fd, "modbus-server")
	})

	t.Run("client-1 polls both RTUs at the points they serve", func(t *testing.T) {
		comms := c.xml(t, "client-1", domainSceptreConfig).one(t, "field-device/comms")
		rtu1 := c.xml(t, "rtu-1", domainSceptreConfig).one(t, "field-device/comms/dnp3-server")
		rtu2 := c.xml(t, "rtu-2", domainSceptreConfig).one(t, "field-device/comms/modbus-server")

		dnp3 := comms.one(t, "dnp3-client/dnp3-connection")
		domainWantText(t, dnp3, "endpoint", "tcp://10.117.4.101:20000")
		domainWantText(t, dnp3, "address", "10")
		domainWantText(t, comms, "dnp3-client/command-interface", "tcp://10.117.4.120:1330")
		domainWantSet(t, "client-1 DNP3 points of rtu-1", domainSceptrePoints(dnp3), domainSceptrePoints(rtu1)...)

		modbus := comms.one(t, "modbus-client/modbus-connection")
		domainWantText(t, modbus, "endpoint", "tcp://10.117.4.102:502")
		domainWantText(t, comms, "modbus-client/command-interface", "tcp://10.117.4.120:1331")
		domainWantSet(t, "client-1 Modbus points of rtu-2", domainSceptrePoints(modbus), domainSceptrePoints(rtu2)...)
	})

	t.Run("fep-1 polls rtu-1 and serves DNP3 itself", func(t *testing.T) {
		comms := c.xml(t, "fep-1", domainSceptreConfig).one(t, "field-device/comms")
		rtu1 := c.xml(t, "rtu-1", domainSceptreConfig).one(t, "field-device/comms/dnp3-server")

		conn := comms.one(t, "dnp3-client/dnp3-connection")
		domainWantText(t, conn, "endpoint", "tcp://10.117.4.101:20000")
		domainWantText(t, conn, "address", "10")
		domainWantSet(t, "fep-1 DNP3 points of rtu-1", domainSceptrePoints(conn), domainSceptrePoints(rtu1)...)
		domainWantText(t, comms, "dnp3-server/endpoint", "tcp://10.117.4.110:20000")
	})

	t.Run("every config the app wrote parses", func(t *testing.T) {
		// On the disks, through the XML helper (c.xml parses strictly) ...
		for _, vm := range []string{"rtu-1", "rtu-2", "fep-1", "client-1"} {
			c.xml(t, vm, domainSceptreConfig)
		}

		// ... and everything under the app's output directory.
		domainAllXMLParse(t, filepath.Join(c.baseDir(t), "sceptre"))
	})

	t.Run("elk-labelled hosts get the beats start script", func(t *testing.T) {
		for _, vm := range []string{"provider", "rtu-1", "rtu-2"} {
			d := c.disk(t, vm)
			domainWantContains(t, vm+" elk-start.sh", d.read(t, domainStartup+"elk-start.sh"), "service filebeat start")
		}

		var fdlist map[string]map[string]int

		domainJSON(t, "elk/fdlist.json", domainReadHost(t, filepath.Join(c.baseDir(t), "elk", "fdlist.json")), &fdlist)

		if fdlist["rtu-1"]["20000-local"] != 1 || fdlist["rtu-2"]["502-local"] != 1 {
			t.Errorf("elk/fdlist.json = %v, want rtu-1 on DNP3 20000 and rtu-2 on Modbus 502", fdlist)
		}
	})
}

func domainSceptreOverrides() domainRender {
	marker := "<?xml version=\"1.0\"?>\n<SCEPTRE><field-device><name>e2e-override</name></field-device></SCEPTRE>\n"
	start := "#!/bin/sh\necho e2e-override-start\n"

	// The start script override's mode as written, which the umask narrows.
	var startMode os.FileMode

	return domainRender{
		name:  "e2e-dom-sceov",
		title: "DR-SCE-02 override files from the asset directory win",
		topology: func(*domainCase) string {
			return strings.Replace(domainSoapTopology, "{{extra}}", "", 1)
		},
		scenario: func(c *domainCase) string {
			c.file(c.t, "asset/injects/override/rtu-1_config.xml", marker)
			c.file(c.t, "asset/injects/override/rtu-9_config.xml", marker)
			info, err := os.Stat(c.file(c.t, "asset/injects/override/rtu-1_sceptre-start.sh", start))
			if err != nil {
				c.t.Fatal(err)
			}

			startMode = info.Mode().Perm()

			return domainScenario(domainApp{
				name:     "sceptre",
				assetDir: c.path("asset"),
				hosts: []domainAppHost{
					{"provider", domainMD{
						"type": "provider", "simulator": "PyPower", "case": c.file(c.t, "case300.py", domainCase300),
						"publish_endpoint": "udp://*;239.0.0.1:40000",
					}},
					{"rtu-1", domainMD{
						"type": "fd-server", "provider": "provider", "infrastructure": "power-transmission",
						"dnp3": []domainMD{{"name": "bus-242", "type": "bus"}},
					}},
					{"rtu-2", domainMD{
						"type": "fd-server", "provider": "provider", "infrastructure": "power-transmission",
						"dnp3": []domainMD{{"name": "bus-243", "type": "bus"}},
					}},
				},
			})
		},
		check: func(t *testing.T, c *domainCase) {
			t.Helper()

			domainWantSame(t, c.disk(t, "rtu-1"), domainSceptreConfig, c.path("asset/injects/override/rtu-1_config.xml"))
			domainWantSame(t, c.disk(t, "rtu-2"), domainSceptreConfig, filepath.Join(c.baseDir(t), "sceptre", "rtu-2", "config.xml"))
			domainWantText(t, c.xml(t, "rtu-2", domainSceptreConfig), "field-device/name", "rtu-2")

			domainWantContains(t, "experiment create output", c.createOut, "Overriding 'rtu-1_config.xml'")

			// The start script override comes with the app's permissions 0744
			// from outside the base directory, so phenix injects a copy with
			// them (which checkInjects follows) and leaves the asset file's
			// own mode alone; minimega's umask (022) keeps 0744.
			override := c.path("asset/injects/override/rtu-1_sceptre-start.sh")
			d := c.disk(t, "rtu-1")

			domainWantSame(t, d, domainSceptreStart, override)
			domainWantContains(t, "experiment create output", c.createOut, "Overriding 'rtu-1_sceptre-start.sh'")

			if m := d.mode(t, domainSceptreStart); m != 0o744 {
				t.Errorf("rtu-1 %s mode = %v, want 0744", domainSceptreStart, m)
			}

			if info, err := os.Stat(override); err != nil {
				t.Error(err)
			} else if m := info.Mode().Perm(); m != startMode {
				t.Errorf("the override %s has mode %v after the start, want the %v it was written with", override, m, startMode)
			}

			if !slices.ContainsFunc(c.node(t, "rtu-1").Injections, func(inj domainInjectionDoc) bool {
				return inj.Src == override && inj.Dst == domainSceptreStart && inj.Permissions == "0744"
			}) {
				t.Errorf("rtu-1's stored injections lack %s -> %s with permissions 0744: %+v",
					override, domainSceptreStart, c.node(t, "rtu-1").Injections)
			}

			if !domainHasLine(c.createOut, "override files that matched no injection", "rtu-9_config.xml") {
				t.Errorf("experiment create does not warn about the unused rtu-9_config.xml override:\n%s", domainTail(c.createOut))
			}
		},
	}
}

func domainSceptreWindows() domainRender {
	windows := []struct{ host, exp, mgmt string }{
		{"opc", "10.117.4.150", "172.16.1.50"},
		{"scada", "10.117.4.151", "172.16.1.51"},
		{"hmi", "10.117.4.152", "172.16.1.52"},
		{"hist", "10.117.4.153", "172.16.1.53"},
		{"eng", "10.117.4.154", "172.16.1.54"},
	}

	return domainRender{
		name:  "e2e-dom-scewin",
		title: "DR-SCE-04 the Windows device types on NTFS",
		topology: func(*domainCase) string {
			var extra strings.Builder

			for _, w := range windows {
				extra.WriteString(domainSoapNode(w.host, domainNTFSImage, "windows", w.exp, w.mgmt))
			}

			// rtu-2 stays in the topology but out of the scenario.
			return strings.Replace(domainSoapTopology, "{{extra}}", extra.String(), 1)
		},
		scenario: func(c *domainCase) string {
			return domainScenario(domainApp{
				name: "sceptre",
				hosts: []domainAppHost{
					{"provider", domainMD{
						"type": "provider", "simulator": "PyPower", "case": c.file(c.t, "case300.py", domainCase300),
						"publish_endpoint": "udp://*;239.0.0.1:40000",
					}},
					{"rtu-1", domainMD{
						"type": "fd-server", "provider": "provider", "infrastructure": "power-transmission",
						"dnp3": []domainMD{{"name": "bus-242", "type": "bus"}},
					}},
					{"opc", domainMD{"type": "opc", "connected_rtus": []string{"rtu-1"}}},
					{"scada", domainMD{"type": "scada-server"}},
					{"hmi", domainMD{"type": "hmi"}},
					{"hist", domainMD{"type": "historian"}},
					{"eng", domainMD{"type": "engineer-workstation", "connect_interval": 60}},
				},
			})
		},
		check: domainCheckSceptreWindows,
	}
}

func domainCheckSceptreWindows(t *testing.T, c *domainCase) {
	t.Helper()

	const scheduler = "/ProgramData/Microsoft/Windows/Start Menu/Programs/Startup/"

	t.Run("every Windows host gets both startup schedulers", func(t *testing.T) {
		for _, vm := range []string{"opc", "scada", "hmi", "hist", "eng"} {
			d := c.disk(t, vm)

			for _, p := range []string{
				scheduler + "sceptre-startup_scheduler.cmd", "/sceptre/sceptre-startup.ps1",
				// phenix's own Windows startup.
				scheduler + "startup_scheduler.cmd", "/phenix/phenix-startup.ps1", "/phenix/startup/20-startup.ps1",
			} {
				if !d.has(p) {
					t.Errorf("%s has no %s", vm, p)
				}
			}
		}
	})

	t.Run("each device type gets its config and startup script", func(t *testing.T) {
		for vm, files := range map[string][]string{
			"opc":   {"/sceptre/startup/30-topserver.ps1", "/Users/wwuser/Documents/Configs/Inject/opc.xml"},
			"scada": {"/sceptre/startup/30-scada.ps1"},
			"hmi":   {"/sceptre/startup/30-hmi.ps1"},
			"hist":  {"/sceptre/startup/30-historian.ps1", "/Users/wwuser/Documents/Configs/Inject/historian_config.txt"},
			"eng":   {"/sceptre/startup/30-putty.ps1", "/sceptre/startup/40-auto_winscp.ps1"},
		} {
			for _, p := range files {
				if !c.disk(t, vm).has(p) {
					t.Errorf("%s has no %s", vm, p)
				}
			}
		}
	})

	t.Run("the OPC config parses and polls rtu-1", func(t *testing.T) {
		opc := c.disk(t, "opc").read(t, "/Users/wwuser/Documents/Configs/Inject/opc.xml")

		domainParseXML(t, "opc opc.xml", opc)
		domainWantContains(t, "opc opc.xml", opc, "<dnp_master_ethernet:DestinationIP>10.117.4.101</dnp_master_ethernet:DestinationIP>")
	})

	t.Run("the generated SCADA project lands in the profile's Documents", func(t *testing.T) {
		src := filepath.Join(c.baseDir(t), "sceptre", "scada", "autoproject")
		files := domainHostFiles(t, src)

		if len(files) == 0 {
			t.Fatalf("the app generated no SCADA project under %s", src)
		}

		d := c.disk(t, "scada")

		for _, rel := range files {
			domainWantSame(t, d, path.Join(domainNTFSProfile, "autoproject", rel), filepath.Join(src, rel))
		}
	})
}

func domainSceptreNoProvider() domainRender {
	return domainRender{
		name:  "e2e-dom-scenop",
		title: "DR-SCE-05 the scenario check refuses a model without a provider",
		fails: domainCreateFailed,
		topology: func(*domainCase) string {
			return strings.Replace(domainSoapTopology, "{{extra}}", "", 1)
		},
		scenario: func(*domainCase) string {
			return domainScenario(domainApp{
				name: "sceptre",
				hosts: []domainAppHost{{"rtu-1", domainMD{
					"type": "fd-server", "provider": "provider", "infrastructure": "power-transmission",
					"dnp3": []domainMD{{"name": "bus-242", "type": "bus"}},
				}}},
			})
		},
		check: func(t *testing.T, c *domainCase) {
			t.Helper()

			for _, want := range []string{"sceptre scenario check failed", "no provider is defined", "metadata.provider 'provider' matches no provider host"} {
				domainWantContains(t, "experiment create output", c.createOut, want)
			}

			if out := phenix(t, "experiment", "list"); strings.Contains(out, c.name) {
				t.Errorf("phenix experiment list shows %s after its create failed:\n%s", c.name, out)
			}
		},
	}
}

func domainSceptreHELICS() domainRender {
	return domainRender{
		name:  "e2e-dom-scehx",
		title: "DR-SCE-06 a HELICS provider and broker by label",
		topology: func(*domainCase) string {
			return domainTopology(
				domainHost{name: "provider", ifaces: []domainIface{{"mgmt", "MGMT", "172.16.1.2", 16}}},
				domainHost{name: "bkr", ifaces: []domainIface{{"mgmt", "MGMT", "172.16.1.3", 16}}},
				domainHost{name: "rtu-1", ifaces: []domainIface{{"exp", "EXP", "10.117.4.101", 24}, {"mgmt", "MGMT", "172.16.1.4", 16}}},
			)
		},
		scenario: func(*domainCase) string {
			return domainScenario(domainApp{
				name: "sceptre",
				hosts: []domainAppHost{
					{"provider", domainMD{
						"type": "provider", "simulator": "Helics", "labels": []string{"helics-federate"},
						"helics": domainMD{"broker": "bkr"}, "publish_endpoint": "udp://*;239.0.0.1:40000",
					}},
					{"bkr", domainMD{"labels": []string{"helics-broker"}}},
					{"rtu-1", domainMD{
						"type": "fd-server", "provider": "provider", "infrastructure": "power-transmission",
						"dnp3": []domainMD{{"name": "bus-242", "type": "bus"}},
					}},
				},
			})
		},
		check: domainCheckSceptreHELICS,
	}
}

func domainCheckSceptreHELICS(t *testing.T, c *domainCase) {
	t.Helper()

	t.Run("the provider federate subscribes to every point the RTU reads", func(t *testing.T) {
		d := c.disk(t, "provider")

		var fed struct {
			Name          string `json:"name"`
			BrokerAddress string `json:"broker_address"`
			Subscriptions []struct {
				Key string `json:"key"`
			} `json:"subscriptions"`
		}

		domainJSON(t, "provider helics.json", d.read(t, "/etc/sceptre/helics.json"), &fed)

		if fed.Name != "provider" || fed.BrokerAddress != "172.16.1.3" {
			t.Errorf("provider helics.json name %q broker_address %q, want provider and bkr's 172.16.1.3", fed.Name, fed.BrokerAddress)
		}

		keys := make([]string, 0, len(fed.Subscriptions))
		for _, s := range fed.Subscriptions {
			keys = append(keys, s.Key)
		}

		inputs := c.xml(t, "rtu-1", domainSceptreConfig).texts("field-device/input/*/name")

		want := make([]string, 0, len(inputs))
		for _, name := range inputs {
			want = append(want, "provider/"+name)
		}

		domainWantSet(t, "provider HELICS subscriptions", keys, want...)

		ini := domainINI(d.read(t, "/etc/sceptre/config.ini"))["power-solver-service"]
		if ini["solver-type"] != "Helics" || ini["config-file"] != "/etc/sceptre/helics.json" {
			t.Errorf("provider config.ini = %v, want solver-type Helics with config-file /etc/sceptre/helics.json", ini)
		}
	})

	t.Run("the broker host starts one broker for the one federate", func(t *testing.T) {
		got := c.disk(t, "bkr").read(t, domainStartup+"helics-broker-start.sh")
		want := "helics_broker --autorestart --ipv4 -f 1 --logfile=/etc/sceptre/log/helics_broker.log --fileloglevel=3 &\n"

		if got != want {
			t.Errorf("bkr helics-broker-start.sh = %q, want %q", got, want)
		}
	})
}

// domainWantSceptreTags checks that a bennu field device's tags hang
// together: every point its comms block serves is an external tag, and every
// provider point it reads or writes is the I/O of one.
func domainWantSceptreTags(t *testing.T, vm string, fd *domainXML, comms string) {
	t.Helper()

	names := fd.texts("tags/external-tag/name")
	ios := fd.texts("tags/external-tag/io")

	for _, tag := range fd.texts("comms/" + comms + "/*/tag") {
		if !slices.Contains(names, tag) {
			t.Errorf("%s %s serves %s, which is no external tag", vm, comms, tag)
		}
	}

	for _, id := range append(fd.texts("input/*/id"), fd.texts("output/*/id")...) {
		if !slices.Contains(ios, id) {
			t.Errorf("%s maps provider point %s, which is no external tag's I/O", vm, id)
		}
	}

	if len(names) == 0 || len(fd.texts("input/*/id")) == 0 {
		t.Errorf("%s has %d external tags and %d provider inputs, want some of each", vm, len(names), len(fd.texts("input/*/id")))
	}
}

// domainSceptrePoint returns how a bennu field device serves the provider
// point named name: "<element>@<address>" of the comms point whose tag is the
// external tag fed by that point.
func domainSceptrePoint(fd *domainXML, comms, name string) string {
	var id string

	for _, in := range fd.find("input/*") {
		if strings.TrimSpace(in.childText("name")) == name {
			id = strings.TrimSpace(in.childText("id"))
		}
	}

	var tag string

	for _, et := range fd.find("tags/external-tag") {
		if strings.TrimSpace(et.childText("io")) == id && id != "" {
			tag = strings.TrimSpace(et.childText("name"))
		}
	}

	for _, p := range fd.find("comms/" + comms + "/*") {
		if strings.TrimSpace(p.childText("tag")) == tag && tag != "" {
			return p.name + "@" + strings.TrimSpace(p.childText("address"))
		}
	}

	return ""
}

// domainSceptrePoints lists the points of a bennu comms block as
// "<element>@<address>", which is what a client and a server must agree on.
func domainSceptrePoints(block *domainXML) []string {
	var out []string

	for _, p := range block.kids {
		if p.find("address") != nil && p.find("tag") != nil {
			out = append(out, p.name+"@"+strings.TrimSpace(p.childText("address")))
		}
	}

	return out
}

// ---------------------------------------------------------------------------
// caldera

func domainCalderaRender() domainRender {
	return domainRender{
		name:  "e2e-dom-cal",
		title: "DR-CAL-01 and 02 server nodes and agents",
		topology: func(*domainCase) string {
			return domainTopology(
				domainLinux("alice", "10.20.0.11"),
				domainWindows("bob", "10.20.0.12"),
			)
		},
		scenario: func(c *domainCase) string {
			return domainScenario(domainApp{
				name: "caldera",
				metadata: domainMD{"servers": []domainMD{
					{
						"hostname": "mallory", "image": ext4Image, "memory": 2 * domainMemory,
						"interfaces": []domainMD{{"vlan": "OT", "address": "10.20.0.50/24", "gateway": "10.20.0.254"}},
						"facts":      []string{c.file(c.t, "facts.yml", "id: e2e-facts\nname: e2e facts\nfacts: []\n")},
						"adversaries": []string{c.file(c.t, "adversary.yml",
							"id: e2e-adv\nname: e2e adversary\natomic_ordering: []\n")},
					},
					// A server whose interface has no gateway.
					{
						"hostname": "eve", "image": ext4Image, "memory": domainMemory, "cpu": 1,
						"interfaces": []domainMD{{"vlan": "OT", "address": "10.20.0.51/24"}},
					},
				}},
				hosts: []domainAppHost{
					{"alice", domainMD{"server": "mallory:0"}},
					{"bob", domainMD{"server": "10.20.0.50"}},
				},
			})
		},
		check:   domainCheckCaldera,
		mmCheck: domainCheckCalderaVMs,
	}
}

func domainCheckCaldera(t *testing.T, c *domainCase) {
	t.Helper()

	t.Run("configure adds the servers as KVM nodes", func(t *testing.T) {
		for _, want := range []struct {
			host             string
			vcpus, memory    int
			address, gateway string
		}{
			{"mallory", 2, 2 * domainMemory, "10.20.0.50", "10.20.0.254"},
			{"eve", 1, domainMemory, "10.20.0.51", ""},
		} {
			n := c.node(t, want.host)

			if n.General.VMType != "kvm" || n.Hardware.VCPU != want.vcpus || n.Hardware.Memory != want.memory ||
				n.Hardware.OSType != "linux" || len(n.Hardware.Drives) != 1 || n.Hardware.Drives[0].Image != ext4Image {
				t.Errorf("%s = %+v %+v, want a %d-vCPU %d MB linux kvm node on %s",
					want.host, n.General, n.Hardware, want.vcpus, want.memory, ext4Image)
			}

			if len(n.Network.Interfaces) != 1 {
				t.Fatalf("%s has interfaces %+v, want one", want.host, n.Network.Interfaces)
			}

			iface := n.Network.Interfaces[0]
			if iface.Name != "IF0" || iface.VLAN != "OT" || iface.Address != want.address || iface.Mask != 24 ||
				iface.Gateway != want.gateway {
				t.Errorf("%s interface = %+v, want IF0 on OT with %s/24 and gateway %q", want.host, iface, want.address, want.gateway)
			}

			if !slices.Contains(c.block(t, want.host), "vm launch kvm "+want.host) {
				t.Errorf("the rendered script does not launch %s", want.host)
			}
		}

		iface := c.disk(t, "mallory").read(t, domainStartup+"3_interfaces-start.sh")
		domainWantContains(t, "mallory interfaces script", iface, "ip route add default via 10.20.0.254")

		if s := c.disk(t, "eve").read(t, domainStartup+"3_interfaces-start.sh"); strings.Contains(s, "route add default") {
			t.Errorf("eve's interfaces script adds a default route, but its server interface has no gateway:\n%s", s)
		}
	})

	t.Run("the servers get the config, facts, adversaries and browser setup", func(t *testing.T) {
		for vm, addr := range map[string]string{"mallory": "10.20.0.50", "eve": "10.20.0.51"} {
			d := c.disk(t, vm)

			if !domainHasLine(d.read(t, "/opt/caldera/conf/default.yml"), "api_key_red: ADMIN123") {
				t.Errorf("%s default.yml has no api_key_red: ADMIN123", vm)
			}

			var policies struct {
				Policies struct {
					Bookmarks []struct {
						URL string `json:"URL"`
					} `json:"Bookmarks"`
				} `json:"policies"`
			}

			domainJSON(t, vm+" policies.json", d.read(t, "/etc/firefox/policies/policies.json"), &policies)

			if b := policies.Policies.Bookmarks; len(b) != 1 || b[0].URL != "http://"+addr+":8888" {
				t.Errorf("%s Firefox bookmarks = %+v, want http://%s:8888", vm, b, addr)
			}

			domainWantContains(t, vm+" Caldera.desktop", d.read(t, "/root/.config/autostart/Caldera.desktop"),
				"Exec=firefox http://"+addr+":8888")
		}

		d := c.disk(t, "mallory")
		domainWantSame(t, d, "/opt/caldera/data/sources/facts.yml", c.path("facts.yml"))
		domainWantSame(t, d, "/opt/caldera/data/adversaries/adversary.yml", c.path("adversary.yml"))
	})

	t.Run("the agents call home to the server they name", func(t *testing.T) {
		domainWantContains(t, "alice 90-sandcat-agent.sh", c.disk(t, "alice").read(t, domainStartup+"90-sandcat-agent.sh"),
			`server="http://10.20.0.50:8888"`)
		domainWantContains(t, "bob 90-sandcat-agent.ps1", c.disk(t, "bob").read(t, "/phenix/startup/90-sandcat-agent.ps1"),
			`$server="http://10.20.0.50:8888"`)
	})
}

// domainCheckCalderaVMs checks the added servers as minimega runs them.
func domainCheckCalderaVMs(t *testing.T, c *domainCase) {
	t.Helper()

	t.Run("minimega runs the servers with the app's hardware", func(t *testing.T) {
		info := vmInfo(t, c.name)

		for vm, want := range map[string][2]string{
			"mallory": {"2", strconv.Itoa(2 * domainMemory)},
			"eve":     {"1", strconv.Itoa(domainMemory)},
		} {
			if info[vm]["vcpus"] != want[0] || info[vm]["memory"] != want[1] {
				t.Errorf(
					"minimega runs %s with %s vCPUs and %s MB, want %s and %s",
					vm,
					info[vm]["vcpus"],
					info[vm]["memory"],
					want[0],
					want[1],
				)
			}
		}
	})
}

// ---------------------------------------------------------------------------
// ignition

// domainIgnitionTopology is the topology of the app's own
// tests/test_ignition_input.yaml (sceptre-phenix-apps) with hollow disks.
func domainIgnitionTopology(rtus ...domainHost) string {
	return domainTopology(append([]domainHost{
		domainWindows("OT-scada", "10.68.30.190"),
		domainWindows("hmi-1", "10.68.30.191"),
	}, rtus...)...)
}

func domainIgnitionGateway() domainRender {
	return domainRender{
		name:  "e2e-dom-ign",
		title: "DR-IGN-01 gateway, Perspective, API and an HMI client",
		topology: func(*domainCase) string {
			return domainIgnitionTopology(domainLinux("ieee13-1-rtu", "10.68.30.201"), domainLinux("ieee13-2-rtu", "10.68.30.202"))
		},
		scenario: func(*domainCase) string {
			// The hosts of the app's test input, without the historian.
			return domainScenario(domainApp{
				name: "ignition",
				hosts: []domainAppHost{
					{"OT-scada", domainMD{
						"type":        "gateway",
						"perspective": true,
						"api":         domainMD{"auth": true, "require_https": true, "roles": []string{"Operator"}},
						"connected_rtus": []any{
							"ieee13-1-rtu",
							domainMD{"hostname": "ieee13-2-rtu", "name": "farm-controller", "destination_address": 1023},
						},
					}},
					{"hmi-1", domainMD{"type": "perspective"}},
				},
			})
		},
		check: domainCheckIgnitionGateway,
	}
}

const domainIgnitionData = "/Program Files/Inductive Automation/Ignition/data"

func domainCheckIgnitionGateway(t *testing.T, c *domainCase) {
	t.Helper()

	t.Run("each RTU becomes a DNP3 device of the gateway", func(t *testing.T) {
		d := c.disk(t, "OT-scada")
		devices := domainIgnitionData + "/config/resources/core/com.inductiveautomation.opcua/device/"

		for name, want := range map[string]domainIgnitionConn{
			"ieee13-1-rtu":    {Hostname: "10.68.30.201", Port: 20000, DestinationAddress: 1024, SourceAddress: 1},
			"farm-controller": {Hostname: "10.68.30.202", Port: 20000, DestinationAddress: 1023, SourceAddress: 1},
		} {
			var device struct {
				Settings struct {
					Connectivity domainIgnitionConn `json:"connectivity"`
				} `json:"settings"`
			}

			domainJSON(t, name+" config.json", d.read(t, devices+name+"/config.json"), &device)

			if device.Settings.Connectivity != want {
				t.Errorf("device %s connectivity = %+v, want %+v", name, device.Settings.Connectivity, want)
			}

			if !d.has(devices + name + "/resource.json") {
				t.Errorf("device %s has no resource.json", name)
			}
		}
	})

	t.Run("the Perspective and WebDev projects are in the data tree", func(t *testing.T) {
		d := c.disk(t, "OT-scada")
		projects := domainIgnitionData + "/projects/"

		var project struct {
			Title string `json:"title"`
		}

		domainJSON(t, "hmi project.json", d.read(t, projects+"hmi/project.json"), &project)

		if project.Title != "hmi" {
			t.Errorf("hmi project title = %q, want hmi", project.Title)
		}

		var view struct {
			Root struct {
				Props struct {
					Tabs []string `json:"tabs"`
				} `json:"props"`
			} `json:"root"`
		}

		domainJSON(t, "overview view.json", d.read(t, projects+"hmi/com.inductiveautomation.perspective/views/overview/view.json"), &view)

		for _, tab := range []string{"ieee13-1-rtu", "farm-controller"} {
			if !slices.Contains(view.Root.Props.Tabs, tab) {
				t.Errorf("overview tabs %v lack %s", view.Root.Props.Tabs, tab)
			}
		}

		var api struct {
			DoPost map[string]any `json:"doPost"`
		}

		domainJSON(t, "api tags config.json", d.read(t, projects+"api/com.inductiveautomation.webdev/resources/tags/config.json"), &api)

		for key, want := range map[string]any{"require-auth": true, "require-https": true, "required-roles": "Operator", "user-source": "default"} {
			if api.DoPost[key] != want {
				t.Errorf("api tags doPost %s = %v, want %v", key, api.DoPost[key], want)
			}
		}

		var tags []any

		domainJSON(t, "tags.json", d.read(t, domainIgnitionData+"/config/resources/core/ignition/tag-definition/default/tags.json"), &tags)

		if len(tags) != 1 {
			t.Errorf("tags.json holds %d tags, want the one tag-sync tag", len(tags))
		}
	})

	t.Run("the gateway and the HMI open the Perspective client", func(t *testing.T) {
		for vm, url := range map[string]string{
			"OT-scada": "http://localhost:8088/data/perspective/client/hmi",
			"hmi-1":    "http://10.68.30.190:8088/data/perspective/client/hmi",
		} {
			script := c.disk(t, vm).read(t, "/phenix/user-startup/99-ignition-perspective.ps1")

			domainWantContains(t, vm+" 99-ignition-perspective.ps1", script, url)

			if !domainCRLF(script) {
				t.Errorf("%s 99-ignition-perspective.ps1 does not use CRLF line endings", vm)
			}
		}
	})

	t.Run("every file the app built for the gateway is injected", func(t *testing.T) {
		dir := filepath.Join(c.baseDir(t), "ignition", "OT-scada")
		built := domainHostFiles(t, dir)

		var injected []string

		for _, p := range c.injects(t, "OT-scada") {
			if rel, err := filepath.Rel(dir, p.src); err == nil && !strings.HasPrefix(rel, "..") {
				injected = append(injected, filepath.ToSlash(rel))
			}
		}

		domainWantSet(t, "OT-scada injections from the app's build directory", injected, built...)
	})
}

// domainIgnitionConn is the connectivity block of an Ignition DNP3 device.
type domainIgnitionConn struct {
	Hostname           string `json:"hostname"`
	Port               int    `json:"port"`
	DestinationAddress int    `json:"destinationAddress"`
	SourceAddress      int    `json:"sourceAddress"`
}

func domainIgnitionRestore() domainRender {
	return domainRender{
		name:  "e2e-dom-igngw",
		title: "DR-IGN-02 a gateway backup is restored at boot",
		topology: func(*domainCase) string {
			return domainIgnitionTopology()
		},
		scenario: func(c *domainCase) string {
			backup := make([]byte, 64)
			if _, err := rand.Read(backup); err != nil {
				c.t.Fatal(err)
			}

			return domainScenario(domainApp{
				name:  "ignition",
				hosts: []domainAppHost{{"OT-scada", domainMD{"type": "gateway", "gwbk": c.file(c.t, "base.gwbk", string(backup))}}},
			})
		},
		check: func(t *testing.T, c *domainCase) {
			t.Helper()

			// The app stages the backup at the root of the system drive, and
			// 98-ignition.ps1 moves it into the gateway's data directory as
			// the timestamped __restore file the gateway restores when its
			// service starts again.
			d := c.disk(t, "OT-scada")
			domainWantSame(t, d, "/restore.gwbk", c.path("base.gwbk"))

			script := d.read(t, "/phenix/startup/98-ignition.ps1")

			for _, want := range []string{
				`$staged = 'C:\restore.gwbk'`,
				`$data = 'C:` + strings.ReplaceAll(domainIgnitionData, "/", `\`) + `'`,
				`Move-Item -Path $staged -Destination (Join-Path $data ("__restore_{0}.gwbk" -f $stamp))`,
				`Start-Service -Name Ignition`,
			} {
				domainWantContains(t, "98-ignition.ps1", domainFields(script), want)
			}

			if !domainCRLF(script) {
				t.Error("98-ignition.ps1 does not use CRLF line endings")
			}
		},
	}
}

func domainIgnitionMissingBackup() domainRender {
	return domainRender{
		name:     "e2e-dom-ignab",
		title:    "DR-IGN-02 a missing gateway backup is refused",
		fails:    domainStartFailed,
		topology: func(*domainCase) string { return domainIgnitionTopology() },
		scenario: func(c *domainCase) string {
			return domainScenario(domainApp{
				name:  "ignition",
				hosts: []domainAppHost{{"OT-scada", domainMD{"type": "gateway", "gwbk": c.path("absent.gwbk")}}},
			})
		},
		check: func(t *testing.T, c *domainCase) {
			t.Helper()

			domainWantContains(t, "experiment start output", c.startOut, "gwbk '"+c.path("absent.gwbk")+"' not found")
		},
	}
}

func domainIgnitionBackupAndRTUs() domainRender {
	return domainRender{
		name:  "e2e-dom-ignmx",
		title: "DR-IGN-02 a gateway backup with connected RTUs is refused",
		fails: domainStartFailed,
		topology: func(*domainCase) string {
			return domainIgnitionTopology(domainLinux("ieee13-1-rtu", "10.68.30.201"))
		},
		scenario: func(c *domainCase) string {
			return domainScenario(domainApp{
				name: "ignition",
				hosts: []domainAppHost{{"OT-scada", domainMD{
					"type": "gateway", "gwbk": c.file(c.t, "base.gwbk", "e2e"), "connected_rtus": []string{"ieee13-1-rtu"},
				}}},
			})
		},
		check: func(t *testing.T, c *domainCase) {
			t.Helper()

			domainWantContains(t, "experiment start output", c.startOut, "cannot be combined with 'connected_rtus'")
		},
	}
}

func domainIgnitionUnaddressedRTU() domainRender {
	return domainRender{
		name:  "e2e-dom-ignrt",
		title: "DR-IGN-03 an RTU without an address is refused",
		fails: domainStartFailed,
		topology: func(*domainCase) string {
			return domainIgnitionTopology(domainHost{name: "ghost", ifaces: []domainIface{{"eth0", "OT", "", 0}}})
		},
		scenario: func(*domainCase) string {
			return domainScenario(domainApp{
				name:  "ignition",
				hosts: []domainAppHost{{"OT-scada", domainMD{"type": "gateway", "connected_rtus": []string{"ghost"}}}},
			})
		},
		check: func(t *testing.T, c *domainCase) {
			t.Helper()

			domainWantContains(t, "experiment start output", c.startOut, "RTU 'ghost' has no addressed interface '(first)' in the topology")
		},
	}
}

// ---------------------------------------------------------------------------
// Running a case

// domainRender is one render case: an experiment built from a topology and a
// scenario, what must fail (nothing, its create, or its start), and the
// checks.
type domainRender struct {
	name     string // experiment name, and the name of its topology and scenario configs
	title    string // subtest name
	fails    string // "", domainCreateFailed or domainStartFailed
	topology func(c *domainCase) string
	scenario func(c *domainCase) string
	// before creates other configs the scenario refers to.
	before func(t *testing.T, c *domainCase)
	// check reads the snapshot disks, the stored experiment and the rendered
	// minimega script, or the output of what failed.
	check func(t *testing.T, c *domainCase)
	// mmCheck checks what minimega itself reports for the running VMs.
	mmCheck func(t *testing.T, c *domainCase)
}

// domainCase is the state of one running render case.
type domainCase struct {
	t          *testing.T // the case's test, for the fixtures the configs name
	name       string
	dir        string // fixture files the scenario names
	exp        *experiment
	createOut  string
	startOut   string
	qemuBefore int
	disks      map[string]*domainDisk
	stored     *domainExpDoc // the experiment as stored once it started
	rendered   string        // the minimega script phenix rendered
}

// domainRun runs one render case through phenix.
func domainRun(t *testing.T, rc domainRender) {
	t.Helper()

	c := domainNewCase(t, rc.name)

	c.config(t, "topology", c.name, rc.topology(c))

	if rc.before != nil {
		rc.before(t, c)
	}

	c.config(t, "scenario", c.name, rc.scenario(c))

	args := []string{"experiment", "create", c.name, "--topology", c.name, "--scenario", c.name}

	if rc.fails == domainCreateFailed {
		out, err := phenixRun(args...)
		c.createOut = out

		if err == nil {
			c.exp = &experiment{t: t, name: c.name}
			t.Cleanup(c.exp.teardown)
			t.Fatalf("phenix experiment create %s succeeded, want failure:\n%s", c.name, domainTail(out))
		}

		if _, err := phenixRun("config", "get", "experiment/"+c.name); err == nil {
			t.Errorf("experiment %s is stored although its create failed", c.name)
		}

		c.wantNoNamespace(t)
		rc.check(t, c)

		return
	}

	c.createOut = phenix(t, args...)
	c.exp = &experiment{t: t, name: c.name}

	// Registered after the config cleanups, so it runs first.
	t.Cleanup(c.exp.teardown)

	c.qemuBefore = qemuProcesses(t)

	if rc.fails == domainStartFailed {
		c.startOut = phenixFails(t, "experiment", "start", c.name)
		c.wantNoVMs(t)
		rc.check(t, c)

		return
	}

	c.startOut = c.exp.start()
	c.waitBooted(t)

	t.Run("every injection is on the snapshot disk as rendered", c.checkInjects)
	rc.check(t, c)

	if rc.mmCheck != nil {
		rc.mmCheck(t, c)
	}

	t.Run("stop leaves no VM behind", func(t *testing.T) {
		c.exp.with(t).stop()
		c.wantNoVMs(t)
	})
}

func domainNewCase(t *testing.T, name string) *domainCase {
	t.Helper()

	dir := filepath.Join(cfg.workDir, "domain", name)

	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}

	// Registered first, so it runs after the experiment's teardown: a start
	// that failed in minimega leaves its snapshots, minimega's files directory
	// for the namespace (where the caldera app also makes a directory)
	// outlives the experiment, and a create whose configure stage failed
	// leaves the base directory the apps wrote into, which only delete
	// removes. A namespace left behind is a failure the case reports; it is
	// cleared here so that it does not leak into later tests.
	t.Cleanup(func() {
		domainClearNamespace(t, name)
		domainRemoveSnapshots(t, name)

		if err := os.RemoveAll(filepath.Join(cfg.imagesDir, name)); err != nil {
			t.Errorf("removing minimega's files for %s: %v", name, err)
		}

		base := filepath.Join(domainExperimentsDir, name)

		if _, err := os.Stat(base); err == nil {
			t.Logf("phenix left the base directory %s; removing it", base)

			if err := os.RemoveAll(base); err != nil {
				t.Errorf("removing %s: %v", base, err)
			}
		}
	})

	return &domainCase{t: t, name: name, dir: dir, disks: map[string]*domainDisk{}}
}

// path is where fixture file name lives (or would live).
func (c *domainCase) path(name string) string {
	return filepath.Join(c.dir, filepath.FromSlash(name))
}

// file writes a fixture file the scenario names and returns its path.
func (c *domainCase) file(t *testing.T, name, content string) string {
	t.Helper()

	p := c.path(name)

	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(p, []byte(content), 0o644); err != nil { //nolint:gosec // minimega copies it into guests
		t.Fatal(err)
	}

	return p
}

// config creates a phenix config from YAML, removed when the case ends.
func (c *domainCase) config(t *testing.T, kind, name, yaml string) {
	t.Helper()

	p := c.file(t, name+"-"+kind+".yaml", strings.ReplaceAll(yaml, "{{name}}", c.name))
	phenix(t, "config", "create", p)

	t.Cleanup(func() { _, _ = phenixRun("config", "delete", kind+"/"+name) })
}

// waitBooted waits until minimega runs exactly the VMs that should boot.
func (c *domainCase) waitBooted(t *testing.T) {
	t.Helper()

	var booting []string

	for _, n := range c.doc(t).Spec.Topology.Nodes {
		if _, ok := n.Annotations["phenix/do-not-boot-reason"]; ok {
			t.Errorf("phenix marked %s do-not-boot: %v", n.General.Hostname, n.Annotations["phenix/do-not-boot-reason"])
		}

		if n.General.DoNotBoot == nil || !*n.General.DoNotBoot {
			booting = append(booting, n.General.Hostname)
		}
	}

	c.exp.with(t).waitRunning(domainBootTimeout, booting...)

	domainWantSet(t, "VMs minimega runs", domainKeys(vmInfo(t, c.name)), booting...)
}

// wantNoNamespace checks, without entering (and so creating) it, that minimega
// holds no namespace for the experiment. phenix clears it on stop and when a
// start fails in minimega, and does not create it while an app's configure or
// pre-start stage runs (app/user.go PopulateRuntime).
func (c *domainCase) wantNoNamespace(t *testing.T) {
	t.Helper()

	ns, ok := mmNamespaces(t)[c.name]
	if !ok {
		return
	}

	t.Errorf("minimega still has namespace %s: %v", c.name, ns)

	if info := vmInfo(t, c.name); len(info) > 0 {
		t.Errorf("minimega still has VMs in %s: %v", c.name, domainKeys(info))
	}
}

// wantNoVMs checks that the experiment left no namespace and no VM in
// minimega and no QEMU process behind.
func (c *domainCase) wantNoVMs(t *testing.T) {
	t.Helper()

	c.wantNoNamespace(t)

	eventually(t, domainStopTimeout, time.Second, "QEMU processes to exit", func() (bool, string) {
		n := qemuProcesses(t)

		return n == c.qemuBefore, fmt.Sprintf("%d running, %d before", n, c.qemuBefore)
	})
}

// checkInjects checks phenix's half and minimega's half of every injection:
// each one in the stored spec is in the VM's `disk inject` line, from a copy
// with its permissions for a source outside the base directory, and each
// pair of that line is on the snapshot with the source's bytes.
func (c *domainCase) checkInjects(t *testing.T) {
	base := c.baseDir(t)

	for _, n := range c.doc(t).Spec.Topology.Nodes {
		vm := n.General.Hostname
		if n.General.DoNotBoot != nil && *n.General.DoNotBoot {
			continue
		}

		pairs := c.injects(t, vm)

		for _, inj := range n.Injections {
			src := inj.Src
			if !strings.HasPrefix(src, "/") {
				src = base + "/" + src
			}

			if inj.Permissions != "" && domainStagedSource(base, src) {
				domainWantStaged(t, vm, base, pairs, src, inj.Dst, inj.Permissions)

				continue
			}

			if !slices.Contains(pairs, domainPair{src, inj.Dst}) {
				t.Errorf("%s injection %s -> %s is not in the rendered disk inject line", vm, src, inj.Dst)
			}
		}

		d := c.disk(t, vm)

		for _, p := range pairs {
			// phenix refuses an injection without a source, so a pair from the
			// experiment directory itself comes from an app's path bug (such
			// as "." or "<base>/") and would copy the whole directory.
			if filepath.Clean(p.src) == filepath.Clean(base) {
				t.Errorf("%s injects the whole experiment directory %s as %s", vm, p.src, p.dst)

				continue
			}

			domainWantInjected(t, d, p)
		}
	}
}

// domainStagedSource says whether phenix injects a copy of src instead of
// src itself for an injection with permissions: a regular file outside the
// base directory, whose mode phenix leaves alone (types/version/v1
// injectionSource; a source inside gets the mode itself).
func domainStagedSource(base, src string) bool {
	resolved, err := filepath.EvalSymlinks(src)
	if err != nil {
		return false
	}

	if b, err := filepath.EvalSymlinks(base); err == nil {
		if rel, err := filepath.Rel(b, resolved); err == nil && rel != ".." && !strings.HasPrefix(rel, "../") {
			return false
		}
	}

	info, err := os.Stat(resolved)

	return err == nil && info.Mode().IsRegular()
}

// domainWantStaged checks the copy phenix injects, for an injection with
// permissions, of a source outside the base directory: the VM's disk inject
// line carries a copy under <base>/injections/ to dst, with the source's
// bytes and the injection's permissions.
func domainWantStaged(t *testing.T, vm, base string, pairs []domainPair, src, dst, perms string) {
	t.Helper()

	dir := filepath.Join(base, "injections") + "/"

	i := slices.IndexFunc(pairs, func(p domainPair) bool { return p.dst == dst && strings.HasPrefix(p.src, dir) })
	if i < 0 {
		t.Errorf("%s injection %s -> %s (permissions %s) has no copy under %s in the rendered disk inject line: %v",
			vm, src, dst, perms, dir, pairs)

		return
	}

	staged := pairs[i].src

	if got, want := domainReadHost(t, staged), domainReadHost(t, src); got != want {
		t.Errorf("%s: %s, which phenix injects as %s, does not hold the bytes of its source %s", vm, staged, dst, src)
	}

	want, err := strconv.ParseUint(perms, 8, 32)
	if err != nil {
		t.Errorf("%s injection to %s has permissions %q, not an octal mode", vm, dst, perms)

		return
	}

	if info, err := os.Stat(staged); err != nil {
		t.Errorf("%s: %v", vm, err)
	} else if info.Mode().Perm() != fs.FileMode(want) {
		t.Errorf("%s: %s, which phenix injects as %s, has mode %v, want the injection's %s", vm, staged, dst, info.Mode().Perm(), perms)
	}
}

// domainWantInjected checks one injected file, or every file of an injected
// directory, against its source on the host.
func domainWantInjected(t *testing.T, d *domainDisk, p domainPair) {
	t.Helper()

	dst := domainGuestPath(p.dst)

	info, err := os.Stat(p.src)
	if err != nil {
		t.Errorf("%s: source %s of %s: %v", d.vm, p.src, p.dst, err)

		return
	}

	if !info.IsDir() {
		domainWantSame(t, d, dst, p.src)

		return
	}

	// cp -fr copies a directory into the destination when that exists and
	// as the destination otherwise.
	if into := path.Join(dst, filepath.Base(p.src)); d.isDir(into) {
		dst = into
	}

	for _, rel := range domainHostFiles(t, p.src) {
		domainWantSame(t, d, path.Join(dst, rel), filepath.Join(p.src, rel))
	}
}

// ---------------------------------------------------------------------------
// The stored experiment and the rendered script

type domainExpDoc struct {
	Spec struct {
		BaseDir  string `json:"baseDir"`
		Topology struct {
			Nodes []domainNodeDoc `json:"nodes"`
		} `json:"topology"`
	} `json:"spec"`
}

type domainNodeDoc struct {
	Annotations map[string]any    `json:"annotations"`
	Labels      map[string]string `json:"labels"`
	General     struct {
		Hostname  string `json:"hostname"`
		VMType    string `json:"vm_type"`
		DoNotBoot *bool  `json:"do_not_boot"`
	} `json:"general"`
	Hardware struct {
		VCPU   int    `json:"vcpus"`
		Memory int    `json:"memory"`
		OSType string `json:"os_type"`
		Drives []struct {
			Image string `json:"image"`
		} `json:"drives"`
	} `json:"hardware"`
	Network struct {
		Interfaces []struct {
			Name    string `json:"name"`
			VLAN    string `json:"vlan"`
			Address string `json:"address"`
			Mask    int    `json:"mask"`
			Gateway string `json:"gateway"`
		} `json:"interfaces"`
	} `json:"network"`
	Injections []domainInjectionDoc `json:"injections"`
}

type domainInjectionDoc struct {
	Src         string `json:"src"`
	Dst         string `json:"dst"`
	Permissions string `json:"permissions"`
}

// doc returns the experiment as phenix stored it when it started, which
// holds what the apps changed at configure and pre-start.
func (c *domainCase) doc(t *testing.T) domainExpDoc {
	t.Helper()

	if c.stored != nil {
		return *c.stored
	}

	out := phenix(t, "config", "get", "experiment/"+c.name, "--output", "json")

	var d domainExpDoc
	if err := json.NewDecoder(strings.NewReader(jsonPart(out))).Decode(&d); err != nil {
		t.Fatalf("decoding experiment %s: %v\n%s", c.name, err, domainTail(out))
	}

	c.stored = &d

	return d
}

// node returns a node of the stored experiment.
func (c *domainCase) node(t *testing.T, hostname string) domainNodeDoc {
	t.Helper()

	for _, n := range c.doc(t).Spec.Topology.Nodes {
		if n.General.Hostname == hostname {
			return n
		}
	}

	t.Fatalf("experiment %s has no node %s", c.name, hostname)

	return domainNodeDoc{}
}

func (c *domainCase) baseDir(t *testing.T) string {
	t.Helper()

	base := c.doc(t).Spec.BaseDir
	if base == "" {
		t.Fatalf("experiment %s has no base directory", c.name)
	}

	return base
}

// script returns the minimega script phenix rendered for the case.
func (c *domainCase) script(t *testing.T) string {
	t.Helper()

	if c.rendered == "" {
		c.rendered = domainReadHost(t, filepath.Join(c.baseDir(t), "mm_files", c.name+".mm"))
	}

	return c.rendered
}

// block returns the lines of the rendered script for one VM.
func (c *domainCase) block(t *testing.T, vm string) []string {
	t.Helper()

	var (
		out []string
		in  bool
	)

	for line := range strings.SplitSeq(c.script(t), "\n") {
		line = strings.TrimSpace(line)

		if strings.HasPrefix(line, "## VM: ") {
			in = line == "## VM: "+vm+" ##"

			continue
		}

		if in && line != "" {
			out = append(out, line)
		}
	}

	if len(out) == 0 {
		t.Fatalf("the rendered script has no block for %s", vm)
	}

	return out
}

// domainPair is one "src":"dst" pair of a `disk inject` line.
type domainPair struct {
	src, dst string
}

var domainPairRE = regexp.MustCompile(`"([^"]*)":"([^"]*)"`)

// injects returns the pairs of the VM's `disk inject ... files` line.
func (c *domainCase) injects(t *testing.T, vm string) []domainPair {
	t.Helper()

	var pairs []domainPair

	for _, line := range c.block(t, vm) {
		if !strings.HasPrefix(line, "disk inject ") || strings.Contains(line, " delete files ") {
			continue
		}

		for _, m := range domainPairRE.FindAllStringSubmatch(line, -1) {
			pairs = append(pairs, domainPair{m[1], m[2]})
		}
	}

	return pairs
}

// domainWantPair checks that the VM's disk inject line carries src -> dst.
func domainWantPair(t *testing.T, c *domainCase, vm, src, dst string) {
	t.Helper()

	if !slices.Contains(c.injects(t, vm), domainPair{src, dst}) {
		t.Errorf("%s disk inject line has no %q:%q", vm, src, dst)
	}
}

// domainWantAnnotation checks a node's helics/federate annotation as JSON.
func domainWantAnnotation(t *testing.T, c *domainCase, vm, want string) {
	t.Helper()

	got, err := json.Marshal(c.node(t, vm).Annotations[domainHELICSLabel])
	if err != nil {
		t.Fatal(err)
	}

	if string(got) != want {
		t.Errorf("%s %s annotation = %s, want %s", vm, domainHELICSLabel, got, want)
	}
}

// ---------------------------------------------------------------------------
// Snapshot disks

// domainDisk is the content of a VM's snapshot disk, read once.
type domainDisk struct {
	vm    string
	files map[string]domainFile // by guest path, "/"-rooted
}

type domainFile struct {
	data []byte
	mode fs.FileMode
	dir  bool
}

// disk returns the VM's snapshot disk.
func (c *domainCase) disk(t *testing.T, vm string) *domainDisk {
	t.Helper()

	if d, ok := c.disks[vm]; ok {
		return d
	}

	d := domainSnapshotDisk(t, c, vm)
	d.vm = vm
	c.disks[vm] = d

	return d
}

// snapshot returns the snapshot and the image of the VM's `disk snapshot`
// line.
func (c *domainCase) snapshot(t *testing.T, vm string) (string, string) {
	t.Helper()

	for _, line := range c.block(t, vm) {
		if f := strings.Fields(line); len(f) == 4 && f[0] == "disk" && f[1] == "snapshot" {
			return filepath.Join(cfg.imagesDir, f[3]), f[2]
		}
	}

	t.Fatalf("the rendered script has no disk snapshot line for %s", vm)

	return "", ""
}

// domainSnapshotDisk reads partition 1 of the VM's snapshot, which phenix
// asked minimega to inject into.
func domainSnapshotDisk(t *testing.T, c *domainCase, vm string) *domainDisk {
	t.Helper()

	snap, image := c.snapshot(t, vm)

	if image == domainNTFSImage {
		return domainReadNTFS(t, snap)
	}

	var d *domainDisk

	inspectDisk(t, snap, func(root string) { d = domainWalk(t, root) })

	return d
}

// domainReadNTFS is inspectDisk for the NTFS fixture: noload is an ext4
// option, so the partition is mounted read-only with ntfs-3g.
func domainReadNTFS(t *testing.T, image string) *domainDisk {
	t.Helper()

	nbdMu.Lock()
	defer nbdMu.Unlock()

	nbdConnect(t, image, "-r")

	defer nbdDisconnect(t)

	part := nbdDevice + "p1"

	waitNBDPartition(t, part)

	mnt := t.TempDir()

	unmount, err := domainMountNTFS(part, mnt, "ro")
	if err != nil {
		t.Fatal(err)
	}

	defer func() {
		if err := unmount(); err != nil {
			t.Error(err)
		}
	}()

	return domainWalk(t, mnt)
}

// domainMountNTFS mounts an NTFS partition with ntfs-3g in the foreground,
// so that unmounting can wait for ntfs-3g to have written everything.
func domainMountNTFS(dev, mnt, mode string) (func() error, error) {
	var out bytes.Buffer

	cmd := exec.Command("ntfs-3g", "-o", mode+",no_detach", dev, mnt)
	cmd.Stdout = &out
	cmd.Stderr = &out

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting ntfs-3g on %s: %w", dev, err)
	}

	exited := make(chan error, 1)

	go func() { exited <- cmd.Wait() }()

	deadline := time.Now().Add(domainMountTimeout)

	for !domainMounted(mnt) {
		select {
		case err := <-exited:
			return nil, fmt.Errorf("ntfs-3g %s on %s exited before mounting: %w\n%s", dev, mnt, err, out.String())
		default:
		}

		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			<-exited

			return nil, fmt.Errorf("ntfs-3g did not mount %s on %s within %v:\n%s", dev, mnt, domainMountTimeout, out.String())
		}

		time.Sleep(domainPoll)
	}

	return func() error {
		if err := run("umount", mnt); err != nil {
			_ = cmd.Process.Kill()
			<-exited

			return err
		}

		select {
		case <-exited:
			return nil
		case <-time.After(domainMountTimeout):
			_ = cmd.Process.Kill()
			<-exited

			return fmt.Errorf("ntfs-3g did not exit after unmounting %s", mnt)
		}
	}, nil
}

func domainMounted(mnt string) bool {
	data, err := os.ReadFile("/proc/self/mounts")
	if err != nil {
		return false
	}

	for line := range strings.SplitSeq(string(data), "\n") {
		if f := strings.Fields(line); len(f) > 1 && f[1] == mnt {
			return true
		}
	}

	return false
}

// domainWalk reads every file under a mounted guest filesystem.
func domainWalk(t *testing.T, root string) *domainDisk {
	t.Helper()

	d := &domainDisk{files: map[string]domainFile{}}

	err := filepath.WalkDir(root, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}

		info, err := e.Info()
		if err != nil {
			return err
		}

		f := domainFile{mode: info.Mode().Perm(), dir: e.IsDir()}

		if info.Mode().IsRegular() {
			if f.data, err = os.ReadFile(p); err != nil { //nolint:gosec // guest file
				return err
			}
		}

		d.files[domainGuestPath(filepath.ToSlash(rel))] = f

		return nil
	})
	if err != nil {
		t.Fatalf("reading the guest filesystem at %s: %v", root, err)
	}

	return d
}

// domainGuestPath is the "/"-rooted guest path of an injection destination,
// which may be relative to the partition root ("Users/...").
func domainGuestPath(p string) string {
	return path.Clean("/" + p)
}

func (d *domainDisk) has(p string) bool {
	f, ok := d.files[domainGuestPath(p)]

	return ok && !f.dir
}

func (d *domainDisk) isDir(p string) bool {
	return d.files[domainGuestPath(p)].dir
}

func (d *domainDisk) bytes(t *testing.T, p string) []byte {
	t.Helper()

	f, ok := d.files[domainGuestPath(p)]
	if !ok || f.dir {
		t.Fatalf("%s has no file %s on its snapshot disk", d.vm, p)
	}

	return f.data
}

func (d *domainDisk) read(t *testing.T, p string) string {
	t.Helper()

	return string(d.bytes(t, p))
}

func (d *domainDisk) mode(t *testing.T, p string) fs.FileMode {
	t.Helper()

	d.bytes(t, p)

	return d.files[domainGuestPath(p)].mode
}

// under lists the files below directory p.
func (d *domainDisk) under(p string) []string {
	prefix := domainGuestPath(p) + "/"

	var out []string

	for name, f := range d.files {
		if !f.dir && strings.HasPrefix(name, prefix) {
			out = append(out, name)
		}
	}

	sort.Strings(out)

	return out
}

// domainWantSame checks that a guest file holds the bytes of a host file.
func domainWantSame(t *testing.T, d *domainDisk, guest, host string) {
	t.Helper()

	want, err := os.ReadFile(host) //nolint:gosec // test fixture
	if err != nil {
		t.Errorf("reading %s: %v", host, err)

		return
	}

	f, ok := d.files[domainGuestPath(guest)]

	switch {
	case !ok || f.dir:
		t.Errorf("%s has no file %s (from %s) on its snapshot disk", d.vm, guest, host)
	case !bytes.Equal(f.data, want):
		t.Errorf("%s %s (%d bytes) differs from %s (%d bytes)", d.vm, guest, len(f.data), host, len(want))
	}
}

// domainHostFiles lists the regular files under a host directory, relative
// and "/"-separated.
func domainHostFiles(t *testing.T, dir string) []string {
	t.Helper()

	var out []string

	err := filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
		if err != nil || !e.Type().IsRegular() {
			return err
		}

		rel, err := filepath.Rel(dir, p)
		out = append(out, filepath.ToSlash(rel))

		return err
	})
	if err != nil {
		t.Fatalf("listing %s: %v", dir, err)
	}

	return out
}

func domainReadHost(t *testing.T, p string) string {
	t.Helper()

	data, err := os.ReadFile(p) //nolint:gosec // phenix output
	if err != nil {
		t.Fatal(err)
	}

	return string(data)
}

// domainRemoveSnapshots removes the injection snapshots of an experiment, which
// a start that failed after `disk snapshot` leaves in minimega's files.
func domainRemoveSnapshots(t *testing.T, name string) {
	t.Helper()

	matches, _ := filepath.Glob(filepath.Join(cfg.imagesDir, "*_"+name+"_*_snapshot"))

	for _, m := range matches {
		if err := os.Remove(m); err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("removing snapshot %s: %v", m, err)
		}
	}
}

// domainClearNamespace removes the experiment's minimega namespace when it
// outlived the experiment (logged, not failed).
func domainClearNamespace(t *testing.T, name string) {
	t.Helper()

	resp, err := mmRun("", "namespace")
	if err != nil {
		t.Logf("listing minimega namespaces: %v", err)

		return
	}

	for _, row := range mmRows(resp) {
		if row["namespace"] == name {
			t.Logf("minimega still has namespace %s after the experiment; clearing it", name)

			if _, err := mmRun("", "clear namespace "+name); err != nil {
				t.Errorf("clearing minimega namespace %s: %v", name, err)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// The NTFS fixture

// domainNTFSImages creates the hollow NTFS disk for one test and removes it
// when the test ends.
func domainNTFSImages(t *testing.T) {
	t.Helper()

	if err := domainMakeNTFSImage(); err != nil {
		t.Fatalf("creating the hollow NTFS fixture: %v", err)
	}

	t.Cleanup(func() {
		if err := os.Remove(filepath.Join(cfg.imagesDir, domainNTFSImage)); err != nil {
			t.Errorf("removing %s: %v", domainNTFSImage, err)
		}
	})
}

func domainMakeNTFSImage() error {
	raw := filepath.Join(cfg.workDir, "e2e-domain-ntfs.raw")

	if err := run("truncate", "-s", "64M", raw); err != nil {
		return err
	}

	defer os.Remove(raw)

	sfdisk := exec.Command("sfdisk", "-q", raw)
	sfdisk.Stdin = strings.NewReader("label: dos\n,,7\n")

	if out, err := sfdisk.CombinedOutput(); err != nil {
		return fmt.Errorf("sfdisk: %w\n%s", err, out)
	}

	out, err := exec.Command("losetup", "-fP", "--show", raw).Output()
	if err != nil {
		return fmt.Errorf("losetup: %w", err)
	}

	loop := strings.TrimSpace(string(out))

	defer func() { _ = run("losetup", "-d", loop) }()

	part := loop + "p1"
	deadline := time.Now().Add(10 * time.Second)

	for {
		_, err := os.Stat(part)
		if err == nil {
			break
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("partition %s did not appear: %w", part, err)
		}

		time.Sleep(domainPoll)
	}

	if err := run("mkfs.ntfs", "-Q", "-F", "-L", "e2e", part); err != nil {
		return err
	}

	mnt, err := os.MkdirTemp(cfg.workDir, "ntfs")
	if err != nil {
		return err
	}

	defer os.Remove(mnt)

	unmount, err := domainMountNTFS(part, mnt, "rw")
	if err != nil {
		return err
	}

	writeErr := os.WriteFile(filepath.Join(mnt, ext4Marker), []byte("base\n"), 0o644) //nolint:gosec // guest file
	if writeErr == nil {
		writeErr = os.MkdirAll(filepath.Join(mnt, filepath.FromSlash(domainNTFSProfile)), 0o755) //nolint:gosec // guest dir
	}

	if err := unmount(); err != nil {
		return err
	}

	if writeErr != nil {
		return writeErr
	}

	return run("qemu-img", "convert", "-q", "-f", "raw", "-O", "qcow2", raw, filepath.Join(cfg.imagesDir, domainNTFSImage))
}

// ---------------------------------------------------------------------------
// Topologies and scenarios

type domainIface struct {
	name, vlan, address string
	mask                int
}

// domainHost is a hollow topology node; os and image default to a Linux node
// on the ext4 fixture, and an interface without an address uses DHCP.
type domainHost struct {
	name, os, image string
	dnb             bool
	ifaces          []domainIface
	annotations     string // a YAML flow mapping, or ""
}

// domainLinux is a hollow Linux node with one /24 interface on VLAN OT.
func domainLinux(name, address string) domainHost {
	return domainHost{name: name, ifaces: []domainIface{{"eth0", "OT", address, 24}}}
}

// domainWindows is a hollow Windows node with one /24 interface on VLAN OT.
func domainWindows(name, address string) domainHost {
	return domainHost{name: name, os: "windows", image: domainNTFSImage, ifaces: []domainIface{{"eth0", "OT", address, 24}}}
}

// domainTopology renders hollow nodes into a Topology named {{name}}.
func domainTopology(hosts ...domainHost) string {
	var b strings.Builder

	b.WriteString("apiVersion: phenix.sandia.gov/v1\nkind: Topology\nmetadata:\n  name: {{name}}\nspec:\n  nodes:\n")

	for _, h := range hosts {
		osType, image := h.os, h.image
		if osType == "" {
			osType = "linux"
		}

		if image == "" {
			image = ext4Image
		}

		fmt.Fprintf(&b, "  - type: VirtualMachine\n    general:\n      hostname: %s\n      do_not_boot: %t\n", h.name, h.dnb)
		fmt.Fprintf(&b, "    hardware:\n      vcpus: 1\n      memory: %d\n      os_type: %s\n      drives:\n      - image: %s\n",
			domainMemory, osType, image)

		if h.annotations != "" {
			fmt.Fprintf(&b, "    annotations: %s\n", h.annotations)
		}

		b.WriteString("    network:\n      interfaces:\n")

		for _, i := range h.ifaces {
			fmt.Fprintf(&b, "      - name: %s\n        type: ethernet\n        vlan: %s\n", i.name, i.vlan)

			if i.address == "" {
				b.WriteString("        proto: dhcp\n")
			} else {
				fmt.Fprintf(&b, "        proto: static\n        address: %s\n        mask: %d\n", i.address, i.mask)
			}
		}
	}

	return b.String()
}

type domainApp struct {
	name         string
	metadata     domainMD
	assetDir     string
	fromScenario string
	hosts        []domainAppHost
}

type domainAppHost struct {
	hostname string
	metadata domainMD
}

// domainScenario renders apps into a v2 Scenario named {{name}} for the
// topology {{name}}. Metadata is written as JSON, which YAML reads as flow
// mappings.
func domainScenario(apps ...domainApp) string {
	var b strings.Builder

	b.WriteString(
		"apiVersion: phenix.sandia.gov/v2\nkind: Scenario\nmetadata:\n  name: {{name}}\n  annotations:\n    topology: {{name}}\nspec:\n  apps:\n",
	)

	for _, a := range apps {
		fmt.Fprintf(&b, "  - name: %s\n", a.name)

		if a.fromScenario != "" {
			fmt.Fprintf(&b, "    fromScenario: %s\n", a.fromScenario)
		}

		if a.assetDir != "" {
			fmt.Fprintf(&b, "    assetDir: %s\n", a.assetDir)
		}

		if a.metadata != nil {
			fmt.Fprintf(&b, "    metadata: %s\n", mustJSON(a.metadata))
		}

		if len(a.hosts) > 0 {
			b.WriteString("    hosts:\n")

			for _, h := range a.hosts {
				fmt.Fprintf(&b, "    - hostname: %s\n      metadata: %s\n", mustJSON(h.hostname), mustJSON(h.metadata))
			}
		}
	}

	return b.String()
}

// domainTLSPair returns a throwaway self-signed key and certificate in PEM,
// made for this run.
func domainTLSPair(t *testing.T) (string, string) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "e2e-ot-sim"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}

	return string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})),
		string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// ---------------------------------------------------------------------------
// XML

// domainXML is an element of a parsed XML document.
type domainXML struct {
	name  string
	attrs []xml.Attr
	chars string // the element's own character data, CDATA included
	kids  []*domainXML
}

// domainParseXML parses an XML document strictly, every token up to the end,
// so a mismatched end tag anywhere fails it.
func domainParseXML(t *testing.T, what, data string) *domainXML {
	t.Helper()

	var (
		dec   = xml.NewDecoder(strings.NewReader(data))
		root  *domainXML
		stack []*domainXML
	)

	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			t.Fatalf("%s is not well-formed XML: %v", what, err)
		}

		switch tok := tok.(type) {
		case xml.StartElement:
			e := &domainXML{name: tok.Name.Local, attrs: tok.Attr}

			if len(stack) == 0 {
				if root != nil {
					t.Fatalf("%s has more than one root element", what)
				}

				root = e
			} else {
				parent := stack[len(stack)-1]
				parent.kids = append(parent.kids, e)
			}

			stack = append(stack, e)
		case xml.EndElement:
			stack = stack[:len(stack)-1]
		case xml.CharData:
			if len(stack) > 0 {
				stack[len(stack)-1].chars += string(tok)
			}
		}
	}

	if root == nil {
		t.Fatalf("%s has no root element", what)
	}

	return root
}

// xml parses a file on a VM's snapshot disk.
func (c *domainCase) xml(t *testing.T, vm, p string) *domainXML {
	t.Helper()

	return domainParseXML(t, vm+" "+p, c.disk(t, vm).read(t, p))
}

var domainStepRE = regexp.MustCompile(`^([^\[]+)((?:\[@[^=\]]+=[^\]]*\])*)$`)

var domainPredRE = regexp.MustCompile(`\[@([^=\]]+)=([^\]]*)\]`)

// find returns the elements below n that match a path of steps separated by
// "/", each a name (or *) with optional [@attr=value] predicates.
func (n *domainXML) find(p string) []*domainXML {
	cur := []*domainXML{n}

	for step := range strings.SplitSeq(p, "/") {
		m := domainStepRE.FindStringSubmatch(step)
		if m == nil {
			panic("bad XML path step " + step)
		}

		preds := domainPredRE.FindAllStringSubmatch(m[2], -1)

		var next []*domainXML

		for _, e := range cur {
			for _, k := range e.kids {
				if m[1] != "*" && k.name != m[1] {
					continue
				}

				ok := true

				for _, pr := range preds {
					ok = ok && k.attr(pr[1]) == pr[2]
				}

				if ok {
					next = append(next, k)
				}
			}
		}

		cur = next
	}

	return cur
}

// one returns the single element at path p.
func (n *domainXML) one(t *testing.T, p string) *domainXML {
	t.Helper()

	found := n.find(p)
	if len(found) != 1 {
		t.Fatalf("<%s> has %d elements at %s, want 1", n.name, len(found), p)
	}

	return found[0]
}

func (n *domainXML) text(t *testing.T, p string) string {
	t.Helper()

	return strings.TrimSpace(n.one(t, p).chars)
}

func (n *domainXML) texts(p string) []string {
	found := n.find(p)
	out := make([]string, 0, len(found))

	for _, e := range found {
		out = append(out, strings.TrimSpace(e.chars))
	}

	return out
}

func (n *domainXML) childText(name string) string {
	if found := n.find(name); len(found) > 0 {
		return found[0].chars
	}

	return ""
}

func (n *domainXML) attr(name string) string {
	for _, a := range n.attrs {
		if a.Name.Local == name {
			return a.Value
		}
	}

	return ""
}

// modules lists the names of an OT-sim config's CPU modules.
func (n *domainXML) modules() []string {
	found := n.find("cpu/module")
	out := make([]string, 0, len(found))

	for _, e := range found {
		out = append(out, e.attr("name"))
	}

	return out
}

func domainWantText(t *testing.T, n *domainXML, p, want string) {
	t.Helper()

	found := n.find(p)
	if len(found) != 1 {
		t.Errorf("<%s> has %d elements at %s, want one holding %q", n.name, len(found), p, want)

		return
	}

	if got := strings.TrimSpace(found[0].chars); got != want {
		t.Errorf("<%s> %s = %q, want %q", n.name, p, got, want)
	}
}

func domainWantAttr(t *testing.T, n *domainXML, name, want string) {
	t.Helper()

	if got := n.attr(name); got != want {
		t.Errorf("<%s %s=%q>, want %q", n.name, name, got, want)
	}
}

// domainWantNone checks that none of the paths exist below n.
func domainWantNone(t *testing.T, what string, n *domainXML, paths ...string) {
	t.Helper()

	for _, p := range paths {
		if found := n.find(p); len(found) > 0 {
			t.Errorf("%s: <%s> has %s (%q), want none", what, n.name, p, strings.TrimSpace(found[0].chars))
		}
	}
}

// domainAllXMLParse parses every .xml file under a host directory.
func domainAllXMLParse(t *testing.T, dir string) {
	t.Helper()

	files := domainHostFiles(t, dir)

	var parsed int

	for _, rel := range files {
		if strings.HasSuffix(rel, ".xml") {
			domainParseXML(t, filepath.Join(dir, rel), domainReadHost(t, filepath.Join(dir, rel)))

			parsed++
		}
	}

	if parsed == 0 {
		t.Errorf("no XML files under %s", dir)
	}
}

// domainReg is one OT-sim register or point.
type domainReg struct {
	kind, typ, address, tag, dataType, scaling string
}

// domainOTSimRegs returns the registers of an OT-sim Modbus block or the
// points of a DNP3 block (below its outstation or master).
func domainOTSimRegs(block *domainXML) []domainReg {
	var out []domainReg

	elems := block.find("register")
	for _, inner := range []string{"outstation", "master"} {
		for _, e := range block.find(inner) {
			elems = append(elems, e.kids...)
		}
	}

	for _, e := range elems {
		switch e.name {
		case "register", "input", "output":
			out = append(out, domainReg{
				kind: e.name, typ: e.attr("type"), dataType: e.attr("data-type"),
				address: strings.TrimSpace(e.childText("address")),
				tag:     strings.TrimSpace(e.childText("tag")),
				scaling: strings.TrimSpace(e.childText("scaling")),
			})
		}
	}

	return out
}

func domainRegTags(regs []domainReg) []string {
	out := make([]string, 0, len(regs))
	for _, r := range regs {
		out = append(out, r.tag)
	}

	return out
}

func domainWantRegs(t *testing.T, what string, got, want []domainReg) {
	t.Helper()

	if fmt.Sprintf("%+v", got) != fmt.Sprintf("%+v", want) {
		t.Errorf("%s registers:\n got  %+v\n want %+v", what, got, want)
	}
}

// domainWantAnalogRegisters checks that every analog Modbus register is a
// signed 32-bit register with the server's scaling.
func domainWantAnalogRegisters(t *testing.T, what string, regs []domainReg) {
	t.Helper()

	for _, r := range regs {
		if (r.typ == "input" || r.typ == "holding") && (r.dataType != "int32" || r.scaling != "-2") {
			t.Errorf(
				"%s register %s at %s has data-type %q and scaling %q, want int32 and -2",
				what,
				r.tag,
				r.address,
				r.dataType,
				r.scaling,
			)
		}
	}
}

// domainWantPolls checks that a client block polls each point at the address,
// type and scale the server serves it; all also wants every served point
// polled.
func domainWantPolls(t *testing.T, what string, client, server *domainXML, all bool) {
	t.Helper()

	served := map[string]domainReg{}
	for _, r := range domainOTSimRegs(server) {
		served[r.kind+"/"+r.typ+"@"+r.address] = r
	}

	polled := domainOTSimRegs(client)

	for _, r := range polled {
		s, ok := served[r.kind+"/"+r.typ+"@"+r.address]

		switch {
		case !ok:
			t.Errorf("%s polls %s %s at %s, which the server does not serve", what, r.typ, r.tag, r.address)
		case s.tag != r.tag || s.dataType != r.dataType || !domainOpposite(s.scaling, r.scaling):
			t.Errorf("%s polls %+v, but the server serves %+v there", what, r, s)
		}
	}

	if all && len(polled) != len(served) {
		t.Errorf("%s polls %d points, but the server serves %d", what, len(polled), len(served))
	}
}

// domainOpposite reports whether a client's scaling undoes the server's.
func domainOpposite(server, client string) bool {
	if server == "" || client == "" {
		return server == client
	}

	s, err1 := strconv.Atoi(server)
	c, err2 := strconv.Atoi(client)

	return err1 == nil && err2 == nil && s == -c
}

// domainWantClientOfServers checks that every client connection of an OT-sim
// client targets a server the polled device runs, and polls what it serves.
func domainWantClientOfServers(t *testing.T, vm string, client, device *domainXML) {
	t.Helper()

	for _, proto := range []string{"modbus", "dnp3"} {
		for _, conn := range client.find(proto + "[@mode=client]") {
			endpoint := strings.TrimSpace(conn.childText("endpoint"))

			var server *domainXML

			for _, s := range device.find(proto + "[@mode=server]") {
				if strings.TrimSpace(s.childText("endpoint")) == endpoint {
					server = s
				}
			}

			if server == nil {
				t.Errorf("%s polls %s at %s, but no %s server runs there", vm, proto, endpoint, proto)

				continue
			}

			domainWantPolls(t, vm+" "+proto+" client of "+endpoint, conn, server, false)
		}
	}
}

// ---------------------------------------------------------------------------
// Text, JSON and INI

// domainFields collapses all whitespace, so generated scripts compare
// regardless of template indentation.
func domainFields(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func domainWantContains(t *testing.T, what, text, want string) {
	t.Helper()

	if !strings.Contains(text, want) {
		t.Errorf("%s lacks %q:\n%s", what, want, domainTail(text))
	}
}

// domainHasLine reports whether one line of text holds every want.
func domainHasLine(text string, want ...string) bool {
	for line := range strings.SplitSeq(text, "\n") {
		ok := true

		for _, w := range want {
			ok = ok && strings.Contains(line, w)
		}

		if ok {
			return true
		}
	}

	return false
}

// domainCRLF reports whether every line of a script ends in CRLF.
func domainCRLF(s string) bool {
	return strings.Count(s, "\n") > 0 && strings.Count(s, "\r\n") == strings.Count(s, "\n")
}

// domainWantSet compares two string lists as sorted sets.
func domainWantSet(t *testing.T, what string, got []string, want ...string) {
	t.Helper()

	g, w := slices.Clone(got), slices.Clone(want)
	sort.Strings(g)
	sort.Strings(w)

	if !slices.Equal(g, w) {
		t.Errorf("%s = %v, want %v", what, g, w)
	}
}

func domainJSON(t *testing.T, what, data string, v any) {
	t.Helper()

	if err := json.Unmarshal([]byte(data), v); err != nil {
		t.Fatalf("%s is not the JSON expected: %v\n%s", what, err, domainTail(data))
	}
}

// domainINI parses an INI file into sections of keys.
func domainINI(data string) map[string]map[string]string {
	out := map[string]map[string]string{}
	section := ""

	for line := range strings.SplitSeq(data, "\n") {
		line = strings.TrimSpace(line)

		switch {
		case line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";"):
		case strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]"):
			section = strings.Trim(line, "[]")
		default:
			if k, v, ok := strings.Cut(line, "="); ok {
				if out[section] == nil {
					out[section] = map[string]string{}
				}

				out[section][strings.TrimSpace(k)] = strings.TrimSpace(v)
			}
		}
	}

	return out
}

// domainTail returns the end of long command output for failure messages.
func domainTail(s string) string {
	const keep = 4000

	if len(s) > keep {
		return "..." + s[len(s)-keep:]
	}

	return s
}

func domainHead(list []string) []string {
	const keep = 5

	if len(list) > keep {
		return list[:keep]
	}

	return list
}

func domainKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}

	sort.Strings(out)

	return out
}
