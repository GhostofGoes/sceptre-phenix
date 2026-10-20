//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Tests for the sceptre-phenix-apps user apps that run on generic guests
// (mgmt_tap, protonuke, wireguard, the single-node scheduler and the Go
// mirror app), and for the user app execution contract itself. The E2E
// workflow installs the apps for the apps shard and then sets E2E_APPS=true.

const (
	appsMgmtTap   = "phenix-app-mgmt_tap"
	appsProtonuke = "phenix-app-protonuke"
	appsWireguard = "phenix-app-wireguard"
	appsMirror    = "phenix-app-mirror"
	appsScheduler = "phenix-scheduler-single-node"

	// appsTestApp is the user app the contract tests install as
	// /usr/local/bin/phenix-app-e2e.
	appsTestApp = "e2e"

	// appsEditedMemory is the memory the test app's configure stage gives the
	// first node of the topology.
	appsEditedMemory = 320

	appsHollowMemory = 256
	appsGuestMemory  = 1024

	// appsPhenixDir is phenix's default base directory, which the E2E
	// workflow keeps.
	appsPhenixDir = "/phenix"

	// appsGuestImage is the copy-on-write image the guest test builds on top
	// of the tools guest, with protonuke injected.
	appsGuestImage = "e2e-apps-guest.qc2"

	// The mirror app's defaults: its mirror network, the address it gives the
	// first cluster host's mirror tap and the address of the first monitor.
	appsMirrorNet       = "10.248.171.0/24"
	appsMirrorTapCIDR   = "10.248.171.1/24"
	appsMirrorMonitorIP = "10.248.171.254"

	appsNukeServerArgs = "-serve -http -level info -logfile /var/log/protonuke.log"
	appsNukeClientArgs = "-http -level info -logfile /var/log/protonuke.log 10.20.0.11"

	appsRunning = "RUNNING"
)

// appsStages are the lifecycle stages a user app is run for.
func appsStages() []string {
	return []string{"configure", "pre-start", "post-start", "running", "cleanup"}
}

// ---------------------------------------------------------------------------
// the user app execution contract

// TestAppsUserAppContract drives an experiment through every lifecycle stage
// with a user app that records what phenix gives it (argv, stdin, PHENIX_
// environment) and edits the experiment, and checks what phenix makes of
// each stage's output: which edits it keeps, the rendered minimega script,
// what minimega runs, exit 101 rescheduling, and its logs.
func TestAppsUserAppContract(t *testing.T) {
	requireApps(t, "apps", appsMgmtTap, appsProtonuke, appsWireguard, appsMirror, appsScheduler)
	hollowImages(t)
	appsInstallTestApp(t)

	t.Run("discovery", appsCheckDiscovery)

	c := &appsContract{
		name:       "e2e-apc",
		host:       headnode(t),
		qemuBefore: qemuProcesses(t),
	}
	c.dir = appsRecordDir(t, c.name)

	topo := topology(appsHollow("c1", "LAN", "10.61.0.1"), appsHollow("c2", "LAN", "10.61.0.2"))
	scenario := appsTestAppScenario(c.dir, map[string][]string{
		"configure":  {"edit"},
		"pre-start":  {"schedule", "edit"},
		"post-start": {"edit"},
		"running":    {"fail", "edit"},
		"cleanup":    {"edit"},
	})

	exp, createOut := appsCreate(t, c.name, topo, scenario)
	c.exp = exp

	t.Run("configure runs once at create and only its spec is kept", func(t *testing.T) { c.checkConfigure(t, createOut) })

	startOut := exp.start()

	t.Run("pre-start reruns after exit 101 rescheduled the experiment", func(t *testing.T) { c.checkPreStart(t, startOut) })
	t.Run("the rendered script carries the configure and pre-start edits", c.checkScript)
	t.Run("minimega runs what the edited spec asks for", c.checkMinimega)
	t.Run("post-start sees the running VMs and may only set its status", c.checkPostStart)
	t.Run("phenix util app-json prints what apps receive", c.checkAppJSON)
	t.Run("running stage", c.checkRunning)
	t.Run("cleanup sees the VMs and may change spec and status", c.checkCleanup)
}

// appsContract is the experiment TestAppsUserAppContract drives.
type appsContract struct {
	name       string
	dir        string // where the test app records each run
	host       string // the runner's minimega host name
	exp        *experiment
	qemuBefore int
}

func appsCheckDiscovery(t *testing.T) {
	t.Helper()

	apps := appsListed(phenix(t, "experiment", "apps"), "Apps:")

	// The sceptre-phenix-apps console scripts are symlinks into the virtual
	// environment; the test app is a regular file.
	for _, want := range []string{appsTestApp, "mgmt_tap", "protonuke", "wireguard", "mirror"} {
		if !slices.Contains(apps, want) {
			t.Errorf("phenix experiment apps lists %v, which lacks the installed user app %s", apps, want)
		}
	}

	if schedulers := appsListed(phenix(t, "experiment", "schedulers"), "Schedulers:"); !slices.Contains(schedulers, "single-node") {
		t.Errorf("phenix experiment schedulers lists %v, which lacks the installed single-node scheduler", schedulers)
	}
}

func (c *appsContract) checkConfigure(t *testing.T, createOut string) {
	t.Helper()

	recs := appsRecords(t, c.dir, "configure")
	if len(recs) != 1 {
		t.Fatalf("the configure stage ran %d times at experiment create, want once", len(recs))
	}

	rec := recs[0]

	appsCheckInvocation(t, rec, c.name)
	appsCheckHosts(t, rec, c.host)

	if len(rec.exp.VMs) != 0 {
		t.Errorf("configure stdin lists VMs %v, but nothing is launched at create", appsVMNames(rec.exp.VMs))
	}

	if rec.exp.Status.StartTime != "" {
		t.Errorf("configure stdin has status.startTime %q for an experiment that never started", rec.exp.Status.StartTime)
	}

	appsWantLogged(t, createOut, "e2e-app says configure 1")

	stored := appsStored(t, c.name)

	for _, n := range stored.Spec.Topology.Nodes {
		if n.Labels["e2e-configure"] != "1" {
			t.Errorf("stored node %s has labels %v, want the e2e-configure label the configure stage added", n.General.Hostname, n.Labels)
		}
	}

	if n := stored.node("c1"); n == nil || n.Hardware.Memory != appsEditedMemory {
		t.Errorf("stored node c1 = %+v, want the memory %d the configure stage set", n, appsEditedMemory)
	}

	if n := stored.node("c2"); n == nil || n.Hardware.Memory != appsHollowMemory {
		t.Errorf("stored node c2 = %+v, want the topology's memory %d", n, appsHollowMemory)
	}

	if st, ok := stored.Status.Apps[appsTestApp]; ok {
		t.Errorf("phenix stored status.apps.%s = %v from the configure stage; it keeps only the spec configure prints", appsTestApp, st)
	}
}

func (c *appsContract) checkPreStart(t *testing.T, startOut string) {
	t.Helper()

	recs := appsRecords(t, c.dir, "pre-start")
	if len(recs) != 2 {
		t.Fatalf("the pre-start stage ran %d times, want twice: exit 101 makes phenix run the scheduler named on stdout "+
			"and run the stage again", len(recs))
	}

	for _, rec := range recs {
		appsCheckInvocation(t, rec, c.name)
		appsCheckHosts(t, rec, c.host)

		if len(rec.exp.VMs) != 0 {
			t.Errorf("pre-start call %d stdin lists VMs %v before minimega launched anything", rec.call, appsVMNames(rec.exp.VMs))
		}
	}

	first, second := recs[0].exp, recs[1].exp

	if n := first.node("c1"); n == nil || n.Hardware.Memory != appsEditedMemory || n.Labels["e2e-configure"] != "1" {
		t.Errorf("pre-start does not see the spec configure stored: c1 = %+v", n)
	}

	if len(first.Spec.Schedules) != 0 {
		t.Errorf("the first pre-start sees spec.schedules %v before any scheduler ran", first.Spec.Schedules)
	}

	if want := map[string]string{"c1": c.host, "c2": c.host}; !maps.Equal(second.Spec.Schedules, want) {
		t.Errorf("after exit 101 with round-robin, the second pre-start sees spec.schedules %v, want %v", second.Spec.Schedules, want)
	}

	appsWantLogged(t, startOut, "e2e-app says pre-start 1")
	appsWantLogged(t, startOut, "e2e-app says pre-start 2")
}

func (c *appsContract) checkScript(t *testing.T) {
	t.Helper()

	script := c.exp.with(t).mmScript()

	for _, vm := range []string{"c1", "c2"} {
		block := appsVMBlock(script, vm)
		if block == "" {
			t.Errorf("the rendered minimega script has no block for %s", vm)

			continue
		}

		memory := appsHollowMemory
		if vm == "c1" {
			memory = appsEditedMemory
		}

		for _, line := range []string{
			fmt.Sprintf("vm config memory %d", memory),
			"vm config schedule " + c.host,
			`vm config tags "e2e-configure" "1"`,
			`vm config tags "e2e-pre-start" "2"`,
			"vm launch kvm " + vm,
		} {
			if !appsHasLine(block, line) {
				t.Errorf("the rendered script's %s block lacks %q:\n%s", vm, line, block)
			}
		}
	}
}

func (c *appsContract) checkMinimega(t *testing.T) {
	t.Helper()

	c.exp.with(t).waitRunning(time.Minute, "c1", "c2")

	info := vmInfo(t, c.name)

	for _, vm := range []string{"c1", "c2"} {
		row := info[vm]

		memory := appsHollowMemory
		if vm == "c1" {
			memory = appsEditedMemory
		}

		if row["memory"] != strconv.Itoa(memory) {
			t.Errorf("minimega runs %s with memory %q, want %d", vm, row["memory"], memory)
		}

		if row["host"] != c.host {
			t.Errorf("minimega runs %s on %q, want %q", vm, row["host"], c.host)
		}

		var tags map[string]string
		if err := json.Unmarshal([]byte(row["tags"]), &tags); err != nil || tags["e2e-configure"] != "1" || tags["e2e-pre-start"] != "2" {
			t.Errorf("minimega tags for %s = %q, want the labels the configure and pre-start stages added", vm, row["tags"])
		}
	}
}

func (c *appsContract) checkPostStart(t *testing.T) {
	t.Helper()

	recs := appsRecords(t, c.dir, "post-start")
	if len(recs) != 1 {
		t.Fatalf("the post-start stage ran %d times, want once", len(recs))
	}

	rec := recs[0]

	appsCheckInvocation(t, rec, c.name)
	appsCheckHosts(t, rec, c.host)

	stored := appsStored(t, c.name)
	lan := stored.Status.VLANs["LAN"]

	if lan == 0 || rec.exp.Status.VLANs["LAN"] != lan {
		t.Errorf("post-start sees status.vlans %v; phenix stored %v", rec.exp.Status.VLANs, stored.Status.VLANs)
	}

	if want := map[string]string{"c1": c.host, "c2": c.host}; !maps.Equal(rec.exp.Status.Schedules, want) {
		t.Errorf("post-start sees status.schedules %v, want %v", rec.exp.Status.Schedules, want)
	}

	appsCheckRuntimeVMs(t, rec, c.name, lan, "c1", "c2")

	st := appsAppStatus(stored, appsTestApp)
	if st["stage"] != "post-start" || !reflect.DeepEqual(st["vms"], []any{"c1", "c2"}) {
		t.Errorf("stored status.apps.%s = %v, want what post-start printed (stage post-start, vms [c1 c2])", appsTestApp, st)
	}

	for _, n := range stored.Spec.Topology.Nodes {
		if _, ok := n.Labels["e2e-post-start"]; ok {
			t.Errorf(
				"phenix stored the spec change post-start printed on node %s; it keeps only status.apps.%s from post-start",
				n.General.Hostname,
				appsTestApp,
			)
		}
	}
}

func (c *appsContract) checkAppJSON(t *testing.T) {
	t.Helper()

	recs := appsRecords(t, c.dir, "post-start")
	if len(recs) == 0 {
		t.Fatal("the post-start stage recorded nothing")
	}

	raw := appsPhenixStdout(t, "util", "app-json", c.name)

	var printed, received map[string]any

	if err := json.Unmarshal(raw, &printed); err != nil {
		t.Fatalf("phenix util app-json printed something that is not one JSON document: %v\n%s", err, appsHead(raw))
	}

	if err := json.Unmarshal(recs[0].raw, &received); err != nil {
		t.Fatal(err)
	}

	if d := appsDiff("spec", appsNormalize(printed["spec"]), appsNormalize(received["spec"])); d != "" {
		t.Errorf("phenix util app-json prints a spec that differs from the one post-start received: %s", d)
	}

	var doc appsExpDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}

	if got, want := appsVMStates(doc.VMs), appsVMStates(recs[0].exp.VMs); !maps.Equal(got, want) {
		t.Errorf("phenix util app-json lists VMs %v; post-start received %v", got, want)
	}

	if len(doc.Hosts) != 1 || doc.Hosts[0].Name != c.host {
		t.Errorf("phenix util app-json lists hosts %+v, want only %s", doc.Hosts, c.host)
	}
}

func (c *appsContract) checkRunning(t *testing.T) {
	t.Helper()

	out := phenixFails(t, "experiment", "trigger", "running", c.name, appsTestApp)
	appsWantFailed(t, out, appsTestApp)
	appsWantLogged(t, out, "e2e-app fails running on purpose")

	if !appsIsRunning(t, c.name) {
		t.Errorf("experiment %s stopped running because a running stage failed", c.name)
	}

	out = phenix(t, "experiment", "trigger", "running", c.name, appsTestApp)
	appsWantLogged(t, out, "e2e-app says running 2")

	recs := appsRecords(t, c.dir, "running")
	if len(recs) != 2 {
		t.Fatalf("the running stage ran %d times for two triggers, want twice", len(recs))
	}

	stored := appsStored(t, c.name)

	for _, rec := range recs {
		appsCheckInvocation(t, rec, c.name)
		appsCheckHosts(t, rec, c.host)
		appsCheckRuntimeVMs(t, rec, c.name, stored.Status.VLANs["LAN"], "c1", "c2")

		if rec.exp.Status.StartTime == "" {
			t.Errorf("running call %d stdin has no status.startTime for a running experiment", rec.call)
		}
	}

	for _, n := range stored.Spec.Topology.Nodes {
		if _, ok := n.Labels["e2e-running"]; ok {
			t.Errorf(
				"phenix stored the spec change the running stage printed on node %s; it keeps only status.apps.%s",
				n.General.Hostname,
				appsTestApp,
			)
		}
	}

	if st := appsAppStatus(stored, appsTestApp); st["stage"] != "running" {
		t.Errorf("stored status.apps.%s = %v after a running stage that printed stage=running", appsTestApp, st)
	}
}

func (c *appsContract) checkCleanup(t *testing.T) {
	t.Helper()

	out := c.exp.with(t).stop()
	appsWantLogged(t, out, "e2e-app says cleanup 1")

	recs := appsRecords(t, c.dir, "cleanup")
	if len(recs) != 1 {
		t.Fatalf("the cleanup stage ran %d times at stop, want once", len(recs))
	}

	rec := recs[0]

	appsCheckInvocation(t, rec, c.name)
	appsCheckHosts(t, rec, c.host)

	// phenix runs cleanup before it clears the namespace.
	if states := appsVMStates(rec.exp.VMs); !maps.Equal(states, map[string]string{"c1": appsRunning, "c2": appsRunning}) {
		t.Errorf("cleanup stdin lists VMs %v, want c1 and c2 RUNNING (cleanup runs before phenix kills them)", states)
	}

	stored := appsStored(t, c.name)

	if stored.Status.StartTime != "" {
		t.Errorf("status.startTime = %q after stop", stored.Status.StartTime)
	}

	for _, n := range stored.Spec.Topology.Nodes {
		if n.Labels["e2e-cleanup"] != "1" {
			t.Errorf("stored node %s has labels %v, want the e2e-cleanup label the cleanup stage added", n.General.Hostname, n.Labels)
		}
	}

	if st := appsAppStatus(stored, appsTestApp); st["stage"] != "cleanup" {
		t.Errorf("stored status.apps.%s = %v, want what the cleanup stage printed", appsTestApp, st)
	}

	appsWantNoNamespace(t, c.name)
	appsWaitQEMU(t, c.qemuBefore)
}

// TestAppsUserAppExitCodes checks how phenix handles a user app that fails a
// stage or prints more than the experiment on stdout.
func TestAppsUserAppExitCodes(t *testing.T) {
	requireApps(t, "apps")
	hollowImages(t)
	appsInstallTestApp(t)

	topo := topology(appsHollow("x1", "LAN", "10.61.1.1"))

	t.Run("configure exit 1 fails the create", func(t *testing.T) {
		const name = "e2e-apx-cfg"

		args := appsConfigs(t, name, topo, appsTestAppScenario(appsRecordDir(t, name), map[string][]string{"configure": {"fail"}}))
		t.Cleanup(func() { _, _ = phenixRun("experiment", "delete", name, "--force") })

		out := phenixFails(t, args...)
		appsWantFailed(t, out, appsTestApp)
		appsWantLogged(t, out, "e2e-app fails configure on purpose")

		if appsExists(name) {
			t.Errorf("experiment %s was stored although its configure stage failed", name)
		}

		appsWantNoNamespace(t, name)
	})

	t.Run("stdout that is not only the experiment fails the create", func(t *testing.T) {
		const name = "e2e-apx-out"

		args := appsConfigs(t, name, topo, appsTestAppScenario(appsRecordDir(t, name), map[string][]string{"configure": {"chatty"}}))
		t.Cleanup(func() { _, _ = phenixRun("experiment", "delete", name, "--force") })

		out := phenixFails(t, args...)
		if !strings.Contains(out, "unmarshaling experiment from JSON") {
			t.Errorf("experiment create does not report that the app's stdout is not an experiment:\n%s", out)
		}

		if appsExists(name) {
			t.Errorf("experiment %s was stored although its configure stage printed a progress line before the experiment", name)
		}

		appsWantNoNamespace(t, name)
	})

	t.Run("pre-start exit 1 fails the start before anything launches", func(t *testing.T) {
		const name = "e2e-apx-pre"

		dir := appsRecordDir(t, name)
		appsCreate(t, name, topo, appsTestAppScenario(dir, map[string][]string{"pre-start": {"fail"}}))

		qemuBefore := qemuProcesses(t)

		out := phenixFails(t, "experiment", "start", name)
		appsWantFailed(t, out, appsTestApp)

		if appsIsRunning(t, name) {
			t.Errorf("experiment %s is running although its pre-start stage failed", name)
		}

		if recs := appsRecords(t, dir, "post-start"); len(recs) != 0 {
			t.Errorf("post-start ran %d times after pre-start failed", len(recs))
		}

		// A namespaced command would create the namespace; without one,
		// minimega runs none of the experiment's VMs.
		appsWantNoNamespace(t, name)

		if n := qemuProcesses(t); n > qemuBefore {
			t.Errorf("%d QEMU processes run after the failed start, %d before", n, qemuBefore)
		}
	})

	t.Run("post-start exit 1 fails the start and cleans up", func(t *testing.T) {
		const name = "e2e-apx-post"

		dir := appsRecordDir(t, name)
		appsCreate(t, name, topo, appsTestAppScenario(dir, map[string][]string{"post-start": {"fail"}}))

		qemuBefore := qemuProcesses(t)

		out := phenixFails(t, "experiment", "start", name)
		appsWantFailed(t, out, appsTestApp)

		recs := appsRecords(t, dir, "cleanup")
		if len(recs) != 1 {
			t.Errorf("the cleanup stage ran %d times after post-start failed, want once", len(recs))
		} else if states := appsVMStates(recs[0].exp.VMs); states["x1"] != appsRunning {
			t.Errorf("cleanup after a failed post-start sees VMs %v, want x1 RUNNING", states)
		}

		appsWantNoNamespace(t, name)
		appsWaitQEMU(t, qemuBefore)

		if appsIsRunning(t, name) {
			t.Errorf("experiment %s is running although its post-start stage failed", name)
		}
	})

	t.Run("cleanup exit 1 fails the stop but still stops", func(t *testing.T) {
		const name = "e2e-apx-clean"

		exp, _ := appsCreate(t, name, topo, appsTestAppScenario(appsRecordDir(t, name), map[string][]string{"cleanup": {"fail"}}))

		qemuBefore := qemuProcesses(t)

		exp.start()
		exp.waitRunning(time.Minute, "x1")

		out := phenixFails(t, "experiment", "stop", name)
		appsWantFailed(t, out, appsTestApp)

		if appsIsRunning(t, name) {
			t.Errorf("experiment %s still counts as running after a stop whose cleanup stage failed", name)
		}

		appsWantNoNamespace(t, name)
		appsWaitQEMU(t, qemuBefore)
	})
}

// ---------------------------------------------------------------------------
// sceptre-phenix-apps on the JSON phenix gives user apps

// TestAppsConformance runs the installed apps directly, on the JSON phenix
// gives a user app for a running experiment (phenix util app-json), and
// checks the runtime contract every app must keep: stdout is the experiment
// as one JSON document and nothing else, stderr is JSON log lines, unknown
// fields survive, and bad input fails with a nonzero exit.
func TestAppsConformance(t *testing.T) {
	requireApps(t, "apps", appsMgmtTap, appsProtonuke, appsWireguard, appsMirror)
	hollowImages(t)

	const name = "e2e-apconf"

	exp, _ := appsCreate(t, name, topology(appsHollow("c1", "LAN", "10.63.0.1"), appsHollow("c2", "LAN", "10.63.0.2")), "")
	exp.start()
	exp.waitRunning(time.Minute, "c1", "c2")

	c := newAppsConformance(t, name)

	t.Run("every stage", c.checkStages)
	t.Run("pre-start injections", c.checkInjections)
	t.Run("invalid input", c.checkInvalidInput)
	t.Run("dry-run logs", c.checkDryRunLogs)
	t.Run("mirror", c.checkMirror)
}

// appsConformance holds the input TestAppsConformance feeds the apps.
type appsConformance struct {
	name   string
	work   string         // the spec.baseDir the apps write into
	base   []byte         // what phenix gives a user app for the experiment
	input  []byte         // base with the apps' scenario entries and an unknown key
	doc    map[string]any // input, decoded
	wgPriv string
	wgPeer string
}

func newAppsConformance(t *testing.T, name string) *appsConformance {
	t.Helper()

	c := &appsConformance{
		name: name,
		work: filepath.Join(cfg.workDir, "apps-conformance", name),
		base: appsPhenixStdout(t, "util", "app-json", name),
	}

	_ = os.RemoveAll(c.work)

	// The startup default app creates <baseDir>/startup in phenix runs; the
	// injector apps write into it without creating it.
	if err := os.MkdirAll(filepath.Join(c.work, "startup"), 0o750); err != nil {
		t.Fatal(err)
	}

	// The Python apps name staged files by their resolved path (safe_join).
	if resolved, err := filepath.EvalSymlinks(c.work); err == nil {
		c.work = resolved
	}

	// The staged wg0.conf holds a private key; do not leave it among the
	// logs the workflow uploads.
	t.Cleanup(func() { _ = os.RemoveAll(c.work) })

	c.wgPriv, _ = appsWGKey(t)
	_, c.wgPeer = appsWGKey(t)

	apps := []any{
		map[string]any{"name": "protonuke", "hosts": []any{
			map[string]any{"hostname": "c1", "metadata": map[string]any{"args": "-serve -http"}},
		}},
		map[string]any{"name": "wireguard", "hosts": []any{
			map[string]any{"hostname": "c1", "metadata": map[string]any{
				"interface": map[string]any{"private_key": c.wgPriv, "address": "10.100.0.1/24"},
				"peers":     []any{map[string]any{"public_key": c.wgPeer, "allowed_ips": "10.100.0.2/32"}},
			}},
		}},
		map[string]any{"name": "mgmt_tap", "metadata": map[string]any{"vlan": "LAN", "subnet": "10.63.0.0/24"}},
		map[string]any{"name": "mirror", "hosts": []any{
			map[string]any{"hostname": "c2", "metadata": map[string]any{"interface": "mirror0", "vlans": []any{"LAN"}}},
		}},
	}

	c.input = appsWithApps(t, c.base, c.work, apps)

	if err := json.Unmarshal(c.input, &c.doc); err != nil {
		t.Fatal(err)
	}

	return c
}

func (c *appsConformance) env(dryrun bool) []string {
	return appsAppEnv(c.name, dryrun)
}

func (c *appsConformance) checkStages(t *testing.T) {
	t.Helper()

	cases := []struct {
		cmd    string
		stages []string
	}{
		{appsProtonuke, appsStages()},
		{appsWireguard, appsStages()},
		// mgmt_tap's later stages create and delete host taps; its lifecycle
		// test covers them.
		{appsMgmtTap, []string{"configure", "pre-start", "running"}},
	}

	for _, tc := range cases {
		for _, stage := range tc.stages {
			what := tc.cmd + " " + stage

			t.Run(strings.TrimPrefix(what, "phenix-app-"), func(t *testing.T) {
				res := appsExec(t, c.env(false), c.input, tc.cmd, stage)
				if res.code != 0 {
					t.Fatalf("%s exited %d on the experiment phenix gives it:\n%s", what, res.code, appsHead(res.stderr))
				}

				out := appsWantJSONDoc(t, what, res.stdout)
				appsWantJSONLogs(t, what, res.stderr)

				if out == nil {
					return
				}

				if !reflect.DeepEqual(out["x-e2e"], c.doc["x-e2e"]) {
					t.Errorf("%s dropped or changed the unknown top-level field x-e2e: %v", what, out["x-e2e"])
				}

				// The injectors change the spec in pre-start; checkInjections
				// covers that.
				if stage == "pre-start" && tc.cmd != appsMgmtTap {
					return
				}

				if d := appsDiff("spec", appsNormalize(out["spec"]), appsNormalize(c.doc["spec"])); d != "" {
					t.Errorf("%s changed a spec it has nothing to do with in this stage: %s", what, d)
				}
			})
		}
	}
}

func (c *appsConformance) checkInjections(t *testing.T) {
	t.Helper()

	before := appsSpecOf(t, c.doc).node("c1")
	if before == nil {
		t.Fatal("the input has no node c1")
	}

	t.Run("protonuke", func(t *testing.T) {
		res := appsExec(t, c.env(false), c.input, appsProtonuke, "pre-start")
		if res.code != 0 {
			t.Fatalf("protonuke pre-start exited %d:\n%s", res.code, appsHead(res.stderr))
		}

		src := filepath.Join(c.work, "startup", "c1-protonuke")
		appsWantInjections(
			t,
			"protonuke",
			appsSpecOf(t, appsWantJSONDoc(t, "protonuke pre-start", res.stdout)),
			before,
			src,
			"/etc/default/protonuke",
		)

		data, err := os.ReadFile(src) //nolint:gosec // test harness
		if err != nil {
			t.Fatalf("protonuke wrote no %s: %v", src, err)
		}

		if !appsProtonukeEnv("-serve -http").Match(bytes.TrimSpace(data)) {
			t.Errorf("%s = %q, want PROTONUKE_ARGS set to the host's args", src, data)
		}
	})

	t.Run("wireguard", func(t *testing.T) {
		res := appsExec(t, c.env(false), c.input, appsWireguard, "pre-start")
		if res.code != 0 {
			t.Fatalf("wireguard pre-start exited %d:\n%s", res.code, appsHead(res.stderr))
		}

		src := filepath.Join(c.work, "startup", "c1-wireguard.conf")
		spec := appsSpecOf(t, appsWantJSONDoc(t, "wireguard pre-start", res.stdout))
		appsWantInjections(t, "wireguard", spec, before, src, "/etc/wireguard/wg0.conf")

		if n := spec.node("c1"); n != nil {
			for _, inj := range n.Injections {
				if inj.Dst == "/etc/phenix/startup/wireguard-enable.sh" {
					t.Errorf("wireguard injected an enable script for a host without boot: true")
				}
			}
		}

		appsWantWireguardConf(t, src, c.wgPriv, map[string]string{
			"Interface.Address": "10.100.0.1/24",
			"Peer.PublicKey":    c.wgPeer,
			"Peer.AllowedIPs":   "10.100.0.2/32",
		})
	})
}

func (c *appsConformance) checkInvalidInput(t *testing.T) {
	t.Helper()

	for _, cmd := range []string{appsProtonuke, appsWireguard, appsMgmtTap, appsMirror} {
		res := appsExec(t, c.env(false), []byte("this is not an experiment\n"), cmd, "configure")
		if res.code == 0 {
			t.Errorf("%s exited 0 on stdin that is not an experiment", cmd)
		}

		if len(bytes.TrimSpace(res.stdout)) != 0 {
			t.Errorf("%s printed %q on stdout for stdin that is not an experiment", cmd, appsHead(res.stdout))
		}

		res = appsExec(t, c.env(false), c.input, cmd, "teardown")
		if res.code == 0 {
			t.Errorf("%s exited 0 for the unknown stage teardown", cmd)
		}

		if len(bytes.TrimSpace(res.stdout)) != 0 {
			t.Errorf("%s printed %q on stdout for the unknown stage teardown", cmd, appsHead(res.stdout))
		}
	}

	// An app must be in the scenario it is run for.
	noApps := appsWithApps(t, c.base, c.work, []any{})

	if res := appsExec(t, c.env(false), noApps, appsProtonuke, "configure"); res.code == 0 {
		t.Errorf("%s exited 0 for an experiment whose scenario does not list it", appsProtonuke)
	}
}

func (c *appsConformance) checkDryRunLogs(t *testing.T) {
	t.Helper()

	const what = "protonuke dry-run pre-start"

	res := appsExec(t, c.env(true), c.input, appsProtonuke, "pre-start")
	if res.code != 0 {
		t.Fatalf("%s exited %d:\n%s", what, res.code, appsHead(res.stderr))
	}

	appsWantJSONDoc(t, what, res.stdout)

	// phenix gives apps PHENIX_LOG_FILE=stderr in dry runs too, and parses
	// their stderr as JSON log lines.
	appsWantJSONLogs(t, what, res.stderr)
}

func (c *appsConformance) checkMirror(t *testing.T) {
	t.Helper()

	res := appsExec(t, c.env(false), c.input, appsMirror, "configure")
	if res.code != 0 {
		t.Fatalf("mirror configure exited %d:\n%s", res.code, appsHead(res.stderr))
	}

	appsWantJSONLogs(t, "mirror configure", res.stderr)

	if out := appsWantJSONDoc(t, "mirror configure", res.stdout); out != nil {
		appsWantMirrorIface(t, appsSpecOf(t, out).node("c2"), "c2")

		if !reflect.DeepEqual(out["x-e2e"], c.doc["x-e2e"]) {
			t.Errorf("%s configure dropped or changed the unknown top-level field x-e2e: %v", appsMirror, out["x-e2e"])
		}
	}

	// checkInvalidInput covers stdin that is not an experiment and an
	// unknown stage.
	if res := appsExec(t, c.env(false), nil, appsMirror); res.code == 0 {
		t.Errorf("%s exited 0 when run without a stage", appsMirror)
	}
}

// ---------------------------------------------------------------------------
// phenix-scheduler-single-node

// appsCheckSchedulerEdges feeds the single-node scheduler what phenix gives
// it (app) with no cluster hosts, which it must refuse by name, and with an
// external node added, which minimega does not run and so it must not
// schedule.
func appsCheckSchedulerEdges(t *testing.T, env []string, app map[string]any) {
	t.Helper()

	input, err := json.Marshal(map[string]any{"spec": app["spec"], "hosts": []any{}})
	if err != nil {
		t.Fatal(err)
	}

	res := appsExec(t, env, input, appsScheduler)
	if res.code == 0 || !strings.Contains(string(res.stderr), "phenix passed no cluster hosts") {
		t.Errorf("%s with no hosts exited %d with %q, want a nonzero exit that says phenix passed no cluster hosts",
			appsScheduler, res.code, appsHead(res.stderr))
	}

	if len(bytes.TrimSpace(res.stdout)) != 0 {
		t.Errorf("%s printed %q on stdout with no hosts", appsScheduler, appsHead(res.stdout))
	}

	// A copy of the spec through JSON, so app keeps its own.
	var spec map[string]any

	raw, err := json.Marshal(app["spec"])
	if err == nil {
		err = json.Unmarshal(raw, &spec)
	}

	topo, _ := spec["topology"].(map[string]any)
	nodes, _ := topo["nodes"].([]any)

	if err != nil || len(nodes) == 0 {
		t.Fatalf("the app input phenix gives %s has no topology nodes (%v): %v", appsScheduler, err, app["spec"])
	}

	topo["nodes"] = append(nodes, map[string]any{
		"type": "VirtualMachine", "external": true, "general": map[string]any{"hostname": "e2e-ext1"},
	})

	if input, err = json.Marshal(map[string]any{"spec": spec, "hosts": app["hosts"]}); err != nil {
		t.Fatal(err)
	}

	res = appsExec(t, env, input, appsScheduler)
	if res.code != 0 {
		t.Fatalf("%s exited %d with an external node:\n%s", appsScheduler, res.code, appsHead(res.stderr))
	}

	var out struct {
		Spec struct {
			Schedules map[string]string `json:"schedules"`
		} `json:"spec"`
	}

	if err := json.Unmarshal(res.stdout, &out); err != nil {
		t.Fatalf("%s printed no experiment with an external node: %v\n%s", appsScheduler, err, appsHead(res.stdout))
	}

	if host, ok := out.Spec.Schedules["e2e-ext1"]; ok {
		t.Errorf("%s scheduled the external node e2e-ext1 on %q, want it left out", appsScheduler, host)
	}

	for _, vm := range []string{"s1", "s2", "s3"} {
		if out.Spec.Schedules[vm] == "" {
			t.Errorf("%s with an external node left %s unscheduled: %v", appsScheduler, vm, out.Spec.Schedules)
		}
	}
}

// TestAppsScheduler checks the single-node user scheduler: its protocol
// (no arguments, JSON in and out) and that it schedules every VM on the only
// cluster host, as phenix's REST API, rendered script and minimega see it.
func TestAppsScheduler(t *testing.T) {
	requireApps(t, "apps", appsScheduler)
	hollowImages(t)

	const name = "e2e-sched"

	host := headnode(t)
	vms := []string{"s1", "s2", "s3"}

	exp, _ := appsCreate(t, name, topology(
		appsHollow("s1", "LAN", "10.64.0.1"),
		appsHollow("s2", "LAN", "10.64.0.2"),
		appsHollow("s3", "LAN", "10.64.0.3"),
	), "")

	t.Run("protocol", func(t *testing.T) {
		env := []string{"PHENIX_LOG_LEVEL=DEBUG", "PHENIX_LOG_FILE=stderr", "PHENIX_DIR=" + appsPhenixDir}

		res := appsExec(t, env, nil, appsScheduler, "extra-arg")
		if res.code == 0 || !strings.Contains(string(res.stderr), "must not pass any arguments") {
			t.Errorf(
				"%s extra-arg exited %d with %q, want a nonzero exit that says it takes no arguments",
				appsScheduler,
				res.code,
				appsHead(res.stderr),
			)
		}

		if res := appsExec(t, env, []byte("this is not an experiment\n"), appsScheduler); res.code == 0 {
			t.Errorf("%s exited 0 on stdin that is not an experiment", appsScheduler)
		}

		var app map[string]any
		if err := json.Unmarshal(appsPhenixStdout(t, "util", "app-json", name), &app); err != nil {
			t.Fatal(err)
		}

		// phenix gives a user scheduler the spec and the schedulable hosts
		// (scheduler/user.go).
		input, err := json.Marshal(map[string]any{"spec": app["spec"], "hosts": app["hosts"], "x-e2e": "unknown fields survive"})
		if err != nil {
			t.Fatal(err)
		}

		res = appsExec(t, env, input, appsScheduler)
		if res.code != 0 {
			t.Fatalf("%s exited %d on what phenix gives it:\n%s", appsScheduler, res.code, appsHead(res.stderr))
		}

		if out := appsWantJSONDoc(t, appsScheduler, res.stdout); out != nil && out["x-e2e"] != "unknown fields survive" {
			t.Errorf("%s dropped the unknown top-level field x-e2e: %v", appsScheduler, out["x-e2e"])
		}

		appsCheckSchedulerEdges(t, env, app)
	})

	t.Run("schedules every VM on the only host", func(t *testing.T) {
		phenix(t, "experiment", "schedule", name, "single-node")

		var got struct {
			Schedule []struct {
				VM   string `json:"vm"`
				Host string `json:"host"`
			} `json:"schedule"`
		}

		apiOK(t, http.MethodGet, "/experiments/"+name+"/schedule", nil, &got)

		schedule := make(map[string]string)
		for _, s := range got.Schedule {
			schedule[s.VM] = s.Host
		}

		want := map[string]string{"s1": host, "s2": host, "s3": host}

		if !maps.Equal(schedule, want) {
			t.Errorf("GET /experiments/%s/schedule = %v, want %v", name, schedule, want)
		}

		exp.with(t).start()

		script := exp.with(t).mmScript()

		for _, vm := range vms {
			if !appsHasLine(appsVMBlock(script, vm), "vm config schedule "+host) {
				t.Errorf("the rendered script does not schedule %s on %s", vm, host)
			}
		}

		exp.with(t).waitRunning(time.Minute, vms...)

		for vm, row := range vmInfo(t, name) {
			if row["host"] != host {
				t.Errorf("minimega runs %s on %q, want %q", vm, row["host"], host)
			}
		}
	})
}

// ---------------------------------------------------------------------------
// mgmt_tap

// TestAppsMgmtTap checks the host tap mgmt_tap creates on an experiment
// VLAN, as minimega, Open vSwitch and the host's network stack see it.
func TestAppsMgmtTap(t *testing.T) {
	requireApps(t, "apps", appsMgmtTap)
	hollowImages(t)

	topo := topology(appsHollow("h1", "MGMT", "192.168.250.10"), appsHollow("h2", "LAN", "10.62.0.2"))

	mgmt := func(md map[string]any) string { return appsScenario(appsApp("mgmt_tap", md)) }

	t.Run("lifecycle", func(t *testing.T) {
		const name = "e2e-mt"

		exp, _ := appsCreate(t, name, topo, mgmt(map[string]any{"vlan": "MGMT", "subnet": "192.168.250.0/24"}))
		exp.start()

		tap := appsWantHostTap(t, name, "phenix", "MGMT", "192.168.250.1/24")

		exp.stop()

		appsWaitLinkGone(t, tap)
		appsWantNoNamespace(t, name)
	})

	// Without bridge and vlan in its metadata, mgmt_tap uses the experiment's
	// default bridge and the VLAN alias MGMT.
	t.Run("the experiment's default bridge and the default VLAN", func(t *testing.T) {
		const name, bridge = "e2e-mtb", "e2emtbr"

		appsDestroyBridge(t, bridge)

		exp, _ := appsCreate(t, name, topo, mgmt(map[string]any{"subnet": "192.168.250.0/24"}), "--default-bridge", bridge)
		exp.start()
		exp.waitRunning(time.Minute, "h1", "h2")

		appsWantHostTap(t, name, bridge, "MGMT", "192.168.250.1/24")

		for vm, row := range vmInfo(t, name) {
			for _, tap := range splitList(row["tap"]) {
				if br, err := hostCmd("ovs-vsctl", "port-to-br", tap); err != nil || br != bridge {
					t.Errorf("%s tap %s is on bridge %q (%v), want %s", vm, tap, br, err, bridge)
				}
			}
		}
	})

	t.Run("a missing VLAN alias fails the start", func(t *testing.T) {
		const name = "e2e-mtv"

		appsCreate(t, name, topo, mgmt(map[string]any{"vlan": "NOPE"}))

		links := appsLinks(t)
		qemuBefore := qemuProcesses(t)

		out := phenixFails(t, "experiment", "start", name)
		appsWantFailed(t, out, "mgmt_tap")
		appsWantLogged(t, out, "Cannot find VLAN ID for alias NOPE")

		if appsIsRunning(t, name) {
			t.Errorf("experiment %s is running although mgmt_tap failed", name)
		}

		appsWantNoNamespace(t, name)
		appsWaitQEMU(t, qemuBefore)
		appsWaitNoNewLinks(t, links)
	})

	t.Run("experiment names longer than nine characters", func(t *testing.T) {
		// The two names share their first nine characters, so a namespace or tap
		// name cut to nine characters would be the same for both.
		const first, second, short = "e2e-mtlongname1", "e2e-mtlongname2", "e2e-mtlon"

		t.Cleanup(func() { _, _ = mmRun("", "clear namespace "+short) })

		e1, _ := appsCreate(t, first, topo, mgmt(map[string]any{"vlan": "MGMT", "subnet": "192.168.250.0/24"}))
		e2, _ := appsCreate(t, second, topo, mgmt(map[string]any{"vlan": "MGMT", "subnet": "192.168.251.0/24"}))

		e1.start()

		if rows := mmTable(t, first, "tap"); len(rows) != 1 {
			t.Errorf("namespace %s has host taps %v, want the one mgmt_tap created for it", first, rows)

			if _, ok := mmNamespaces(t)[short]; ok {
				t.Logf("namespace %s has host taps %v", short, mmTable(t, short, "tap"))
			}
		}

		e2.start()

		if !appsHostHasAddr(t, "192.168.251.1/24") {
			t.Errorf("experiment %s got no host tap with 192.168.251.1/24 and its start still succeeded", second)
		}

		e2.stop()
		e1.stop()

		if _, ok := mmNamespaces(t)[short]; ok {
			t.Errorf("minimega still has namespace %s, which mgmt_tap created for %s and %s, after both stopped", short, first, second)
		}
	})

	// Last, since a regression leaves an untagged host tap on the phenix
	// bridge until the dry-run experiment stops.
	t.Run("a dry run leaves the host alone", func(t *testing.T) {
		const name = "e2e-mtd"

		exp, _ := appsCreate(t, name, topo, mgmt(map[string]any{"vlan": "MGMT", "subnet": "192.168.250.0/24"}))

		links := appsLinks(t)

		exp.start("--dry-run")

		if added := appsNewLinks(links, appsLinks(t)); len(added) > 0 {
			t.Errorf("a dry-run start created host interfaces %v", added)
		}

		exp.stop()
	})
}

// ---------------------------------------------------------------------------
// phenix-app-mirror (hollow VMs; the guest test covers GRE with traffic)

// TestAppsMirror checks the mirror app's ERSPAN control plane on the host and
// that a failing mirror app fails the start.
func TestAppsMirror(t *testing.T) {
	requireApps(t, "apps", appsMirror)
	hollowImages(t)

	topo := topology(
		appsHollow("src", "LAN", "10.65.0.1"),
		appsHollow("dst", "LAN", "10.65.0.2"),
		appsNode("monh", ext4Image, appsHollowMemory),
	)

	mirror := func(md map[string]any) string {
		return appsScenario(appsApp("mirror", md, appsHost{"monh", map[string]any{"interface": "mirror0", "vlans": []any{"LAN"}}}))
	}

	t.Run("erspan v2", func(t *testing.T) {
		const name = "e2e-mir"

		// Metadata without a version is read as the v0 format, which nests
		// erspan under directGRE.
		exp, _ := appsCreate(t, name, topo, mirror(map[string]any{
			"version": "v1",
			"erspan":  map[string]any{"enabled": true, "version": 2, "direction": 0, "hwid": 1},
		}))
		appsRemoveMirrors(t, name)

		appsWantMirrorIface(t, appsStored(t, name).node("monh"), "monh")

		exp.start()
		exp.waitRunning(time.Minute, "src", "dst", "monh")

		st := appsWantMirror(t, name, "monh", "erspan")

		for key, want := range map[string]string{
			"erspan_ver":  "2",
			"erspan_dir":  "0",
			"erspan_hwid": "1",
		} {
			if got := appsOVSString(mustOVS(t, "get", "interface", st.port, "options:"+key)); got != want {
				t.Errorf("ERSPAN port %s options:%s = %q, want %q", st.port, key, got, want)
			}
		}

		appsWantMirrorPorts(t, name, st, "src", "dst")

		exp.stop()

		appsWantMirrorGone(t, st)
	})

	t.Run("a failing post-start fails the start", func(t *testing.T) {
		const name = "e2e-mirbad"

		appsCreate(t, name, topo, mirror(map[string]any{
			"version": "v1",
			"erspan":  map[string]any{"enabled": true, "version": 3},
		}))

		out, err := phenixRun("experiment", "start", name)

		if err == nil {
			t.Errorf("experiment start succeeded although the mirror app's post-start failed on ERSPAN version 3:\n%s", out)
		} else if !strings.Contains(out, "unknown ERSPAN version (3)") {
			t.Errorf("experiment start does not report the mirror app's error:\n%s", out)
		}

		// The mirror app removes the tap it created before it fails; minimega
		// deletes host taps from its reaper, about a second later.
		eventually(t, 15*time.Second, 500*time.Millisecond, "the failed mirror app's tap to go", func() (bool, string) {
			ports, _ := hostCmd("ovs-vsctl", "list-ports", "phenix")
			for p := range strings.FieldsSeq(ports) {
				if strings.HasSuffix(p, "-mirror") {
					return false, "mirror tap " + p + " is still on bridge phenix"
				}
			}

			return true, ""
		})

		if st, ok := appsStored(t, name).Status.Apps["mirror"]; ok {
			t.Errorf("status.apps.mirror = %v although the mirror app failed", st)
		}
	})
}

// ---------------------------------------------------------------------------
// protonuke and wireguard input validation

// TestAppsInjectorValidation checks that protonuke and wireguard reject
// metadata that would inject extra lines into the files they stage.
func TestAppsInjectorValidation(t *testing.T) {
	requireApps(t, "apps", appsProtonuke, appsWireguard)
	hollowImages(t)

	key, _ := appsWGKey(t)
	topo := topology(appsHollow("n1", "LAN", "10.66.0.1"))

	cases := []struct {
		exp, app, file string
		metadata       map[string]any
	}{
		{"e2e-pnnl", "protonuke", "n1-protonuke", map[string]any{"args": "-http\nrm -rf /"}},
		{"e2e-wgnl", "wireguard", "n1-wireguard.conf", map[string]any{
			"interface": map[string]any{"private_key": key, "address": "10.0.0.1/24\nPostUp = id"},
			"peers":     []any{},
		}},
	}

	for _, tc := range cases {
		t.Run(tc.app, func(t *testing.T) {
			appsCreate(t, tc.exp, topo, appsScenario(appsApp(tc.app, nil, appsHost{"n1", tc.metadata})))

			out := phenixFails(t, "experiment", "start", tc.exp)
			appsWantFailed(t, out, tc.app)
			appsWantLogged(t, out, "must not contain newlines")

			path := filepath.Join(appsStored(t, tc.exp).Spec.BaseDir, "startup", tc.file)
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("%s staged %s although it rejected the host's metadata (stat: %v)", tc.app, path, err)
			}

			if appsIsRunning(t, tc.exp) {
				t.Errorf("experiment %s is running although %s failed", tc.exp, tc.app)
			}

			appsWantNoNamespace(t, tc.exp)
		})
	}
}

// ---------------------------------------------------------------------------
// protonuke, wireguard, mirror and mgmt_tap on real guests

// TestAppsGuests runs protonuke, wireguard, the mirror app and mgmt_tap
// together on three real guests and checks what each app did on the host
// and inside the guests: protonuke traffic between two guests, the wireguard
// configuration in the guests and the tunnel it sets up between them, the
// host reaching the guests through the mgmt_tap tap, and that traffic
// mirrored over GRE reaches the monitor.
func TestAppsGuests(t *testing.T) {
	requireApps(t, "apps", appsProtonuke, appsWireguard, appsMirror, appsMgmtTap)

	image := appsPrepareGuestImage(t)

	g := &appsGuests{name: "e2e-appg", qemuBefore: qemuProcesses(t)}
	g.k1, g.p1 = appsWGKey(t)
	g.k2, g.p2 = appsWGKey(t)

	exp, _ := appsCreate(t, g.name, topology(
		appsNode("ga1", image, appsGuestMemory, appsNIC{"eth0", "LAN", "10.20.0.11"}),
		appsNode("ga2", image, appsGuestMemory, appsNIC{"eth0", "LAN", "10.20.0.12"}),
		appsNode("mon", image, appsGuestMemory),
	), g.scenario())
	g.exp = exp
	appsRemoveMirrors(t, g.name)

	t.Run("configure gave the monitor its mirror interface", func(t *testing.T) {
		appsWantMirrorIface(t, appsStored(t, g.name).node("mon"), "mon")
	})

	start := time.Now()

	exp.start()
	exp.waitRunning(time.Minute, "ga1", "ga2", "mon")

	// The Python apps name staged files by their resolved path (safe_join).
	g.baseDir = appsStored(t, g.name).Spec.BaseDir
	if resolved, err := filepath.EvalSymlinks(g.baseDir); err == nil {
		g.baseDir = resolved
	}

	t.Run("pre-start staged and injected the protonuke and wireguard files", g.checkStaged)
	t.Run("the mirror's tap, tunnel and mirror on the host", g.checkMirrorHost)
	t.Run("the mgmt_tap host tap", g.checkMgmtTapHost)

	waitCCClients(t, g.name, "ga1", "ga2", "mon")
	t.Logf("the guests registered with C2 %v after start", time.Since(start).Round(time.Second))

	g.gatherFacts(t)

	t.Run("protonuke serves and fetches between the guests", g.checkProtonuke)
	t.Run("the wireguard configuration reached the guests", g.checkWireguard)
	t.Run("the wireguard tunnel connects the guests", g.checkWireguardTunnel)
	t.Run("the host reaches the guests through the mgmt_tap tap", g.checkMgmtTapPing)
	t.Run("the mirror delivers the guests' traffic to the monitor over GRE", g.checkMirrorTraffic)
	t.Run("stop removes what the apps created on the host", g.checkStop)
}

// appsGuests is the experiment TestAppsGuests drives.
type appsGuests struct {
	name       string
	baseDir    string
	exp        *experiment
	qemuBefore int

	// wireguard keys: k for private, p for public
	k1, p1, k2, p2 string

	mirror  appsMirrorState
	mgmtTap string
	facts   map[string]map[string]string // per guest
}

func (g *appsGuests) scenario() string {
	return appsScenario(
		appsApp("protonuke", nil,
			appsHost{"ga1", map[string]any{"args": appsNukeServerArgs}},
			appsHost{"ga2", map[string]any{"args": appsNukeClientArgs}},
		),
		appsApp("wireguard", nil,
			appsHost{"ga1", map[string]any{
				"boot":      true,
				"interface": map[string]any{"private_key": g.k1, "address": "10.100.0.1/24", "listen_port": 51820},
				"peers":     []any{map[string]any{"public_key": g.p2, "allowed_ips": "10.100.0.2/32"}},
			}},
			appsHost{"ga2", map[string]any{
				"interface": map[string]any{"private_key": g.k2, "address": "10.100.0.2/24"},
				"peers": []any{map[string]any{
					"public_key": g.p1, "allowed_ips": "10.100.0.0/24", "server_endpoint": "10.20.0.11:51820", "keepalive": 5,
				}},
			}},
		),
		appsApp("mirror", nil, appsHost{"mon", map[string]any{"interface": "mirror0", "vlans": []any{"LAN"}}}),
		appsApp("mgmt_tap", map[string]any{"vlan": "LAN", "subnet": "10.20.0.0/24"}),
	)
}

func (g *appsGuests) startupFile(name string) string {
	return filepath.Join(g.baseDir, "startup", name)
}

func (g *appsGuests) checkStaged(t *testing.T) {
	t.Helper()

	script := g.exp.with(t).mmScript()

	for vm, args := range map[string]string{"ga1": appsNukeServerArgs, "ga2": appsNukeClientArgs} {
		block := appsVMBlock(script, vm)

		nuke := g.startupFile(vm + "-protonuke")

		if data, err := os.ReadFile(nuke); err != nil { //nolint:gosec // test harness
			t.Errorf("protonuke staged no %s: %v", nuke, err)
		} else if !appsProtonukeEnv(args).Match(bytes.TrimSpace(data)) {
			t.Errorf("%s = %q, want PROTONUKE_ARGS set to %q", nuke, data, args)
		}

		appsWantInject(t, block, vm, nuke, "/etc/default/protonuke")

		wg := g.startupFile(vm + "-wireguard.conf")

		if info, err := os.Stat(wg); err != nil {
			t.Errorf("wireguard staged no %s: %v", wg, err)
		} else if info.Mode().Perm() != 0o600 {
			t.Errorf("%s has mode %o, want 600: it holds the private key", wg, info.Mode().Perm())
		}

		appsWantInject(t, block, vm, wg, "/etc/wireguard/wg0.conf")
	}

	appsWantWireguardConf(t, g.startupFile("ga1-wireguard.conf"), g.k1, map[string]string{
		"Interface.Address":    "10.100.0.1/24",
		"Interface.ListenPort": "51820",
		"Peer.PublicKey":       g.p2,
		"Peer.AllowedIPs":      "10.100.0.2/32",
	})
	appsWantWireguardConf(t, g.startupFile("ga2-wireguard.conf"), g.k2, map[string]string{
		"Interface.Address":        "10.100.0.2/24",
		"Peer.PublicKey":           g.p1,
		"Peer.AllowedIPs":          "10.100.0.0/24",
		"Peer.Endpoint":            "10.20.0.11:51820",
		"Peer.PersistentKeepalive": "5",
	})

	enable := g.startupFile("ga1-wireguard-enable.sh")

	if data, err := os.ReadFile(enable); err != nil { //nolint:gosec // test harness
		t.Errorf("wireguard staged no %s for a host with boot: true: %v", enable, err)
	} else if got := strings.TrimSpace(string(data)); got != "systemctl enable --now wg-quick@wg0" {
		t.Errorf("%s = %q, want it to enable wg-quick@wg0", enable, got)
	}

	appsWantInject(t, appsVMBlock(script, "ga1"), "ga1", enable, "/etc/phenix/startup/wireguard-enable.sh")

	if strings.Contains(appsVMBlock(script, "ga2"), "wireguard-enable.sh") {
		t.Error("the rendered script injects a wireguard enable script into ga2, which does not set boot: true")
	}
}

func (g *appsGuests) checkMirrorHost(t *testing.T) {
	t.Helper()

	g.mirror = appsWantMirror(t, g.name, "mon", "gre")
	appsWantMirrorPorts(t, g.name, g.mirror, "ga1", "ga2")
}

func (g *appsGuests) checkMgmtTapHost(t *testing.T) {
	t.Helper()

	g.mgmtTap = appsWantHostTap(t, g.name, "phenix", "LAN", "10.20.0.1/24")
}

// gatherFacts collects, in one C2 command per guest, everything the guest
// checks need. protonuke traffic is waited for inside the guests.
func (g *appsGuests) gatherFacts(t *testing.T) {
	t.Helper()

	wireguard := [][2]string{
		// The wireguard enable script is ga1's last startup script; wait for
		// phenix.service to have run it before looking at wg0.
		{"startup", "for i in $(seq 60); do [ \"$(systemctl is-active phenix.service)\" = activating ] || break; sleep 1; done; " +
			"systemctl is-active phenix.service"},
		{"wg-mode", "stat -c %a /etc/wireguard/wg0.conf"},
		{"wg-sum", "sha256sum < /etc/wireguard/wg0.conf | cut -d' ' -f1"},
		{"wg-enable", "cat /etc/phenix/startup/wireguard-enable.sh || echo absent"},
		{"wg-tools", "command -v wg >/dev/null && echo present || echo absent"},
		{"wg-enabled", "systemctl is-enabled wg-quick@wg0.service"},
		{"wg-active", "systemctl is-active wg-quick@wg0.service"},
		{"wg-link", "ip -o link show dev wg0 >/dev/null && echo present || echo absent"},
		{"wg-address", "ip -4 -o addr show dev wg0 | awk '{print $4}'"},
		{"wg-port", "wg show wg0 listen-port"},
		{"wg-peers", "wg show wg0 peers"},
	}

	g.facts = map[string]map[string]string{
		"ga1": appsGuestFacts(t, g.name, "ga1", append([][2]string{
			{"nuke-env", "sha256sum < /etc/default/protonuke | cut -d' ' -f1"},
			{"nuke-binary", "test -x /usr/local/bin/protonuke && echo ok"},
			{"nuke-service", "systemctl is-active protonuke.service"},
			{"nuke-listening", "ss -Htln 'sport = :80' | wc -l"},
			{"nuke-traffic", appsWaitForLog(`grep -cE 'http 10\.20\.0\.12:[0-9]+ ' /var/log/protonuke.log`)},
		}, wireguard...)),
		"ga2": appsGuestFacts(t, g.name, "ga2", append([][2]string{
			{"nuke-env", "sha256sum < /etc/default/protonuke | cut -d' ' -f1"},
			{"nuke-binary", "test -x /usr/local/bin/protonuke && echo ok"},
			{"nuke-service", "systemctl is-active protonuke.service"},
			{"nuke-traffic", appsWaitForLog(`grep -cF 'http 10.20.0.11 http://10.20.0.11' /var/log/protonuke.log`)},
		}, wireguard...)),
		"mon": appsGuestFacts(t, g.name, "mon", [][2]string{
			{"address", "ip -4 -o addr show | grep -c ' " + appsMirrorMonitorIP + "/24 '"},
			{"gre", "timeout 60 tcpdump -nni eth0 -c 5 'ip proto 47'"},
		}),
	}
}

func (g *appsGuests) checkProtonuke(t *testing.T) {
	t.Helper()

	for _, vm := range []string{"ga1", "ga2"} {
		facts := g.facts[vm]

		if want := appsSHA256(t, g.startupFile(vm+"-protonuke")); facts["nuke-env"] != want {
			t.Errorf("%s /etc/default/protonuke does not match the file protonuke staged (sha256 %q, want %q)", vm, facts["nuke-env"], want)
		}

		if facts["nuke-binary"] != "ok" {
			t.Errorf("%s has no executable /usr/local/bin/protonuke", vm)
		}

		if facts["nuke-service"] != "active" {
			t.Errorf("%s protonuke.service is %q, want active", vm, facts["nuke-service"])
		}

		if n, _ := strconv.Atoi(facts["nuke-traffic"]); n < 1 {
			t.Errorf("%s protonuke logged no HTTP traffic with its peer within 90 s (count %q)", vm, facts["nuke-traffic"])
		}
	}

	if n, _ := strconv.Atoi(g.facts["ga1"]["nuke-listening"]); n < 1 {
		t.Errorf("ga1's protonuke server does not listen on port 80 (%q listeners)", g.facts["ga1"]["nuke-listening"])
	}
}

func (g *appsGuests) checkWireguard(t *testing.T) {
	t.Helper()

	for vm, enable := range map[string]string{"ga1": "systemctl enable --now wg-quick@wg0", "ga2": "absent"} {
		facts := g.facts[vm]

		if facts["wg-mode"] != "600" {
			t.Errorf("%s /etc/wireguard/wg0.conf has mode %q, want 600", vm, facts["wg-mode"])
		}

		if want := appsSHA256(t, g.startupFile(vm+"-wireguard.conf")); facts["wg-sum"] != want {
			t.Errorf("%s /etc/wireguard/wg0.conf does not match the file wireguard staged", vm)
		}

		if facts["wg-enable"] != enable {
			t.Errorf("%s /etc/phenix/startup/wireguard-enable.sh = %q, want %q", vm, facts["wg-enable"], enable)
		}

		if facts["wg-tools"] != "present" {
			t.Errorf("%s has no wg command; the tools guest (%s) comes with wireguard-tools", vm, cfg.tools)
		}
	}

	// boot: true on ga1 enables and starts wg-quick@wg0 from its startup
	// scripts, so its tunnel interface is up with the staged configuration.
	ga1 := g.facts["ga1"]

	if ga1["startup"] != "active" {
		t.Errorf("ga1 phenix.service is %q after its startup scripts, want active: the wireguard enable script failed", ga1["startup"])
	}

	for fact, want := range map[string]string{
		"wg-enabled": "enabled",
		"wg-active":  "active",
		"wg-link":    "present",
		"wg-address": "10.100.0.1/24",
		"wg-port":    "51820",
		"wg-peers":   g.p2,
	} {
		if ga1[fact] != want {
			t.Errorf("ga1 %s = %q, want %q", fact, ga1[fact], want)
		}
	}

	// ga2 does not set boot: true, so nothing brings its tunnel up at boot.
	ga2 := g.facts["ga2"]

	if ga2["wg-enabled"] == "enabled" || ga2["wg-active"] == "active" || ga2["wg-link"] != "absent" {
		t.Errorf("ga2 wg-quick@wg0 is %s and %s and wg0 is %s, want it neither enabled nor started and no wg0: ga2 does not set boot: true",
			ga2["wg-enabled"], ga2["wg-active"], ga2["wg-link"])
	}
}

// checkWireguardTunnel brings up ga2's tunnel from the configuration the
// wireguard app staged and reaches ga1's tunnel address through it, which
// needs both guests' keys, addresses and allowed IPs, ga1's listen port and
// ga2's endpoint to match.
func (g *appsGuests) checkWireguardTunnel(t *testing.T) {
	t.Helper()

	got := appsGuestFacts(t, g.name, "ga2", [][2]string{
		{"up", "wg-quick up wg0 >/dev/null && echo up"},
		{"route", "ip route get 10.100.0.1 | grep -o 'dev [^ ]*'"},
		{"ping", "for i in $(seq 20); do ping -c 1 -W 2 10.100.0.1 >/dev/null && { echo reached; exit 0; }; sleep 1; done; echo unreached"},
		{"handshake", "wg show wg0 latest-handshakes"},
	})

	if got["up"] != "up" {
		t.Fatalf("wg-quick up wg0 failed on ga2 with the staged /etc/wireguard/wg0.conf (output %q)", got["up"])
	}

	if got["route"] != "dev wg0" {
		t.Errorf("ga2 routes 10.100.0.1 via %q, want dev wg0", got["route"])
	}

	if got["ping"] != "reached" {
		t.Errorf("ga2 did not reach ga1's tunnel address 10.100.0.1 within 60 s (%q)", got["ping"])
	}

	if f := strings.Fields(got["handshake"]); len(f) != 2 || f[0] != g.p1 || f[1] == "0" {
		t.Errorf("ga2 wg0 latest handshakes = %q, want a handshake with ga1's key %s", got["handshake"], g.p1)
	}
}

func (g *appsGuests) checkMgmtTapPing(t *testing.T) {
	t.Helper()

	if g.mgmtTap == "" {
		t.Fatal("no mgmt_tap host tap was found")
	}

	eventually(t, time.Minute, 2*time.Second, "the host to ping ga1 and ga2 through "+g.mgmtTap, func() (bool, string) {
		for _, ip := range []string{"10.20.0.11", "10.20.0.12"} {
			if out, err := hostCmd("ping", "-c", "1", "-W", "2", "-I", g.mgmtTap, ip); err != nil {
				return false, fmt.Sprintf("%s: %v %s", ip, err, out)
			}
		}

		return true, ""
	})
}

func (g *appsGuests) checkMirrorTraffic(t *testing.T) {
	t.Helper()

	if g.mirror.port == "" {
		t.Fatal("the mirror app created no mirror")
	}

	// Host side: the mirror counts what it sends into the tunnel.
	txPackets := regexp.MustCompile(`tx_packets=(\d+)`)

	eventually(t, time.Minute, 2*time.Second, "mirror "+g.mirror.port+" to send packets", func() (bool, string) {
		stats, err := hostCmd("ovs-vsctl", "get", "mirror", g.mirror.port, "statistics")
		if err != nil {
			return false, stats
		}

		m := txPackets.FindStringSubmatch(stats)

		return m != nil && m[1] != "0", stats
	})

	facts := g.facts["mon"]

	if facts["address"] != "1" {
		t.Errorf("mon does not have %s/24 on its mirror interface", appsMirrorMonitorIP)
	}

	if gre := facts["gre"]; !strings.Contains(gre, "GREv0") || !regexp.MustCompile(`10\.20\.0\.1[12]`).MatchString(gre) {
		t.Errorf("mon captured no GRE packets carrying ga1/ga2 traffic within 60 s:\n%s", gre)
	}
}

func (g *appsGuests) checkStop(t *testing.T) {
	t.Helper()

	g.exp.with(t).stop()

	if g.mgmtTap != "" {
		appsWaitLinkGone(t, g.mgmtTap)
	}

	if g.mirror.port != "" {
		appsWantMirrorGone(t, g.mirror)
	}

	appsWantNoNamespace(t, g.name)
	appsWaitQEMU(t, g.qemuBefore)
}

// appsPrepareGuestImage builds a copy-on-write image on the tools guest
// (cfg.tools), which has the wireguard-tools the wireguard app's guests need,
// with protonuke and its systemd unit injected the way the images
// repository's recipes do it (phenix image inject-miniexe): the protonuke app
// only stages /etc/default/protonuke and expects the image to run protonuke.
func appsPrepareGuestImage(t *testing.T) string {
	t.Helper()

	requireImage(t, cfg.tools)

	const protonuke = "/usr/local/bin/protonuke"

	if _, err := os.Stat(protonuke); err != nil {
		t.Fatalf("the E2E workflow installs protonuke at %s for the protonuke app's guests: %v", protonuke, err)
	}

	path := filepath.Join(cfg.imagesDir, appsGuestImage)
	_ = os.Remove(path)

	if err := run(
		"qemu-img",
		"create",
		"-q",
		"-f",
		"qcow2",
		"-F",
		"qcow2",
		"-b",
		filepath.Join(cfg.imagesDir, cfg.tools),
		path,
	); err != nil {
		t.Fatal(err)
	}

	// Registered before the experiment, so it runs after the teardown.
	t.Cleanup(func() { _ = os.Remove(path) })

	phenix(t, "image", "inject-miniexe", protonuke, path)

	inspectDisk(t, path, func(root string) {
		if info, err := os.Stat(filepath.Join(root, "usr/local/bin/protonuke")); err != nil || info.Mode().Perm()&0o111 == 0 {
			t.Errorf("inject-miniexe left no executable /usr/local/bin/protonuke in %s (%v)", path, err)
		}

		// Stat follows the enablement symlink into the mounted image.
		if _, err := os.Stat(filepath.Join(root, "etc/systemd/system/multi-user.target.wants/protonuke.service")); err != nil {
			t.Errorf("inject-miniexe did not install and enable protonuke.service in %s: %v", path, err)
		}
	})

	return appsGuestImage
}

// ---------------------------------------------------------------------------
// the test user app

// appsTestAppScript is phenix-app-e2e. It needs only the Python 3 standard
// library.
const appsTestAppScript = `#!/usr/bin/python3
# phenix-app-e2e: a phenix user app the E2E tests install and remove.
#
# Every run records its argv, its PHENIX_ environment and its stdin under the
# directory the scenario names in metadata.dir, as <stage>.<call>.*, and then
# does what metadata.actions[<stage>] asks for that call (the last entry
# repeats):
#   ""        print nothing (no change)
#   edit      print the experiment with a label on every node, a memory change
#             (configure only) and status.apps.e2e set
#   fail      exit 1
#   schedule  print "round-robin" and exit 101
#   chatty    print a progress line before the experiment
import json
import os
import sys

APP = "e2e"


def log(level, msg):
    sys.stderr.write(json.dumps({"level": level, "msg": msg}) + "\n")
    sys.stderr.flush()


def write(path, data):
    with open(path + ".tmp", "w") as f:
        f.write(data)
    os.rename(path + ".tmp", path)


def main():
    stage = sys.argv[1] if len(sys.argv) > 1 else ""
    raw = sys.stdin.read()

    try:
        exp = json.loads(raw)
        app = [a for a in exp["spec"]["scenario"]["apps"] if a.get("name") == APP][0]
        md = app.get("metadata") or {}
        rec = md["dir"]
    except Exception as err:
        log("ERROR", "e2e-app cannot read the experiment on stdin: %r" % (err,))
        return 2

    os.makedirs(rec, exist_ok=True)

    call = 1
    while os.path.exists(os.path.join(rec, "%s.%d.json" % (stage, call))):
        call += 1

    base = os.path.join(rec, "%s.%d" % (stage, call))
    env = {k: v for k, v in os.environ.items() if k.startswith("PHENIX_")}

    write(base + ".argv", json.dumps(sys.argv))
    write(base + ".env", json.dumps(env))
    write(base + ".json", raw)

    log("INFO", "e2e-app says %s %d" % (stage, call))

    actions = (md.get("actions") or {}).get(stage) or [""]
    action = actions[min(call, len(actions)) - 1]

    if action == "fail":
        log("ERROR", "e2e-app fails %s on purpose" % stage)
        return 1

    if action == "schedule":
        sys.stdout.write("round-robin\n")
        return 101

    if action == "chatty":
        sys.stdout.write("e2e-app: working on %s\n" % stage)
        sys.stdout.write(json.dumps(exp))
        return 0

    if action == "edit":
        nodes = exp["spec"]["topology"]["nodes"]

        for node in nodes:
            labels = node.get("labels") or {}
            labels["e2e-" + stage] = str(call)
            node["labels"] = labels

        if stage == "configure" and md.get("memory"):
            nodes[0]["hardware"]["memory"] = md["memory"]

        status = exp.get("status") or {}
        apps = status.get("apps") or {}
        apps[APP] = {
            "stage": stage,
            "call": call,
            "vms": sorted(vm["name"] for vm in exp.get("vms") or []),
        }
        status["apps"] = apps
        exp["status"] = status

        sys.stdout.write(json.dumps(exp))

    return 0


sys.exit(main())
`

// appsInstallTestApp installs phenix-app-e2e as a regular file on the PATH
// of the CLI and the UI the harness started, and removes it when the test
// ends.
func appsInstallTestApp(t *testing.T) {
	t.Helper()

	path := filepath.Join("/usr/local/bin", "phenix-app-"+appsTestApp)

	if err := os.WriteFile(path, []byte(appsTestAppScript), 0o755); err != nil { //nolint:gosec // an executable user app
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = os.Remove(path) })

	if found, err := exec.LookPath(filepath.Base(path)); err != nil || found != path {
		t.Fatalf("user app %s is not first on PATH (found %q, %v)", path, found, err)
	}
}

// appsRecordDir returns an empty directory for the test app's recordings of
// one experiment.
func appsRecordDir(t *testing.T, name string) string {
	t.Helper()

	dir := filepath.Join(cfg.workDir, "apps-records", name)

	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}

	return dir
}

// appsTestAppScenario is a scenario that runs only the test app, recording
// into dir and acting per stage as actions says.
func appsTestAppScenario(dir string, actions map[string][]string) string {
	return appsScenario(appsApp(appsTestApp, map[string]any{"dir": dir, "memory": appsEditedMemory, "actions": actions}))
}

// appsRecord is one run of the test app.
type appsRecord struct {
	stage string
	call  int
	argv  []string
	env   map[string]string
	raw   []byte
	exp   appsExpDoc
}

// appsRecords returns the test app's runs of stage, in order.
func appsRecords(t *testing.T, dir, stage string) []appsRecord {
	t.Helper()

	var recs []appsRecord

	for call := 1; ; call++ {
		base := filepath.Join(dir, fmt.Sprintf("%s.%d", stage, call))

		// The app writes the stdin last, so the other files exist with it.
		raw, err := os.ReadFile(base + ".json") //nolint:gosec // test harness
		if errors.Is(err, os.ErrNotExist) {
			break
		}

		if err != nil {
			t.Fatal(err)
		}

		rec := appsRecord{stage: stage, call: call, raw: raw}

		appsReadJSON(t, base+".argv", &rec.argv)
		appsReadJSON(t, base+".env", &rec.env)

		if err := json.Unmarshal(raw, &rec.exp); err != nil {
			t.Errorf("%s call %d: stdin is not the experiment as JSON: %v", stage, call, err)
		}

		recs = append(recs, rec)
	}

	return recs
}

func appsReadJSON(t *testing.T, path string, v any) {
	t.Helper()

	data, err := os.ReadFile(path) //nolint:gosec // test harness
	if err != nil {
		t.Fatal(err)
	}

	if err := json.Unmarshal(data, v); err != nil {
		t.Fatalf("decoding %s: %v", path, err)
	}
}

// appsCheckInvocation checks how phenix ran the test app (app/user.go): the
// stage as the only argument, the PHENIX_ environment, and the experiment as
// one JSON document on stdin.
func appsCheckInvocation(t *testing.T, rec appsRecord, expName string) {
	t.Helper()

	what := fmt.Sprintf("%s call %d", rec.stage, rec.call)

	if len(rec.argv) != 2 || filepath.Base(rec.argv[0]) != "phenix-app-"+appsTestApp || rec.argv[1] != rec.stage {
		t.Errorf("%s: argv = %q, want [phenix-app-%s %s]", what, rec.argv, appsTestApp, rec.stage)
	}

	// phenix passes its own PHENIX_LOG_LEVEL through when it is set, even to
	// an empty value, and DEBUG otherwise (app/user.go, util.GetEnv).
	logLevel, inherited := os.LookupEnv("PHENIX_LOG_LEVEL")
	if !inherited {
		logLevel = "DEBUG"
	}

	for key, want := range map[string]string{
		"PHENIX_DIR":            appsPhenixDir,
		"PHENIX_FILES_DIR":      filepath.Join(appsPhenixDir, "images", expName, "files"),
		"PHENIX_LOG_FILE":       "stderr",
		"PHENIX_LOG_LEVEL":      logLevel,
		"PHENIX_DRYRUN":         "false",
		"PHENIX_STORE_ENDPOINT": cfg.store,
		// The apps' is_headnode trims these.
		"PHENIX_HOSTNAME_SUFFIXES": appsHostnameSuffixes(),
	} {
		if got, ok := rec.env[key]; !ok || got != want {
			t.Errorf("%s: %s = %q (set: %v), want %q", what, key, got, ok, want)
		}
	}

	var top map[string]json.RawMessage
	if err := json.Unmarshal(rec.raw, &top); err != nil {
		t.Errorf("%s: stdin is not one JSON object: %v", what, err)

		return
	}

	for _, key := range []string{"metadata", "spec", "status", "hosts"} {
		if _, ok := top[key]; !ok {
			t.Errorf("%s: stdin has no %q (keys %v)", what, key, slices.Sorted(maps.Keys(top)))
		}
	}

	if rec.exp.Metadata.Name != expName || rec.exp.Spec.ExperimentName != expName {
		t.Errorf(
			"%s: stdin is for experiment %q (spec.experimentName %q), want %s",
			what,
			rec.exp.Metadata.Name,
			rec.exp.Spec.ExperimentName,
			expName,
		)
	}
}

// appsCheckHosts checks the cluster hosts phenix gives apps: on one runner,
// the headnode, which is also the only schedulable host.
func appsCheckHosts(t *testing.T, rec appsRecord, host string) {
	t.Helper()

	if len(rec.exp.Hosts) != 1 || rec.exp.Hosts[0].Name != host || !rec.exp.Hosts[0].Schedulable || !rec.exp.Hosts[0].Headnode {
		t.Errorf("%s call %d: stdin hosts = %+v, want only %s, schedulable and the headnode", rec.stage, rec.call, rec.exp.Hosts, host)
	}
}

// appsCheckRuntimeVMs checks the VMs phenix gives an app against what
// minimega reports for them.
func appsCheckRuntimeVMs(t *testing.T, rec appsRecord, ns string, lan int, want ...string) {
	t.Helper()

	what := fmt.Sprintf("%s call %d", rec.stage, rec.call)
	info := vmInfo(t, ns)

	got := make(map[string]appsVMDoc, len(rec.exp.VMs))
	for _, vm := range rec.exp.VMs {
		got[vm.Name] = vm
	}

	if names := appsVMNames(rec.exp.VMs); !slices.Equal(names, want) {
		t.Errorf("%s: stdin lists VMs %v, want %v", what, names, want)
	}

	for _, name := range want {
		vm, ok := got[name]
		if !ok {
			continue
		}

		row := info[name]

		if vm.State != appsRunning || !vm.Running {
			t.Errorf("%s: %s state %q (running %v), want RUNNING", what, name, vm.State, vm.Running)
		}

		if vm.Host != row["host"] {
			t.Errorf("%s: %s host %q, minimega reports %q", what, name, vm.Host, row["host"])
		}

		if taps := splitList(row["tap"]); len(taps) == 0 || !slices.Equal(vm.Taps, taps) {
			t.Errorf("%s: %s taps %v, minimega reports %q", what, name, vm.Taps, row["tap"])
		}

		if nets := []string{fmt.Sprintf("LAN (%d)", lan)}; !slices.Equal(vm.Networks, nets) {
			t.Errorf("%s: %s networks %v, want %v (minimega reports %q)", what, name, vm.Networks, nets, row["vlan"])
		}
	}
}

// ---------------------------------------------------------------------------
// experiment documents

type appsIface struct {
	Name    string `json:"name"`
	VLAN    string `json:"vlan"`
	Address string `json:"address"`
	Mask    int    `json:"mask"`
	Proto   string `json:"proto"`
	Bridge  string `json:"bridge"`
}

type appsInjection struct {
	Src string `json:"src"`
	Dst string `json:"dst"`
}

type appsNodeDoc struct {
	General struct {
		Hostname string `json:"hostname"`
	} `json:"general"`
	Hardware struct {
		Memory int `json:"memory"`
	} `json:"hardware"`
	Labels  map[string]string `json:"labels"`
	Network struct {
		Interfaces []appsIface `json:"interfaces"`
	} `json:"network"`
	Injections []appsInjection `json:"injections"`
}

type appsSpecDoc struct {
	ExperimentName string            `json:"experimentName"`
	BaseDir        string            `json:"baseDir"`
	Schedules      map[string]string `json:"schedules"`
	Topology       struct {
		Nodes []appsNodeDoc `json:"nodes"`
	} `json:"topology"`
}

func (s appsSpecDoc) node(hostname string) *appsNodeDoc {
	for i := range s.Topology.Nodes {
		if s.Topology.Nodes[i].General.Hostname == hostname {
			return &s.Topology.Nodes[i]
		}
	}

	return nil
}

type appsHostDoc struct {
	Name        string `json:"name"`
	Schedulable bool   `json:"schedulable"`
	Headnode    bool   `json:"headnode"`
}

type appsVMDoc struct {
	Name     string   `json:"name"`
	Host     string   `json:"host"`
	State    string   `json:"state"`
	Running  bool     `json:"running"`
	Networks []string `json:"networks"`
	Taps     []string `json:"taps"`
}

// appsExpDoc is the part of an experiment the tests read, both as phenix
// stores it and as it gives it to user apps.
type appsExpDoc struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec   appsSpecDoc `json:"spec"`
	Status struct {
		StartTime string            `json:"startTime"`
		Schedules map[string]string `json:"schedules"`
		VLANs     map[string]int    `json:"vlans"`
		Apps      map[string]any    `json:"apps"`
	} `json:"status"`
	Hosts []appsHostDoc `json:"hosts"`
	VMs   []appsVMDoc   `json:"vms"`
}

func (d appsExpDoc) node(hostname string) *appsNodeDoc {
	return d.Spec.node(hostname)
}

// appsStored returns the experiment config phenix stores.
func appsStored(t *testing.T, name string) appsExpDoc {
	t.Helper()

	out := appsPhenixStdout(t, "config", "get", "experiment/"+name, "--output", "json")

	var d appsExpDoc
	if err := json.Unmarshal(out, &d); err != nil {
		t.Fatalf("decoding experiment %s: %v\n%s", name, err, appsHead(out))
	}

	return d
}

func appsExists(name string) bool {
	_, err := phenixRun("config", "get", "experiment/"+name)

	return err == nil
}

func appsIsRunning(t *testing.T, name string) bool {
	t.Helper()

	return appsStored(t, name).Status.StartTime != ""
}

func appsAppStatus(d appsExpDoc, app string) map[string]any {
	st, _ := d.Status.Apps[app].(map[string]any)

	return st
}

func appsSpecOf(t *testing.T, doc map[string]any) appsSpecDoc {
	t.Helper()

	var spec appsSpecDoc

	data, err := json.Marshal(doc["spec"])
	if err == nil {
		err = json.Unmarshal(data, &spec)
	}

	if err != nil {
		t.Fatalf("decoding a spec: %v", err)
	}

	return spec
}

func appsVMNames(vms []appsVMDoc) []string {
	names := make([]string, 0, len(vms))
	for _, vm := range vms {
		names = append(names, vm.Name)
	}

	sort.Strings(names)

	return names
}

func appsVMStates(vms []appsVMDoc) map[string]string {
	states := make(map[string]string, len(vms))
	for _, vm := range vms {
		states[vm.Name] = vm.State
	}

	return states
}

// ---------------------------------------------------------------------------
// configs and experiments

type appsNIC struct {
	name, vlan, address string
}

// appsNode returns a Linux VM topology node with the given static /24
// interfaces (none is allowed).
func appsNode(hostname, image string, memory int, nics ...appsNIC) string {
	var b strings.Builder

	fmt.Fprintf(&b, `
  - type: VirtualMachine
    general:
      hostname: %s
    hardware:
      vcpus: 1
      memory: %d
      os_type: linux
      drives:
      - image: %s
    network:
      interfaces:`, hostname, memory, image)

	if len(nics) == 0 {
		b.WriteString(" []")
	}

	for _, n := range nics {
		fmt.Fprintf(&b, `
      - name: %s
        type: ethernet
        proto: static
        vlan: %s
        address: %s
        mask: 24`, n.name, n.vlan, n.address)
	}

	return b.String()
}

// appsHollow returns a hollow node with one interface.
func appsHollow(hostname, vlan, address string) string {
	return appsNode(hostname, ext4Image, appsHollowMemory, appsNIC{"eth0", vlan, address})
}

// appsScenario wraps app entries in a v2 Scenario config named {{name}}.
func appsScenario(apps ...string) string {
	return `apiVersion: phenix.sandia.gov/v2
kind: Scenario
metadata:
  name: {{name}}
  annotations:
    topology: {{name}}
spec:
  apps:` + strings.Join(apps, "")
}

type appsHost struct {
	hostname string
	metadata map[string]any
}

// appsApp renders one scenario app entry. Metadata is written as JSON, which
// YAML reads as a flow mapping.
func appsApp(name string, metadata map[string]any, hosts ...appsHost) string {
	var b strings.Builder

	fmt.Fprintf(&b, "\n  - name: %s", name)

	if metadata != nil {
		fmt.Fprintf(&b, "\n    metadata: %s", mustJSON(metadata))
	}

	if len(hosts) > 0 {
		b.WriteString("\n    hosts:")

		for _, h := range hosts {
			fmt.Fprintf(&b, "\n    - hostname: %s\n      metadata: %s", h.hostname, mustJSON(h.metadata))
		}
	}

	return b.String()
}

// appsConfigs creates the topology and scenario configs, removed when the
// test ends, and returns the `phenix experiment create` arguments for them.
func appsConfigs(t *testing.T, name, topo, scenario string) []string {
	t.Helper()

	// Registered first, so it runs after the experiment's teardown.
	t.Cleanup(func() { appsClearNamespace(t, name) })

	topo = strings.ReplaceAll(topo, "{{name}}", name)
	phenix(t, "config", "create", writeFile(t, name+"-topology.yaml", topo))

	t.Cleanup(func() { _, _ = phenixRun("config", "delete", "topology/"+name) })

	args := []string{"experiment", "create", name, "--topology", name}

	if scenario != "" {
		scenario = strings.ReplaceAll(scenario, "{{name}}", name)
		path := writeFile(t, name+"-scenario.yaml", scenario)

		// A scenario with a private key leaves the work directory, which the
		// workflow uploads with the logs, once phenix has read it, also when
		// that fails the test.
		if appsSecret.MatchString(scenario) {
			defer func() {
				if err := os.Remove(path); err != nil {
					t.Errorf("removing the scenario file with private keys: %v", err)
				}
			}()
		}

		phenix(t, "config", "create", path)

		t.Cleanup(func() { _, _ = phenixRun("config", "delete", "scenario/"+name) })

		args = append(args, "--scenario", name)
	}

	return args
}

// appsCreate is newExperiment that also returns what experiment create
// printed, which includes the configure stage's logs.
func appsCreate(t *testing.T, name, topo, scenario string, createArgs ...string) (*experiment, string) {
	t.Helper()

	out := phenix(t, append(appsConfigs(t, name, topo, scenario), createArgs...)...)

	e := &experiment{t: t, name: name}

	// Registered after the config cleanups, so it runs first. A leftover
	// namespace is checked before teardown's clearNamespace would clear it
	// without failing the test.
	t.Cleanup(func() {
		e.stopAndDelete()
		appsClearNamespace(t, name)
	})

	return e, out
}

// appsClearNamespace fails the test, and clears the namespace, when the
// experiment's minimega namespace outlived the experiment. phenix asks
// minimega for a user app's VMs only in the stages of a running experiment
// that has them (app/user.go PopulateRuntime), and a stop clears the
// namespace, so a leftover one is a leak; the tests check namespaces with
// mmNamespaces, which does not create one.
func appsClearNamespace(t *testing.T, name string) {
	t.Helper()

	resp, err := mmRun("", "namespace")
	if err != nil {
		t.Errorf("listing minimega namespaces to look for a leftover %s: %v", name, err)

		return
	}

	for _, row := range mmRows(resp) {
		if row["namespace"] != name {
			continue
		}

		var vms []string

		if resp, err := mmRun(name, "vm info"); err == nil {
			for _, vm := range mmRows(resp) {
				vms = append(vms, vm["name"]+" "+vm["state"])
			}
		}

		t.Errorf("minimega still has namespace %s, with the VMs %v, after the experiment was deleted; clearing it", name, vms)

		if _, err := mmRun("", "clear namespace "+name); err != nil {
			t.Errorf("clearing minimega namespace %s: %v", name, err)
		}
	}
}

// ---------------------------------------------------------------------------
// apps, phenix and host tools

// appsPhenixStdout runs the phenix CLI and returns only what it printed on
// stdout.
func appsPhenixStdout(t *testing.T, args ...string) []byte {
	t.Helper()

	out, err := phenixStdout(args...)
	if err != nil {
		t.Fatalf("phenix %s: %v", strings.Join(args, " "), err)
	}

	return []byte(out)
}

type appsResult struct {
	stdout, stderr []byte
	code           int
}

// appsExec runs an app directly with extra environment env.
func appsExec(t *testing.T, env []string, stdin []byte, command string, args ...string) appsResult {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	var stdout, stderr bytes.Buffer

	cmd := exec.CommandContext(ctx, command, args...) //nolint:gosec // test harness
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdin = bytes.NewReader(stdin)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	res := appsResult{}

	var exitErr *exec.ExitError

	if err := cmd.Run(); errors.As(err, &exitErr) {
		res.code = exitErr.ExitCode()
	} else if err != nil {
		t.Fatalf("running %s: %v", command, err)
	}

	res.stdout, res.stderr = stdout.Bytes(), stderr.Bytes()

	return res
}

// appsAppEnv is the environment phenix adds for a user app (app/user.go).
func appsAppEnv(expName string, dryrun bool) []string {
	return []string{
		"PHENIX_DIR=" + appsPhenixDir,
		"PHENIX_FILES_DIR=" + filepath.Join(appsPhenixDir, "images", expName, "files"),
		"PHENIX_LOG_LEVEL=DEBUG",
		"PHENIX_LOG_FILE=stderr",
		"PHENIX_DRYRUN=" + strconv.FormatBool(dryrun),
		"PHENIX_STORE_ENDPOINT=" + cfg.store,
		"PHENIX_HOSTNAME_SUFFIXES=" + appsHostnameSuffixes(),
	}
}

// appsHostnameSuffixes is the --hostname-suffixes phenix passes apps and
// SCORCH components: its default, or a PHENIX_HOSTNAME_SUFFIXES the phenix it
// runs inherits, which viper ignores when empty.
func appsHostnameSuffixes() string {
	if v := os.Getenv("PHENIX_HOSTNAME_SUFFIXES"); v != "" {
		return v
	}

	return "-minimega,-phenix"
}

// appsWithApps returns the app input base with spec.baseDir set to baseDir,
// spec.scenario listing apps, and an unknown top-level field.
func appsWithApps(t *testing.T, base []byte, baseDir string, apps []any) []byte {
	t.Helper()

	var doc map[string]any
	if err := json.Unmarshal(base, &doc); err != nil {
		t.Fatalf("phenix util app-json printed something that is not one JSON document: %v\n%s", err, appsHead(base))
	}

	spec, ok := doc["spec"].(map[string]any)
	if !ok {
		t.Fatalf("phenix util app-json printed no spec:\n%s", appsHead(base))
	}

	spec["baseDir"] = baseDir
	spec["scenario"] = map[string]any{"apps": apps}
	doc["x-e2e"] = map[string]any{"kept": "unknown fields survive"}

	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}

	return data
}

// appsWantJSONDoc checks that stdout is one JSON object and nothing else,
// and returns it.
func appsWantJSONDoc(t *testing.T, what string, stdout []byte) map[string]any {
	t.Helper()

	dec := json.NewDecoder(bytes.NewReader(stdout))

	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		t.Errorf("%s: stdout is not one JSON object: %v\n%s", what, err, appsHead(stdout))

		return nil
	}

	var extra json.RawMessage
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		t.Errorf("%s: stdout holds more than the experiment (%v): %s", what, err, appsHead(extra))
	}

	return doc
}

// appsWantJSONLogs checks that every stderr line is a JSON log entry with
// the level and msg keys phenix reads (util/plog/stream.go).
func appsWantJSONLogs(t *testing.T, what string, stderr []byte) {
	t.Helper()

	for i, line := range strings.Split(string(stderr), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Errorf("%s: stderr line %d is not a JSON log entry: %s", what, i+1, appsHead([]byte(line)))

			return
		}

		level, okLevel := entry["level"].(string)
		_, okMsg := entry["msg"].(string)

		if !okLevel || !okMsg || level == "" {
			t.Errorf("%s: stderr line %d lacks a level or msg: %s", what, i+1, appsHead([]byte(line)))

			return
		}
	}
}

// appsWantInjections checks that a pre-start stage added the injection
// src -> dst to node c1 and kept the node's earlier injections.
func appsWantInjections(t *testing.T, app string, spec appsSpecDoc, before *appsNodeDoc, src, dst string) {
	t.Helper()

	node := spec.node("c1")
	if node == nil {
		t.Errorf("%s pre-start dropped node c1", app)

		return
	}

	if !slices.Contains(node.Injections, appsInjection{Src: src, Dst: dst}) {
		t.Errorf("%s pre-start injections for c1 = %+v, want %s -> %s", app, node.Injections, src, dst)
	}

	for _, inj := range before.Injections {
		if !slices.Contains(node.Injections, inj) {
			t.Errorf("%s pre-start dropped the existing injection %+v from c1", app, inj)
		}
	}
}

// appsWantInject checks that a VM's block of the rendered script injects src
// as dst.
func appsWantInject(t *testing.T, block, vm, src, dst string) {
	t.Helper()

	if pair := fmt.Sprintf(`"%s":"%s"`, src, dst); !strings.Contains(block, pair) {
		t.Errorf("the rendered script does not inject %s into %s as %s:\n%s", src, vm, dst, block)
	}
}

// appsProtonukeEnv matches the environment file protonuke stages for args,
// with or without spaces around '=' (systemd accepts both).
func appsProtonukeEnv(args string) *regexp.Regexp {
	return regexp.MustCompile(`^PROTONUKE_ARGS\s*=\s*` + regexp.QuoteMeta(args) + `$`)
}

// appsWantWireguardConf checks a staged wg0.conf. Keys are compared but never
// printed.
func appsWantWireguardConf(t *testing.T, path, privateKey string, want map[string]string) {
	t.Helper()

	data, err := os.ReadFile(path) //nolint:gosec // test harness
	if err != nil {
		t.Errorf("wireguard staged no %s: %v", path, err)

		return
	}

	conf := make(map[string]string)
	section := ""

	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(line)

		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.Trim(line, "[]")

			continue
		}

		if key, value, ok := strings.Cut(line, "="); ok {
			conf[section+"."+strings.TrimSpace(key)] = strings.TrimSpace(value)
		}
	}

	if conf["Interface.PrivateKey"] != privateKey {
		t.Errorf("%s: [Interface] PrivateKey is not the scenario's private key", path)
	}

	for key, value := range want {
		if strings.HasSuffix(key, "Key") {
			if conf[key] != value {
				t.Errorf("%s: %s is not the scenario's key", path, key)
			}
		} else if conf[key] != value {
			t.Errorf("%s: %s = %q, want %q", path, key, conf[key], value)
		}
	}
}

// appsWGKey returns a fresh WireGuard key pair, base64 encoded as wg prints
// them.
func appsWGKey(t *testing.T) (string, string) {
	t.Helper()

	key, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	return base64.StdEncoding.EncodeToString(key.Bytes()), base64.StdEncoding.EncodeToString(key.PublicKey().Bytes())
}

func appsSHA256(t *testing.T, path string) string {
	t.Helper()

	data, err := os.ReadFile(path) //nolint:gosec // test harness
	if err != nil {
		t.Errorf("reading %s: %v", path, err)

		return ""
	}

	sum := sha256.Sum256(data)

	return hex.EncodeToString(sum[:])
}

// appsWantFailed checks that phenix reported a user app's failure.
func appsWantFailed(t *testing.T, out, app string) {
	t.Helper()

	if want := fmt.Sprintf("user app %s command phenix-app-%s failed", app, app); !strings.Contains(out, want) {
		t.Errorf("phenix does not report %q:\n%s", want, appsRedact(out))
	}
}

// appsWantLogged checks that phenix relayed a line the app logged.
func appsWantLogged(t *testing.T, out, msg string) {
	t.Helper()

	if !strings.Contains(out, msg) {
		t.Errorf("phenix did not log %q from the app:\n%s", msg, appsRedact(out))
	}
}

// appsListed returns the comma-separated names phenix prints after prefix.
func appsListed(out, prefix string) []string {
	for line := range strings.SplitSeq(out, "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), prefix)
		if !ok {
			continue
		}

		names := make([]string, 0, strings.Count(rest, ",")+1)
		for name := range strings.SplitSeq(rest, ",") {
			names = append(names, strings.TrimSpace(name))
		}

		return names
	}

	return nil
}

// appsVMBlock returns a VM's block of a rendered minimega script.
func appsVMBlock(script, vm string) string {
	_, block, found := strings.Cut(script, "## VM: "+vm+" ##")
	if !found {
		return ""
	}

	block, _, _ = strings.Cut(block, "## VM: ")

	return block
}

func appsHasLine(text, want string) bool {
	for line := range strings.SplitSeq(text, "\n") {
		if strings.TrimSpace(line) == want {
			return true
		}
	}

	return false
}

// appsHead returns the start of data for a failure message, with private
// keys withheld.
func appsHead(data []byte) string {
	const limit = 2000

	text := appsRedact(string(data))
	if len(text) > limit {
		return text[:limit] + "..."
	}

	return text
}

// appsSecret matches the WireGuard private keys the tests put in scenarios,
// up to the key itself: as JSON (also escaped, once or more, inside a log
// line), as YAML (quoted or not), in a wg0.conf and as Go prints a struct or
// a map.
var appsSecret = regexp.MustCompile(
	`(?i)(\\*"private_?key\\*"\s*:\s*\\*"|private_?key\s*[=:]\s*["']?)[A-Za-z0-9+/=]+`,
)

// appsRedact withholds the private keys in text, which the workflow uploads
// with the logs.
func appsRedact(text string) string {
	return appsSecret.ReplaceAllString(text, "${1}<withheld>")
}

// appsNormalize drops null, empty object and empty array values from a
// decoded JSON value, which phenix's encoders use interchangeably.
func appsNormalize(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))

		for k, val := range x {
			n := appsNormalize(val)

			switch nv := n.(type) {
			case nil:
				continue
			case map[string]any:
				if len(nv) == 0 {
					continue
				}
			case []any:
				if len(nv) == 0 {
					continue
				}
			}

			out[k] = n
		}

		return out
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = appsNormalize(val)
		}

		return out
	default:
		return v
	}
}

// appsDiff describes the first difference between two decoded JSON values,
// or returns "".
func appsDiff(path string, a, b any) string {
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok {
			return fmt.Sprintf("%s: %s vs %s", path, appsHead([]byte(mustJSON(a))), appsHead([]byte(mustJSON(b))))
		}

		keys := slices.Sorted(maps.Keys(av))
		for k := range bv {
			if _, ok := av[k]; !ok {
				keys = append(keys, k)
			}
		}

		for _, k := range keys {
			if d := appsDiff(path+"."+k, av[k], bv[k]); d != "" {
				return d
			}
		}

		return ""
	case []any:
		bv, ok := b.([]any)
		if !ok || len(av) != len(bv) {
			return fmt.Sprintf("%s: %s vs %s", path, appsHead([]byte(mustJSON(a))), appsHead([]byte(mustJSON(b))))
		}

		for i := range av {
			if d := appsDiff(fmt.Sprintf("%s[%d]", path, i), av[i], bv[i]); d != "" {
				return d
			}
		}

		return ""
	default:
		if reflect.DeepEqual(a, b) {
			return ""
		}

		if strings.HasSuffix(strings.ToLower(path), "private_key") {
			return path + ": the values differ (withheld)"
		}

		return fmt.Sprintf("%s: %v vs %v", path, a, b)
	}
}

// appsGuestFacts runs every check in one C2 command, since each round trip
// costs a heartbeat, and returns each check's trimmed stdout by name.
func appsGuestFacts(t *testing.T, ns, vm string, checks [][2]string) map[string]string {
	t.Helper()

	var script strings.Builder

	for i, c := range checks {
		// Each check runs in its own subshell; its stderr is dropped so only
		// its answer is compared.
		fmt.Fprintf(&script, "echo '@@apps %d@@'; ( %s ) 2>/dev/null; echo; ", i, c[1])
	}

	out := guestShell(t, ns, vm, script.String())
	facts := make(map[string]string, len(checks))

	for i, c := range checks {
		_, after, found := strings.Cut(out, fmt.Sprintf("@@apps %d@@", i))
		if !found {
			facts[c[0]] = "(no output)"

			continue
		}

		if next := strings.Index(after, "@@apps "); next >= 0 {
			after = after[:next]
		}

		facts[c[0]] = strings.TrimSpace(after)
	}

	return facts
}

// appsWaitForLog is a guest script that waits up to 90 s for the count
// command (a grep -c) to count at least one line, then prints the count.
func appsWaitForLog(count string) string {
	return "for i in $(seq 90); do [ \"$(" + count + ")\" -gt 0 ] && break; sleep 1; done; " + count
}

// appsOVSString unquotes a string ovs-vsctl prints.
func appsOVSString(s string) string {
	if u, err := strconv.Unquote(s); err == nil {
		return u
	}

	return s
}

// appsOVSFind returns the rows of an Open vSwitch table matching cond, as
// decoded OVSDB JSON values keyed by column.
func appsOVSFind(t *testing.T, table, cond string, columns ...string) []map[string]any {
	t.Helper()

	out := mustOVS(t, "--format=json", "--columns="+strings.Join(columns, ","), "find", table, cond)

	var res struct {
		Data     [][]any  `json:"data"`
		Headings []string `json:"headings"`
	}

	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("decoding ovs-vsctl find %s %s: %v\n%s", table, cond, err, out)
	}

	rows := make([]map[string]any, 0, len(res.Data))

	for _, data := range res.Data {
		row := make(map[string]any, len(res.Headings))

		for i, h := range res.Headings {
			if i < len(data) {
				row[h] = data[i]
			}
		}

		rows = append(rows, row)
	}

	return rows
}

// appsOVSAtoms flattens an OVSDB JSON atom or set into strings; UUIDs become
// their text.
func appsOVSAtoms(v any) []string {
	switch x := v.(type) {
	case string:
		return []string{x}
	case float64:
		return []string{strconv.FormatFloat(x, 'f', -1, 64)}
	case bool:
		return []string{strconv.FormatBool(x)}
	case []any:
		if len(x) != 2 {
			return nil
		}

		tag, _ := x[0].(string)

		switch tag {
		case "uuid", "named-uuid":
			s, _ := x[1].(string)

			return []string{s}
		case "set":
			var out []string

			elems, _ := x[1].([]any)
			for _, e := range elems {
				out = append(out, appsOVSAtoms(e)...)
			}

			return out
		}
	}

	return nil
}

// appsDestroyBridge removes, when the test ends, an Open vSwitch bridge
// minimega created for the test's experiment. Register it before the
// experiment, so it runs after the teardown.
func appsDestroyBridge(t *testing.T, bridge string) {
	t.Helper()

	t.Cleanup(func() {
		_, _ = mmRun("", "bridge destroy "+bridge)
		_, _ = hostCmd("ovs-vsctl", "--if-exists", "del-br", bridge)
	})
}

// appsLinks returns the names of the host's network interfaces.
func appsLinks(t *testing.T) map[string]bool {
	t.Helper()

	out, err := hostCmd("ip", "-o", "link", "show")
	if err != nil {
		t.Fatalf("ip link: %v: %s", err, out)
	}

	links := make(map[string]bool)

	for line := range strings.SplitSeq(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}

		name, _, _ := strings.Cut(strings.TrimSuffix(fields[1], ":"), "@")
		links[name] = true
	}

	return links
}

func appsNewLinks(before, after map[string]bool) []string {
	var added []string

	for name := range after {
		if !before[name] {
			added = append(added, name)
		}
	}

	sort.Strings(added)

	return added
}

func appsWaitNoNewLinks(t *testing.T, before map[string]bool) {
	t.Helper()

	eventually(t, 15*time.Second, 500*time.Millisecond, "no new host interfaces", func() (bool, string) {
		added := appsNewLinks(before, appsLinks(t))

		return len(added) == 0, fmt.Sprintf("new interfaces %v", added)
	})
}

func appsLinkExists(dev string) bool {
	_, err := hostCmd("ip", "-o", "link", "show", "dev", dev)

	return err == nil
}

func appsWaitLinkGone(t *testing.T, dev string) {
	t.Helper()

	// minimega deletes host taps from its reaper, about a second later.
	eventually(t, 15*time.Second, 500*time.Millisecond, "host interface "+dev+" to go", func() (bool, string) {
		return !appsLinkExists(dev), dev + " still exists"
	})
}

func appsHostHasAddr(t *testing.T, cidr string) bool {
	t.Helper()

	out, err := hostCmd("ip", "-4", "-o", "addr", "show")
	if err != nil {
		t.Fatalf("ip addr: %v: %s", err, out)
	}

	return strings.Contains(out, " "+cidr+" ")
}

func appsWantNoNamespace(t *testing.T, name string) {
	t.Helper()

	if ns, ok := mmNamespaces(t)[name]; ok {
		t.Errorf("minimega still has namespace %s: %v", name, ns)
	}
}

func appsWaitQEMU(t *testing.T, want int) {
	t.Helper()

	eventually(t, time.Minute, time.Second, "QEMU processes to exit", func() (bool, string) {
		n := qemuProcesses(t)

		return n <= want, fmt.Sprintf("%d running, %d before", n, want)
	})
}

// appsWantHostTap checks the one host tap in namespace exp on VLAN alias,
// and returns its name.
func appsWantHostTap(t *testing.T, exp, bridge, alias, cidr string) string {
	t.Helper()

	id := appsStored(t, exp).Status.VLANs[alias]
	vlan := fmt.Sprintf("%s (%d)", alias, id)
	all := mmTable(t, exp, "tap")

	var rows []map[string]string

	for _, row := range all {
		if row["vlan"] == vlan {
			rows = append(rows, row)
		}
	}

	if id == 0 || len(rows) != 1 {
		t.Fatalf("namespace %s has host taps %v, want one on VLAN %s", exp, all, vlan)
	}

	tap := rows[0]["tap"]

	if rows[0]["bridge"] != bridge {
		t.Errorf("minimega reports host tap %s on bridge %q, want %s", tap, rows[0]["bridge"], bridge)
	}

	if out, _ := hostCmd("ip", "-4", "-o", "addr", "show", "dev", tap); !strings.Contains(out, "inet "+cidr+" ") {
		t.Errorf("host tap %s does not have %s: %s", tap, cidr, out)
	}

	if br := mustOVS(t, "port-to-br", tap); br != bridge {
		t.Errorf("host tap %s is on Open vSwitch bridge %q, want %s", tap, br, bridge)
	}

	if tag := mustOVS(t, "get", "port", tap, "tag"); tag != strconv.Itoa(id) {
		t.Errorf("host tap %s has tag %s, want VLAN %s", tap, tag, vlan)
	}

	return tap
}

// ---------------------------------------------------------------------------
// mirror

// appsMirrorState is what the mirror app created for one monitor.
type appsMirrorState struct {
	tap    string // the host's mirror tap
	port   string // the tunnel port, which is also the mirror's name
	bridge string
	ip     string // the monitor's address
}

// appsWantMirrorIface checks the interface the mirror app's configure stage
// gives a monitor node.
func appsWantMirrorIface(t *testing.T, node *appsNodeDoc, hostname string) {
	t.Helper()

	if node == nil {
		t.Errorf("there is no node %s", hostname)

		return
	}

	want := appsIface{Name: "mirror0", VLAN: "mirror", Address: appsMirrorMonitorIP, Mask: 24, Proto: "static", Bridge: "phenix"}

	if !slices.Contains(node.Network.Interfaces, want) {
		t.Errorf("node %s interfaces = %+v, want %+v from the mirror app's configure stage", hostname, node.Network.Interfaces, want)
	}
}

// appsWantMirror checks the mirror app's status and its host tap and tunnel
// port for monitor, after post-start.
func appsWantMirror(t *testing.T, exp, monitor, kind string) appsMirrorState {
	t.Helper()

	stored := appsStored(t, exp)

	raw, ok := stored.Status.Apps["mirror"]
	if !ok {
		t.Fatal("status.apps has no mirror entry after start: the mirror app's post-start did not record its status; see the phenix log")
	}

	var status struct {
		TapName string `json:"tapName"`
		Subnet  string `json:"subnet"`
		Mirrors map[string]struct {
			MirrorName   string `json:"mirrorName"`
			MirrorBridge string `json:"mirrorBridge"`
			IP           string `json:"ip"`
		} `json:"mirrors"`
	}

	data, err := json.Marshal(raw)
	if err == nil {
		err = json.Unmarshal(data, &status)
	}

	if err != nil {
		t.Fatalf("decoding status.apps.mirror %v: %v", raw, err)
	}

	m := status.Mirrors[monitor]
	st := appsMirrorState{tap: status.TapName, port: m.MirrorName, bridge: m.MirrorBridge, ip: m.IP}

	if !regexp.MustCompile(`^[A-Za-z0-9]{8}-mirror$`).MatchString(st.tap) || status.Subnet != appsMirrorNet ||
		st.ip != appsMirrorMonitorIP || st.bridge != "phenix" || len(st.port) != 15 {
		t.Fatalf(
			"status.apps.mirror = %s, want tapName <8 characters>-mirror, subnet %s and mirrors.%s with a 15-character mirrorName, mirrorBridge phenix and ip %s",
			data,
			appsMirrorNet,
			monitor,
			appsMirrorMonitorIP,
		)
	}

	if out, _ := hostCmd("ip", "-4", "-o", "addr", "show", "dev", st.tap); !strings.Contains(out, "inet "+appsMirrorTapCIDR+" ") {
		t.Errorf("mirror tap %s does not have %s: %s", st.tap, appsMirrorTapCIDR, out)
	}

	if br := mustOVS(t, "port-to-br", st.tap); br != st.bridge {
		t.Errorf("mirror tap %s is on bridge %q, want %s", st.tap, br, st.bridge)
	}

	if tag, want := mustOVS(t, "get", "port", st.tap, "tag"), strconv.Itoa(stored.Status.VLANs["mirror"]); tag != want {
		t.Errorf("mirror tap %s has tag %s, want the mirror VLAN %s", st.tap, tag, want)
	}

	if typ := appsOVSString(mustOVS(t, "get", "interface", st.port, "type")); typ != kind {
		t.Errorf("tunnel port %s has type %q, want %s", st.port, typ, kind)
	}

	if remote := appsOVSString(mustOVS(t, "get", "interface", st.port, "options:remote_ip")); remote != st.ip {
		t.Errorf("tunnel port %s has remote_ip %q, want the monitor's %s", st.port, remote, st.ip)
	}

	if e := mustOVS(t, "get", "interface", st.port, "error"); e != "[]" && e != "" {
		t.Errorf("Open vSwitch could not set up tunnel port %s: %s", st.port, e)
	}

	return st
}

// appsWantMirrorPorts checks that the mirror selects exactly the given VMs'
// taps on VLAN LAN and outputs to its tunnel port.
func appsWantMirrorPorts(t *testing.T, exp string, st appsMirrorState, vms ...string) {
	t.Helper()

	info := vmInfo(t, exp)

	var want []string

	for _, vm := range vms {
		for _, tap := range splitList(info[vm]["tap"]) {
			want = append(want, mustOVS(t, "get", "port", tap, "_uuid"))
		}
	}

	rows := appsOVSFind(t, "mirror", `name="`+st.port+`"`, "select_dst_port", "select_vlan", "output_port")
	if len(rows) != 1 {
		t.Fatalf("Open vSwitch has %d mirrors named %s, want one", len(rows), st.port)
	}

	got := appsOVSAtoms(rows[0]["select_dst_port"])

	slices.Sort(got)
	slices.Sort(want)

	if !slices.Equal(got, want) {
		t.Errorf("mirror %s selects ports %v, want the taps of %v: %v", st.port, got, vms, want)
	}

	if vlans, lan := appsOVSAtoms(
		rows[0]["select_vlan"],
	), strconv.Itoa(
		appsStored(t, exp).Status.VLANs["LAN"],
	); !slices.Equal(
		vlans,
		[]string{lan},
	) {
		t.Errorf("mirror %s selects VLANs %v, want LAN (%s)", st.port, vlans, lan)
	}

	if out := appsOVSAtoms(rows[0]["output_port"]); !slices.Equal(out, []string{mustOVS(t, "get", "port", st.port, "_uuid")}) {
		t.Errorf("mirror %s outputs to %v, want its tunnel port", st.port, out)
	}
}

// appsRemoveMirrors removes, when t ends, the Open vSwitch mirrors and tunnel
// ports experiment exp's status says the mirror app created, in case the
// app's cleanup did not run. minimega does not own them (the app creates them
// with `shell ovs-vsctl`), so clearing the namespace leaves them behind; the
// mirror's host tap belongs to the namespace. Register it after the
// experiment, so it runs before the teardown, while the status is stored.
func appsRemoveMirrors(t *testing.T, exp string) {
	t.Helper()

	t.Cleanup(func() {
		out, err := phenixRun("config", "get", "experiment/"+exp, "--output", "json")
		if err != nil {
			return
		}

		var d appsExpDoc
		if json.Unmarshal([]byte(jsonPart(out)), &d) != nil {
			return
		}

		mirrors, _ := appsAppStatus(d, "mirror")["mirrors"].(map[string]any)

		for _, m := range mirrors {
			cfg, _ := m.(map[string]any)
			name, _ := cfg["mirrorName"].(string)
			bridge, _ := cfg["mirrorBridge"].(string)

			if name == "" || bridge == "" {
				continue
			}

			_, _ = hostCmd("ovs-vsctl", "--", "--id=@m", "get", "mirror", name, "--", "remove", "bridge", bridge, "mirrors", "@m")
			_, _ = hostCmd("ovs-vsctl", "--if-exists", "del-port", bridge, name)
		}
	})
}

// appsWantMirrorGone checks that the mirror app's cleanup removed the mirror,
// its tunnel port and its host tap.
func appsWantMirrorGone(t *testing.T, st appsMirrorState) {
	t.Helper()

	eventually(t, 15*time.Second, 500*time.Millisecond, "the mirror app's Open vSwitch objects to go", func() (bool, string) {
		if rows := appsOVSFind(t, "mirror", `name="`+st.port+`"`, "_uuid"); len(rows) > 0 {
			return false, "mirror " + st.port + " still exists"
		}

		ports, _ := hostCmd("ovs-vsctl", "list-ports", st.bridge)
		for p := range strings.FieldsSeq(ports) {
			if p == st.port || p == st.tap {
				return false, "port " + p + " is still on " + st.bridge
			}
		}

		if appsLinkExists(st.tap) {
			return false, "host interface " + st.tap + " still exists"
		}

		return true, ""
	})
}
