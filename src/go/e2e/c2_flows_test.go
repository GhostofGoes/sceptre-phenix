//go:build e2e

package e2e

// C2 flows (F4): how minimega's cc clients register and how phenix detects
// them by hostname or UUID, topology commands and startup scripts delivered
// through the rendered cc script, phenix's ExecC2Command options as the
// subject under test, cc mounts over 9p through the CLI and REST, port
// forwards, the forward reaper and startup autotunnels, and what a guest
// reboot and a power cycle do to C2. Real guests (cfg.guest, built from
// sceptre-phenix-images with miniccc as a systemd service) provide the C2
// agent; hollow VMs stand in for VMs that never register.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"phenix/util/common"
	phenixmm "phenix/util/mm"
)

const (
	c2fFlowsExp = "e2e-c2flows"
	c2fCLIExp   = "e2e-c2cli"

	// c2fMountRoot is phenix's default VM mount directory (common.MountDir).
	c2fMountRoot = "/phenix/mounts"

	// c2fMountUIAddr is where TestC2Flows runs a second phenix ui with the
	// vm-mount feature, which the harness's shared UI does not enable.
	c2fMountUIAddr = "127.0.0.1:3181"

	// c2fGuestPort is the TCP port the tests serve from inside guests.
	c2fGuestPort = 8080

	// phenix picks forward host ports in [50000, 65000) (web/forward/forward.go).
	c2fForwardPortMin = 50000
	c2fForwardPortMax = 65000

	// c2fBootTimeout bounds the four real guests phenix starts together once
	// c2a has registered, until each registers over C2; in CI five booting
	// together took 4.3 to 4.6 minutes.
	c2fBootTimeout = 2 * guestBootTimeout

	// c2fAfterC2a is the node-level delay of c2b, c2p, c2d and c2v: phenix
	// starts them once c2a's cc client reports hostname c2a.
	c2fAfterC2a = `
    delay:
      c2:
      - hostname: c2a
        useUUID: false`

	// c2fCmdTimeout bounds one command round trip through phenix's util/mm;
	// responses ride on miniccc's 5 s heartbeat.
	c2fCmdTimeout = 90 * time.Second

	// c2fNotActiveTimeout is the C2 timeout given to probes expected to find
	// no client.
	c2fNotActiveTimeout = 8 * time.Second

	// c2fReaperWait covers two ticks of phenix's 10 s port forward reaper
	// (web/forward/forward.go forwardTickerInterval).
	c2fReaperWait = 25 * time.Second

	// c2fExpireWait bounds how long minimega keeps the cc client of a paused
	// guest: ron drops a client that has not checked in for 30 s, checked
	// every 30 s (minimega internal/ron/ron.go CLIENT_EXPIRED, REAPER_RATE).
	c2fExpireWait = 75 * time.Second

	// c2fReconnectWait bounds how long a resumed guest's miniccc takes to
	// notice its lost connection (2.5 heartbeats, cmd/miniccc/mux.go),
	// connect again and answer the commands minimega gives it again.
	c2fReconnectWait = 2 * time.Minute

	// c2fAutotunnelWait is how long after a guest registers its startup
	// autotunnel must exist.
	c2fAutotunnelWait = time.Minute

	// c2fPowerCycleWatch is how long the power-cycle characterization waits
	// for C2 to return after a VM is started again from QUIT; a fresh boot
	// registers in about 95 s.
	c2fPowerCycleWatch = 150 * time.Second

	// c2fTopologyCommand is c2a's topology `commands:` entry.
	c2fTopologyCommand = `exec sh -c 'echo cmd-ok > /tmp/e2e-cmd; cat /tmp/e2e-cmd'`

	c2fForwardOwnerREST = "global-admin" // the user phenix ui assigns without authentication
	c2fForwardOwnerBot  = "bot"          // the owner of startup autotunnels (app/startup.go)
	c2fLoopback         = "127.0.0.1"
)

// c2fEnv is what the subtests of one C2 test share.
type c2fEnv struct {
	exp         *experiment
	uiLog       int64               // size of the phenix ui log before the experiment started
	uuids       map[string]string   // VM name -> lowercase minimega UUID
	hist        map[string][]string // VM name -> hostnames its cc client reported, in order
	startOut    string              // output of phenix experiment start, for a CLI-started experiment
	forwardAt   time.Time           // when the c2b port forward was created
	forwardPort int                 // the host port minimega listens on for it
}

// c2fListener is one entry of GET /experiments/{exp}/vms/{name}/forwards.
type c2fListener struct {
	Exp     string `json:"exp"`
	VM      string `json:"vm"`
	SrcPort int    `json:"srcPort"`
	DstHost string `json:"dstHost"`
	DstPort int    `json:"dstPort"`
	Owner   string `json:"owner"`
}

// c2fRESTVM is the part of a phenix REST VM the tests read.
type c2fRESTVM struct {
	Name     string `json:"name"`
	State    string `json:"state"`
	Running  bool   `json:"running"`
	CCActive bool   `json:"ccActive"`
}

// TestC2Flows starts one experiment through REST with five real guests and
// a hollow VM and walks every C2 path phenix offers on it:
//
//   - c2a: startup scripts injected into the disk, a topology command and a
//     startup autotunnel;
//   - c2b: a plain guest serving TCP for forwards and test-conn;
//   - c2p: default apps off, so it keeps the hostname phenix images bake in;
//   - c2d: inject_partition 0, so its startup scripts arrive over C2;
//   - c2v: the phenix/startup-via-cc annotation, same delivery;
//   - h1: a hollow VM with no C2 agent, and a startup autotunnel that
//     phenix ui gives up on.
//
// c2b, c2p, c2d and c2v have a `c2` delay on c2a, so c2a boots alone.
func TestC2Flows(t *testing.T) {
	requireImage(t, cfg.guest)
	hollowImages(t)
	c2fUseHarnessMinimega()

	// Registered before the experiment, so it runs after its teardown.
	c2fCleanupHost(t, c2fFlowsExp)

	exp := newExperiment(t, c2fFlowsExp, c2fFlowsTopology(), "")

	// A test that ends before c2a registers leaves the C2 delays pending in
	// phenix ui, which only a REST stop ends before it clears the namespace.
	delayStopRESTOnCleanup(t, exp)

	// Runs first. The REST stop above drops phenix ui's forwards itself;
	// should it fail, the teardown stops the experiment through the CLI,
	// which drops forwards in its own process only, so phenix ui must not
	// hold one by then.
	t.Cleanup(func() { c2fCloseTunnels(t, exp.name, "c2a", "c2b") })

	start := time.Now()
	uiLog := fileSize(c2fUILog())

	apiOK(t, http.MethodPost, "/experiments/"+exp.name+"/start", nil, nil)
	exp.waitRunning(time.Minute, "c2a", "h1")

	// phenix gives up on c2a's startup autotunnel (C2-07a) when c2a's cc
	// client has not registered DefaultC2Timeout (5 min, util/mm/option.go)
	// after PostStart, and five guests booting together on the runner's 4
	// vCPUs took about 4.5 min. So the other guests wait in BUILDING for c2a
	// (c2fAfterC2a): phenix starts them at its first C2 check after c2a
	// registered, and runs PostStart, and with it the autotunnel's wait, only
	// after that (handleDelayedVMs, api/experiment/experiment.go). Nothing
	// they are checked for has a deadline.
	later := []string{"c2b", "c2p", "c2d", "c2v"}

	env := &c2fEnv{exp: exp, uiLog: uiLog}

	if !t.Run("guests register with C2", func(t *testing.T) {
		_, first := c2fWaitClients(t, exp.name, map[string]string{"c2a": "c2a"}, guestBootTimeout)
		registered := time.Now()

		// phenix checks C2 delays every 5 s (c2CheckInterval).
		(&experiment{t: t, name: exp.name}).waitRunning(time.Minute, later...)

		t.Logf("c2a registered %v after start; phenix started %v %v later",
			registered.Sub(start).Round(time.Second), later, time.Since(registered).Round(time.Second))

		env.uuids, env.hist = c2fWaitClients(t, exp.name, c2fFlowsClients(), c2fBootTimeout)

		hist := first["c2a"]

		for _, host := range env.hist["c2a"] {
			if hist[len(hist)-1] != host {
				hist = append(hist, host)
			}
		}

		env.hist["c2a"] = hist

		t.Logf("all five guests registered %v after start; hostnames seen: %v",
			time.Since(start).Round(time.Second), env.hist)
	}) {
		t.FailNow()
	}

	t.Run("C2-01 cc clients match minimega's VMs", func(t *testing.T) { c2fCheckClients(t, env) })
	t.Run("C2-01 phenix detects clients by hostname and by UUID", func(t *testing.T) { c2fCheckDetection(t, env) })
	t.Run("C2-01 VM names fold case", func(t *testing.T) { c2fCheckCaseFolding(t, env) })
	t.Run("C2-02 topology commands run through the cc script", func(t *testing.T) { c2fCheckTopologyCommands(t, env) })
	t.Run("C2-03 startup scripts over C2 with inject_partition 0", func(t *testing.T) {
		c2fCheckStartupDelivery(t, env, "c2d", "10.40.0.4")
	})
	t.Run("C2-03 startup scripts over C2 with phenix/startup-via-cc", func(t *testing.T) {
		c2fCheckStartupDelivery(t, env, "c2v", "10.40.0.5")
	})
	t.Run("C2-03 startup scripts are injected only where configured", func(t *testing.T) {
		c2fCheckStartupInjections(t, env)
	})
	t.Run("C2-03 a guest miniccc reconnects after a long pause keeps its addresses", func(t *testing.T) {
		c2fCheckReconnect(t, env, "c2d", "10.40.0.4")
	})

	if !t.Run("guests serve TCP", func(t *testing.T) {
		c2fStartServer(t, exp.name, "c2a")
		c2fStartServer(t, exp.name, "c2b")
	}) {
		t.FailNow()
	}

	t.Run("C2-04 ExecC2Command waits and leaves no cc filter", func(t *testing.T) { c2fCheckExecWait(t, env) })
	t.Run("C2-04 ExecC2Command responses, exit codes and test-conn", func(t *testing.T) { c2fCheckExecBatch(t, env) })
	t.Run("C2-04 ExecC2Command sends a file before the command", func(t *testing.T) { c2fCheckSendFile(t, env) })
	t.Run("C2-04 ExecC2Command gives up on a VM without C2", func(t *testing.T) { c2fCheckExecInactive(t, env) })
	t.Run("C2-06 REST port forward and its data path", func(t *testing.T) { c2fCheckForward(t, env) })
	t.Run("C2-06f REST port forward without a host", func(t *testing.T) { c2fCheckForwardNoHost(t, env) })
	t.Run("C2-07a startup autotunnel of a REST-started experiment", func(t *testing.T) {
		c2fCheckAutotunnel(t, env, "c2a")
	})
	t.Run("C2-05 cc mount through the CLI", func(t *testing.T) { c2fCheckMountCLI(t, env) })
	t.Run("C2-05c CLI mount to a custom host path", func(t *testing.T) { c2fCheckMountCustomPath(t, env) })
	t.Run("C2-05 cc mount through REST", func(t *testing.T) { c2fCheckMountREST(t, env) })
	t.Run("C2-06e port forward survives the reaper", func(t *testing.T) { c2fCheckForwardSurvives(t, env) })
	t.Run("C2-06h REST port forward delete", func(t *testing.T) { c2fCheckForwardDelete(t, env) })
	t.Run("C2-06 the reaper drops a forward whose tunnel is gone", func(t *testing.T) { c2fCheckReaperDrops(t, env, "c2a") })
	t.Run("C2-07c startup autotunnel waits for C2, then gives up", func(t *testing.T) {
		c2fCheckAutotunnelGivesUp(t, env, "h1")
	})
	t.Run("C2-05f stop cleans up mounts, forwards and the namespace", func(t *testing.T) { c2fCheckStop(t, env) })
}

// TestC2CLIStartAndReboot covers what only a CLI-started experiment and a
// power cycle show: that a CLI start warns and creates no startup autotunnel,
// C2 across a guest reboot, and C2 after a VM goes QUIT and is started again.
func TestC2CLIStartAndReboot(t *testing.T) {
	requireImage(t, cfg.guest)

	c2fCleanupHost(t, c2fCLIExp)

	exp := newExperiment(t, c2fCLIExp, topology(
		c2fGuest("c2a", "10.41.0.1", `
    annotations:
      phenix/startup-autotunnel: ["8080"]`, ""),
		c2fGuest("c2b", "10.41.0.2", "", ""),
	), "")

	// Should a tunnel to c2a exist after all, close it: the CLI stop in the
	// teardown does not run the stop hook of the phenix ui holding its forward.
	t.Cleanup(func() { c2fCloseTunnels(t, exp.name, "c2a") })

	start := time.Now()

	startOut := exp.start()
	exp.waitRunning(time.Minute, "c2a", "c2b")

	env := &c2fEnv{exp: exp, startOut: startOut}

	if !t.Run("guests register with C2", func(t *testing.T) {
		env.uuids, env.hist = c2fWaitClients(t, exp.name, map[string]string{"c2a": "c2a", "c2b": "c2b"}, guestBootTimeout)

		t.Logf("both guests registered %v after start", time.Since(start).Round(time.Second))
	}) {
		t.FailNow()
	}

	var bootID string

	t.Run("C2-08a vm restart resets the guest in place", func(t *testing.T) { bootID = c2fCheckReset(t, env, "c2b") })
	t.Run("C2-07b startup autotunnel of a CLI-started experiment", func(t *testing.T) { c2fCheckCLIAutotunnel(t, env) })
	t.Run("C2-08a C2 survives a guest reboot", func(t *testing.T) { c2fCheckRebootC2(t, env, "c2b", bootID) })
	t.Run("C2-08b C2 after shutdown and start", func(t *testing.T) {
		c2fCharacterizePowerCycle(t, env, "c2b")
	})
}

// TestC2AutotunnelStopBeforeRegistration stops, through the REST API, an
// experiment whose startup autotunnel still waits for its VM's C2 client; a
// hollow VM never registers. The stop must end the wait, which asks minimega
// about the VM in the experiment's namespace, so minimega must not get the
// namespace back from it.
func TestC2AutotunnelStopBeforeRegistration(t *testing.T) {
	hollowImages(t)

	const name = "e2e-c2-atstop"

	exp := newExperiment(t, name, topology(hollowNode("h1", "ATS", "10.44.0.1", `
    annotations:
      phenix/startup-autotunnel: ["8080"]`)), "")
	delayStopRESTOnCleanup(t, exp)

	uiLog := delayLogOffset(t, delayUILog())

	apiOK(t, http.MethodPost, "/experiments/"+name+"/start", nil, nil)
	exp.waitRunning(time.Minute, "h1")

	eventually(t, time.Minute, time.Second, "the startup app's post-start", func() (bool, string) {
		log := delayLogSince(t, delayUILog(), uiLog)

		return strings.Contains(log, "'startup' default app (post-start)"), "not logged yet"
	})

	// The wait asks minimega every 2 s or so; let it ask a few times.
	time.Sleep(5 * time.Second)

	apiOK(t, http.MethodPost, "/experiments/"+name+"/stop", nil, nil)

	eventually(t, 30*time.Second, time.Second, "the autotunnel wait to end", func() (bool, string) {
		log := delayLogSince(t, delayUILog(), uiLog)

		return strings.Contains(log, "startup autotunnel not created: the experiment stopped before the C2 client registered"),
			"not logged yet"
	})

	// The wait would otherwise go on for up to 5 minutes (the C2 timeout),
	// asking minimega about the VM and so creating its namespace again.
	for end := time.Now().Add(30 * time.Second); time.Now().Before(end); time.Sleep(time.Second) {
		if ns, ok := mmNamespaces(t)[name]; ok {
			t.Fatalf("minimega has namespace %s again after the stop: %v", name, ns)
		}
	}
}

// ---------------------------------------------------------------------------
// topology

// c2fGuest returns a real-guest topology node on VLAN LAN. nodeExtra is
// inserted at node level (indent 4) and driveExtra under the first drive
// (indent 8); both start with a newline.
func c2fGuest(hostname, address, nodeExtra, driveExtra string) string {
	return fmt.Sprintf(`
  - type: VirtualMachine
    general:
      hostname: %s%s
    hardware:
      vcpus: 1
      memory: 1024
      os_type: linux
      drives:
      - image: %s%s
    network:
      interfaces:
      - name: eth0
        type: ethernet
        proto: static
        vlan: LAN
        address: %s
        mask: 24`, hostname, nodeExtra, cfg.guest, driveExtra, address)
}

func c2fFlowsTopology() string {
	return topology(
		c2fGuest("c2a", "10.40.0.1", `
    annotations:
      phenix/startup-autotunnel: ["8080"]
    commands:
    - "`+c2fTopologyCommand+`"`, ""),
		c2fGuest("c2b", "10.40.0.2", c2fAfterC2a, ""),
		c2fGuest("c2p", "10.40.0.3", `
    annotations:
      phenix/default-apps: false`+c2fAfterC2a, ""),
		c2fGuest("c2d", "10.40.0.4", c2fAfterC2a, `
        inject_partition: 0`),
		c2fGuest("c2v", "10.40.0.5", `
    annotations:
      phenix/startup-via-cc: true`+c2fAfterC2a, ""),
		hollowNode("h1", "LAN", "10.40.0.10", `
    annotations:
      phenix/startup-autotunnel: ["8080"]`),
	)
}

// c2fUseHarnessMinimega points phenix's own minimega client, which these
// tests call as the subject under test, at the harness's daemon.
func c2fUseHarnessMinimega() {
	common.MinimegaBase = cfg.mmBase //nolint:reassign // the harness's minimega -base
}

// ---------------------------------------------------------------------------
// registration and detection (C2-01)

// c2fFlowsClients maps each real guest of TestC2Flows to the hostname its cc
// client reports once it has booted. c2d and c2v get theirs from startup
// scripts that run over C2 only after they register, so any hostname ("")
// will do; C2-03 waits for their own.
func c2fFlowsClients() map[string]string {
	return map[string]string{"c2a": "c2a", "c2b": "c2b", "c2p": "phenix", "c2d": "", "c2v": ""}
}

// c2fWaitClients waits until each VM in want has a cc client reporting the
// given hostname, or any hostname for an empty one, matching clients to VMs
// by UUID, and records the hostnames each client reported on the way.
func c2fWaitClients(
	t *testing.T,
	ns string,
	want map[string]string,
	timeout time.Duration,
) (map[string]string, map[string][]string) {
	t.Helper()

	info := vmInfo(t, ns)

	uuids := make(map[string]string, len(want))
	byUUID := make(map[string]string, len(want))

	for vm := range want {
		uuid := strings.ToLower(info[vm]["uuid"])
		if uuid == "" {
			t.Fatalf("minimega reports no UUID for %s: %v", vm, info[vm])
		}

		uuids[vm] = uuid
		byUUID[uuid] = vm
	}

	hist := make(map[string][]string, len(want))

	eventually(t, timeout, time.Second, fmt.Sprintf("cc clients reporting %v", want), func() (bool, string) {
		now := c2fClientHostnames(t, ns)

		for uuid, host := range now {
			vm, ok := byUUID[uuid]
			if !ok {
				continue
			}

			if h := hist[vm]; len(h) == 0 || h[len(h)-1] != host {
				hist[vm] = append(h, host)
			}
		}

		var missing []string

		for vm, host := range want {
			got, ok := now[uuids[vm]]

			switch {
			case !ok:
				missing = append(missing, vm+" has no cc client")
			case host != "" && got != host:
				missing = append(missing, fmt.Sprintf("%s reports %q, want %q", vm, got, host))
			}
		}

		sort.Strings(missing)

		return len(missing) == 0, strings.Join(missing, "; ")
	})

	return uuids, hist
}

// c2fClientHostnames maps the lowercase UUID of each cc client in ns to the
// hostname it reports.
func c2fClientHostnames(t *testing.T, ns string) map[string]string {
	t.Helper()

	out := make(map[string]string)

	for _, row := range mmTable(t, ns, "cc clients") {
		out[strings.ToLower(row["uuid"])] = row["hostname"]
	}

	return out
}

func c2fCheckClients(t *testing.T, env *c2fEnv) {
	t.Helper()

	var (
		ns      = env.exp.name
		clients = c2fClientHostnames(t, ns)
		want    = c2fFlowsClients()
	)

	for vm, host := range want {
		got, ok := clients[env.uuids[vm]]

		switch {
		case !ok:
			t.Errorf("cc clients has no client with %s's UUID %s: %v", vm, env.uuids[vm], clients)
		case host != "" && got != host:
			t.Errorf("cc client of %s reports hostname %q, want %q", vm, got, host)
		}
	}

	if len(clients) != len(want) {
		t.Errorf("cc clients lists %d clients, want %d (one per real guest): %v", len(clients), len(want), clients)
	}

	if _, ok := clients[strings.ToLower(vmInfo(t, ns)["h1"]["uuid"])]; ok {
		t.Error("hollow VM h1, which runs no miniccc, has a cc client")
	}

	var list struct {
		VMs []c2fRESTVM `json:"vms"`
	}

	apiOK(t, http.MethodGet, "/experiments/"+ns+"/vms", nil, &list)

	active := make(map[string]bool)

	for _, vm := range list.VMs {
		active[vm.Name] = vm.CCActive
	}

	for vm := range want {
		if !active[vm] {
			t.Errorf("REST reports ccActive false for %s, which has a cc client", vm)
		}
	}

	if a, ok := active["h1"]; !ok || a {
		t.Errorf("REST ccActive for hollow h1 = %v (listed %v), want false", a, ok)
	}
}

// c2fCheckDetection runs phenix's C2 readiness check (the subject under test)
// in both modes. Hostname mode looks for a client reporting the VM's name, so
// c2p, which reports "phenix", is not ready; UUID mode finds it.
func c2fCheckDetection(t *testing.T, env *c2fEnv) {
	t.Helper()

	type probe struct {
		name   string
		opts   []phenixmm.C2Option
		active bool
	}

	probes := []probe{
		{"c2a by hostname", []phenixmm.C2Option{phenixmm.C2VM("c2a")}, true},
		{"c2p, reporting hostname phenix, by hostname", []phenixmm.C2Option{phenixmm.C2VM("c2p")}, false},
		{"h1, without miniccc, by hostname", []phenixmm.C2Option{phenixmm.C2VM("h1")}, false},
		{"c2p by UUID", []phenixmm.C2Option{phenixmm.C2VM("c2p"), phenixmm.C2IDClientsByUUID()}, true},
		{"h1 by UUID", []phenixmm.C2Option{phenixmm.C2VM("h1"), phenixmm.C2IDClientsByUUID()}, false},
		{"C2A, mis-cased, by hostname", []phenixmm.C2Option{phenixmm.C2VM("C2A")}, true},
	}

	type result struct {
		err  error
		took time.Duration
	}

	var (
		results = make([]result, len(probes))
		wg      sync.WaitGroup
	)

	for i, p := range probes {
		wg.Add(1)

		go func() {
			defer wg.Done()

			opts := append([]phenixmm.C2Option{
				phenixmm.C2NS(env.exp.name),
				phenixmm.C2Timeout(c2fNotActiveTimeout),
			}, p.opts...)

			began := time.Now()
			err := phenixmm.IsC2ClientActive(opts...)
			results[i] = result{err: err, took: time.Since(began)}
		}()
	}

	wg.Wait()

	for i, p := range probes {
		r := results[i]

		t.Logf("%s: %v after %v", p.name, r.err, r.took.Round(time.Millisecond))

		switch {
		case p.active && r.err != nil:
			t.Errorf("%s: IsC2ClientActive = %v, want the client found", p.name, r.err)
		case !p.active && !errors.Is(r.err, phenixmm.ErrC2ClientNotActive):
			t.Errorf("%s: IsC2ClientActive = %v, want ErrC2ClientNotActive", p.name, r.err)
		case !p.active && r.took < c2fNotActiveTimeout:
			t.Errorf("%s: gave up after %v, before its %v timeout", p.name, r.took, c2fNotActiveTimeout)
		}
	}
}

// c2fCheckCaseFolding sends a command to "C2A" through phenix and checks it
// ran on c2a and nowhere else.
func c2fCheckCaseFolding(t *testing.T, env *c2fEnv) {
	t.Helper()

	ns := env.exp.name

	id := c2fExecID(t, ns, "C2A", phenixmm.C2Command("hostname"), phenixmm.C2Wait())

	if got := strings.TrimSpace(c2fResponseFile(t, ns, id, env.uuids["c2a"], "stdout")); got != "c2a" {
		t.Errorf("command %s sent to C2A printed %q, want c2a", id, got)
	}

	if got := c2fResponseClients(t, ns, id); !slices.Equal(got, []string{env.uuids["c2a"]}) {
		t.Errorf("command %s sent to C2A was answered by %v, want only c2a (%s)", id, got, env.uuids["c2a"])
	}

	if got := c2fTyped(t, ns, "C2A", id, phenixmm.C2ResponseTypeStdout()); got != "c2a" {
		t.Errorf("typed stdout of C2A's response = %q, want c2a", got)
	}
}

// ---------------------------------------------------------------------------
// topology commands and startup delivery (C2-02, C2-03)

func c2fCheckTopologyCommands(t *testing.T, env *c2fEnv) {
	t.Helper()

	var (
		ns       = env.exp.name
		uuid     = env.uuids["c2a"]
		ccScript = c2fReadScript(t, ns, ns+"-cc.mm")
	)

	if got, want := c2fBlock(ccScript, "c2a"), []string{"cc filter name=c2a", "cc " + c2fTopologyCommand}; !slices.Equal(got, want) {
		t.Errorf("cc script block for c2a = %q, want %q\n%s", got, want, ccScript)
	}

	for _, vm := range []string{"c2b", "c2p", "h1"} {
		if strings.Contains(ccScript, "## VM: "+vm+" ##") {
			t.Errorf("cc script has a block for %s, which has no commands:\n%s", vm, ccScript)
		}
	}

	if !strings.HasSuffix(strings.TrimSpace(ccScript), "clear cc filter") {
		t.Errorf("cc script does not end by clearing the cc filter:\n%s", ccScript)
	}

	var row map[string]string

	eventually(t, 30*time.Second, time.Second, "c2a's topology command to be answered", func() (bool, string) {
		row = c2fCommandRow(t, ns, func(r map[string]string) bool { return strings.Contains(r["command"], "echo cmd-ok") })
		if row == nil {
			return false, "not listed in cc commands"
		}

		return row["responses"] != "0", "responses " + row["responses"]
	})

	// minimega re-sends every command to a client that reconnects, so a
	// second answer from c2a is not phenix's doing.
	if row["responses"] != "1" {
		t.Logf("c2a answered its topology command %s times (minimega re-delivers on reconnect)", row["responses"])
	}

	if !strings.Contains(row["filter"], "name=c2a") {
		t.Errorf("topology command filter = %q, want name=c2a", row["filter"])
	}

	id := row["id"]

	if got := c2fResponseFile(t, ns, id, uuid, "stdout"); got != "cmd-ok\n" {
		t.Errorf("topology command stdout = %q, want %q", got, "cmd-ok\n")
	}

	if got := c2fResponseFile(t, ns, id, uuid, "exitcode"); got != "0" {
		t.Errorf("topology command exit code = %q, want 0", got)
	}

	if got := c2fResponseClients(t, ns, id); !slices.Equal(got, []string{uuid}) {
		t.Errorf("topology command was answered by %v, want only c2a (%s)", got, uuid)
	}

	if got := mmText(t, ns, "cc response "+id+" raw"); got != "cmd-ok\n\n" {
		t.Errorf("cc response %s raw = %q, want %q", id, got, "cmd-ok\n\n")
	}
}

// c2fStartupKinds are the startup scripts phenix generates for a Linux node
// without a startup scenario app.
func c2fStartupKinds() []string {
	return []string{"hostname", "timezone", "interfaces"}
}

// c2fReplayAddress is an address outside the experiment's networks that a
// rerun of the interfaces script would flush.
const c2fReplayAddress = "192.0.2.77/32"

// c2fInterfacesRan is where the run-once guard of a Linux interfaces script
// delivered over C2 records the boot it ran in (app/startup.go
// startupC2RanDir).
const c2fInterfacesRan = "/run/phenix/startup-c2/interfaces.boot"

// c2fGuardReport is a guest shell snippet that prints marker=boot when the
// interfaces script's run-once guard recorded the guest's current boot.
const c2fGuardReport = `[ "$(cat ` + c2fInterfacesRan + `)" = "$(cat /proc/sys/kernel/random/boot_id)" ] && echo marker=boot`

func c2fCheckStartupDelivery(t *testing.T, env *c2fEnv, vm, address string) {
	t.Helper()

	var (
		ns   = env.exp.name
		uuid = env.uuids[vm]
	)

	// The disk keeps its snapshot but gets no startup scripts injected.
	block := c2fBlock(c2fReadScript(t, ns, ns+".mm"), vm)

	if !slices.ContainsFunc(block, func(l string) bool { return strings.HasPrefix(l, "disk snapshot "+cfg.guest+" ") }) {
		t.Errorf("minimega script block for %s has no disk snapshot of %s: %q", vm, cfg.guest, block)
	}

	for _, l := range block {
		if strings.HasPrefix(l, "disk inject") && strings.Contains(l, "/etc/phenix/startup/") {
			t.Errorf("minimega script still injects startup scripts into %s: %s", vm, l)
		}
	}

	// The cc script sends each script and runs it with exec, which minimega
	// gives each client when it connects.
	kinds := c2fStartupKinds()
	want := make([]string, 0, 1+2*len(kinds))
	want = append(want, "cc filter name="+vm)

	for _, kind := range kinds {
		want = append(want,
			fmt.Sprintf("cc send %s/%s-%s.sh", ns, vm, kind),
			fmt.Sprintf("cc exec bash /tmp/miniccc/files/%s/%s-%s.sh", ns, vm, kind),
		)
	}

	if got := c2fBlock(c2fReadScript(t, ns, ns+"-cc.mm"), vm); !slices.Equal(got, want) {
		t.Errorf("cc script block for %s =\n%q\nwant\n%q", vm, got, want)
	}

	// The scripts are staged under minimega's file path, readable by root only.
	for _, kind := range c2fStartupKinds() {
		path := filepath.Join(cfg.imagesDir, ns, fmt.Sprintf("%s-%s.sh", vm, kind))

		st, err := os.Stat(path)
		if err != nil {
			t.Errorf("staged startup script: %v", err)

			continue
		}

		if st.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %v, want 0600", path, st.Mode().Perm())
		}
	}

	hostScript, err := os.ReadFile(filepath.Join(cfg.imagesDir, ns, vm+"-hostname.sh"))
	if err == nil && !strings.Contains(string(hostScript), "hostnamectl set-hostname "+vm) {
		t.Errorf("staged hostname script for %s does not set its hostname:\n%s", vm, hostScript)
	}

	// The guest first reports the hostname baked into phenix images, then its
	// own once the delivered hostname script ran. That script is the first
	// command the guest gets when it registers, so one command round trip
	// bounds the wait.
	c2fWaitHostname(t, env, vm, c2fCmdTimeout)

	if h := env.hist[vm]; len(h) < 2 || h[0] != "phenix" || h[len(h)-1] != vm {
		t.Errorf("%s reported hostnames %v over C2, want phenix and then %s", vm, h, vm)
	}

	c2fCheckStartupResponses(t, ns, vm, uuid)

	if out := c2fExec(t, ns, vm, "ip -4 -o addr show"); !strings.Contains(out, address+"/24") {
		t.Errorf("%s does not have %s/24 from the C2-delivered interfaces script:\n%s", vm, address, out)
	}

	// minimega gives the scripts to the client again whenever it reconnects.
	// Run in the same boot again, the interfaces script must leave the
	// guest's addresses alone instead of flushing them: it must run, and end
	// with status 0 at the guard, which recorded the current boot.
	replay := fmt.Sprintf(
		"ip addr add %[1]s dev eth0 && bash /tmp/miniccc/files/%[2]s/%[3]s-interfaces.sh; echo rc=$?; %[4]s; "+
			"ip -4 -o addr show dev eth0; ip addr del %[1]s dev eth0",
		c2fReplayAddress, ns, vm, c2fGuardReport,
	)

	out := guestShell(t, ns, vm, replay)
	if lines := strings.Fields(out); !slices.Contains(lines, "rc=0") || !slices.Contains(lines, "marker=boot") {
		t.Errorf("%s's C2-delivered interfaces script, run again in the same boot, did not exit 0 "+
			"with %s holding the current boot ID:\n%s", vm, c2fInterfacesRan, out)
	}

	if !strings.Contains(out, c2fReplayAddress) {
		t.Errorf("%s's C2-delivered interfaces script, run again in the same boot, flushed eth0:\n%s", vm, out)
	}

	err = phenixmm.IsC2ClientActive(phenixmm.C2NS(ns), phenixmm.C2VM(vm), phenixmm.C2Timeout(time.Minute))
	if err != nil {
		t.Errorf("hostname-mode C2 check for %s: %v", vm, err)
	}
}

// c2fCheckReconnect pauses vm, whose startup scripts arrive over C2, until
// minimega drops its cc client, and resumes it. miniccc then connects again,
// and minimega gives the new connection every command again, the startup
// scripts included. vm's C2 client must answer in the same boot, and the
// run-once guard must keep the interfaces script from flushing its addresses.
func c2fCheckReconnect(t *testing.T, env *c2fEnv, vm, address string) {
	t.Helper()

	var (
		ns     = env.exp.name
		uuid   = env.uuids[vm]
		script = fmt.Sprintf("[bash /tmp/miniccc/files/%s/%s-interfaces.sh]", ns, vm)
		isExec = func(r map[string]string) bool { return r["command"] == script }
		bootID = "cat /proc/sys/kernel/random/boot_id"
	)

	// minimega counts a response each time a client runs the command.
	responses := func() int {
		n, _ := strconv.Atoi(c2fCommandRow(t, ns, isExec)["responses"])

		return n
	}

	before := responses()
	if before == 0 {
		t.Fatalf("cc commands has no answered exec of %s", script)
	}

	boot := strings.TrimSpace(c2fExec(t, ns, vm, bootID))

	// An address that a rerun of the interfaces script would flush.
	guestShell(t, ns, vm, "ip addr add "+c2fReplayAddress+" dev eth0")

	phenix(t, "vm", "pause", ns, vm)

	running := false

	// A subtest that fails while vm is paused must not leave it paused.
	defer func() {
		if !running {
			_, _ = phenixRun("vm", "resume", ns, vm)
		}
	}()

	paused := time.Now()

	eventually(t, c2fExpireWait, time.Second, "minimega to drop the cc client of paused "+vm, func() (bool, string) {
		_, listed := c2fClientHostnames(t, ns)[uuid]

		return !listed, "still listed"
	})

	t.Logf("minimega dropped the cc client of %s %v into its pause", vm, time.Since(paused).Round(time.Second))

	phenix(t, "vm", "resume", ns, vm)

	running, resumed := true, time.Now()

	eventually(t, c2fReconnectWait, time.Second, vm+"'s cc client to connect again and get its startup scripts", func() (bool, string) {
		n := responses()

		return n > before, fmt.Sprintf("%d runs of %s, %d before the pause", n, script, before)
	})

	t.Logf("%s ran its interfaces script again %v after the resume", vm, time.Since(resumed).Round(time.Second))

	if got := strings.TrimSpace(c2fExec(t, ns, vm, bootID)); got != boot {
		t.Errorf("%s answers C2 with boot ID %q after the reconnect, want %q from before the pause", vm, got, boot)
	}

	out := guestShell(t, ns, vm, c2fGuardReport+"; ip -4 -o addr show dev eth0; ip addr del "+c2fReplayAddress+" dev eth0")

	if !slices.Contains(strings.Fields(out), "marker=boot") {
		t.Errorf("%s holds no record of its current boot in %s after the reconnect:\n%s", vm, c2fInterfacesRan, out)
	}

	if !strings.Contains(out, c2fReplayAddress) || !strings.Contains(out, address+"/24") {
		t.Errorf("%s's interfaces script, given again on the reconnect, flushed eth0:\n%s", vm, out)
	}

	var v c2fRESTVM

	apiOK(t, http.MethodGet, "/experiments/"+ns+"/vms/"+vm, nil, &v)

	if !v.Running || !v.CCActive {
		t.Errorf("REST reports %s after the reconnect as %+v, want running with ccActive", vm, v)
	}
}

// c2fWaitHostname waits until vm's cc client reports vm as its hostname and
// adds the hostnames it reports on the way to env.hist.
func c2fWaitHostname(t *testing.T, env *c2fEnv, vm string, timeout time.Duration) {
	t.Helper()

	eventually(t, timeout, time.Second, vm+"'s cc client reporting hostname "+vm, func() (bool, string) {
		host, ok := c2fClientHostnames(t, env.exp.name)[env.uuids[vm]]
		if !ok {
			return false, "it has no cc client"
		}

		if h := env.hist[vm]; len(h) == 0 || h[len(h)-1] != host {
			env.hist[vm] = append(h, host)
		}

		return host == vm, fmt.Sprintf("it reports %q", host)
	})
}

// c2fCheckStartupResponses checks that every startup file arrived and every
// startup script ran in the guest.
func c2fCheckStartupResponses(t *testing.T, ns, vm, uuid string) {
	t.Helper()

	for _, kind := range c2fStartupKinds() {
		var (
			rel  = fmt.Sprintf("%s/%s-%s.sh", ns, vm, kind)
			send = c2fCommandRow(t, ns, func(r map[string]string) bool { return r["sent"] == "["+rel+"]" })
			exec = c2fCommandRow(t, ns, func(r map[string]string) bool {
				return r["command"] == "[bash /tmp/miniccc/files/"+rel+"]"
			})
		)

		if send == nil || exec == nil {
			t.Errorf("cc commands lacks the send (%v) or exec (%v) of %s", send, exec, rel)

			continue
		}

		if send["responses"] == "0" {
			t.Errorf("send of %s was never answered: %v", rel, send)
		}

		if exec["once"] != "false" {
			t.Errorf("startup script %s is exec-once, which minimega marks sent before any guest connects: %v", rel, exec)
		}

		code := c2fResponseFile(t, ns, exec["id"], uuid, "exitcode")
		stderr, _ := os.ReadFile(filepath.Join(c2fResponseDir(ns, exec["id"]), uuid, "stderr"))

		// The timezone script's result depends on tzdata in the image, which
		// is the images repository's concern, not phenix's.
		if kind == "timezone" {
			t.Logf("%s timezone script exited %s (stderr %q)", vm, code, stderr)

			continue
		}

		if code != "0" {
			t.Errorf("%s %s script exited %s: %s", vm, kind, code, stderr)
		}
	}
}

// c2fCheckStartupInjections reads partition 1 of each guest's injection
// snapshot: c2a got its startup scripts injected, c2d and c2v did not.
func c2fCheckStartupInjections(t *testing.T, env *c2fEnv) {
	t.Helper()

	script := c2fReadScript(t, env.exp.name, env.exp.name+".mm")

	for _, tc := range []struct {
		vm       string
		injected bool
	}{
		{"c2a", true},
		{"c2d", false},
		{"c2v", false},
	} {
		var snap string

		for _, l := range c2fBlock(script, tc.vm) {
			if f := strings.Fields(l); len(f) == 4 && f[0] == "disk" && f[1] == "snapshot" {
				snap = f[3]
			}
		}

		if snap == "" {
			t.Errorf("no disk snapshot for %s in the minimega script", tc.vm)

			continue
		}

		if !filepath.IsAbs(snap) {
			snap = filepath.Join(cfg.imagesDir, snap)
		}

		inspectDisk(t, snap, func(root string) {
			_, err := os.Stat(filepath.Join(root, "etc/phenix/startup/1_hostname-start.sh"))
			if got := err == nil; got != tc.injected {
				t.Errorf("%s: /etc/phenix/startup/1_hostname-start.sh present = %v, want %v", tc.vm, got, tc.injected)
			}
		})
	}
}

// c2fStartServer starts a TCP server on c2fGuestPort in the guest that
// answers every connection with "e2e-<vm>" and closes it. ncat ships with
// every image phenix builds (its default packages).
func c2fStartServer(t *testing.T, ns, vm string) {
	t.Helper()

	script := fmt.Sprintf(`command -v ncat >/dev/null 2>&1 || { echo "ncat is not installed"; exit 0; }
nohup setsid ncat -lk %[1]d --sh-exec 'echo e2e-%[2]s' >/dev/null 2>&1 </dev/null &
for _ in $(seq 1 20); do
  if (exec 3<>/dev/tcp/127.0.0.1/%[1]d) 2>/dev/null; then echo listening; exit 0; fi
  sleep 0.5
done
echo "nothing listens on %[1]d"`, c2fGuestPort, vm)

	if out := strings.TrimSpace(guestShell(t, ns, vm, script)); out != "listening" {
		t.Fatalf("starting a TCP server in %s: %s", vm, out)
	}
}

// ---------------------------------------------------------------------------
// ExecC2Command (C2-04), with phenix's util/mm as the subject under test

func c2fCheckExecWait(t *testing.T, env *c2fEnv) {
	t.Helper()

	ns := env.exp.name

	id, err := phenixmm.ExecC2Command(
		phenixmm.C2NS(ns),
		phenixmm.C2VM("c2a"),
		phenixmm.C2Command("echo hi"),
		phenixmm.C2Wait(),
		phenixmm.C2Timeout(c2fCmdTimeout),
	)
	if err != nil {
		t.Fatalf("ExecC2Command echo hi: %v", err)
	}

	// The filter is namespace state in minimega; left set, it would steer
	// every later cc command in the namespace to c2a.
	if rows := mmTable(t, ns, "cc filter"); len(rows) != 0 {
		t.Errorf("ExecC2Command left the namespace cc filter set: %v", rows)
	}

	if n, err := strconv.Atoi(id); err != nil || n <= 0 {
		t.Fatalf("ExecC2Command returned command id %q, want a positive integer", id)
	}

	if got := strings.TrimSpace(c2fRaw(t, ns, id)); got != "hi" {
		t.Errorf("raw response to echo hi = %q, want hi", got)
	}

	if got := c2fTyped(t, ns, "c2a", id, phenixmm.C2ResponseTypeStdout()); got != "hi" {
		t.Errorf("typed stdout of echo hi = %q, want hi", got)
	}
}

// c2fCheckExecBatch issues several commands without waiting, then checks each
// response: they are answered on the same heartbeat.
func c2fCheckExecBatch(t *testing.T, env *c2fEnv) {
	t.Helper()

	var (
		ns     = env.exp.name
		peer   = "10.40.0.2"
		fail   = c2fExecID(t, ns, "c2a", phenixmm.C2Command(`sh -c "echo oops >&2; exit 3"`))
		poison = c2fExecID(t, ns, "c2a", phenixmm.C2Command("echo 7/x/stdout: poison"))
		pass   = c2fExecID(t, ns, "c2a", phenixmm.C2TestConn(fmt.Sprintf("tcp %s %d wait 5s", peer, c2fGuestPort)))
		closed = c2fExecID(t, ns, "c2a", phenixmm.C2TestConn("tcp "+peer+" 9 wait 2s"))
	)

	if got := c2fTyped(t, ns, "c2a", fail, phenixmm.C2ResponseTypeStderr()); got != "oops" {
		t.Errorf("typed stderr of the failing command = %q, want oops", got)
	}

	if got := c2fTypedNow(t, ns, "c2a", fail, phenixmm.C2ResponseTypeStdout()); got != "" {
		t.Errorf("typed stdout of the failing command = %q, want empty", got)
	}

	if got := c2fResponseFile(t, ns, fail, env.uuids["c2a"], "exitcode"); got != "3" {
		t.Errorf("exit code of the failing command = %q, want 3", got)
	}

	// Guest output that looks like minimega's response header stays output.
	if got := c2fTyped(t, ns, "c2a", poison, phenixmm.C2ResponseTypeStdout()); got != "7/x/stdout: poison" {
		t.Errorf("typed stdout of a header-like line = %q, want %q", got, "7/x/stdout: poison")
	}

	want := fmt.Sprintf("%s:%d | pass", peer, c2fGuestPort)
	if got := c2fTyped(t, ns, "c2a", pass, phenixmm.C2ResponseTypeStdout()); got != want {
		t.Errorf("test-conn to c2b's server = %q, want %q", got, want)
	}

	if got := c2fTyped(t, ns, "c2a", closed, phenixmm.C2ResponseTypeStdout()); got != peer+":9 | fail" {
		t.Errorf("test-conn to a closed port = %q, want %q", got, peer+":9 | fail")
	}
}

func c2fCheckSendFile(t *testing.T, env *c2fEnv) {
	t.Helper()

	var (
		ns      = env.exp.name
		rel     = ns + "/c2-payload.txt"
		payload = fmt.Sprintf("e2e payload %d", time.Now().UnixNano())
		path    = filepath.Join(cfg.imagesDir, rel)
	)

	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, []byte(payload+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	id := c2fExecID(t, ns, "c2a",
		phenixmm.C2SendFile(rel),
		phenixmm.C2Command("cat /tmp/miniccc/files/"+rel),
		phenixmm.C2Wait(),
	)

	if got := strings.TrimSpace(c2fRaw(t, ns, id)); got != payload {
		t.Errorf("the guest read %q from the sent file, want %q", got, payload)
	}

	send := c2fCommandRow(t, ns, func(r map[string]string) bool { return r["sent"] == "["+rel+"]" })
	if send == nil {
		t.Fatalf("cc commands has no send of %s", rel)
	}

	sendID, _ := strconv.Atoi(send["id"])
	execID, _ := strconv.Atoi(id)

	if send["responses"] == "0" || sendID >= execID {
		t.Errorf("send of %s = %v, want answered and issued before command %s", rel, send, id)
	}
}

func c2fCheckExecInactive(t *testing.T, env *c2fEnv) {
	t.Helper()

	const timeout = 5 * time.Second

	began := time.Now()

	_, err := phenixmm.ExecC2Command(
		phenixmm.C2NS(env.exp.name),
		phenixmm.C2VM("h1"),
		phenixmm.C2Command("true"),
		phenixmm.C2Timeout(timeout),
	)
	took := time.Since(began)

	if !errors.Is(err, phenixmm.ErrC2ClientNotActive) {
		t.Errorf("ExecC2Command on hollow h1 = %v, want ErrC2ClientNotActive", err)
	}

	if took > timeout+10*time.Second {
		t.Errorf("ExecC2Command on hollow h1 took %v with a %v timeout", took, timeout)
	}
}

// c2fExecID runs ExecC2Command against vm and returns the command id.
func c2fExecID(t *testing.T, ns, vm string, opts ...phenixmm.C2Option) string {
	t.Helper()

	opts = append([]phenixmm.C2Option{
		phenixmm.C2NS(ns),
		phenixmm.C2VM(vm),
		phenixmm.C2Timeout(c2fCmdTimeout),
	}, opts...)

	id, err := phenixmm.ExecC2Command(opts...)
	if err != nil {
		t.Fatalf("ExecC2Command on %s: %v", vm, err)
	}

	return id
}

// c2fExec runs command in vm through phenix and returns its raw response.
func c2fExec(t *testing.T, ns, vm, command string) string {
	t.Helper()

	return c2fRaw(t, ns, c2fExecID(t, ns, vm, phenixmm.C2Command(command), phenixmm.C2Wait()))
}

// c2fRaw waits for command id to be answered and returns phenix's raw
// response. minimega counts a response before it writes the response files,
// so an empty read is retried.
func c2fRaw(t *testing.T, ns, id string) string {
	t.Helper()

	if _, err := phenixmm.WaitForC2Response(
		phenixmm.C2NS(ns),
		phenixmm.C2CommandID(id),
		phenixmm.C2Timeout(c2fCmdTimeout),
	); err != nil {
		t.Fatalf("WaitForC2Response %s: %v", id, err)
	}

	var out string

	eventually(t, 15*time.Second, time.Second, "the response to command "+id, func() (bool, string) {
		var err error

		out, err = phenixmm.GetC2Response(phenixmm.C2NS(ns), phenixmm.C2CommandID(id))

		return err == nil && strings.TrimSpace(out) != "", fmt.Sprintf("%q (%v)", out, err)
	})

	return out
}

// c2fTyped waits for a non-empty typed response of vm to command id and
// returns it trimmed.
func c2fTyped(t *testing.T, ns, vm, id string, typ phenixmm.C2Option) string {
	t.Helper()

	if _, err := phenixmm.WaitForC2Response(
		phenixmm.C2NS(ns),
		phenixmm.C2CommandID(id),
		phenixmm.C2Timeout(c2fCmdTimeout),
	); err != nil {
		t.Fatalf("WaitForC2Response %s: %v", id, err)
	}

	var out string

	eventually(t, 15*time.Second, time.Second, "the typed response to command "+id, func() (bool, string) {
		out = c2fTypedNow(t, ns, vm, id, typ)

		return out != "", "empty"
	})

	return out
}

// c2fTypedNow returns the typed response of vm to command id as it is now.
func c2fTypedNow(t *testing.T, ns, vm, id string, typ phenixmm.C2Option) string {
	t.Helper()

	out, err := phenixmm.GetC2Response(phenixmm.C2NS(ns), phenixmm.C2VM(vm), phenixmm.C2CommandID(id), typ)
	if err != nil {
		t.Fatalf("GetC2Response %s: %v", id, err)
	}

	return strings.TrimSpace(out)
}

// ---------------------------------------------------------------------------
// port forwards and autotunnels (C2-06, C2-07)

func c2fCheckForward(t *testing.T, env *c2fEnv) {
	t.Helper()

	ns := env.exp.name
	q := url.Values{"src": {"9000"}, "host": {c2fLoopback}, "dst": {strconv.Itoa(c2fGuestPort)}}

	if code, body := api(t, http.MethodPost, "/experiments/"+ns+"/vms/c2b/forwards?"+q.Encode(), nil); code != http.StatusNoContent {
		t.Fatalf("creating a forward to c2b: status %d: %s", code, body)
	}

	env.forwardAt = time.Now()

	want := c2fListener{Exp: ns, VM: "c2b", SrcPort: 9000, DstHost: c2fLoopback, DstPort: c2fGuestPort, Owner: c2fForwardOwnerREST}
	if got := c2fForwards(t, ns, "c2b"); !slices.Contains(got, want) {
		t.Errorf("forwards of c2b = %+v, want %+v", got, want)
	}

	var rows []map[string]string

	for _, row := range mmTable(t, ns, "cc tunnel list c2b") {
		if row["dst"] == c2fLoopback && row["dst port"] == strconv.Itoa(c2fGuestPort) {
			rows = append(rows, row)
		}
	}

	if len(rows) != 1 {
		t.Fatalf("cc tunnel list c2b has %d tunnels to %s:%d, want 1: %v", len(rows), c2fLoopback, c2fGuestPort, rows)
	}

	port, _ := strconv.Atoi(rows[0]["src port"])
	if port < c2fForwardPortMin || port >= c2fForwardPortMax {
		t.Errorf("forward host port %d is outside [%d, %d)", port, c2fForwardPortMin, c2fForwardPortMax)
	}

	env.forwardPort = port

	c2fWaitFetch(t, port, "e2e-c2b")
}

func c2fCheckForwardNoHost(t *testing.T, env *c2fEnv) {
	t.Helper()

	var (
		ns   = env.exp.name
		path = "/experiments/" + ns + "/vms/c2b/forwards?"
		dst  = c2fGuestPort + 1
	)

	// The host defaults to 127.0.0.1, so this is a valid request.
	code, body := api(t, http.MethodPost, path+url.Values{"src": {"9001"}, "dst": {strconv.Itoa(dst)}}.Encode(), nil)
	if code != http.StatusNoContent {
		t.Fatalf("creating a forward without a host: status %d: %s", code, body)
	}

	want := c2fListener{Exp: ns, VM: "c2b", SrcPort: 9001, DstHost: c2fLoopback, DstPort: dst, Owner: c2fForwardOwnerREST}
	if got := c2fForwards(t, ns, "c2b"); !slices.Contains(got, want) {
		t.Errorf("forwards of c2b = %+v, want %+v", got, want)
	}

	if _, ok := c2fFindTunnel(t, ns, "c2b", dst); !ok {
		t.Errorf("cc tunnel list c2b has no tunnel to %s:%d", c2fLoopback, dst)
	}

	if code, body := api(t, http.MethodDelete, path+url.Values{"dst": {strconv.Itoa(dst)}}.Encode(), nil); code != http.StatusNoContent {
		t.Errorf("deleting the forward without a host: status %d: %s", code, body)
	}
}

// c2fCheckAutotunnel checks the tunnel the startup app's
// phenix/startup-autotunnel annotation asks phenix ui to create once vm's C2
// is up, and that it carries data.
func c2fCheckAutotunnel(t *testing.T, env *c2fEnv, vm string) {
	t.Helper()

	ns := env.exp.name

	var port int

	eventually(t, c2fAutotunnelWait, 2*time.Second, "the startup autotunnel to "+vm, func() (bool, string) {
		var ok bool

		port, ok = c2fFindTunnel(t, ns, vm, c2fGuestPort)

		return ok, fmt.Sprintf("tunnels %v", mmTable(t, ns, "cc tunnel list "+vm))
	})

	want := c2fListener{Exp: ns, VM: vm, SrcPort: c2fGuestPort, DstHost: c2fLoopback, DstPort: c2fGuestPort, Owner: c2fForwardOwnerBot}
	if got := c2fForwards(t, ns, vm); !slices.Contains(got, want) {
		t.Errorf("forwards of %s = %+v, want %+v", vm, got, want)
	}

	c2fWaitFetch(t, port, "e2e-"+vm)
}

// c2fCheckAutotunnelGivesUp checks the startup autotunnel of vm, which has no
// C2 agent: phenix ui waits DefaultC2Timeout from PostStart for its cc client,
// then logs that it gave up and creates no forward. It must run before the
// stop, which ends the wait before it can give up.
func c2fCheckAutotunnelGivesUp(t *testing.T, env *c2fEnv, vm string) {
	t.Helper()

	const (
		postStart = "'startup' default app (post-start)"
		gaveUp    = "startup autotunnel not created: waiting for the C2 client"
		// slack covers the log line of PostStart, written once it returned,
		// against the wait it started.
		slack = 5 * time.Second
	)

	ns := env.exp.name

	started := c2fLogLines(c2fUILog(), env.uiLog, postStart)
	if len(started) == 0 {
		t.Fatalf("the phenix ui log has no %q line since %s started", postStart, ns)
	}

	ranAt, err := c2fLogTime(started[0])
	if err != nil {
		t.Fatal(err)
	}

	var line string

	eventually(t, max(time.Until(ranAt.Add(phenixmm.DefaultC2Timeout+time.Minute)), time.Second), 2*time.Second,
		"phenix ui to give up on the startup autotunnel of "+vm, func() (bool, string) {
			lines := c2fLogLines(c2fUILog(), env.uiLog, gaveUp, "exp="+ns, "vm="+vm)
			if len(lines) == 0 {
				return false, fmt.Sprintf("PostStart ran at %s", ranAt.Format(time.TimeOnly))
			}

			line = lines[0]

			return true, ""
		})

	at, err := c2fLogTime(line)
	if err != nil {
		t.Fatal(err)
	}

	if waited := at.Sub(ranAt); waited < phenixmm.DefaultC2Timeout-slack {
		t.Errorf("phenix ui gave up on the autotunnel of %s %v after PostStart, want its %v C2 wait:\n%s",
			vm, waited.Round(time.Second), phenixmm.DefaultC2Timeout, line)
	}

	// minimega cannot list the tunnels of a VM without a cc client (ron
	// ListForwards answers "no such client"), so phenix's forwards are what
	// is left to check.
	if got := c2fForwards(t, ns, vm); len(got) != 0 {
		t.Errorf("phenix ui lists forwards of %s, which has no C2: %+v", vm, got)
	}
}

func c2fCheckForwardSurvives(t *testing.T, env *c2fEnv) {
	t.Helper()

	if env.forwardAt.IsZero() {
		t.Fatal("the C2-06 forward to c2b was not created")
	}

	// The reaper drops forwards whose tunnel `cc tunnel list` no longer
	// shows; waiting out two ticks proves its filters match a live tunnel.
	if wait := time.Until(env.forwardAt.Add(c2fReaperWait)); wait > 0 {
		time.Sleep(wait)
	}

	ns := env.exp.name

	want := c2fListener{Exp: ns, VM: "c2b", SrcPort: 9000, DstHost: c2fLoopback, DstPort: c2fGuestPort, Owner: c2fForwardOwnerREST}
	if got := c2fForwards(t, ns, "c2b"); !slices.Contains(got, want) {
		t.Errorf("the forward to c2b is gone %v after it was created: %+v", time.Since(env.forwardAt).Round(time.Second), got)
	}

	if port, ok := c2fFindTunnel(t, ns, "c2b", c2fGuestPort); !ok || port != env.forwardPort {
		t.Errorf("tunnel to c2b:%d = port %d (found %v), want port %d", c2fGuestPort, port, ok, env.forwardPort)
	}
}

func c2fCheckForwardDelete(t *testing.T, env *c2fEnv) {
	t.Helper()

	var (
		ns   = env.exp.name
		path = "/experiments/" + ns + "/vms/c2b/forwards?" + url.Values{
			"host": {c2fLoopback}, "dst": {strconv.Itoa(c2fGuestPort)},
		}.Encode()
	)

	if code, body := api(t, http.MethodDelete, path, nil); code != http.StatusNoContent {
		t.Fatalf("deleting the forward to c2b: status %d: %s", code, body)
	}

	if _, ok := c2fFindTunnel(t, ns, "c2b", c2fGuestPort); ok {
		t.Error("the minimega tunnel to c2b is still listed after its forward was deleted")
	}

	if env.forwardPort != 0 {
		addr := net.JoinHostPort(c2fLoopback, strconv.Itoa(env.forwardPort))

		eventually(t, 15*time.Second, time.Second, addr+" to stop listening", func() (bool, string) {
			conn, err := net.DialTimeout("tcp", addr, time.Second)
			if err != nil {
				return true, ""
			}

			_ = conn.Close()

			return false, "still accepts connections"
		})
	}

	for _, l := range c2fForwards(t, ns, "c2b") {
		if l.DstPort == c2fGuestPort {
			t.Errorf("forward still listed after delete: %+v", l)
		}
	}

	code, body := api(t, http.MethodDelete, path, nil)
	if code != http.StatusBadRequest || !strings.Contains(string(body), "forward not found") {
		t.Errorf("deleting the forward again: status %d: %s, want 400 forward not found", code, body)
	}
}

// c2fCheckReaperDrops closes vm's autotunnel in minimega behind phenix's
// back: phenix reaps the forward before it lists forwards.
func c2fCheckReaperDrops(t *testing.T, env *c2fEnv, vm string) {
	t.Helper()

	var (
		ns = env.exp.name
		id string
	)

	for _, row := range mmTable(t, ns, "cc tunnel list "+vm) {
		if row["dst"] == c2fLoopback && row["dst port"] == strconv.Itoa(c2fGuestPort) {
			id = row["id"]
		}
	}

	if id == "" {
		t.Fatalf("%s has no tunnel to %s:%d to close", vm, c2fLoopback, c2fGuestPort)
	}

	mm(t, ns, fmt.Sprintf("cc tunnel close %s %s", vm, id))

	for _, l := range c2fForwards(t, ns, vm) {
		if l.DstPort == c2fGuestPort {
			t.Errorf("phenix still lists %+v after its minimega tunnel was closed", l)
		}
	}
}

// c2fCheckCLIAutotunnel checks the documented limit of startup autotunnels
// (skills/phenix/references/annotations.md): only phenix ui creates port
// forwards, so `phenix experiment start` warns that it creates none
// (app/startup.go Startup.PostStart) and c2a gets no tunnel, while the REST
// start of TestC2Flows gets its autotunnel (C2-07a). No process can create the
// tunnel later, so this needs no wait.
func c2fCheckCLIAutotunnel(t *testing.T, env *c2fEnv) {
	t.Helper()

	const warning = "startup autotunnel not created: autotunnels are created only for experiments started through phenix ui"

	ns := env.exp.name

	warned := false

	for line := range strings.SplitSeq(env.startOut, "\n") {
		warned = warned || strings.Contains(line, warning) && strings.Contains(line, "vm=c2a")
	}

	if !warned {
		t.Errorf("phenix experiment start does not warn that c2a's startup autotunnel is not created:\n%s", env.startOut)
	}

	if _, ok := c2fFindTunnel(t, ns, "c2a", c2fGuestPort); ok {
		t.Errorf("a CLI-started experiment got a tunnel to c2a:%d: %v", c2fGuestPort, mmTable(t, ns, "cc tunnel list c2a"))
	}

	if got := c2fForwards(t, ns, "c2a"); len(got) != 0 {
		t.Errorf("phenix ui lists forwards of c2a in a CLI-started experiment: %+v", got)
	}
}

// c2fForwards lists phenix's port forwards of vm through REST.
func c2fForwards(t *testing.T, ns, vm string) []c2fListener {
	t.Helper()

	var body struct {
		Listeners []c2fListener `json:"listeners"`
	}

	apiOK(t, http.MethodGet, "/experiments/"+ns+"/vms/"+vm+"/forwards", nil, &body)

	return body.Listeners
}

// c2fFindTunnel returns the host port of minimega's tunnel to 127.0.0.1:port
// in vm.
func c2fFindTunnel(t *testing.T, ns, vm string, port int) (int, bool) {
	t.Helper()

	for _, row := range mmTable(t, ns, "cc tunnel list "+vm) {
		if row["dst"] == c2fLoopback && row["dst port"] == strconv.Itoa(port) {
			src, err := strconv.Atoi(row["src port"])

			return src, err == nil
		}
	}

	return 0, false
}

// c2fWaitFetch connects to a forwarded host port and waits until it reads
// want from the guest's server.
func c2fWaitFetch(t *testing.T, port int, want string) {
	t.Helper()

	addr := net.JoinHostPort(c2fLoopback, strconv.Itoa(port))

	eventually(t, 30*time.Second, time.Second, "reading "+want+" through "+addr, func() (bool, string) {
		got, err := c2fFetch(addr)

		return strings.TrimSpace(got) == want, fmt.Sprintf("read %q (%v)", got, err)
	})
}

// c2fFetch reads what a TCP server sends before it closes the connection.
func c2fFetch(addr string) (string, error) {
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return "", err
	}
	defer conn.Close()

	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	data, err := io.ReadAll(conn)
	if len(data) > 0 {
		return string(data), nil
	}

	return "", err
}

// ---------------------------------------------------------------------------
// cc mount over 9p (C2-05)

func c2fCheckMountCLI(t *testing.T, env *c2fEnv) {
	t.Helper()

	var (
		ns  = env.exp.name
		dir = filepath.Join(c2fMountRoot, ns, "c2a")
	)

	if out := phenix(t, "vm", "mount", ns, "c2a"); !strings.Contains(out, "mounted at: "+dir) {
		t.Errorf("phenix vm mount output does not name %s:\n%s", dir, out)
	}

	if fs := c2fFSType(t, dir); fs != "9p" {
		t.Fatalf("%s is mounted as %q, want 9p", dir, fs)
	}

	if got := strings.TrimSpace(c2fReadMounted(t, filepath.Join(dir, "etc/hostname"))); got != "c2a" {
		t.Errorf("/etc/hostname through the mount = %q, want c2a", got)
	}

	row := c2fMountRow(t, ns, "c2a")
	if row == nil || row["path"] != dir || strings.ToLower(row["uuid"]) != env.uuids["c2a"] {
		t.Errorf("cc mount row for c2a = %v, want path %s and uuid %s", row, dir, env.uuids["c2a"])
	}

	if row != nil {
		// minimega: the UFS server behind a cc mount listens on every
		// interface without authentication (internal/ron/ufs.go).
		addr := row["addr"]
		port, _ := strconv.Atoi(addr[strings.LastIndex(addr, ":")+1:])
		t.Logf("cc mount serves c2a's filesystem from %s; host listeners on that port: %v", addr, c2fTCPListeners(port))
	}

	// Mounting a mounted VM again at its path succeeds (phenix tolerates
	// minimega's "already connected" there).
	if out := phenix(t, "vm", "mount", ns, "c2a"); !strings.Contains(out, "mounted at: "+dir) {
		t.Errorf("second phenix vm mount output does not name %s:\n%s", dir, out)
	}

	if n := len(c2fMountRows(t, ns, "c2a")); n != 1 {
		t.Errorf("cc mount lists c2a %d times after mounting it twice, want 1", n)
	}

	// minimega answers "already connected" wherever the VM is mounted, so a
	// mount at another path must fail instead of reporting an empty directory.
	other := filepath.Join(cfg.workDir, "c2-other-mount")

	t.Cleanup(func() { _ = os.Remove(other) })

	if out := phenixFails(t, "vm", "mount", ns, "c2a", other); !strings.Contains(out, "already mounted elsewhere") {
		t.Errorf("phenix vm mount of c2a, mounted at %s, to %s failed without saying so:\n%s", dir, other, out)
	}

	if fs := c2fFSType(t, other); fs != "" {
		t.Errorf("the refused mount of c2a left %s mounted as %q", other, fs)
	}
}

func c2fCheckMountCustomPath(t *testing.T, env *c2fEnv) {
	t.Helper()

	var (
		ns     = env.exp.name
		custom = filepath.Join(cfg.workDir, "c2-custom-mount")
		dir    = filepath.Join(c2fMountRoot, ns, "c2b")
	)

	if err := os.MkdirAll(custom, 0o750); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		c2fDetachMounts(t, custom)
		_ = os.Remove(custom)
	})

	if out := phenix(t, "vm", "mount", ns, "c2b", custom); !strings.Contains(out, "mounted at: "+custom) {
		t.Errorf("phenix vm mount output does not name %s:\n%s", custom, out)
	}

	if fs := c2fFSType(t, custom); fs != "9p" {
		t.Errorf("phenix vm mount reported %s, but it is mounted as %q, want 9p", custom, fs)
	}

	if fs := c2fFSType(t, dir); fs != "" {
		t.Errorf("mounting c2b to %s also mounted the default path %s (%s)", custom, dir, fs)
	}

	if row := c2fMountRow(t, ns, "c2b"); row == nil || row["path"] != custom {
		t.Errorf("cc mount row for c2b = %v, want path %s", row, custom)
	}

	phenix(t, "vm", "unmount", ns, "c2b")

	eventually(t, 15*time.Second, time.Second, "c2b to be unmounted", func() (bool, string) {
		a, b := c2fFSType(t, custom), c2fFSType(t, dir)

		return a != "9p" && b != "9p" && c2fMountRow(t, ns, "c2b") == nil, fmt.Sprintf("%q, %q", a, b)
	})
}

func c2fCheckMountREST(t *testing.T, env *c2fEnv) {
	t.Helper()

	var (
		ns   = env.exp.name
		base = "/api/v1/experiments/" + ns + "/vms/c2a"
		dir  = filepath.Join(c2fMountRoot, ns, "c2a")
	)

	c2fStartMountUI(t)

	// c2a is still mounted by the CLI (C2-05).
	code, body := c2fMountHTTP(t, http.MethodGet, base+"/files?"+c2fPathQuery("/"), nil, "")
	if code != http.StatusOK {
		t.Errorf("listing c2a's CLI mount through REST: status %d: %s", code, body)
	}

	mount := func() {
		t.Helper()

		if code, body := c2fMountHTTP(t, http.MethodPost, base+"/mount", nil, ""); code != http.StatusOK {
			t.Fatalf("mounting c2a through REST: status %d: %s", code, body)
		}

		eventually(t, 30*time.Second, time.Second, "c2a's root directory through REST", func() (bool, string) {
			code, body := c2fMountHTTP(t, http.MethodGet, base+"/files?"+c2fPathQuery("/"), nil, "")

			var list struct {
				Files []struct {
					Name  string `json:"name"`
					IsDir bool   `json:"isDir"`
				} `json:"files"`
			}

			if code != http.StatusOK || json.Unmarshal(body, &list) != nil {
				return false, fmt.Sprintf("status %d: %s", code, body)
			}

			for _, f := range list.Files {
				if f.Name == "etc" && f.IsDir {
					return true, ""
				}
			}

			return false, fmt.Sprintf("no etc directory in %s", body)
		})
	}

	unmount := func() {
		t.Helper()

		if code, body := c2fMountHTTP(t, http.MethodDelete, base+"/unmount", nil, ""); code != http.StatusOK {
			t.Errorf("unmounting c2a through REST: status %d: %s", code, body)
		}
	}

	waitUnmounted := func() {
		t.Helper()

		eventually(t, 15*time.Second, time.Second, "c2a to be unmounted", func() (bool, string) {
			fs := c2fFSType(t, dir)

			return fs != "9p" && c2fMountRow(t, ns, "c2a") == nil, "mounted as " + fs
		})
	}

	mount()

	code, body = c2fMountHTTP(t, http.MethodGet, base+"/files/download?"+c2fPathQuery("/etc/hostname"), nil, "")
	if code != http.StatusOK || strings.TrimSpace(string(body)) != "c2a" {
		t.Errorf("downloading /etc/hostname: status %d: %q, want c2a", code, body)
	}

	c2fCheckMountUpload(t, ns, base)
	c2fCheckMountSibling(t, ns, base)

	// The UI unmounts when its file browser closes, which must leave the CLI's
	// mount mounted.
	unmount()

	if fs := c2fFSType(t, dir); fs != "9p" || c2fMountRow(t, ns, "c2a") == nil {
		t.Errorf("REST unmount took down the CLI's mount of c2a (now mounted as %q)", fs)
	}

	phenix(t, "vm", "unmount", ns, "c2a")
	waitUnmounted()

	// A mount REST made is REST's to unmount.
	mount()
	unmount()
	waitUnmounted()
}

// c2fCheckMountUpload uploads a file into c2a's /tmp through REST and reads
// it back inside the guest.
func c2fCheckMountUpload(t *testing.T, ns, base string) {
	t.Helper()

	content := fmt.Sprintf("uploaded %d", time.Now().UnixNano())

	body, ctype := c2fMultipart(t, "e2e-upload.txt", content)
	if code, resp := c2fMountHTTP(t, http.MethodPut, base+"/files/upload?"+c2fPathQuery("/tmp"), body, ctype); code != http.StatusOK {
		t.Fatalf("uploading to c2a:/tmp: status %d: %s", code, resp)
	}

	if got := strings.TrimSpace(guestShell(t, ns, "c2a", "cat /tmp/e2e-upload.txt")); got != content {
		t.Errorf("c2a reads %q from the uploaded file, want %q", got, content)
	}
}

// c2fCheckMountSibling asks c2a's mount for a sibling directory whose name
// shares c2a's prefix; every file endpoint must refuse it.
func c2fCheckMountSibling(t *testing.T, ns, base string) {
	t.Helper()

	sibling := filepath.Join(c2fMountRoot, ns, "c2ab")

	if err := os.MkdirAll(sibling, 0o750); err != nil {
		t.Fatal(err)
	}

	defer os.RemoveAll(sibling)

	const secret = "e2e-sibling-content"

	if err := os.WriteFile(filepath.Join(sibling, "secret"), []byte(secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	upload, ctype := c2fMultipart(t, "e2e-escape.txt", "escape")

	for _, req := range []struct {
		method, path string
		body         io.Reader
		ctype        string
	}{
		{http.MethodGet, base + "/files?" + c2fPathQuery("../c2ab"), nil, ""},
		{http.MethodGet, base + "/files/download?" + c2fPathQuery("../c2ab/secret"), nil, ""},
		{http.MethodPut, base + "/files/upload?" + c2fPathQuery("../c2ab"), upload, ctype},
	} {
		code, body := c2fMountHTTP(t, req.method, req.path, req.body, req.ctype)
		if code != http.StatusBadRequest || strings.Contains(string(body), secret) {
			t.Errorf("%s %s: status %d: %s, want 400 without the sibling's content", req.method, req.path, code, body)
		}
	}

	if _, err := os.Stat(filepath.Join(sibling, "e2e-escape.txt")); err == nil {
		t.Error("an upload through c2a's mount landed in the sibling directory")
	}
}

func c2fCheckStop(t *testing.T, env *c2fEnv) {
	t.Helper()

	var (
		ns      = env.exp.name
		expDir  = filepath.Join(c2fMountRoot, ns)
		mounted = filepath.Join(expDir, "c2b")
	)

	// Leave a mount in place for the stop to clean up.
	phenix(t, "vm", "mount", ns, "c2b")

	if fs := c2fFSType(t, mounted); fs != "9p" {
		t.Fatalf("%s is mounted as %q before stop, want 9p", mounted, fs)
	}

	// And a port forward: the stop must drop it before it clears the
	// namespace, since phenix ui's reaper asks minimega about the tunnels of
	// the forwards it holds every 10 s, which would create the namespace again.
	ws := recordWS(t)
	q := url.Values{"src": {"9002"}, "host": {c2fLoopback}, "dst": {strconv.Itoa(c2fGuestPort)}}

	if code, body := api(t, http.MethodPost, "/experiments/"+ns+"/vms/c2b/forwards?"+q.Encode(), nil); code != http.StatusNoContent {
		t.Fatalf("creating a forward to c2b for the stop: status %d: %s", code, body)
	}

	if _, ok := c2fFindTunnel(t, ns, "c2b", c2fGuestPort); !ok {
		t.Fatalf("cc tunnel list c2b has no tunnel to %s:%d before the stop", c2fLoopback, c2fGuestPort)
	}

	apiOK(t, http.MethodPost, "/experiments/"+ns+"/stop", nil, nil)

	stopped := time.Now()

	// The stopping hook drops the forward, and announces it, before the stop
	// clears the namespace, so before phenix ui announces the stop. The
	// reaper drops and announces it too, but only once the stop is stored,
	// moments before that announcement: the order tells the two apart.
	// Polled without eventually, so the checks below still run.
	var msgs []wsMessage

	for end := stopped.Add(5 * time.Second); time.Now().Before(end); time.Sleep(200 * time.Millisecond) {
		if msgs = ws.messages("", ""); c2fWSIndex(msgs, "experiment", ns, "stop") >= 0 {
			break
		}
	}

	deleted, stop := c2fWSIndex(msgs, "experiment/vm/forward", ns+"/c2b", "delete"), c2fWSIndex(msgs, "experiment", ns, "stop")
	if deleted < 0 || stop < 0 || deleted > stop {
		t.Errorf("phenix announced the deletion of the forward to c2b as message %d and the stop as message %d, "+
			"want the deletion first, from the stop itself: forward %v, experiment %v",
			deleted, stop, ws.actions("experiment/vm/forward", ns+"/c2b"), ws.actions("experiment", ns))
	}

	eventually(t, 30*time.Second, time.Second, "the experiment's mounts to be cleaned up", func() (bool, string) {
		table, err := c2fMountTable()
		if err != nil {
			return false, err.Error()
		}

		for mp, fs := range table {
			if mp == expDir || strings.HasPrefix(mp, expDir+"/") {
				return false, fmt.Sprintf("%s still mounted (%s)", mp, fs)
			}
		}

		_, err = os.Stat(expDir)

		return os.IsNotExist(err), fmt.Sprintf("%s: %v", expDir, err)
	})

	if nsRow, ok := mmNamespaces(t)[ns]; ok {
		t.Errorf("namespace %s exists after stop: %v", ns, nsRow)
	}

	eventually(t, time.Minute, time.Second, "QEMU processes to exit", func() (bool, string) {
		n := qemuProcesses(t)

		return n == 0, fmt.Sprintf("%d still running", n)
	})

	// A forward that outlived the stop would have its tunnel listed, and so
	// the namespace created again, at one of the reaper's next two ticks.
	time.Sleep(time.Until(stopped.Add(c2fReaperWait)))

	if nsRow, ok := mmNamespaces(t)[ns]; ok {
		t.Errorf("namespace %s exists %v after the stop: %v", ns, time.Since(stopped).Round(time.Second), nsRow)
	}

	for _, vm := range []string{"c2a", "c2b"} {
		if got := c2fForwards(t, ns, vm); len(got) != 0 {
			t.Errorf("forwards of %s survive the stop: %+v", vm, got)
		}
	}

	if nsRow, ok := mmNamespaces(t)[ns]; ok {
		t.Errorf("listing the forwards of stopped %s created its namespace again: %v", ns, nsRow)
	}
}

// c2fWSIndex returns the index in msgs of the first websocket message about
// the named resource with the given action, or -1. phenix's broker sends a
// client its messages in the order they were published.
func c2fWSIndex(msgs []wsMessage, resourceType, name, action string) int {
	return slices.IndexFunc(msgs, func(m wsMessage) bool {
		return m.Resource.Type == resourceType && m.Resource.Name == name && m.Resource.Action == action
	})
}

// c2fStartMountUI runs a phenix ui with the vm-mount feature on
// c2fMountUIAddr, sharing the store and minimega, until the test ends. It
// uses its own unix socket so the CLI keeps talking to the shared UI.
func c2fStartMountUI(t *testing.T) {
	t.Helper()

	// A listener left on the address (a phenix ui from an aborted run) would
	// answer the readiness probe while this phenix ui fails to bind.
	if conn, err := net.DialTimeout("tcp", c2fMountUIAddr, time.Second); err == nil {
		_ = conn.Close()

		t.Fatalf("something already listens on %s, where the vm-mount phenix ui listens; stop it", c2fMountUIAddr)
	}

	logPath := filepath.Join(cfg.workDir, "c2-mount-ui.log")

	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}

	sock := filepath.Join(cfg.workDir, "c2-mount-ui.sock")

	cmd := exec.Command(cfg.phenix, phenixArgs(
		"--unix-socket", sock,
		"ui", "--listen-endpoint", c2fMountUIAddr, "--features", "vm-mount",
	)...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	if err := startChild(cmd); err != nil {
		_ = logFile.Close()

		t.Fatalf("starting a vm-mount phenix ui: %v", err)
	}

	exited := make(chan struct{})

	go func() {
		_ = cmd.Wait()

		close(exited)
	}()

	t.Cleanup(func() {
		_ = cmd.Process.Signal(os.Interrupt)

		select {
		case <-exited:
		case <-time.After(15 * time.Second):
			_ = cmd.Process.Kill()
			<-exited
		}

		_ = logFile.Close()
		_ = os.Remove(sock)
	})

	eventually(t, time.Minute, 500*time.Millisecond, "the vm-mount phenix ui to answer", func() (bool, string) {
		select {
		case <-exited:
			t.Fatalf("the vm-mount phenix ui exited while starting (%s); see %s", cmd.ProcessState, logPath)
		default:
		}

		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+c2fMountUIAddr+"/version", nil)
		if err != nil {
			return false, err.Error()
		}

		resp, err := httpClient.Do(req)
		if err != nil {
			return false, err.Error()
		}

		_ = resp.Body.Close()

		return resp.StatusCode == http.StatusOK, resp.Status
	})
}

// c2fMountHTTP calls the vm-mount phenix ui and returns the status and body.
func c2fMountHTTP(t *testing.T, method, path string, body io.Reader, contentType string) (int, []byte) {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), method, "http://"+c2fMountUIAddr+path, body)
	if err != nil {
		t.Fatal(err)
	}

	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

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

func c2fPathQuery(p string) string {
	return url.Values{"path": {p}}.Encode()
}

// c2fMultipart builds a multipart upload body with one file field.
func c2fMultipart(t *testing.T, name, content string) (io.Reader, string) {
	t.Helper()

	var (
		buf bytes.Buffer
		w   = multipart.NewWriter(&buf)
	)

	fw, err := w.CreateFormFile("file", name)
	if err == nil {
		_, err = io.WriteString(fw, content)
	}

	if err == nil {
		err = w.Close()
	}

	if err != nil {
		t.Fatal(err)
	}

	return &buf, w.FormDataContentType()
}

// c2fMountRows returns minimega's cc mount rows for vm.
func c2fMountRows(t *testing.T, ns, vm string) []map[string]string {
	t.Helper()

	var rows []map[string]string

	for _, row := range mmTable(t, ns, "cc mount") {
		if row["name"] == vm {
			rows = append(rows, row)
		}
	}

	return rows
}

func c2fMountRow(t *testing.T, ns, vm string) map[string]string {
	t.Helper()

	if rows := c2fMountRows(t, ns, vm); len(rows) > 0 {
		return rows[0]
	}

	return nil
}

// c2fMountTable maps each mount point on the host to its filesystem type.
func c2fMountTable() (map[string]string, error) {
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return nil, err
	}

	out := make(map[string]string)

	for line := range strings.SplitSeq(string(data), "\n") {
		fields := strings.Fields(line)

		// The mount point is field 5; the filesystem type follows the "-"
		// separator (proc(5)).
		if sep := slices.Index(fields, "-"); sep > 4 && sep+1 < len(fields) {
			out[fields[4]] = fields[sep+1]
		}
	}

	return out, nil
}

// c2fFSType returns the filesystem type mounted at path, or "" when nothing
// is mounted there.
func c2fFSType(t *testing.T, path string) string {
	t.Helper()

	table, err := c2fMountTable()
	if err != nil {
		t.Fatal(err)
	}

	return table[path]
}

// c2fCleanupHost removes what experiment exp leaves on the host: 9p mounts a
// failed test left behind and the emptied mount directories, the injection
// snapshots, and the experiment's directory under minimega's file path
// (staged startup scripts, sent files, cc responses). Register it before
// newExperiment so it runs after the experiment's teardown.
func c2fCleanupHost(t *testing.T, exp string) {
	t.Helper()

	t.Cleanup(func() {
		mounts := filepath.Join(c2fMountRoot, exp)

		c2fDetachMounts(t, mounts)

		// Remove, not RemoveAll: a directory still holding files is not a
		// detached mount point.
		if entries, err := os.ReadDir(mounts); err == nil {
			for _, e := range entries {
				_ = os.Remove(filepath.Join(mounts, e.Name()))
			}

			_ = os.Remove(mounts)
		}

		snapshots, _ := filepath.Glob(filepath.Join(cfg.imagesDir, "*_"+exp+"_*_snapshot"))
		for _, s := range snapshots {
			_ = os.Remove(s)
		}

		_ = os.RemoveAll(filepath.Join(cfg.imagesDir, exp))
	})
}

// c2fCloseTunnels closes every minimega tunnel of the given VMs, then lists
// their forwards through phenix ui, which reaps each forward whose tunnel is
// gone. phenix ui then holds no forward whose 10 s reaper could query, and so
// re-create, the namespace while a CLI stop clears it: the CLI drops forwards
// only in its own process, and the reaper leaves the experiment alone only
// once the stop is stored. It does nothing once the namespace is gone and
// never fails the test.
func c2fCloseTunnels(t *testing.T, ns string, vms ...string) {
	t.Helper()

	resp, err := mmRun("", "namespace")
	if err != nil || !slices.ContainsFunc(mmRows(resp), func(r map[string]string) bool { return r["namespace"] == ns }) {
		return
	}

	for _, vm := range vms {
		if resp, err := mmRun(ns, "cc tunnel list "+vm); err == nil {
			for _, row := range mmRows(resp) {
				_, _ = mmRun(ns, fmt.Sprintf("cc tunnel close %s %s", vm, row["id"]))
			}
		}

		_, _ = api(t, http.MethodGet, "/experiments/"+ns+"/vms/"+vm+"/forwards", nil)
	}
}

// c2fDetachMounts lazily unmounts any 9p mount at or under root that a test
// left behind.
func c2fDetachMounts(t *testing.T, root string) {
	t.Helper()

	table, err := c2fMountTable()
	if err != nil {
		return
	}

	for mp, fs := range table {
		if fs == "9p" && (mp == root || strings.HasPrefix(mp, root+"/")) {
			t.Logf("cleanup: detaching leftover 9p mount %s", mp)

			_ = syscall.Unmount(mp, syscall.MNT_DETACH)
		}
	}
}

// c2fReadMounted reads a file through a 9p mount, failing instead of hanging
// when the guest side stops answering.
func c2fReadMounted(t *testing.T, path string) string {
	t.Helper()

	type result struct {
		data []byte
		err  error
	}

	done := make(chan result, 1)

	go func() {
		data, err := os.ReadFile(path)
		done <- result{data, err}
	}()

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("reading %s: %v", path, r.err)
		}

		return string(r.data)
	case <-time.After(30 * time.Second):
		t.Fatalf("reading %s through the mount did not finish in 30 s", path)
	}

	return ""
}

// c2fTCPListeners returns the local addresses (as /proc/net/tcp prints them)
// of host IPv4 sockets listening on port; 00000000 is every interface.
func c2fTCPListeners(port int) []string {
	data, err := os.ReadFile("/proc/net/tcp")
	if err != nil {
		return nil
	}

	var (
		out    []string
		suffix = fmt.Sprintf(":%04X", port)
	)

	for line := range strings.SplitSeq(string(data), "\n") {
		if f := strings.Fields(line); len(f) > 3 && f[3] == "0A" && strings.HasSuffix(f[1], suffix) {
			out = append(out, f[1])
		}
	}

	return out
}

// ---------------------------------------------------------------------------
// reboot and power cycle (C2-08)

// c2fCheckReset reboots vm with phenix vm restart, which resets a running
// guest through QMP without relaunching QEMU, and returns the guest's boot
// ID from before the reset.
func c2fCheckReset(t *testing.T, env *c2fEnv, vm string) string {
	t.Helper()

	ns := env.exp.name

	bootID, ok := c2fBootID(t, ns, env.uuids[vm], time.Minute)
	if !ok {
		t.Fatalf("%s did not report its boot ID over C2", vm)
	}

	pid := vmInfo(t, ns)[vm]["pid"]

	phenix(t, "vm", "restart", ns, vm)

	row := vmInfo(t, ns)[vm]
	if row["state"] != "RUNNING" {
		t.Errorf("%s is %s right after phenix vm restart, want RUNNING", vm, row["state"])
	}

	if row["pid"] != pid {
		t.Errorf("QEMU of %s was relaunched (pid %s, then %s); a restart of a running VM resets it in place", vm, pid, row["pid"])
	}

	return bootID
}

func c2fCheckRebootC2(t *testing.T, env *c2fEnv, vm, before string) {
	t.Helper()

	if before == "" {
		t.Fatal("no boot ID was recorded before the reboot")
	}

	var (
		ns   = env.exp.name
		uuid = env.uuids[vm]
	)

	eventually(t, guestBootTimeout, 5*time.Second, vm+" to answer C2 from a new boot", func() (bool, string) {
		after, ok := c2fBootID(t, ns, uuid, 30*time.Second)
		if !ok {
			return false, "no answer"
		}

		return after != before, "still boot " + after
	})

	eventually(t, time.Minute, 2*time.Second, vm+" to report its hostname again", func() (bool, string) {
		host := c2fClientHostnames(t, ns)[uuid]

		return host == vm, fmt.Sprintf("reports %q", host)
	})
}

// c2fCharacterizePowerCycle shuts vm down, starts it again, and records
// whether its C2 client comes back. minimega's serial re-dial loop gives up
// on the first failed dial, so the prediction is that it never does; that is
// a minimega defect, logged here rather than asserted.
func c2fCharacterizePowerCycle(t *testing.T, env *c2fEnv, vm string) {
	t.Helper()

	var (
		ns      = env.exp.name
		uuid    = env.uuids[vm]
		id      = vmInfo(t, ns)[vm]["id"]
		logPath = filepath.Join(cfg.mmBase, "minimega.log")
		offset  = fileSize(logPath)
		began   = time.Now()
	)

	phenix(t, "vm", "shutdown", ns, vm)
	t.Logf("phenix vm shutdown returned after %v (phenix kills the VM after 30 s without an ACPI power-off)",
		time.Since(began).Round(time.Second))

	eventually(t, 30*time.Second, time.Second, vm+" to be QUIT", func() (bool, string) {
		state := vmInfo(t, ns)[vm]["state"]

		return state == "QUIT", state
	})

	eventually(t, 30*time.Second, 2*time.Second, "phenix to report "+vm+" without C2", func() (bool, string) {
		var v c2fRESTVM

		apiOK(t, http.MethodGet, "/experiments/"+ns+"/vms/"+vm, nil, &v)

		return !v.CCActive && !v.Running && v.State == "QUIT", fmt.Sprintf("%+v", v)
	})

	// minimega: after QEMU exits, ron's DialSerial loop exits on the
	// first failed dial (internal/ron/server.go) and KvmVM.Connect does not
	// dial again for a VM it relaunches (cmd/minimega/kvm.go).
	t.Logf("minimega log after the shutdown: %q",
		c2fLogLines(logPath, offset, "dialing serial port", filepath.Join(cfg.mmBase, id, "cc")))

	phenix(t, "vm", "restart", ns, vm)

	eventually(t, time.Minute, time.Second, vm+" to be RUNNING again", func() (bool, string) {
		state := vmInfo(t, ns)[vm]["state"]

		return state == "RUNNING", state
	})

	restarted := time.Now()

	for time.Since(restarted) < c2fPowerCycleWatch {
		if host, ok := c2fClientHostnames(t, ns)[uuid]; ok {
			t.Logf("minimega: C2 of %s came back (hostname %q) %v after it was started again from QUIT",
				vm, host, time.Since(restarted).Round(time.Second))

			return
		}

		time.Sleep(2 * time.Second)
	}

	t.Logf("minimega: serial re-dial missing: %s has no cc client %v after it was started again from QUIT", vm, c2fPowerCycleWatch)
}

// c2fBootID reads the guest's kernel boot ID over raw minimega C2, or reports
// false when the guest does not answer within wait.
func c2fBootID(t *testing.T, ns, uuid string, wait time.Duration) (string, bool) {
	t.Helper()

	out, ok := c2fTryExec(t, ns, uuid, wait, "cat", "/proc/sys/kernel/random/boot_id")

	return strings.TrimSpace(out), ok && strings.TrimSpace(out) != ""
}

// c2fTryExec runs argv in the guest with the given UUID and returns its
// stdout, or false when no answer arrives within wait. Unlike ccExec it does
// not fail the test when the guest is away; like ccExec it deletes the
// command afterwards, since the guest reboots between these reads and
// minimega gives a client that connects again every command it still holds.
func c2fTryExec(t *testing.T, ns, uuid string, wait time.Duration, argv ...string) (string, bool) {
	t.Helper()

	for _, arg := range argv {
		if err := ccArgSafe(arg); err != nil {
			t.Fatalf("cc exec in %s: %v", uuid, err)
		}
	}

	mm(t, ns, "cc filter uuid="+uuid)

	resp, err := mmRun(ns, "cc exec "+quoteArgs(argv))

	mm(t, ns, "clear cc filter")

	if err != nil || len(resp) == 0 {
		t.Fatalf("cc exec %v: %v", argv, err)
	}

	id := strings.TrimSpace(string(resp[0].Data))
	if _, err := strconv.Atoi(id); err != nil {
		t.Fatalf("cc exec %v in %s: command id %q: %v", argv, uuid, id, err)
	}

	defer ccDeleteCommand(t, ns, id)

	ccWantFilter(t, ns, id, uuid)

	var (
		stdout   = filepath.Join(c2fResponseDir(ns, id), uuid, "stdout")
		deadline = time.Now().Add(wait)
	)

	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(stdout); err == nil && len(data) > 0 {
			return string(data), true
		}

		time.Sleep(time.Second)
	}

	return "", false
}

// c2fUILog is the log of the phenix ui the harness started.
func c2fUILog() string {
	return filepath.Join(cfg.workDir, "phenix-ui.log")
}

// c2fLogTime returns the time a phenix log line starts with.
func c2fLogTime(line string) (time.Time, error) {
	const layout = "2006-01-02 15:04:05.000" // util/plog's TimeFormat

	if len(line) < len(layout) {
		return time.Time{}, fmt.Errorf("no time in phenix log line %q", line)
	}

	//nolint:gosmopolitan // phenix logs its own local time
	at, err := time.ParseInLocation(layout, line[:len(layout)], time.Local)
	if err != nil {
		return time.Time{}, fmt.Errorf("phenix log line %q: %w", line, err)
	}

	return at, nil
}

// c2fLogLines returns the lines of the file after offset that contain every
// needle.
func c2fLogLines(path string, offset int64, needles ...string) []string {
	data, err := os.ReadFile(path)
	if err != nil || int64(len(data)) < offset {
		return nil
	}

	var out []string

	for line := range strings.SplitSeq(string(data[offset:]), "\n") {
		if !slices.ContainsFunc(needles, func(n string) bool { return !strings.Contains(line, n) }) {
			out = append(out, line)
		}
	}

	return out
}

// ---------------------------------------------------------------------------
// minimega C2 state

// c2fReadScript reads a minimega script phenix rendered for experiment ns.
func c2fReadScript(t *testing.T, ns, name string) string {
	t.Helper()

	data, err := os.ReadFile(filepath.Join("/phenix/experiments", ns, "mm_files", name))
	if err != nil {
		t.Fatalf("reading rendered minimega script: %v", err)
	}

	return string(data)
}

// c2fBlock returns the trimmed, non-empty lines of the "## VM: <vm> ##"
// block of a rendered minimega script, up to the next VM block or the
// closing "clear cc filter"; nil when the script has no block for vm.
func c2fBlock(script, vm string) []string {
	var (
		lines []string
		in    bool
	)

	for line := range strings.SplitSeq(script, "\n") {
		line = strings.TrimSpace(line)

		switch {
		case line == "## VM: "+vm+" ##":
			in = true
		case strings.HasPrefix(line, "## VM: "), line == "clear cc filter":
			in = false
		case in && line != "":
			lines = append(lines, line)
		}
	}

	return lines
}

// c2fCommandRow returns the first `cc commands` row of ns that match accepts.
func c2fCommandRow(t *testing.T, ns string, match func(map[string]string) bool) map[string]string {
	t.Helper()

	for _, row := range mmTable(t, ns, "cc commands") {
		if match(row) {
			return row
		}
	}

	return nil
}

// c2fResponseDir is where minimega's cc server for namespace ns keeps the
// responses to command id: <filepath>/<ns>/miniccc_responses/<id>/<uuid>/.
func c2fResponseDir(ns, id string) string {
	return filepath.Join(cfg.imagesDir, ns, "miniccc_responses", id)
}

// c2fResponseFile waits for one response file of a client to command id.
func c2fResponseFile(t *testing.T, ns, id, uuid, name string) string {
	t.Helper()

	var (
		path = filepath.Join(c2fResponseDir(ns, id), uuid, name)
		data []byte
	)

	eventually(t, 30*time.Second, 500*time.Millisecond, path, func() (bool, string) {
		var err error

		data, err = os.ReadFile(path)

		return err == nil, fmt.Sprint(err)
	})

	return string(data)
}

// c2fResponseClients lists the UUIDs of the clients that answered command id.
func c2fResponseClients(t *testing.T, ns, id string) []string {
	t.Helper()

	entries, err := os.ReadDir(c2fResponseDir(ns, id))
	if err != nil {
		t.Fatalf("reading the responses to command %s: %v", id, err)
	}

	out := make([]string, 0, len(entries))

	for _, e := range entries {
		out = append(out, strings.ToLower(e.Name()))
	}

	sort.Strings(out)

	return out
}
