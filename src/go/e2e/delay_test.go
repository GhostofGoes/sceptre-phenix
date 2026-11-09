//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// Delayed starts (flow F5). A node's `delay` holds it in minimega's BUILDING
// state until phenix starts it: after `timer`, once the miniccc clients named
// in `c2` register (by hostname, or by UUID with `useUUID`), or never for
// `user`. The CLI waits for the delays inside `experiment start`; the REST API
// returns at once and waits in the phenix ui process.
//
// Delay targets are hollow VMs. A C2 gate needs a real guest: c2src runs the
// startup app and registers under its own hostname, while c2anon runs no
// default apps and keeps the hostname `phenix` baked into the images phenix
// builds, so only a UUID gate can match it.

const (
	// delayC2Check is how often phenix checks a C2 delay's clients
	// (c2CheckInterval, api/experiment/experiment.go).
	delayC2Check = 5 * time.Second

	// delayC2Latency bounds how long phenix takes to start a C2-delayed VM
	// once its gate's client registered.
	delayC2Latency = 4 * delayC2Check

	// delayRESTLimit bounds REST start and stop calls, which must not wait
	// for pending delays. Before it returns, a start snapshots and injects the
	// disk of every node, which takes about a second each.
	delayRESTLimit = 30 * time.Second

	// delayTimerSlack bounds how late phenix starts a timer-delayed VM after
	// its timer, counted from when phenix launched the undelayed VMs.
	delayTimerSlack = 15 * time.Second

	// delayOrderSlack is how close two delayed starts minimega showed may be
	// for the websocket to announce them in either order: the watch polls
	// once a second.
	delayOrderSlack = 3 * time.Second

	delayNoDefaultApps = `
    annotations:
      phenix/default-apps: false`
	delayUser = `
    delay:
      user: true`
)

func delayTimer(d string) string {
	return "\n    delay:\n      timer: " + d
}

func delayC2(source string, byUUID bool) string {
	return fmt.Sprintf("\n    delay:\n      c2:\n      - hostname: %s\n        useUUID: %t", source, byUUID)
}

// delayTapScenario configures phenix's built-in tap app to create one host
// tap on vlan during pre-start, before any delay is pending.
func delayTapScenario(vlan, ip string) string {
	return fmt.Sprintf(`apiVersion: phenix.sandia.gov/v2
kind: Scenario
metadata:
  name: {{name}}
  annotations:
    topology: {{name}}
spec:
  apps:
  - name: tap
    metadata:
      stage: pre-start
      taps:
      - bridge: phenix
        vlan: %s
        ip: %s
`, vlan, ip)
}

// TestDelayAsyncREST starts an experiment with every kind of delay through
// the REST API (D-01) and checks that each delayed VM starts exactly when its
// trigger fires, as minimega, the websocket and the ui log report it.
func TestDelayAsyncREST(t *testing.T) {
	requireImage(t, cfg.guest)
	hollowImages(t)

	const name = "e2e-dly-rest"

	topo := topology(
		node("c2src", cfg.guest, "DLY", "10.41.0.1"),
		node("c2anon", cfg.guest, "DLY", "10.41.0.2")+delayNoDefaultApps,
		hollowNode("timer", "DLY", "10.41.0.10", delayTimer("10s")),
		hollowNode("user", "DLY", "10.41.0.11", delayUser),
		hollowNode("c2host", "DLY", "10.41.0.12", delayC2("c2src", false)),
		hollowNode("c2never", "DLY", "10.41.0.13", delayC2("c2anon", false)),
		hollowNode("c2uuid", "DLY", "10.41.0.14", delayC2("c2anon", true)),
		hollowNode("twokinds", "DLY", "10.41.0.15", delayTimer("5s")+"\n      user: true"),
	)

	exp := newExperiment(t, name, topo, "")
	delayStopRESTOnCleanup(t, exp)

	ws := recordWS(t)
	uiLog := delayLogOffset(t, delayUILog())

	gates := []delayGate{
		{vm: "c2host", source: "c2src", byUUID: false},
		{vm: "c2uuid", source: "c2anon", byUUID: true},
	}

	watch := newDelayWatch(name, gates...)
	watch.pending["user"] = true
	watch.pending["c2never"] = true
	watch.pending["twokinds"] = true // a user delay wins over its 5s timer

	// The start must return although c2never and twokinds never start; the
	// next subtests check that it returned with every delayed VM BUILDING.
	began := time.Now()
	delayPOST(t, "/experiments/"+name+"/start")
	launched := time.Now()

	t.Run("REST reports a running experiment while delays are pending", func(t *testing.T) {
		got := delayRESTExperiment(t, name)
		if !got.Running || got.StartTime == "" {
			t.Errorf("GET /experiments/%s: running=%t start_time=%q, want a running experiment while delays are pending",
				name, got.Running, got.StartTime)
		}

		if got.DelayedVMs != 6 {
			t.Errorf("GET /experiments/%s: delayed_vms = %d, want 6", name, got.DelayedVMs)
		}
	})

	t.Run("only undelayed VMs boot at start", func(t *testing.T) {
		watch.poll(t)
		delayWantStates(t, watch.info, map[string]string{
			"c2src": "RUNNING", "c2anon": "RUNNING",
			"timer": "BUILDING", "user": "BUILDING", "c2host": "BUILDING", "c2never": "BUILDING", "c2uuid": "BUILDING",
			"twokinds": "BUILDING",
		})
	})

	t.Run("REST reports each VM's delay", func(t *testing.T) {
		delayWantDelays(t, name, map[string]string{
			"c2src": "", "c2anon": "", "timer": "timer:10s", "user": "user",
			"c2host": "cc:c2src", "c2never": "cc:c2anon", "c2uuid": "cc:c2anon",
		})
	})

	t.Run("REST reports the delay phenix enforces for a node with two", func(t *testing.T) {
		// Start enforces a user delay before a C2 delay before a timer.
		delayWantDelays(t, name, map[string]string{"twokinds": "user"})
	})

	t.Run("timer delay starts its VM after the timer", func(t *testing.T) {
		delayWaitTimer(t, watch, "timer", began, launched, 10*time.Second)
	})

	t.Run("C2 delays start their VMs once their gate's client registers", func(t *testing.T) {
		watch.until(t, guestBootTimeout, "c2host and c2uuid RUNNING", func() (bool, string) {
			return watch.started("c2host") && watch.started("c2uuid"), watch.describe("c2host", "c2uuid")
		})

		delayCheckGateLatency(t, watch, gates, began)
	})

	t.Run("hostname C2 delay ignores a client that reports another hostname", func(t *testing.T) {
		host, ok := watch.ccHost[watch.uuids["c2anon"]]
		if !ok {
			t.Fatalf("c2anon's miniccc client is not registered: %s", watch.describe("c2anon"))
		}

		if host == "c2anon" {
			t.Fatal("c2anon's client reports hostname c2anon although no startup app ran for it, so this check proves nothing")
		}

		t.Logf("c2anon's client reports hostname %q", host)

		// Four C2 checks, with c2anon registered, must leave c2never BUILDING.
		watch.hold(t, 4*delayC2Check)
	})

	t.Run("user-delayed VM starts only on request", func(t *testing.T) {
		delete(watch.pending, "user")
		apiOK(t, http.MethodPost, "/experiments/"+name+"/vms/user/start", nil, nil)

		watch.until(t, 30*time.Second, "user RUNNING", func() (bool, string) {
			return watch.started("user"), watch.describe("user")
		})
	})

	t.Run("websocket announces each delayed start in order", func(t *testing.T) {
		delayWantWSStarts(t, ws, watch, "timer", "c2host", "c2uuid")

		for _, vm := range []string{"c2never", "twokinds"} {
			if acts := ws.actions("experiment/vm", name+"/"+vm); len(acts) > 0 {
				t.Errorf("websocket reported %v for %s, which never started", acts, vm)
			}
		}
	})

	t.Run("phenix ui logs each delayed start", func(t *testing.T) {
		delayWantLog(t, uiLog, "Time delayed VM timer started", "C2 delayed VM c2host started", "C2 delayed VM c2uuid started")

		log := delayLogSince(t, delayUILog(), uiLog)

		for _, vm := range []string{"c2never", "twokinds"} {
			if strings.Contains(log, "delayed VM "+vm+" started") {
				t.Errorf("the phenix ui log reports that %s started", vm)
			}
		}
	})

	t.Run("REST stop cancels the pending C2 delay", func(t *testing.T) {
		delayPOST(t, "/experiments/"+name+"/stop")

		ws.waitAction(t, 10*time.Second, "experiment", name, "stop")
		delayWantNoQEMU(t)
		delayWantRESTStopped(t, name)

		// Stop ends the start goroutine, and a C2 check it had in flight,
		// before it clears the namespace.
		if ns, ok := mmNamespaces(t)[name]; ok {
			t.Errorf("namespace %s exists after stop: %v", name, ns)
		}

		delayWantOnlyCanceled(t, uiLog, name)
	})
}

// TestDelaySyncCLI starts an experiment with timer, user, hostname and UUID
// C2 delays through the CLI (D-02), which waits for the automatic delays
// before it returns.
func TestDelaySyncCLI(t *testing.T) {
	requireImage(t, cfg.guest)
	hollowImages(t)

	const name = "e2e-dly-cli"

	topo := topology(
		node("c2src", cfg.guest, "DLY", "10.42.0.1"),
		hollowNode("timer", "DLY", "10.42.0.10", delayTimer("10s")),
		hollowNode("user", "DLY", "10.42.0.11", delayUser),
		hollowNode("c2host", "DLY", "10.42.0.12", delayC2("c2src", false)),
		hollowNode("c2uuid", "DLY", "10.42.0.13", delayC2("c2src", true)),
	)

	exp := newExperiment(t, name, topo, "")

	gates := []delayGate{
		{vm: "c2host", source: "c2src", byUUID: false},
		{vm: "c2uuid", source: "c2src", byUUID: true},
	}

	watch := newDelayWatch(name, gates...)
	watch.pending["user"] = true

	cli := delayStartCLI(t, exp)

	t.Run("start launches only undelayed VMs", func(t *testing.T) {
		watch.until(t, time.Minute, "c2src RUNNING", func() (bool, string) {
			return watch.started("c2src"), watch.describe("c2src")
		})

		// The watch has polled since start, so the timer subtest catches a
		// timer VM that started early.
		delayWantStates(t, watch.info, map[string]string{
			"c2src": "RUNNING", "user": "BUILDING", "c2host": "BUILDING", "c2uuid": "BUILDING",
		})
	})

	t.Run("experiment is running while delays are pending", func(t *testing.T) {
		if cli.exited() {
			t.Fatalf("experiment start exited (%v) before its delayed VMs started:\n%s", cli.err, cli.output())
		}

		if !exp.running() {
			t.Errorf("phenix reports %s stopped while experiment start waits for its delays, although its VMs run", name)
		}
	})

	t.Run("timer delay starts its VM after the timer", func(t *testing.T) {
		// phenix starts counting right after it started c2src.
		delayWaitTimer(t, watch, "timer", cli.began, watch.running["c2src"], 10*time.Second)
	})

	t.Run("C2 delays start their VMs once their gate's client registers", func(t *testing.T) {
		eventually(t, guestBootTimeout, time.Second, "c2host and c2uuid RUNNING", func() (bool, string) {
			// Read before polling: when start had exited before the poll,
			// the poll sees every VM it started.
			exited := cli.exited()

			watch.poll(t)

			if watch.started("c2host") && watch.started("c2uuid") {
				return true, ""
			}

			if exited {
				t.Fatalf("experiment start exited (%v) before c2host and c2uuid started:\n%s", cli.err, cli.output())
			}

			return false, watch.describe("c2host", "c2uuid")
		})

		delayCheckGateLatency(t, watch, gates, cli.began)
	})

	t.Run("start returns once every automatic delay fired", func(t *testing.T) {
		if err := cli.wait(t, 2*time.Minute); err != nil {
			t.Fatalf("experiment start failed: %v\n%s", err, cli.output())
		}

		if d := cli.ended.Sub(cli.began); d < 10*time.Second {
			t.Errorf("experiment start returned after %v, before the 10s timer delay", d)
		}

		out := cli.output()
		for _, want := range []string{"Time delayed VM timer started", "C2 delayed VM c2host started", "C2 delayed VM c2uuid started"} {
			if !strings.Contains(out, want) {
				t.Errorf("experiment start output lacks %q:\n%s", want, out)
			}
		}

		if !exp.running() {
			t.Errorf("phenix reports %s stopped after experiment start succeeded", name)
		}
	})

	t.Run("user-delayed VM waits until it is resumed", func(t *testing.T) {
		watch.poll(t)
		delete(watch.pending, "user")

		phenix(t, "vm", "resume", name, "user")

		watch.until(t, 30*time.Second, "user RUNNING", func() (bool, string) {
			return watch.started("user"), watch.describe("user")
		})
	})

	t.Run("stop", func(t *testing.T) {
		(&experiment{t: t, name: name}).stop()
		delayWantNoQEMU(t)

		if ns, ok := mmNamespaces(t)[name]; ok {
			t.Errorf("namespace %s exists after stop: %v", name, ns)
		}
	})
}

// TestDelayStopPendingTimerREST stops, through the REST API, an experiment
// whose timer delay is still pending and whose tap app created a host tap
// during pre-start (D-03). The stop must cancel the delay without a second
// namespace teardown, and the tap app's cleanup must remove the tap.
func TestDelayStopPendingTimerREST(t *testing.T) {
	hollowImages(t)

	const (
		name  = "e2e-dly-stop"
		tapIP = "10.43.0.254/24"
	)

	delayCleanTapLeaks(t)

	topo := topology(
		hollowNode("idle", "DLY", "10.43.0.1", ""),
		hollowNode("timer", "DLY", "10.43.0.10", delayTimer("120s")),
	)

	exp := newExperiment(t, name, topo, delayTapScenario("DLY", tapIP))
	delayStopRESTOnCleanup(t, exp)

	ws := recordWS(t)
	uiLog := delayLogOffset(t, delayUILog())

	delayPOST(t, "/experiments/"+name+"/start")

	tap := delayAppTap(t, name, tapIP)

	t.Run("experiment status records the tap", func(t *testing.T) {
		delayWantTapStatus(t, exp, tap)
	})

	t.Run("timer VM waits while the undelayed VM runs", func(t *testing.T) {
		delayWantStates(t, vmInfo(t, name), map[string]string{"idle": "RUNNING", "timer": "BUILDING"})
	})

	t.Run("REST stop cancels the pending timer", func(t *testing.T) {
		destroys := delayNamespaceDestroys(t, name)

		// The 120s timer is pending, so a stop that waited for it would fail
		// this bound.
		delayPOST(t, "/experiments/"+name+"/stop")

		ws.waitAction(t, 10*time.Second, "experiment", name, "stop")

		// The canceled start goroutine must leave the teardown to stop: one
		// `clear namespace`, not a second one racing the tap app's cleanup.
		for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(time.Second) {
			if n := delayNamespaceDestroys(t, name) - destroys; n > 1 {
				t.Fatalf("minimega destroyed namespace %s %d times during the stop, want once", name, n)
			}
		}

		if n := delayNamespaceDestroys(t, name) - destroys; n != 1 {
			t.Errorf("minimega destroyed namespace %s %d times during the stop, want once", name, n)
		}

		delayWantNoQEMU(t)
		delayWantRESTStopped(t, name)

		if ns, ok := mmNamespaces(t)[name]; ok {
			t.Errorf("namespace %s exists after stop: %v", name, ns)
		}
	})

	t.Run("the timer VM never started", func(t *testing.T) {
		if acts := ws.actions("experiment/vm", name+"/timer"); len(acts) > 0 {
			t.Errorf("websocket reported %v for the timer VM, which the stop should have canceled", acts)
		}

		if log := delayLogSince(t, delayUILog(), uiLog); strings.Contains(log, "Time delayed VM timer started") {
			t.Error("the phenix ui log reports that the canceled timer VM started")
		}
	})

	t.Run("tap app cleanup removes the tap", func(t *testing.T) {
		delayWantTapGone(t, tap)
	})

	t.Run("stop logs no teardown errors", func(t *testing.T) {
		log := delayLogSince(t, delayUILog(), uiLog)
		if strings.Contains(log, "killing experiment VMs") {
			t.Errorf("the phenix ui log reports a failed namespace teardown:\n%s", delayLinesWith(log, "killing experiment VMs"))
		}

		delayWantOnlyCanceled(t, uiLog, name)
	})
}

// TestDelaySIGINTDuringSyncDelay interrupts `phenix experiment start` while
// it waits for a timer delay (D-04). Start must fail, tear the experiment
// down, and run the cleanup apps for what pre-start created.
func TestDelaySIGINTDuringSyncDelay(t *testing.T) {
	hollowImages(t)

	const (
		name  = "e2e-dly-int"
		tapIP = "10.44.0.254/24"
	)

	delayCleanTapLeaks(t)

	topo := topology(
		hollowNode("idle", "DLY", "10.44.0.1", ""),
		hollowNode("timer", "DLY", "10.44.0.10", delayTimer("120s")),
	)

	exp := newExperiment(t, name, topo, delayTapScenario("DLY", tapIP))
	cli := delayStartCLI(t, exp)

	tap := delayAppTap(t, name, tapIP)
	exp.waitRunning(time.Minute, "idle")
	cli.waitDelaying(t)

	if s := vmInfo(t, name)["timer"]["state"]; s != "BUILDING" {
		t.Fatalf("timer VM is %q, want BUILDING while its 120s delay is pending", s)
	}

	if cli.exited() {
		t.Fatalf("experiment start exited (%v) while a delay was pending:\n%s", cli.err, cli.output())
	}

	cli.interrupt(t)
	err := cli.wait(t, time.Minute)

	t.Run("start fails", func(t *testing.T) {
		if err == nil {
			t.Errorf("experiment start exited 0 after SIGINT during a pending delay:\n%s", cli.output())
		}
	})

	t.Run("VMs and namespace are gone", func(t *testing.T) {
		if ns, ok := mmNamespaces(t)[name]; ok {
			t.Errorf("namespace %s exists after the interrupted start: %v", name, ns)
		}

		delayWantNoQEMU(t)
	})

	t.Run("experiment is not running", func(t *testing.T) {
		if exp.running() {
			t.Errorf("phenix reports %s running after its start was interrupted", name)
		}
	})

	t.Run("tap is off the bridge", func(t *testing.T) {
		eventually(t, 10*time.Second, time.Second, "OVS port "+tap+" to go", func() (bool, string) {
			br, ok := delayOVSBridge(tap)

			return !ok, "still on bridge " + br
		})
	})

	t.Run("cleanup apps remove the tap's network namespace", func(t *testing.T) {
		eventually(t, 10*time.Second, time.Second, "network namespace "+tap+" to go", func() (bool, string) {
			return !delayNetNSExists(tap), "the tap app's network namespace is still there"
		})
	})
}

// TestDelayStartFailure makes a timer-delayed VM impossible to start by
// killing and flushing it in minimega while its delay is pending (D-05). The
// REST start must report the failure and stop the experiment; the CLI start
// must fail and tear the experiment down.
func TestDelayStartFailure(t *testing.T) {
	hollowImages(t)

	const name = "e2e-dly-fail"

	topo := topology(
		hollowNode("idle", "DLY", "10.45.0.1", ""),
		hollowNode("timer", "DLY", "10.45.0.10", delayTimer("15s")),
	)

	exp := newExperiment(t, name, topo, "")
	delayStopRESTOnCleanup(t, exp)

	t.Run("REST", func(t *testing.T) {
		ws := recordWS(t)
		uiLog := delayLogOffset(t, delayUILog())
		destroys := delayNamespaceDestroys(t, name)

		began := time.Now()
		delayPOST(t, "/experiments/"+name+"/start")

		delayRemovePendingVM(t, name, "timer")

		msg := ws.waitAction(t, time.Minute, "experiment/vm", name+"/timer", "error")
		if d := time.Since(began); d < 15*time.Second {
			t.Errorf("the delayed start failed %v after start, before its 15s timer", d)
		}

		var result map[string]string
		if err := json.Unmarshal(msg.Result, &result); err != nil || result["error"] != "unable to start delayed VM timer" {
			t.Errorf("websocket error result = %s (%v), want {\"error\": \"unable to start delayed VM timer\"}", msg.Result, err)
		}

		// Poll the store, not REST: a REST read of a running experiment
		// queries minimega and could re-create the namespace the stop clears.
		eventually(t, 20*time.Second, time.Second, "phenix to stop "+name, func() (bool, string) {
			return !exp.running(), "status.startTime is still set"
		})

		delayWantRESTStopped(t, name)
		delayWantNoQEMU(t)

		if ns, ok := mmNamespaces(t)[name]; ok {
			t.Errorf("namespace %s exists after phenix stopped the experiment: %v", name, ns)
		}

		// Stop tears the failed start down alone: the cleanup apps, then one
		// `clear namespace`.
		if n := delayNamespaceDestroys(t, name) - destroys; n != 1 {
			t.Errorf("minimega destroyed namespace %s %d times while phenix stopped the failed start, want once", name, n)
		}

		delayWantLog(t, uiLog, "delayed error starting experiment", "starting VM timer")
	})

	t.Run("CLI", func(t *testing.T) {
		if exp.running() {
			t.Fatalf("%s is still running after the REST case", name)
		}

		destroys := delayNamespaceDestroys(t, name)
		cli := delayStartCLI(t, exp)

		// Removed earlier, the VM would fail start's launch check instead.
		cli.waitDelaying(t)
		delayRemovePendingVM(t, name, "timer")

		if err := cli.wait(t, time.Minute); err == nil {
			t.Errorf("experiment start exited 0 although delayed VM timer could not start:\n%s", cli.output())
		}

		if out := cli.output(); !strings.Contains(out, "starting VM timer") {
			t.Errorf("experiment start output does not name the VM that failed to start:\n%s", out)
		}

		if d := cli.ended.Sub(cli.began); d < 15*time.Second {
			t.Errorf("experiment start failed after %v, before the 15s timer delay", d)
		}

		if ns, ok := mmNamespaces(t)[name]; ok {
			t.Errorf("namespace %s exists after the failed start: %v", name, ns)
		}

		if n := delayNamespaceDestroys(t, name) - destroys; n != 1 {
			t.Errorf("minimega destroyed namespace %s %d times during the failed start, want once", name, n)
		}

		delayWantNoQEMU(t)

		if exp.running() {
			t.Errorf("phenix reports %s running after its start failed", name)
		}
	})
}

// TestDelayRejectsInvalidDelay checks that phenix refuses delays it cannot
// honor instead of silently starting, or never starting, the VM.
func TestDelayRejectsInvalidDelay(t *testing.T) {
	hollowImages(t)

	t.Run("timer without a unit", func(t *testing.T) {
		delayRejected(t, "e2e-dly-badtmr", topology(hollowNode("badtimer", "DLY", "10.46.0.1", delayTimer(`"5"`))), "badtimer")
	})

	t.Run("C2 delay on an unknown VM", func(t *testing.T) {
		delayRejected(t, "e2e-dly-badc2", topology(hollowNode("badc2", "DLY", "10.46.0.3", delayC2("nosuchvm", false))),
			"badc2", "nosuchvm")
	})
}

// ---------------------------------------------------------------------------
// watching delayed VMs

// delayGate is one delay.c2 entry: vm may start only once source's miniccc
// client is registered, by hostname or by UUID.
type delayGate struct {
	vm, source string
	byUUID     bool
}

// delayWatch polls minimega and records when each VM was first RUNNING and
// when each cc client first registered. It fails the test when a VM in
// pending leaves BUILDING, or a gated VM runs before its gate opened.
type delayWatch struct {
	ns       string
	gates    []delayGate
	pending  map[string]bool
	info     map[string]map[string]string // last vm info, by VM
	running  map[string]time.Time         // VM -> first seen RUNNING
	hostname map[string]time.Time         // cc client hostname -> first seen
	uuid     map[string]time.Time         // cc client UUID -> first seen
	ccHost   map[string]string            // cc client UUID -> its last hostname
	uuids    map[string]string            // VM -> UUID
	reported map[string]bool
}

func newDelayWatch(ns string, gates ...delayGate) *delayWatch {
	return &delayWatch{
		ns:       ns,
		gates:    gates,
		pending:  make(map[string]bool),
		info:     make(map[string]map[string]string),
		running:  make(map[string]time.Time),
		hostname: make(map[string]time.Time),
		uuid:     make(map[string]time.Time),
		ccHost:   make(map[string]string),
		uuids:    make(map[string]string),
		reported: make(map[string]bool),
	}
}

func (w *delayWatch) poll(t *testing.T) {
	t.Helper()

	// vm info first: a VM phenix started had its gate checked before that,
	// so the cc clients read after it lists the gate's client.
	info := vmInfo(t, w.ns)
	clients := mmTable(t, w.ns, "cc clients")
	now := time.Now()

	w.info = info

	for vm, row := range info {
		w.uuids[vm] = strings.ToLower(row["uuid"])

		if _, seen := w.running[vm]; !seen && row["state"] == "RUNNING" {
			w.running[vm] = now
		}
	}

	for _, row := range clients {
		id := strings.ToLower(row["uuid"])

		if _, seen := w.uuid[id]; !seen {
			w.uuid[id] = now
		}

		if _, seen := w.hostname[row["hostname"]]; !seen {
			w.hostname[row["hostname"]] = now
		}

		w.ccHost[id] = row["hostname"]
	}

	// A CLI start is watched from before it launched the VMs, while vm info
	// does not list them yet.
	for vm := range w.pending {
		if row, listed := info[vm]; listed && row["state"] != "BUILDING" {
			w.reportf(t, "pending "+vm, "%s is %q, want BUILDING: nothing should have started it", vm, row["state"])
		}
	}

	for _, g := range w.gates {
		if _, open := w.opened(g); !open && info[g.vm]["state"] == "RUNNING" {
			w.reportf(t, "gate "+g.vm, "%s is RUNNING before %s's miniccc client registered (by UUID: %t)", g.vm, g.source, g.byUUID)
		}
	}
}

// opened returns when g's source client first registered.
func (w *delayWatch) opened(g delayGate) (time.Time, bool) {
	if g.byUUID {
		id := w.uuids[g.source]
		if id == "" {
			return time.Time{}, false
		}

		at, ok := w.uuid[id]

		return at, ok
	}

	at, ok := w.hostname[g.source]

	return at, ok
}

func (w *delayWatch) started(vm string) bool {
	_, ok := w.running[vm]

	return ok
}

func (w *delayWatch) describe(vms ...string) string {
	parts := make([]string, 0, len(vms)+1)

	for _, vm := range vms {
		parts = append(parts, fmt.Sprintf("%s %q", vm, w.info[vm]["state"]))
	}

	hosts := make([]string, 0, len(w.ccHost))
	for id, h := range w.ccHost {
		hosts = append(hosts, h+"="+id)
	}

	return strings.Join(append(parts, fmt.Sprintf("cc clients %v", hosts)), ", ")
}

func (w *delayWatch) reportf(t *testing.T, key, format string, args ...any) {
	t.Helper()

	if !w.reported[key] {
		w.reported[key] = true

		t.Errorf(format, args...)
	}
}

// until polls every second until done reports true.
func (w *delayWatch) until(t *testing.T, timeout time.Duration, what string, done func() (bool, string)) {
	t.Helper()

	eventually(t, timeout, time.Second, what, func() (bool, string) {
		w.poll(t)

		return done()
	})
}

// hold keeps polling for d, so the pending and gate checks run throughout.
func (w *delayWatch) hold(t *testing.T, d time.Duration) {
	t.Helper()

	for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(time.Second) {
		w.poll(t)
	}
}

// delayWaitTimer waits for a timer-delayed VM and checks that it started no
// sooner than its timer after notBefore, just before the start request, and
// no later than delayTimerSlack after its timer counted from launched, when
// phenix had launched the undelayed VMs and began counting. The start request
// itself snapshots and injects every disk first, so it is no upper bound.
func delayWaitTimer(t *testing.T, w *delayWatch, vm string, notBefore, launched time.Time, timer time.Duration) {
	t.Helper()

	w.until(t, timer+delayTimerSlack+15*time.Second, vm+" RUNNING", func() (bool, string) {
		return w.started(vm), w.describe(vm)
	})

	if after := w.running[vm].Sub(notBefore); after < timer {
		t.Errorf("%s started %v after the start request, before its %v timer", vm, after, timer)
	}

	after := w.running[vm].Sub(launched)
	if limit := timer + delayTimerSlack; after > limit {
		t.Errorf("%s started %v after phenix launched the undelayed VMs, want at most %v for a %v timer", vm, after, limit, timer)
	}

	t.Logf("%s started %v after phenix launched the undelayed VMs", vm, after.Round(time.Second))
}

// delayCheckGateLatency checks that each gated VM started soon after its
// gate's client registered.
func delayCheckGateLatency(t *testing.T, w *delayWatch, gates []delayGate, began time.Time) {
	t.Helper()

	for _, g := range gates {
		at, ok := w.opened(g)
		if !ok {
			t.Errorf("%s is RUNNING but %s's client was never seen registered (by UUID: %t)", g.vm, g.source, g.byUUID)

			continue
		}

		latency := w.running[g.vm].Sub(at)
		if latency > delayC2Latency {
			t.Errorf("%s started %v after %s's client registered (by UUID: %t), want at most %v",
				g.vm, latency, g.source, g.byUUID, delayC2Latency)
		}

		t.Logf("%s's client registered %v after start (by UUID: %t); %s started %v later",
			g.source, at.Sub(began).Round(time.Second), g.byUUID, g.vm, latency.Round(time.Second))
	}
}

func delayWantStates(t *testing.T, info map[string]map[string]string, want map[string]string) {
	t.Helper()

	for vm, state := range want {
		if got := info[vm]["state"]; got != state {
			t.Errorf("minimega reports %s %q, want %s", vm, got, state)
		}
	}
}

// delayRemovePendingVM waits until vm waits for its delay while VM idle runs,
// then kills and flushes vm, so phenix's later `vm start` finds no such VM.
func delayRemovePendingVM(t *testing.T, ns, vm string) {
	t.Helper()

	eventually(t, time.Minute, time.Second, vm+" BUILDING and idle RUNNING", func() (bool, string) {
		info := vmInfo(t, ns)

		return info[vm]["state"] == "BUILDING" && info["idle"]["state"] == "RUNNING",
			fmt.Sprintf("%s %q, idle %q", vm, info[vm]["state"], info["idle"]["state"])
	})

	mm(t, ns, "vm kill "+vm)
	mm(t, ns, "vm flush "+vm)

	if row, ok := vmInfo(t, ns)[vm]; ok {
		t.Fatalf("minimega still lists %s after vm kill and vm flush: %v", vm, row)
	}
}

// ---------------------------------------------------------------------------
// REST

type delayExperimentJSON struct {
	Name       string `json:"name"`
	Running    bool   `json:"running"`
	StartTime  string `json:"start_time"`
	DelayedVMs int    `json:"delayed_vms"`
}

func delayRESTExperiment(t *testing.T, name string) delayExperimentJSON {
	t.Helper()

	var e delayExperimentJSON
	apiOK(t, http.MethodGet, "/experiments/"+name, nil, &e)

	return e
}

func delayWantRESTStopped(t *testing.T, name string) {
	t.Helper()

	if got := delayRESTExperiment(t, name); got.Running || got.StartTime != "" {
		t.Errorf("GET /experiments/%s after stop: running=%t start_time=%q, want stopped", name, got.Running, got.StartTime)
	}
}

// delayWantDelays checks the delayed_start each VM has in GET .../vms.
func delayWantDelays(t *testing.T, exp string, want map[string]string) {
	t.Helper()

	var list struct {
		VMs []struct {
			Name         string `json:"name"`
			DelayedStart string `json:"delayed_start"`
		} `json:"vms"`
	}

	apiOK(t, http.MethodGet, "/experiments/"+exp+"/vms", nil, &list)

	got := make(map[string]string, len(list.VMs))
	for _, vm := range list.VMs {
		got[vm.Name] = vm.DelayedStart
	}

	for vm, delay := range want {
		if d, ok := got[vm]; !ok || d != delay {
			t.Errorf("GET /experiments/%s/vms: %s delayed_start = %q (listed: %t), want %q", exp, vm, d, ok, delay)
		}
	}
}

// delayWantWSStarts checks that the websocket announced each VM's delayed
// start, with the VM in the result, in the order the watch saw minimega start
// them. Starts within delayOrderSlack of each other may come in either order.
func delayWantWSStarts(t *testing.T, ws *wsRecorder, w *delayWatch, vms ...string) {
	t.Helper()

	exp := w.ns

	for _, vm := range vms {
		msg := ws.waitAction(t, 15*time.Second, "experiment/vm", exp+"/"+vm, "start")

		var body struct {
			Name string `json:"name"`
		}

		if err := json.Unmarshal(msg.Result, &body); err != nil || body.Name != vm {
			t.Errorf("websocket start of %s carries VM %q (%v), want %s", vm, body.Name, err, vm)
		}
	}

	var order []string

	for _, m := range ws.messages("experiment/vm", "") {
		if m.Resource.Action == "start" && strings.HasPrefix(m.Resource.Name, exp+"/") {
			order = append(order, strings.TrimPrefix(m.Resource.Name, exp+"/"))
		}
	}

	pos := make(map[string]int, len(order))
	for i, vm := range order {
		if _, ok := pos[vm]; !ok {
			pos[vm] = i
		}
	}

	for _, a := range vms {
		for _, b := range vms {
			if w.running[a].Add(delayOrderSlack).Before(w.running[b]) && pos[a] > pos[b] {
				t.Errorf("websocket announced delayed starts in the order %v, want %s before %s, which minimega started %v later",
					order, a, b, w.running[b].Sub(w.running[a]).Round(time.Second))
			}
		}
	}
}

// delayStopRESTOnCleanup stops the experiment through the REST API when a
// test ends with it running. A REST start leaves pending delays waiting in
// the phenix ui process: a REST stop ends them before it clears the
// namespace, while the CLI stop of the experiment's own teardown ends them
// only at their next check. A namespace that outlived the stopped experiment
// was created again by something still asking minimega about it, so it fails
// the test, and is cleared.
func delayStopRESTOnCleanup(t *testing.T, e *experiment) {
	t.Helper()

	t.Cleanup(func() {
		if e.running() {
			if code, body := api(t, http.MethodPost, "/experiments/"+e.name+"/stop", nil); code != http.StatusOK {
				t.Logf("cleanup: REST stop of %s: status %d: %s", e.name, code, body)
			}
		}

		if row, ok := mmNamespaces(t)[e.name]; ok && !e.running() {
			t.Errorf("cleanup: namespace %s outlived the stopped experiment (%v); clearing it", e.name, row)

			_, _ = mmRun("", "clear namespace "+e.name)
		}
	})
}

// delayPOST posts to the REST API and fails the test unless phenix answers
// with a 2xx status within delayRESTLimit, so a call that waits for a delay
// that never fires fails instead of hanging.
func delayPOST(t *testing.T, path string) {
	t.Helper()

	const limit = delayRESTLimit

	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+cfg.uiAddr+"/api/v1"+path, nil)
	if err != nil {
		t.Fatal(err)
	}

	began := time.Now()

	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s did not answer within %v: %v", path, limit, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("POST %s: reading body: %v", path, err)
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		t.Fatalf("POST %s: status %d: %s", path, resp.StatusCode, body)
	}

	t.Logf("POST %s answered in %v", path, time.Since(began).Round(time.Millisecond))
}

// ---------------------------------------------------------------------------
// background CLI start

// delayBuffer is an [io.Writer] that can be read while a process writes it.
type delayBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *delayBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p)
}

func (b *delayBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}

// delayCLI is a `phenix experiment start` running in the background.
type delayCLI struct {
	cmd   *exec.Cmd
	out   delayBuffer
	began time.Time
	ended time.Time // set before done is closed
	err   error     // set before done is closed
	done  chan struct{}
}

// delayStartCLI runs `phenix experiment start` for e in the background. When
// the test ends it interrupts a start that is still running, and clears what
// an interrupted start left in minimega, which the experiment's teardown does
// not stop because the start never recorded a start time.
func delayStartCLI(t *testing.T, e *experiment) *delayCLI {
	t.Helper()

	c := &delayCLI{done: make(chan struct{})}
	c.cmd = exec.Command(cfg.phenix, phenixArgs("experiment", "start", e.name)...) //nolint:gosec // test harness
	c.cmd.Stdout = &c.out
	c.cmd.Stderr = &c.out
	c.began = time.Now()

	if err := startChild(c.cmd); err != nil {
		t.Fatalf("starting phenix experiment start %s: %v", e.name, err)
	}

	go func() {
		c.err = c.cmd.Wait()
		c.ended = time.Now()

		close(c.done)
	}()

	t.Cleanup(func() {
		if !c.exited() {
			_ = c.cmd.Process.Signal(os.Interrupt)

			select {
			case <-c.done:
			case <-time.After(30 * time.Second):
				_ = c.cmd.Process.Kill()
				<-c.done
			}
		}

		if _, ok := mmNamespaces(t)[e.name]; ok && !e.running() {
			_, _ = mmRun("", "clear namespace "+e.name)
		}
	})

	return c
}

func (c *delayCLI) exited() bool {
	select {
	case <-c.done:
		return true
	default:
		return false
	}
}

func (c *delayCLI) output() string {
	return c.out.String()
}

// waitDelaying waits until the start reports that it waits for its delayed
// VMs, which it does once it launched the VMs and checked the launch.
func (c *delayCLI) waitDelaying(t *testing.T) {
	t.Helper()

	eventually(t, 2*time.Minute, time.Second, "experiment start to wait for its delayed VMs", func() (bool, string) {
		if c.exited() {
			t.Fatalf("experiment start exited (%v) before it waited for its delayed VMs:\n%s", c.err, c.output())
		}

		return strings.Contains(c.output(), "Waiting for delayed VMs to be started"), "no such line in its output yet"
	})
}

func (c *delayCLI) interrupt(t *testing.T) {
	t.Helper()

	if err := c.cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("sending SIGINT to experiment start: %v", err)
	}
}

// wait waits for the start to exit and returns its error.
func (c *delayCLI) wait(t *testing.T, timeout time.Duration) error {
	t.Helper()

	select {
	case <-c.done:
		return c.err
	case <-time.After(timeout):
		t.Fatalf("experiment start did not exit within %v:\n%s", timeout, c.output())

		return nil
	}
}

// delayRejected creates an experiment from topo and checks that phenix
// refuses it, at config creation, experiment creation or experiment start,
// with an error that names one of names. A start that is still waiting after
// 30 s has accepted the delay.
func delayRejected(t *testing.T, name, topo string, names ...string) {
	t.Helper()

	named := func(step, out string) {
		t.Helper()

		for _, n := range names {
			if strings.Contains(out, n) {
				return
			}
		}

		t.Errorf("%s refused the delay with an error that names none of %v:\n%s", step, names, out)
	}

	topo = strings.ReplaceAll(topo, "{{name}}", name)

	out, err := phenixRun("config", "create", writeFile(t, name+"-topology.yaml", topo))
	if err != nil {
		named("config create", out)

		return
	}

	t.Cleanup(func() { _, _ = phenixRun("config", "delete", "topology/"+name) })

	out, err = phenixRun("experiment", "create", name, "--topology", name)
	if err != nil {
		named("experiment create", out)

		return
	}

	e := &experiment{t: t, name: name}
	t.Cleanup(e.teardown)

	cli := delayStartCLI(t, e)

	select {
	case <-cli.done:
	case <-time.After(30 * time.Second):
		t.Errorf("phenix accepted the delay: experiment start is still waiting for it after 30 s:\n%s", cli.output())

		return
	}

	if cli.err == nil {
		t.Errorf("phenix accepted the delay: experiment start exited 0 and minimega reports %v\n%s",
			delayStates(vmInfo(t, name)), cli.output())

		return
	}

	named("experiment start", cli.output())

	if _, ok := mmNamespaces(t)[name]; ok {
		if info := vmInfo(t, name); len(info) > 0 {
			t.Errorf("the refused start left VMs in minimega: %v", delayStates(info))
		}
	}
}

func delayStates(info map[string]map[string]string) map[string]string {
	out := make(map[string]string, len(info))
	for vm, row := range info {
		out[vm] = row["state"]
	}

	return out
}

// ---------------------------------------------------------------------------
// tap app

// delayAppTap waits until phenix's tap app created its host tap in
// experiment ns as the app documents it: a minimega host tap moved into a
// network namespace of the same name, holding ip, on the phenix bridge. It
// returns the tap's name.
func delayAppTap(t *testing.T, ns, ip string) string {
	t.Helper()

	var name string

	eventually(t, time.Minute, time.Second, "the tap app's host tap", func() (bool, string) {
		rows := mmTable(t, ns, "tap")
		taps := make([]string, 0, len(rows))

		name = ""

		for _, row := range rows {
			taps = append(taps, row["tap"])

			if strings.HasSuffix(row["tap"], "-tapapp") {
				name = row["tap"]
			}
		}

		if name == "" {
			return false, fmt.Sprintf("minimega lists taps %v", taps)
		}

		if !delayNetNSExists(name) {
			return false, "no network namespace " + name
		}

		addr := exec.Command("ip", "netns", "exec", name, "ip", "-4", "-o", "addr", "show", "dev", name) //nolint:gosec // test

		out, err := addr.CombinedOutput()
		if err != nil || !strings.Contains(string(out), "inet "+ip+" ") {
			return false, fmt.Sprintf("%s in its namespace: %v %s", name, err, out)
		}

		return true, ""
	})

	if br, ok := delayOVSBridge(name); !ok || br != "phenix" {
		t.Errorf("tap %s is on OVS bridge %q (port exists: %t), want phenix", name, br, ok)
	}

	return name
}

// delayWantTapStatus checks that the experiment status records the tap the
// tap app created, which is what its cleanup deletes.
func delayWantTapStatus(t *testing.T, e *experiment, tap string) {
	t.Helper()

	data, err := json.Marshal((&experiment{t: t, name: e.name}).doc().Status.Apps["tap"])
	if err != nil {
		t.Fatal(err)
	}

	var status struct {
		Host string `json:"host"`
		Taps []struct {
			Name string `json:"name"`
			VLAN string `json:"vlan"`
		} `json:"taps"`
	}

	if err := json.Unmarshal(data, &status); err != nil {
		t.Fatalf("decoding status.apps.tap %s: %v", data, err)
	}

	if len(status.Taps) != 1 || status.Taps[0].Name != tap || status.Host == "" {
		t.Errorf("status.apps.tap = %s, want host set and one tap named %s", data, tap)
	}
}

func delayWantTapGone(t *testing.T, tap string) {
	t.Helper()

	eventually(t, 10*time.Second, time.Second, "tap "+tap+" to go", func() (bool, string) {
		if br, ok := delayOVSBridge(tap); ok {
			return false, "still on OVS bridge " + br
		}

		if delayNetNSExists(tap) {
			return false, "its network namespace is still there"
		}

		return true, ""
	})
}

const delayNetNSDir = "/var/run/netns"

func delayNetNSExists(name string) bool {
	_, err := os.Stat(filepath.Join(delayNetNSDir, name))

	return err == nil
}

// delayOVSBridge returns the OVS bridge holding port, if any.
func delayOVSBridge(port string) (string, bool) {
	out, err := exec.Command("ovs-vsctl", "port-to-br", port).Output() //nolint:gosec // test

	return strings.TrimSpace(string(out)), err == nil
}

// delayTapLeftovers lists the tap app's network namespaces and OVS ports.
func delayTapLeftovers(t *testing.T) map[string]bool {
	t.Helper()

	out := make(map[string]bool)

	entries, err := os.ReadDir(delayNetNSDir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}

	for _, e := range entries {
		if strings.HasSuffix(e.Name(), "-tapapp") {
			out[e.Name()] = true
		}
	}

	ports, err := exec.Command("ovs-vsctl", "list-ports", "phenix").Output()
	if err == nil {
		for p := range strings.FieldsSeq(string(ports)) {
			if strings.HasSuffix(p, "-tapapp") {
				out[p] = true
			}
		}
	}

	return out
}

// delayCleanTapLeaks registers, before the experiment exists so it runs
// after the experiment's teardown, a cleanup that removes any tap app
// network namespace or OVS port the test's experiment left on the host.
func delayCleanTapLeaks(t *testing.T) {
	t.Helper()

	before := delayTapLeftovers(t)

	t.Cleanup(func() {
		for name := range delayTapLeftovers(t) {
			if before[name] {
				continue
			}

			t.Logf("cleanup: removing leftover tap %s", name)

			if delayNetNSExists(name) {
				_ = run("ip", "netns", "delete", name)
			}

			_ = run("ovs-vsctl", "--if-exists", "del-port", name)
		}
	})
}

// ---------------------------------------------------------------------------
// logs and teardown

func delayUILog() string {
	return filepath.Join(cfg.workDir, "phenix-ui.log")
}

// delayMMLog is minimega's -logfile, which the E2E minimega action puts
// under its -base.
func delayMMLog() string {
	return filepath.Join(cfg.mmBase, "minimega.log")
}

func delayLogOffset(t *testing.T, path string) int64 {
	t.Helper()

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("log %s: %v", path, err)
	}

	return fi.Size()
}

// delayLogSince returns what was written to the log at path after offset.
func delayLogSince(t *testing.T, path string, offset int64) string {
	t.Helper()

	data, err := os.ReadFile(path) //nolint:gosec // test
	if err != nil {
		t.Fatalf("reading log %s: %v", path, err)
	}

	if int64(len(data)) < offset {
		return string(data)
	}

	return string(data[offset:])
}

func delayLinesWith(log, substr string) string {
	var lines []string

	for line := range strings.SplitSeq(log, "\n") {
		if strings.Contains(line, substr) {
			lines = append(lines, line)
		}
	}

	return strings.Join(lines, "\n")
}

// delayWantLog waits until the phenix ui log, after offset, holds each want.
func delayWantLog(t *testing.T, offset int64, want ...string) {
	t.Helper()

	eventually(t, 15*time.Second, time.Second, "lines in the phenix ui log", func() (bool, string) {
		log := delayLogSince(t, delayUILog(), offset)

		for _, w := range want {
			if !strings.Contains(log, w) {
				return false, "missing " + w
			}
		}

		return true, ""
	})
}

// delayWantOnlyCanceled checks that every delayed start error the phenix ui
// logged for exp after offset is the cancellation a stop causes.
func delayWantOnlyCanceled(t *testing.T, offset int64, exp string) {
	t.Helper()

	expAttr := regexp.MustCompile(`(^|\s)exp=` + regexp.QuoteMeta(exp) + `(\s|$)`)

	for line := range strings.SplitSeq(delayLogSince(t, delayUILog(), offset), "\n") {
		if strings.Contains(line, "delayed error starting experiment") && expAttr.MatchString(line) &&
			!strings.Contains(line, "context canceled") {
			t.Errorf("the phenix ui logged a delayed start error other than the stop's cancellation:\n%s", line)
		}
	}
}

// delayNamespaceDestroys counts the times minimega logged destroying
// namespace ns.
func delayNamespaceDestroys(t *testing.T, ns string) int {
	t.Helper()

	data, err := os.ReadFile(delayMMLog())
	if err != nil {
		t.Fatalf("reading the minimega log %s: %v", delayMMLog(), err)
	}

	destroyed := regexp.MustCompile(`destroying namespace: ` + regexp.QuoteMeta(ns) + `($|[^A-Za-z0-9_-])`)

	return len(destroyed.FindAllIndex(data, -1))
}

func delayWantNoQEMU(t *testing.T) {
	t.Helper()

	eventually(t, 30*time.Second, time.Second, "QEMU processes to exit", func() (bool, string) {
		n := qemuProcesses(t)

		return n == 0, fmt.Sprintf("%d still running", n)
	})
}
