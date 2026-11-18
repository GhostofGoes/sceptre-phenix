//go:build e2e

package e2e

// VM operations (F6), REST and websocket VM views (F8), and snapshots,
// restore, commit and memory dumps (F14), on one experiment whose VMs share
// the custom default bridge opsBridge:
//
//   - k1, k2: real guests (cfg.guest, miniccc as a systemd service, systemd
//     powers them off on ACPI) for everything seen from inside a guest;
//   - h1, h2: hollow VMs on injection snapshots of the ext4 fixture;
//   - hn: a hollow VM with snapshot: false on a private blank disk.
//
// The subtests run in a fixed order. minimega's `vm save` status fails for
// the whole namespace while any KVM VM in it is QUIT, which phenix must not
// depend on: S-02a snapshots h1 while h2 is powered off. The hollow checks run
// while the real guests boot, and k2 is redeployed while k1 reboots.

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image/png"
	"io"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"phenix/util/common"
)

const (
	opsExp       = "e2e-ops"
	opsBridge    = "e2eopsbr"
	opsHNImage   = "e2e-ops-hn.qc2"
	opsISOImage  = "e2e-ops-cd.iso"
	opsCommitImg = "e2e-ops-commit.qc2"
	opsK2Addr    = "10.60.0.2"
	opsLAN       = "LAN"

	opsRunning = "RUNNING"
	opsPaused  = "PAUSED"
	opsQuit    = "QUIT"

	// opsStateWait bounds a VM state change phenix asks minimega for.
	opsStateWait = 30 * time.Second
	// opsExecWait bounds one C2 round trip to a booted guest; responses
	// ride on miniccc's 5 s heartbeat.
	opsExecWait = 2 * time.Minute
	// opsRestoreC2Watch is how long the restore check waits for C2 to come
	// back to a restored guest.
	opsRestoreC2Watch = 150 * time.Second
	// opsShutdownFallback is how long phenix vm shutdown waits for a VM to
	// power off before it kills it (api/vm/vm.go shutdownTimeout).
	opsShutdownFallback = 30 * time.Second
	// opsScreenshotCache is how long phenix ui caches a VM screenshot
	// (web/util/screenshot.go screenshotCacheDuration).
	opsScreenshotCache = 10 * time.Second
	// opsSaveStatusTimeout is how long phenix polls a save QEMU reports no
	// migration for before it gives up (api/vm/save.go vmSaveStatusTimeout).
	opsSaveStatusTimeout = time.Minute

	opsScreenshotSize = 215
	opsSmallShot      = 100
	opsMiB            = 1 << 20
	opsHollowMemMiB   = 256
	opsK2MemMiB       = 1536
	opsK2CPUs         = "2"
	opsIsoSize        = 2 * opsMiB
	opsPercent        = 100
)

// opsRFBGreeting is what a VNC (RFB 3.8) server sends first.
const opsRFBGreeting = "RFB 003.008\n"

// opsEnv is what the subtests of TestOpsVM share.
type opsEnv struct {
	exp       *experiment
	ws        *wsRecorder
	filesDir  string
	k1BootID  string    // boot ID of k1 before checkRestart reset it
	restartAt time.Time // when checkRestart reset k1

	// commitToken is what checkShutdown wrote into k1's disk for the
	// commit to carry.
	commitToken string

	// markedOverlay is k1's instance disk, once checkShutdown found both
	// of its markers on it; checkResetDisk proves the reset by their absence.
	markedOverlay string

	// restoredTokens are the markers k2 held in memory when it was saved
	// and after it was saved, before checkSnapshotRestore restored it.
	restoredTokens [2]string
}

// opsVM is the part of a phenix REST VM the tests read.
type opsVM struct {
	Name       string            `json:"name"`
	State      string            `json:"state"`
	Running    bool              `json:"running"`
	Busy       bool              `json:"busy"`
	CCActive   bool              `json:"ccActive"`
	CdRom      string            `json:"cdRom"`
	Cpus       int               `json:"cpus"`
	RAM        int               `json:"ram"`
	Networks   []string          `json:"networks"`
	Tags       map[string]string `json:"tags"`
	Screenshot string            `json:"screenshot"`
	Captures   []json.RawMessage `json:"captures"`
}

// opsHost is the part of a GET /hosts entry the tests read.
type opsHost struct {
	Name        string `json:"name"`
	CPUs        int    `json:"cpus"`
	CPUCommit   int    `json:"cpucommit"`
	MemTotal    int    `json:"memtotal"`
	MemCommit   int    `json:"memcommit"`
	VMs         int    `json:"vms"`
	Schedulable bool   `json:"schedulable"`
	Headnode    bool   `json:"headnode"`
	DiskUsage   struct {
		Phenix float64 `json:"diskphenix"`
	} `json:"diskusage"`
}

// TestOpsVM walks the VM operations phenix offers on a running experiment
// (F6 V-01 to V-09), the REST and websocket views of its VMs (F8 W-01 to
// W-04), and snapshots, restore, commit and memory dumps (F14 S-01 to S-04),
// checking each through minimega, Open vSwitch, the guests and the disks.
func TestOpsVM(t *testing.T) {
	requireImage(t, cfg.guest)
	hollowImages(t)

	o := opsSetup(t)

	// While the real guests boot: hollow VMs only.
	t.Run("W-03 GET hosts", o.checkHosts)
	t.Run("V-01 pause and resume", o.checkPauseResume)
	t.Run("V-07 labels on a running VM", o.checkLabels)
	t.Run("V-09 optical disc", o.checkCdrom)
	t.Run("W-01 screenshots", o.checkScreenshots)
	t.Run("W-02 VNC", o.checkVNC)
	t.Run("W-04 websocket VM events", o.checkWebsocket)
	t.Run("S-02b snapshot of a VM without a disk snapshot", o.checkSnapshotNoDiskSnapshot)
	t.Run("S-02c snapshot of a paused VM", o.checkSnapshotPaused)
	t.Run("S-04a memory dump through the CLI", o.checkMemoryDumpCLI)
	t.Run("V-06b reset-disk without a disk snapshot", o.checkResetDiskNoSnapshot)

	began := time.Now()

	waitCCClients(t, opsExp, "k1", "k2")
	t.Logf("k1 and k2 registered with C2 %v after the hollow checks", time.Since(began).Round(time.Second))

	t.Run("V-08 interface disconnect and reconnect", o.checkNet)
	t.Run("V-02a restart resets a running VM in place", o.checkRestart)
	t.Run("V-05 redeploy", o.checkRedeploy)
	t.Run("V-02b the restarted guest booted again", o.checkRebooted)
	t.Run("V-05e the redeployed guest", o.checkRedeployedGuest)
	t.Run("V-01b pause and resume of a booted guest", o.checkPauseResumeGuest)
	t.Run("S-01 snapshot and restore", o.checkSnapshotRestore)
	t.Run("S-04b memory dump through REST", o.checkMemoryDumpREST)
	t.Run("V-03a V-03b shutdown of a guest", o.checkShutdown)
	t.Run("S-03 commit", o.checkCommit)
	t.Run("V-06a reset-disk", o.checkResetDisk)
	t.Run("V-03c restart of a powered-off VM", o.checkRestartFromQuit)
	t.Run("V-03d shutdown of a VM that ignores ACPI", o.checkShutdownFallback)
	t.Run("S-02a snapshot while another VM is powered off", o.checkSnapshotWithQuitVM)
	t.Run("V-04 kill", o.checkKill)
	t.Run("C2 after relaunches from QUIT", o.checkC2AfterRelaunch)
}

// opsSetup creates the shared experiment and starts it.
func opsSetup(t *testing.T) *opsEnv {
	t.Helper()

	hn := filepath.Join(cfg.imagesDir, opsHNImage)
	if err := run("qemu-img", "create", "-q", "-f", "qcow2", hn, "32M"); err != nil {
		t.Fatal(err)
	}

	iso := filepath.Join(cfg.imagesDir, opsISOImage)
	if err := os.WriteFile(iso, bytes.Repeat([]byte{0}, opsIsoSize), 0o600); err != nil {
		t.Fatal(err)
	}

	// Registered before the experiment, so these run after it is deleted.
	t.Cleanup(func() {
		_ = run("ovs-vsctl", "--if-exists", "del-br", opsBridge)

		leftovers, _ := filepath.Glob(filepath.Join(cfg.imagesDir, opsExp+"_*"))
		for _, path := range append(leftovers, hn, iso, filepath.Join(cfg.imagesDir, opsCommitImg)) {
			_ = os.Remove(path)
		}

		_ = os.RemoveAll(filepath.Join(cfg.imagesDir, opsExp))
	})

	hnNode := `
  - type: VirtualMachine
    general:
      hostname: hn
      snapshot: false
    hardware:
      vcpus: 1
      memory: 256
      os_type: linux
      drives:
      - image: ` + opsHNImage + `
    network:
      interfaces:
      - name: eth0
        type: ethernet
        proto: static
        vlan: LAN
        address: 10.60.0.13
        mask: 24`

	topo := topology(
		node("k1", cfg.guest, opsLAN, "10.60.0.1"),
		node("k2", cfg.guest, opsLAN, opsK2Addr),
		hollowNode("h1", opsLAN, "10.60.0.11", ""),
		hollowNode("h2", opsLAN, "10.60.0.12", ""),
		hnNode,
	)

	o := &opsEnv{
		ws:       recordWS(t),
		filesDir: filepath.Join(cfg.imagesDir, opsExp, "files"),
	}

	o.exp = newExperiment(t, opsExp, topo, "", "--default-bridge", opsBridge)
	o.exp.start()
	o.exp.waitRunning(time.Minute, "k1", "k2", "h1", "h2", "hn")

	return o
}

// ---------------------------------------------------------------------------
// hollow VMs, while the real guests boot

// checkHosts covers W-03: GET /hosts against the host itself and against the
// VMs minimega runs.
func (o *opsEnv) checkHosts(t *testing.T) {
	var cluster struct {
		Hosts []opsHost `json:"hosts"`
	}

	apiOK(t, http.MethodGet, "/hosts", nil, &cluster)

	if len(cluster.Hosts) != 1 {
		t.Fatalf("GET /hosts lists %d hosts, want this runner only: %+v", len(cluster.Hosts), cluster.Hosts)
	}

	h := cluster.Hosts[0]

	if want := common.TrimHostnameSuffixes(headnode(t)); h.Name != want || !h.Headnode || !h.Schedulable {
		t.Errorf("host %q headnode %v schedulable %v, want %q, true, true", h.Name, h.Headnode, h.Schedulable, want)
	}

	if h.CPUs != runtime.NumCPU() {
		t.Errorf("cpus = %d, this runner has %d", h.CPUs, runtime.NumCPU())
	}

	if want := opsMemTotalMiB(t); h.MemTotal < want-want/opsPercent || h.MemTotal > want+want/opsPercent {
		t.Errorf("memtotal = %d MB, /proc/meminfo reports %d MB", h.MemTotal, want)
	}

	var vms, vcpus, memory int

	for ns := range mmNamespaces(t) {
		for _, row := range vmInfo(t, ns) {
			cpu, _ := strconv.Atoi(row["vcpus"])
			mem, _ := strconv.Atoi(row["memory"])

			vms++
			vcpus += cpu
			memory += mem
		}
	}

	if h.VMs != vms || h.CPUCommit != vcpus || h.MemCommit != memory {
		t.Errorf("vms %d, cpucommit %d, memcommit %d; minimega runs %d VMs with %d vCPUs and %d MB",
			h.VMs, h.CPUCommit, h.MemCommit, vms, vcpus, memory)
	}

	if h.DiskUsage.Phenix <= 0 || h.DiskUsage.Phenix > opsPercent {
		t.Errorf("diskusage.diskphenix = %v, want a percentage", h.DiskUsage.Phenix)
	}
}

// checkPauseResume covers V-01: pause stops the VM's captures and pauses it,
// resume runs it again, and REST reports both states.
func (o *opsEnv) checkPauseResume(t *testing.T) {
	phenix(t, "vm", "capture", "start", opsExp, "h1", "0", "v01.pcap")

	rows := opsCaptures(t, "h1")
	if len(rows) != 1 || rows[0]["path"] != filepath.Join(o.filesDir, "v01.pcap") {
		t.Fatalf("capture rows for h1 = %v, want one writing %s", rows, filepath.Join(o.filesDir, "v01.pcap"))
	}

	phenix(t, "vm", "pause", opsExp, "h1")
	defer opsResume(t, "h1")

	if state := vmInfo(t, opsExp)["h1"]["state"]; state != opsPaused {
		t.Errorf("h1 is %s after phenix vm pause, want PAUSED", state)
	}

	if rows := opsCaptures(t, "h1"); len(rows) != 0 {
		t.Errorf("pausing h1 left its captures running: %v", rows)
	}

	if v := opsGetVM(t, "h1"); v.Running || v.State != opsPaused || len(v.Captures) != 0 {
		t.Errorf("REST reports paused h1 as running %v, state %q, %d captures", v.Running, v.State, len(v.Captures))
	}

	phenix(t, "vm", "resume", opsExp, "h1")

	if state := vmInfo(t, opsExp)["h1"]["state"]; state != opsRunning {
		t.Errorf("h1 is %s after phenix vm resume, want RUNNING", state)
	}

	if v := opsGetVM(t, "h1"); !v.Running || v.State != opsRunning {
		t.Errorf("REST reports resumed h1 as running %v, state %q", v.Running, v.State)
	}
}

// checkPauseResumeGuest covers V-01 on a booted real guest: phenix vm pause
// freezes k2 without rebooting it, and once phenix vm resume runs it again
// its C2 client answers. The pause is too short for minimega to drop the
// client; TestC2Flows pauses a guest until it does and checks the reconnect.
func (o *opsEnv) checkPauseResumeGuest(t *testing.T) {
	const bootID = "cat /proc/sys/kernel/random/boot_id"

	before := opsExec(t, "k2", bootID)

	phenix(t, "vm", "pause", opsExp, "k2")
	defer opsResume(t, "k2")

	if state := vmInfo(t, opsExp)["k2"]["state"]; state != opsPaused {
		t.Errorf("k2 is %s after phenix vm pause, want PAUSED", state)
	}

	if v := opsGetVM(t, "k2"); v.Running || v.State != opsPaused {
		t.Errorf("REST reports paused k2 as running %v, state %q", v.Running, v.State)
	}

	phenix(t, "vm", "resume", opsExp, "k2")

	if after := opsExec(t, "k2", bootID); after != before {
		t.Errorf("resumed k2 answers C2 with boot ID %q, want %q from before the pause", after, before)
	}

	if v := opsGetVM(t, "k2"); !v.Running || v.State != opsRunning || !v.CCActive {
		t.Errorf("REST reports resumed k2 as running %v, state %q, ccActive %v", v.Running, v.State, v.CCActive)
	}
}

// checkLabels covers V-07: labels change on a running VM in minimega, REST
// and the stored spec, and nothing else may change while it runs.
func (o *opsEnv) checkLabels(t *testing.T) {
	phenix(t, "vm", "set", opsExp, "h2", "--label-changes", "role=web")

	if tags := opsH2Tags(t); tags["role"] != "web" {
		t.Errorf("minimega tags of h2 = %v after --label-changes role=web", tags)
	}

	if v := opsGetVM(t, "h2"); v.Tags["role"] != "web" {
		t.Errorf("REST tags of h2 = %v after --label-changes role=web", v.Tags)
	}

	phenix(t, "vm", "set", opsExp, "h2", "--append-labels", "--label-changes", "tier=2")

	if tags := opsH2Tags(t); tags["role"] != "web" || tags["tier"] != "2" {
		t.Errorf("minimega tags of h2 = %v after appending tier=2, want role=web and tier=2", tags)
	}

	note := `say "hi" twice`

	phenix(t, "vm", "set", opsExp, "h2", "--append-labels", "--label-changes", "note="+note)

	want := map[string]string{"role": "web", "tier": "2", "note": note}

	if labels := opsNodeLabels(t, o.exp, "h2"); !maps.Equal(labels, want) {
		t.Errorf("stored labels of h2 = %v, want %v", labels, want)
	}

	if tags := opsH2Tags(t); tags["note"] != note {
		t.Errorf("minimega tag note of h2 = %q, want %q verbatim", tags["note"], note)
	}

	// minimega parses a namespaced vm tag a second time from a copy that quotes only arguments with
	// whitespace, and exits when that parse fails (pkg/minicli/input.go:50-60, cmd/minimega/cli.go:395).
	code, body := api(t, http.MethodPatch, opsVMPath("h2", ""), map[string]any{
		"tag_update_mode": "ADD",
		"tags":            map[string]string{"quote": `a"b`},
	})
	if code != http.StatusBadRequest || !strings.Contains(string(body), `label "quote" of VM h2`) {
		t.Errorf("PATCH label quote=a\"b of h2: status %d %q, want 400 naming h2 and the label", code, bytes.TrimSpace(body))
	}

	if tags := opsH2Tags(t); tags["note"] != note || tags["quote"] != "" {
		t.Errorf("minimega tags of h2 = %v after the refused label quote, want note %q and no quote", tags, note)
	}

	if labels := opsNodeLabels(t, o.exp, "h2"); !maps.Equal(labels, want) {
		t.Errorf("stored labels of h2 = %v after the refused label quote, want %v", labels, want)
	}

	out := phenixFails(t, "vm", "set", opsExp, "h2", "--cpu", "2")
	if !strings.Contains(out, "only interface connections and tags can be updated while experiment is running") {
		t.Errorf("phenix vm set --cpu on a running VM failed for another reason:\n%s", out)
	}

	if cpus := vmInfo(t, opsExp)["h2"]["vcpus"]; cpus != "1" {
		t.Errorf("h2 has %s vCPUs after the refused --cpu 2, want 1", cpus)
	}
}

// checkCdrom covers V-09: REST inserts and ejects an optical disc by absolute
// and relative path, and refuses a host file and a path minimega cannot
// carry.
func (o *opsEnv) checkCdrom(t *testing.T) {
	var (
		iso  = filepath.Join(cfg.imagesDir, opsISOImage)
		path = opsVMPath("h2", "/cdrom")
	)

	if code, body := api(t, http.MethodPost, path+"?isoPath="+url.QueryEscape(iso), nil); code != http.StatusNoContent {
		t.Fatalf("POST %s: status %d: %s", path, code, body)
	}

	if got := vmInfo(t, opsExp)["h2"]["cdrom"]; got != iso {
		t.Errorf("minimega cdrom of h2 = %q, want %q", got, iso)
	}

	if v := opsGetVM(t, "h2"); v.CdRom != iso {
		t.Errorf("REST cdRom of h2 = %q, want %q", v.CdRom, iso)
	}

	o.ws.waitAction(t, 10*time.Second, "experiment/vm", opsExp+"/h2", "cdrom-inserted")

	if code, body := api(t, http.MethodDelete, path, nil); code != http.StatusNoContent {
		t.Fatalf("DELETE %s: status %d: %s", path, code, body)
	}

	if got := vmInfo(t, opsExp)["h2"]["cdrom"]; got != "" {
		t.Errorf("minimega cdrom of h2 = %q after the eject, want none", got)
	}

	o.ws.waitAction(t, 10*time.Second, "experiment/vm", opsExp+"/h2", "cdrom-ejected")

	// minimega: vm cdrom eject discards its error (cmd/minimega/vm_cli.go:632-645), so ejecting an empty drive succeeds.
	code, body := api(t, http.MethodDelete, path, nil)
	t.Logf("ejecting the empty drive again: status %d %s", code, bytes.TrimSpace(body))

	// minimega stats a relative path against the daemon's working directory, not -filepath
	// (cmd/minimega/vm_cli.go:647-650), so phenix sends the path in the images directory it checked.
	if code, body := api(t, http.MethodPost, path+"?isoPath="+url.QueryEscape(opsISOImage), nil); code != http.StatusNoContent {
		t.Errorf("inserting %s by a path relative to the images directory: status %d: %s", opsISOImage, code, body)
	} else if got := vmInfo(t, opsExp)["h2"]["cdrom"]; got != iso {
		t.Errorf("minimega cdrom of h2 = %q after inserting %s, want %q", got, opsISOImage, iso)
	}

	if code, body := api(t, http.MethodDelete, path, nil); code != http.StatusNoContent {
		t.Fatalf("DELETE %s: status %d: %s", path, code, body)
	}

	// A host file would be attached by QEMU as root, and a quote made minimega exit when it parsed the
	// namespaced command again; phenix refuses both before minimega sees them.
	for _, refused := range []string{"/etc/hostname", `a"b.iso`} {
		code, body := api(t, http.MethodPost, path+"?isoPath="+url.QueryEscape(refused), nil)
		if code != http.StatusBadRequest || !strings.Contains(string(body), "VM h2") {
			t.Errorf("inserting %q: status %d: %s, want 400 naming h2", refused, code, bytes.TrimSpace(body))
		}
	}

	if got := vmInfo(t, opsExp)["h2"]["cdrom"]; got != "" {
		t.Errorf("minimega cdrom of h2 = %q after the refused discs, want none", got)
	}
}

// checkScreenshots covers W-01 (a) to (d) and the size of cached
// screenshots.
func (o *opsEnv) checkScreenshots(t *testing.T) {
	path := opsVMPath("h2", "/screenshot.png")

	code, hdr, data := apiGet(t, path+"?size="+strconv.Itoa(opsScreenshotSize))
	if code != http.StatusOK || hdr.Get("Content-Type") != "image/png" {
		t.Fatalf("GET %s: status %d, Content-Type %q: %.200s", path, code, hdr.Get("Content-Type"), data)
	}

	if w, h := opsPNGSize(t, data); max(w, h) > opsScreenshotSize {
		t.Errorf("screenshot of h2 is %dx%d, want at most %d on its longer side", w, h, opsScreenshotSize)
	}

	if code, _, small := apiGet(t, path+"?size="+strconv.Itoa(opsSmallShot)); code != http.StatusOK {
		t.Errorf("GET %s?size=%d: status %d", path, opsSmallShot, code)
	} else if w, h := opsPNGSize(t, small); max(w, h) > opsSmallShot {
		t.Errorf("screenshot of h2 at size %d is %dx%d", opsSmallShot, w, h)
	}

	code, _, data = apiGet(t, path+"?base64=1")

	encoded, ok := strings.CutPrefix(string(data), "data:image/png;base64,")
	if code != http.StatusOK || !ok {
		t.Errorf("GET %s?base64=1: status %d: %.80s", path, code, data)
	} else if _, err := png.DecodeConfig(opsBase64Reader(encoded)); err != nil {
		t.Errorf("GET %s?base64=1 is not a base64 PNG: %v", path, err)
	}

	var list struct {
		VMs []opsVM `json:"vms"`
	}

	apiOK(t, http.MethodGet, "/experiments/"+opsExp+"/vms?screenshot="+strconv.Itoa(opsScreenshotSize), nil, &list)

	for _, v := range list.VMs {
		if v.Running != strings.HasPrefix(v.Screenshot, "data:image/png;base64,") {
			t.Errorf("VM list: %s is running %v with screenshot %.40q", v.Name, v.Running, v.Screenshot)
		}
	}

	phenix(t, "vm", "pause", opsExp, "h2")
	defer opsResume(t, "h2")

	var body []byte

	eventually(t, opsScreenshotCache+opsStateWait, time.Second, "the screenshot of paused h2 to be refused", func() (bool, string) {
		code, _, body = apiGet(t, path)

		return code != http.StatusOK, fmt.Sprintf("status %d", code)
	})

	if code != http.StatusConflict || !strings.Contains(string(body), "VM is not running") {
		t.Errorf("screenshot of paused h2: status %d %q, want 409 saying the VM is not running", code, bytes.TrimSpace(body))
	}
}

// checkVNC covers W-02: the noVNC page and its websocket, which proxies the
// VM's VNC server at the host and port minimega reports.
func (o *opsEnv) checkVNC(t *testing.T) {
	path := opsVMPath("h2", "/vnc")

	code, hdr, page := apiGet(t, path)
	if code != http.StatusOK || !strings.HasPrefix(hdr.Get("Content-Type"), "text/html") {
		t.Fatalf("GET %s: status %d, Content-Type %q", path, code, hdr.Get("Content-Type"))
	}

	for _, want := range []string{"<title>h2 - " + opsExp + "</title>", "EXP: " + opsExp + " - VM: h2"} {
		if !strings.Contains(string(page), want) {
			t.Errorf("VNC page of h2 lacks %q", want)
		}
	}

	if code, _, _ := apiGet(t, opsVMPath("nosuchvm", "/vnc")); code != http.StatusNotFound {
		t.Errorf("VNC page of a VM that does not exist: status %d, want 404", code)
	}

	port := vmInfo(t, opsExp)["h2"]["vnc_port"]

	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", port), 5*time.Second)
	if err != nil {
		t.Fatalf("dialing the VNC port %s minimega reports for h2: %v", port, err)
	}

	greeting := make([]byte, len(opsRFBGreeting))
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	_, err = io.ReadFull(conn, greeting)
	_ = conn.Close()

	if err != nil || string(greeting) != opsRFBGreeting {
		t.Errorf("VNC port %s greets with %q (%v), want %q", port, greeting, err, opsRFBGreeting)
	}

	ws, _, err := websocket.DefaultDialer.Dial(
		"ws://"+cfg.uiAddr+"/api/v1"+path+"/ws",
		http.Header{"Origin": []string{"http://" + cfg.uiAddr}},
	)
	if err != nil {
		t.Fatalf("connecting to the VNC websocket of h2: %v", err)
	}
	defer ws.Close()

	var got []byte

	_ = ws.SetReadDeadline(time.Now().Add(10 * time.Second))

	for len(got) < len(opsRFBGreeting) {
		typ, data, err := ws.ReadMessage()
		if err != nil {
			t.Fatalf("reading the VNC websocket of h2 after %q: %v", got, err)
		}

		if typ != websocket.BinaryMessage {
			t.Errorf("VNC websocket sent a message of type %d, want binary", typ)
		}

		got = append(got, data...)
	}

	if !bytes.HasPrefix(got, []byte(opsRFBGreeting)) {
		t.Errorf("VNC websocket of h2 starts with %q, want %q", got, opsRFBGreeting)
	}
}

// checkWebsocket covers W-04: pause, start, redeploy and snapshot of h2
// through REST, each announced on the websocket before and after.
func (o *opsEnv) checkWebsocket(t *testing.T) {
	var v opsVM

	apiOK(t, http.MethodPost, opsVMPath("h2", "/stop"), nil, &v)
	defer opsResume(t, "h2")

	if v.State != opsPaused || v.Running {
		t.Errorf("REST stop of h2 returned state %q, running %v", v.State, v.Running)
	}

	apiOK(t, http.MethodPost, opsVMPath("h2", "/start"), nil, &v)

	if v.State != opsRunning || !v.Running {
		t.Errorf("REST start of h2 returned state %q, running %v", v.State, v.Running)
	}

	id := vmInfo(t, opsExp)["h2"]["id"]

	apiOK(t, http.MethodPost, opsVMPath("h2", "/redeploy"), map[string]any{}, &v)

	if !v.Running || v.RAM != opsHollowMemMiB {
		t.Errorf("REST redeploy of h2 returned running %v, ram %d", v.Running, v.RAM)
	}

	if row := vmInfo(t, opsExp)["h2"]; row["state"] != opsRunning || row["id"] == id {
		t.Errorf("after the REST redeploy minimega runs h2 as %q with id %s, want a new VM (was id %s) RUNNING",
			row["state"], row["id"], id)
	}

	if code, body := api(
		t,
		http.MethodPost,
		opsVMPath("h2", "/snapshots"),
		map[string]string{"filename": "w04"},
	); code != http.StatusNoContent {
		t.Fatalf("POST snapshot of h2: status %d: %s", code, body)
	}

	full := opsExp + "/h2"

	o.ws.waitAction(t, 10*time.Second, "experiment/vm", full, "redeployed")
	o.ws.waitAction(t, 10*time.Second, "experiment/vm/snapshot", full, "create")

	var (
		events []string
		bare   []string
	)

	for _, m := range o.ws.messages("experiment/vm", "") {
		switch m.Resource.Name {
		case full:
			events = append(events, m.Resource.Action)
		case "h2":
			events = append(events, m.Resource.Action)
			bare = append(bare, m.Resource.Action)
		}

		o.checkVMResult(t, m)
	}

	if !opsInOrder(events, "stopping", "stop", "starting", "start", "redeploying", "redeployed") {
		t.Errorf(
			"websocket experiment/vm events for h2 = %v, want stopping, stop, starting, start, redeploying, redeployed in order",
			events,
		)
	}

	if len(bare) > 0 {
		t.Errorf("websocket events %v name h2 without its experiment, want %s like the others", bare, full)
	}

	o.checkSnapshotEvents(t, "h2", "creating", "create")

	for _, ext := range []string{".state", ".hdd"} {
		opsNonEmpty(t, filepath.Join(o.filesDir, "h2__w04"+ext))
	}
}

// checkVMResult checks the VM a websocket experiment/vm event of h2 carries.
func (o *opsEnv) checkVMResult(t *testing.T, m wsMessage) {
	t.Helper()

	if m.Resource.Name != opsExp+"/h2" {
		return
	}

	want := map[string]string{"stop": opsPaused, "start": opsRunning, "redeployed": opsRunning}

	state, ok := want[m.Resource.Action]
	if !ok && m.Resource.Action != "redeploying" {
		return
	}

	var v opsVM
	if err := json.Unmarshal(m.Result, &v); err != nil || v.Name != "h2" {
		t.Errorf("websocket %s event of h2 carries %.200s (%v), want the VM", m.Resource.Action, m.Result, err)

		return
	}

	if ok && v.State != state {
		t.Errorf("websocket %s event of h2 carries state %q, want %s", m.Resource.Action, v.State, state)
	}

	if m.Resource.Action == "redeploying" && !v.Busy {
		t.Error("websocket redeploying event of h2 carries a VM that is not busy")
	}
}

// checkSnapshotEvents checks that the websocket announced a snapshot action
// of vm before its completion, with progress in between as a fraction. A
// progress of 0 is valid: QEMU may report a migration that has not sent a
// page yet.
func (o *opsEnv) checkSnapshotEvents(t *testing.T, vm, first, last string) {
	t.Helper()

	var actions []string

	for _, m := range o.ws.messages("experiment/vm/snapshot", opsExp+"/"+vm) {
		actions = append(actions, m.Resource.Action)

		if m.Resource.Action != "progress" {
			continue
		}

		var p struct {
			Percent float64 `json:"percent"`
		}

		if err := json.Unmarshal(m.Result, &p); err != nil || p.Percent < 0 || p.Percent > 1 {
			t.Errorf("snapshot progress of %s = %s (%v), want a fraction in [0, 1]", vm, m.Result, err)
		}
	}

	if !opsInOrder(actions, first, last) {
		t.Errorf("websocket snapshot events of %s = %v, want %s then %s", vm, actions, first, last)
	}

	t.Logf("snapshot events of %s: %v", vm, actions)
}

// checkSnapshotNoDiskSnapshot covers S-02b: a snapshot of a VM whose disk is
// not a minimega snapshot cannot hold the disk, so it is refused before
// anything is saved.
func (o *opsEnv) checkSnapshotNoDiskSnapshot(t *testing.T) {
	code, body := api(t, http.MethodPost, opsVMPath("hn", "/snapshots"), map[string]string{"filename": "s2b"})
	if code == http.StatusNoContent {
		t.Fatal("snapshot of hn, which has snapshot: false, succeeded")
	}

	if code != http.StatusBadRequest || !strings.Contains(string(body), "cannot snapshot VM hn: it runs with snapshot: false") {
		t.Errorf("snapshot of hn: status %d %q, want 400 naming hn and its snapshot: false", code, bytes.TrimSpace(body))
	}

	opsWaitState(t, "hn", opsRunning)

	var leftovers []string

	for _, path := range []string{
		filepath.Join(o.filesDir, "hn__s2b.state"),
		filepath.Join(cfg.imagesDir, opsExp+"_hn__s2b.state"),
	} {
		if _, err := os.Stat(path); err == nil {
			leftovers = append(leftovers, path)
		}
	}

	if len(leftovers) > 0 {
		t.Errorf("the refused snapshot of hn left %v behind", leftovers)
	}
}

// checkSnapshotPaused covers S-02c and records what a memory dump of a
// paused VM does.
func (o *opsEnv) checkSnapshotPaused(t *testing.T) {
	phenix(t, "vm", "pause", opsExp, "h1")
	defer opsResume(t, "h1")

	code, body := api(t, http.MethodPost, opsVMPath("h1", "/snapshots"), map[string]string{"filename": "s2c"})
	if code == http.StatusNoContent || !strings.Contains(string(body), "VM is not running") {
		t.Errorf("snapshot of paused h1: status %d: %s", code, bytes.TrimSpace(body))
	}

	began := time.Now()
	out, err := phenixRun("vm", "memory-snapshot", opsExp, "h1", "mempaused")

	if st, statErr := os.Stat(filepath.Join(o.filesDir, "mempaused.elf")); err == nil && statErr == nil {
		t.Logf("a memory dump of paused h1 completed in %v: %d bytes", time.Since(began).Round(time.Second), st.Size())
	} else {
		t.Logf("a memory dump of paused h1 failed after %v (%v, stat %v):\n%s", time.Since(began).Round(time.Second), err, statErr, out)
	}

	if state := vmInfo(t, opsExp)["h1"]["state"]; state != opsPaused {
		t.Errorf("h1 is %s after the dump, want it still PAUSED", state)
	}
}

// checkMemoryDumpCLI covers S-04a: phenix vm memory-snapshot writes an ELF
// core of all guest memory into the experiment's files.
func (o *opsEnv) checkMemoryDumpCLI(t *testing.T) {
	phenix(t, "vm", "memory-snapshot", opsExp, "h2", "mem1")

	opsELF(t, filepath.Join(o.filesDir, "mem1.elf"), opsHollowMemMiB-1)
	opsWantListed(t, "mem1.elf")

	if state := vmInfo(t, opsExp)["h2"]["state"]; state != opsRunning {
		t.Errorf("h2 is %s after the memory dump, want RUNNING", state)
	}
}

// checkResetDiskNoSnapshot covers V-06 for hn: reset-disk needs a minimega
// disk snapshot.
func (o *opsEnv) checkResetDiskNoSnapshot(t *testing.T) {
	out := phenixFails(t, "vm", "reset-disk", opsExp, "hn")
	if !strings.Contains(out, "snapshot flag for hn was not set") {
		t.Errorf("reset-disk of hn failed for another reason:\n%s", out)
	}

	if state := vmInfo(t, opsExp)["hn"]["state"]; state != opsRunning {
		t.Errorf("hn is %s after the refused reset-disk, want RUNNING", state)
	}
}

// ---------------------------------------------------------------------------
// real guests

// checkNet covers V-08: an interface disconnected from its VLAN carries no
// traffic and cannot be captured, and reconnects to the experiment's default
// bridge on the same VLAN.
func (o *opsEnv) checkNet(t *testing.T) {
	before := vmInfo(t, opsExp)["k2"]

	if got := opsPing(t, true); got != "reachable" {
		t.Fatalf("k1 cannot reach k2 over VLAN LAN before the disconnect: %s", got)
	}

	phenix(t, "vm", "net", "disconnect", opsExp, "k2", "0")

	// Reconnect k2 if a check below ends the test before the reconnect, so
	// the later checks on k2 start from its topology network.
	reconnected := false

	defer func() {
		if !reconnected {
			if out, err := phenixRun("vm", "net", "connect", opsExp, "k2", "0", opsLAN); err != nil {
				t.Errorf("reconnecting k2 after a failed check: %v\n%s", err, out)
			}
		}
	}()

	if vlan := vmInfo(t, opsExp)["k2"]["vlan"]; strings.Trim(vlan, "[]") != "disconnected" {
		t.Errorf("minimega vlan of k2 = %q after the disconnect, want disconnected", vlan)
	}

	if v := opsGetVM(t, "k2"); !slices.Equal(v.Networks, []string{"disconnected"}) {
		t.Errorf("REST networks of k2 = %v after the disconnect, want [disconnected]", v.Networks)
	}

	if got := opsPing(t, false); got != "unreachable" {
		t.Errorf("k1 still reaches disconnected k2: %s", got)
	}

	out := phenixFails(t, "vm", "capture", "start", opsExp, "k2", "0", "v08.pcap")
	if !strings.Contains(out, "cannot capture on a disconnected interface") {
		t.Errorf("capture on the disconnected interface failed for another reason:\n%s", out)
	}

	if rows := opsCaptures(t, "k2"); len(rows) != 0 {
		t.Errorf("minimega captures k2 after the refused capture: %v", rows)
	}

	phenix(t, "vm", "net", "connect", opsExp, "k2", "0", opsLAN)

	reconnected = true
	after := vmInfo(t, opsExp)["k2"]
	if after["vlan"] != before["vlan"] || strings.Trim(after["bridge"], "[]") != opsBridge {
		t.Errorf("k2 vlan %s on bridge %s after the reconnect, want %s on %s", after["vlan"], after["bridge"], before["vlan"], opsBridge)
	}

	opsWantTapBridge(t, after)

	if got := opsPing(t, true); got != "reachable" {
		t.Errorf("k1 cannot reach k2 after the reconnect: %s", got)
	}
}

// checkRestart covers V-02 (a): restart of a running VM resets the guest
// through QMP without relaunching QEMU.
func (o *opsEnv) checkRestart(t *testing.T) {
	o.k1BootID = opsExec(t, "k1", "cat /proc/sys/kernel/random/boot_id")
	before := vmInfo(t, opsExp)["k1"]

	o.restartAt = time.Now()

	phenix(t, "vm", "restart", opsExp, "k1")

	after := vmInfo(t, opsExp)["k1"]
	if after["state"] != opsRunning || after["id"] != before["id"] || after["pid"] != before["pid"] {
		t.Errorf("k1 after restart: state %s id %s pid %s, want RUNNING with id %s and QEMU pid %s",
			after["state"], after["id"], after["pid"], before["id"], before["pid"])
	}
}

// checkRedeploy covers V-05 (a) to (d): redeploy through the CLI and REST
// relaunches k2 as a new VM of the size asked for, and otherwise keeps it.
func (o *opsEnv) checkRedeploy(t *testing.T) {
	mem := strconv.Itoa(opsK2MemMiB)
	row := vmInfo(t, opsExp)["k2"]

	phenix(t, "vm", "redeploy", opsExp, "k2", "--cpu", opsK2CPUs, "--mem", mem)
	row = opsCheckRedeployed(t, row, mem)
	opsWantK2Snapshot(t, row, "--cpu "+opsK2CPUs+" --mem "+mem)

	phenix(t, "vm", "redeploy", opsExp, "k2")
	row = opsCheckRedeployed(t, row, mem)
	opsWantK2Snapshot(t, row, "the CLI without flags")

	var v opsVM

	apiOK(t, http.MethodPost, opsVMPath("k2", "/redeploy"), map[string]any{}, &v)

	if strconv.Itoa(v.Cpus) != opsK2CPUs || v.RAM != opsK2MemMiB {
		t.Errorf("REST redeploy of k2 with {} returned %d CPUs and %d MB, want 2 and %d", v.Cpus, v.RAM, opsK2MemMiB)
	}

	row = opsCheckRedeployed(t, row, mem)
	opsWantK2Snapshot(t, row, "REST with {}")

	code, body := api(t, http.MethodPost, opsVMPath("k2", "/redeploy"), nil)
	if code != http.StatusOK {
		t.Fatalf("REST redeploy of k2 with an empty body: status %d: %s", code, body)
	}

	row = opsCheckRedeployed(t, row, mem)

	opsWantK2Snapshot(t, row, "REST with an empty body")
}

// checkRebooted covers V-02 (b): the guest reset by checkRestart booted again
// and talks C2 with the same UUID.
func (o *opsEnv) checkRebooted(t *testing.T) {
	if o.k1BootID == "" {
		t.Fatal("no boot ID was recorded before the restart")
	}

	out, ok := opsTryExec(t, "k1", guestBootTimeout, "cat /proc/sys/kernel/random/boot_id; cut -d' ' -f1 /proc/uptime")
	if !ok {
		t.Fatalf("k1 did not answer C2 within %v of the restart", time.Since(o.restartAt).Round(time.Second))
	}

	fields := strings.Fields(out)
	if len(fields) != 2 {
		t.Fatalf("k1 answered %q", out)
	}

	if fields[0] == o.k1BootID {
		t.Errorf("k1 still runs boot %s after phenix vm restart", fields[0])
	}

	if up, err := strconv.ParseFloat(fields[1], 64); err != nil || up > time.Since(o.restartAt).Seconds() {
		t.Errorf("k1 uptime %s s, but it was reset %v ago", fields[1], time.Since(o.restartAt).Round(time.Second))
	}

	eventually(t, opsStateWait, 2*time.Second, "REST to report C2 for k1", func() (bool, string) {
		v := opsGetVM(t, "k1")

		return v.CCActive, fmt.Sprintf("ccActive %v", v.CCActive)
	})
}

// checkRedeployedGuest covers V-05 (e): the guest of the last redeploy boots
// with the size, hostname and address it had before.
func (o *opsEnv) checkRedeployedGuest(t *testing.T) {
	uuid := strings.ToLower(vmInfo(t, opsExp)["k2"]["uuid"])

	opsWaitClient(t, uuid)

	// minimega would expand awk's $2, so the guest reports raw values and the
	// test converts them.
	out := opsExec(t, "k2", "nproc; grep '^MemTotal:' /proc/meminfo")

	fields := strings.Fields(out) // nproc, "MemTotal:", kB, "kB"
	if len(fields) != 4 {
		t.Fatalf("k2 answered %q", out)
	}

	kb, _ := strconv.Atoi(fields[2])
	if mem := kb / 1024; fields[0] != opsK2CPUs || mem > opsK2MemMiB || mem < opsK2MemMiB*4/5 {
		t.Errorf("redeployed k2 sees %s CPUs and %d MB, want 2 and about %d", fields[0], mem, opsK2MemMiB)
	}

	// A wrong hostname must not hide the address check, so this polls
	// without eventually, which would end the subtest.
	host := opsClients(t)[uuid]
	for deadline := time.Now().Add(time.Minute); host != "k2" && time.Now().Before(deadline); host = opsClients(t)[uuid] {
		time.Sleep(2 * time.Second)
	}

	if host != "k2" {
		t.Errorf("redeployed k2 still reports hostname %q after a minute, want k2", host)
	}

	script := fmt.Sprintf(`for i in $(seq 15); do ip -4 -o addr show | grep -q ' %s/24 ' && break; sleep 2; done
ip -4 -o addr show | grep -q ' %[1]s/24 ' && echo ok || echo missing`, opsK2Addr)

	if got := opsExec(t, "k2", script); got != "ok" {
		t.Errorf("redeployed k2 lacks its address %s/24", opsK2Addr)
	}
}

// checkSnapshotRestore covers S-01: a snapshot saves the guest's disk and
// memory, the guest keeps running, and a restore brings back the memory as it
// was, under the same UUID.
func (o *opsEnv) checkSnapshotRestore(t *testing.T) {
	tokA := fmt.Sprintf("e2e-ops-before-%d", time.Now().UnixNano())
	tokB := fmt.Sprintf("e2e-ops-after-%d", time.Now().UnixNano())

	if got := opsExec(t, "k2", "echo "+tokA+" > /run/e2e-ops-m && cat /run/e2e-ops-m"); got != tokA {
		t.Fatalf("writing the memory marker in k2: %q", got)
	}

	if code, body := api(
		t,
		http.MethodPost,
		opsVMPath("k2", "/snapshots"),
		map[string]string{"filename": "s1"},
	); code != http.StatusNoContent {
		t.Fatalf("POST snapshot of k2: status %d: %s", code, body)
	}

	o.ws.waitAction(t, 10*time.Second, "experiment/vm/snapshot", opsExp+"/k2", "create")
	o.checkSnapshotEvents(t, "k2", "creating", "create")

	for _, ext := range []string{".state", ".hdd"} {
		opsNonEmpty(t, filepath.Join(o.filesDir, "k2__s1"+ext))
	}

	if info := opsQemuImg(t, filepath.Join(o.filesDir, "k2__s1.hdd")); info.Format != "qcow2" {
		t.Errorf("the saved disk of k2 is %q, want qcow2", info.Format)
	} else {
		t.Logf("the saved disk of k2 is backed by %q", info.BackingFilename)
	}

	var snaps struct {
		Snapshots []string `json:"snapshots"`
	}

	apiOK(t, http.MethodGet, opsVMPath("k2", "/snapshots"), nil, &snaps)

	if !slices.Contains(snaps.Snapshots, "k2__s1") {
		t.Errorf("GET snapshots of k2 = %v, want k2__s1", snaps.Snapshots)
	}

	before := opsWaitState(t, "k2", opsRunning)

	if got := opsExec(t, "k2", "echo "+tokB+" > /run/e2e-ops-m && cat /run/e2e-ops-m"); got != tokB {
		t.Fatalf("k2 does not run after its snapshot: %q", got)
	}

	if code, body := api(t, http.MethodPost, opsVMPath("k2", "/snapshots/k2__s1"), nil); code != http.StatusNoContent {
		t.Fatalf("POST restore of k2: status %d: %s", code, body)
	}

	o.ws.waitAction(t, 10*time.Second, "experiment/vm/snapshot", opsExp+"/k2", "restore")

	after := opsWaitState(t, "k2", opsRunning)
	if after["uuid"] != before["uuid"] || after["id"] == before["id"] {
		t.Errorf(
			"restored k2 has uuid %s and id %s, want uuid %s on a new VM (was id %s)",
			after["uuid"],
			after["id"],
			before["uuid"],
			before["id"],
		)
	}

	if up, err := time.ParseDuration(after["uptime"]); err != nil || up > time.Minute {
		t.Errorf("restored k2 reports uptime %q, want a fresh launch", after["uptime"])
	}

	o.checkRestoredMemory(t, tokA, tokB)
}

// checkRestoredMemory checks a restored k2 from the inside: over C2 when it
// comes back, and always in a memory dump, which must hold the marker written
// before the snapshot and not the one written after it.
func (o *opsEnv) checkRestoredMemory(t *testing.T, tokA, tokB string) {
	t.Helper()

	began := time.Now()

	if got, ok := opsTryExec(t, "k2", opsRestoreC2Watch, "cat /run/e2e-ops-m"); ok {
		t.Logf("C2 came back to restored k2 after %v", time.Since(began).Round(time.Second))

		if got != tokA {
			t.Errorf("restored k2 holds %q in /run, want %q from before the snapshot", got, tokA)
		}
	} else {
		// minimega: no C2 answer from a restored guest within the watch window.
		t.Logf("minimega: restored k2 did not answer C2 within %v", opsRestoreC2Watch)
	}

	o.restoredTokens = [2]string{tokA, tokB}
}

// checkMemoryDumpREST covers S-04b: a memory dump through REST holds the
// memory k2 was restored to.
func (o *opsEnv) checkMemoryDumpREST(t *testing.T) {
	tokA, tokB := o.restoredTokens[0], o.restoredTokens[1]
	if tokA == "" {
		t.Fatal("S-01 did not record its memory markers")
	}

	if code, body := api(
		t,
		http.MethodPost,
		opsVMPath("k2", "/memorySnapshot"),
		map[string]string{"filename": "mem2"},
	); code != http.StatusNoContent {
		t.Fatalf("POST memory snapshot of k2: status %d: %s", code, body)
	}

	path := filepath.Join(o.filesDir, "mem2.elf")

	opsELF(t, path, opsK2MemMiB-1)
	opsWantListed(t, "mem2.elf")

	found := opsScan(t, path, tokA, tokB)

	if !found[tokA] {
		t.Errorf("the memory of restored k2 lacks %s, which it held when it was saved", tokA)
	}

	if found[tokB] {
		t.Errorf("the memory of restored k2 holds %s, which was written after the snapshot it was restored from", tokB)
	}

	if actions := o.ws.actions("experiment/vm/memorySnapshot", opsExp+"/k2"); !opsInOrder(actions, "committing", "commit") {
		t.Errorf("websocket memory snapshot events of k2 = %v, want committing then commit", actions)
	}
}

// checkShutdown covers V-03 (a) and (b): phenix vm shutdown powers a guest
// off through ACPI and keeps the VM, whose disk then holds what the guest
// wrote; a second shutdown does nothing.
func (o *opsEnv) checkShutdown(t *testing.T) {
	tok := fmt.Sprintf("e2e-ops-commit-%d", time.Now().UnixNano())

	script := "echo " + tok + " > /root/e2e-ops-commit && touch /root/e2e-ops-reset && sync && echo written"
	if got := opsExec(t, "k1", script); got != "written" {
		t.Fatalf("writing the disk markers in k1: %q", got)
	}

	o.commitToken = tok
	before := vmInfo(t, opsExp)["k1"]

	began := time.Now()

	phenix(t, "vm", "shutdown", opsExp, "k1")

	took := time.Since(began)

	if row := vmInfo(t, opsExp)["k1"]; row["state"] != opsQuit || row["id"] != before["id"] {
		t.Fatalf("k1 is %q with id %q after phenix vm shutdown, want QUIT with id %s", row["state"], row["id"], before["id"])
	}

	if took >= opsShutdownFallback {
		t.Errorf("k1 took %v to stop: it ignored the ACPI power button and phenix killed it", took.Round(time.Second))
	} else {
		t.Logf("k1 powered off on ACPI in %v", took.Round(time.Second))
	}

	eventually(t, opsStateWait, time.Second, "REST to report k1 powered off", func() (bool, string) {
		v := opsGetVM(t, "k1")

		return v.State == opsQuit && !v.Running && !v.CCActive, fmt.Sprintf("%+v", v)
	})

	overlay := filepath.Join(cfg.mmBase, before["id"], "disk-0.qcow2")

	opsInspectDisk(t, overlay, func(root string) {
		got, ok := guestFile(root, "root/e2e-ops-commit")

		commitOK := ok && strings.TrimSpace(got) == tok
		if !commitOK {
			t.Errorf("the disk of powered-off k1 holds %q in /root/e2e-ops-commit, want %q", got, tok)
		}

		_, resetOK := guestFile(root, "root/e2e-ops-reset")
		if !resetOK {
			t.Error("the disk of powered-off k1 lacks /root/e2e-ops-reset")
		}

		if commitOK && resetOK {
			o.markedOverlay = overlay
		}
	})

	began = time.Now()

	phenix(t, "vm", "shutdown", opsExp, "k1")

	if took := time.Since(began); took > opsStateWait/3 {
		t.Errorf("shutting down powered-off k1 again took %v, want an immediate no-op", took.Round(time.Second))
	}

	if row := vmInfo(t, opsExp)["k1"]; row["state"] != opsQuit || row["id"] != before["id"] {
		t.Errorf("k1 is %q with id %q after a second shutdown, want QUIT with id %s", row["state"], row["id"], before["id"])
	}
}

// checkCommit covers S-03: commit writes the powered-off guest's disk into a
// new standalone image and starts the VM again.
func (o *opsEnv) checkCommit(t *testing.T) {
	if o.commitToken == "" {
		t.Fatal("V-03a did not write the commit marker")
	}

	var (
		path = opsVMPath("k1", "/commit")
		id   = vmInfo(t, opsExp)["k1"]["id"]
	)

	code, body := api(t, http.MethodPost, path, nil)
	if code != http.StatusBadRequest || !strings.Contains(string(body), "must provide new disk name for commit") {
		t.Errorf("commit of k1 without a body: status %d: %s", code, bytes.TrimSpace(body))
	}

	// A commit must not overwrite a host path or an existing image, such as one
	// other VMs boot from; phenix refuses both before it writes.
	var (
		iso   = filepath.Join(cfg.imagesDir, opsISOImage)
		state = vmInfo(t, opsExp)["k1"]["state"]
	)

	before, err := os.Stat(iso)
	if err != nil {
		t.Fatal(err)
	}

	for filename, want := range map[string]string{
		"../e2e-ops-commit.qc2": "invalid file name",
		iso:                     "invalid file name",
		opsISOImage:             "disk image already exists",
	} {
		code, body := api(t, http.MethodPost, path, map[string]string{"filename": filename})
		if code != http.StatusBadRequest || !strings.Contains(string(body), want) {
			t.Errorf("commit of k1 to %q: status %d: %s, want 400 saying %q", filename, code, bytes.TrimSpace(body), want)
		}
	}

	if after, err := os.Stat(iso); err != nil || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		t.Errorf("%s changed after the refused commits: %v", iso, err)
	}

	if got := vmInfo(t, opsExp)["k1"]["state"]; got != state {
		t.Errorf("k1 is %s after the refused commits, want %s as before", got, state)
	}

	began := time.Now()

	var resp struct {
		Disk string `json:"disk"`
		VM   opsVM  `json:"vm"`
	}

	apiOK(t, http.MethodPost, path, map[string]string{"filename": opsCommitImg}, &resp)
	t.Logf("commit of k1 took %v", time.Since(began).Round(time.Second))

	if resp.Disk != opsCommitImg || resp.VM.Name != "k1" {
		t.Errorf("commit of k1 returned disk %q and VM %q", resp.Disk, resp.VM.Name)
	}

	img := filepath.Join(cfg.imagesDir, opsCommitImg)

	if info := opsQemuImg(t, img); info.Format != "qcow2" || info.BackingFilename != "" {
		t.Errorf("committed image is %q backed by %q, want a standalone qcow2", info.Format, info.BackingFilename)
	}

	opsInspectDisk(t, img, func(root string) {
		if got, ok := guestFile(root, "root/e2e-ops-commit"); !ok || strings.TrimSpace(got) != o.commitToken {
			t.Errorf("committed image holds %q in /root/e2e-ops-commit, want %q from the guest", got, o.commitToken)
		}

		if _, ok := guestFile(root, "etc/phenix/startup/1_hostname-start.sh"); !ok {
			t.Error("committed image lacks the startup script phenix injected into k1's snapshot")
		}
	})

	if row := opsWaitState(t, "k1", opsRunning); row["id"] != id {
		t.Errorf("k1 runs with id %s after the commit, want the same VM (%s)", row["id"], id)
	}

	if actions := o.ws.actions("experiment/vm/commit", opsExp+"/k1"); !opsInOrder(actions, "committing", "commit") {
		t.Errorf("websocket commit events of k1 = %v, want committing then commit", actions)
	}
}

// checkResetDisk covers V-06 for k1: reset-disk puts a fresh minimega
// snapshot of the injected disk under the running VM.
func (o *opsEnv) checkResetDisk(t *testing.T) {
	// The markers' absence below proves the reset only if they were there.
	if o.markedOverlay == "" {
		t.Fatal("V-03a did not leave both disk markers on k1's instance disk")
	}

	var (
		before  = opsWaitState(t, "k1", opsRunning)
		overlay = filepath.Join(cfg.mmBase, before["id"], "disk-0.qcow2")
	)

	// minimega creates a VM's instance disk only at its first launch, so the
	// commit's relaunch kept the one the markers were found on.
	if overlay != o.markedOverlay {
		t.Fatalf("k1 runs on %s, not on %s, which V-03a found the markers on", overlay, o.markedOverlay)
	}

	phenix(t, "vm", "reset-disk", opsExp, "k1")

	// minimega's disk snapshot rewrites the overlay in place (qemu-img create
	// keeps its inode), so the markers below prove the reset, and a new QEMU
	// proves the VM was relaunched on it.
	if row := opsWaitState(t, "k1", opsRunning); row["id"] != before["id"] || row["pid"] == before["pid"] {
		t.Errorf("k1 runs with id %s and QEMU pid %s after reset-disk, want the same VM (%s) relaunched with a new pid (not %s)",
			row["id"], row["pid"], before["id"], before["pid"])
	}

	if info := opsQemuImg(t, overlay); !strings.Contains(info.BackingFilename, "_"+opsExp+"_k1_snapshot") {
		t.Errorf("the reset disk of k1 is backed by %q, want its injection snapshot", info.BackingFilename)
	}

	opsInspectDisk(t, overlay, func(root string) {
		for _, marker := range []string{"root/e2e-ops-commit", "root/e2e-ops-reset"} {
			if _, ok := guestFile(root, marker); ok {
				t.Errorf("the reset disk of k1 still holds /%s", marker)
			}
		}

		if _, ok := guestFile(root, "etc/phenix/startup/1_hostname-start.sh"); !ok {
			t.Error("the reset disk of k1 lacks the startup script phenix injected")
		}
	})
}

// checkRestartFromQuit covers V-03 (c): restart of a powered-off VM launches
// it again.
func (o *opsEnv) checkRestartFromQuit(t *testing.T) {
	running := opsWaitState(t, "k1", opsRunning)

	// Power k1 off at once; phenix vm shutdown of a guest still booting may
	// wait out its 30 s fallback.
	mm(t, opsExp, "vm kill k1")
	opsWaitState(t, "k1", opsQuit)

	phenix(t, "vm", "restart", opsExp, "k1")

	row := opsWaitState(t, "k1", opsRunning)
	if row["id"] != running["id"] || row["pid"] == running["pid"] {
		t.Errorf(
			"k1 after a restart from QUIT: id %s pid %s, want id %s in a new QEMU (was pid %s)",
			row["id"],
			row["pid"],
			running["id"],
			running["pid"],
		)
	}
}

// checkShutdownFallback covers V-03 (d): a VM that ignores the ACPI power
// button is killed after phenix's fallback timeout and stays listed.
func (o *opsEnv) checkShutdownFallback(t *testing.T) {
	id := vmInfo(t, opsExp)["h2"]["id"]
	began := time.Now()

	phenix(t, "vm", "shutdown", opsExp, "h2")

	took := time.Since(began)

	if took < opsShutdownFallback || took > opsShutdownFallback+opsStateWait/2 {
		t.Errorf("shutdown of hollow h2 took %v, want phenix's %v fallback", took.Round(time.Second), opsShutdownFallback)
	}

	if row, ok := vmInfo(t, opsExp)["h2"]; !ok || row["state"] != opsQuit || row["id"] != id {
		t.Errorf("h2 after shutdown: %v, want QUIT with id %s", row, id)
	}
}

// checkSnapshotWithQuitVM covers S-02a: while another VM is powered off,
// minimega's `vm save` status table fails for the namespace. phenix reads the
// save status from the VM's own QEMU, so the snapshot of h1 must succeed
// without waiting for a status timeout, move both files into the experiment's
// files, and leave h1 running.
func (o *opsEnv) checkSnapshotWithQuitVM(t *testing.T) {
	if state := vmInfo(t, opsExp)["h2"]["state"]; state != opsQuit {
		t.Fatalf("h2 is %s, want it powered off by V-03d", state)
	}

	// minimega: `vm save` without a VM returns the first QMP error of any VM, so one QUIT VM breaks the status of all (cmd/minimega/vm_cli.go:952-956).
	_, mmErr := mmRun(opsExp, "vm save")
	t.Logf("minimega vm save status with h2 powered off: %v", mmErr)

	var (
		logPath = filepath.Join(cfg.workDir, "phenix-ui.log")
		offset  = fileSize(logPath)
		began   = time.Now()
	)

	code, body := api(t, http.MethodPost, opsVMPath("h1", "/snapshots"), map[string]string{"filename": "s2a"})
	took := time.Since(began)

	t.Logf("snapshot of h1 with h2 powered off: status %d after %v: %s", code, took.Round(time.Second), bytes.TrimSpace(body))

	if code != http.StatusNoContent {
		t.Errorf("snapshot of h1 with h2 powered off: status %d %q, want 204", code, bytes.TrimSpace(body))
	}

	if took >= opsSaveStatusTimeout {
		t.Errorf("snapshot of h1 took %v, as long as phenix waits for a status it never gets", took.Round(time.Second))
	}

	apiOK(t, http.MethodGet, "/experiments", nil, nil)

	if logged := opsFileSince(logPath, offset); strings.Contains(logged, "panic") {
		t.Errorf("phenix ui panicked during the snapshot:\n%s", logged)
	}

	if code == http.StatusNoContent {
		o.ws.waitAction(t, 10*time.Second, "experiment/vm/snapshot", opsExp+"/h1", "create")
	}

	// minimega pauses a VM to save its memory, so phenix must start it again.
	if state := vmInfo(t, opsExp)["h1"]["state"]; state != opsRunning {
		t.Errorf("h1 is %s after the snapshot, want it %s again", state, opsRunning)
	}

	for _, ext := range []string{".state", ".hdd"} {
		if _, err := os.Stat(filepath.Join(o.filesDir, "h1__s2a"+ext)); err != nil {
			t.Errorf("the snapshot of h1 has no %s file in the experiment's files: %v", ext, err)
		}

		left := filepath.Join(cfg.imagesDir, opsExp+"_h1__s2a"+ext)
		if _, err := os.Stat(left); err == nil {
			t.Errorf("the snapshot of h1 left %s in minimega's files directory", left)
		}
	}
}

// checkKill covers V-04 and W-01 (e): kill removes the VM from minimega and
// the REST VM list but not from the topology, and leaves other VMs alone.
func (o *opsEnv) checkKill(t *testing.T) {
	phenix(t, "vm", "kill", opsExp, "h1")

	eventually(t, opsStateWait, time.Second, "minimega to forget h1", func() (bool, string) {
		row, ok := vmInfo(t, opsExp)["h1"]

		return !ok, fmt.Sprintf("h1 is %s", row["state"])
	})

	var list struct {
		VMs []opsVM `json:"vms"`
	}

	apiOK(t, http.MethodGet, "/experiments/"+opsExp+"/vms", nil, &list)

	for _, v := range list.VMs {
		if v.Name == "h1" {
			t.Errorf("REST VM list still has killed h1: %+v", v)
		}
	}

	if v := opsGetVM(t, "h1"); v.State != "" || v.Running {
		t.Errorf("REST reports killed h1 with state %q and running %v, want the topology entry only", v.State, v.Running)
	}

	if _, ok := opsNode(t, o.exp, "h1"); !ok {
		t.Error("killing h1 removed it from the experiment's topology")
	}

	if code, _, _ := apiGet(t, opsVMPath("h1", "/screenshot.png")); code != http.StatusBadRequest {
		t.Errorf("screenshot of killed h1: status %d, want 400", code)
	}

	if row, ok := vmInfo(t, opsExp)["h2"]; !ok || row["state"] != opsQuit {
		t.Errorf("killing h1 removed powered-off h2 from minimega (h2: %v)", row)
	}
}

// checkC2AfterRelaunch checks that REST reports the C2 state minimega has for
// k1 after it was started from QUIT by commit, reset-disk and restart, and
// records whether k1 has C2 again; without it the subtest is skipped, since
// that part only characterizes minimega.
func (o *opsEnv) checkC2AfterRelaunch(t *testing.T) {
	uuid := strings.ToLower(vmInfo(t, opsExp)["k1"]["uuid"])

	host, ok := opsClients(t)[uuid]
	v := opsGetVM(t, "k1")

	// A client registering between the two minimega reads leaves REST free
	// to report either.
	if _, okAfter := opsClients(t)[uuid]; okAfter == ok && v.CCActive != ok {
		t.Errorf("REST reports k1 with ccActive %v, while minimega lists a cc client for it: %v", v.CCActive, ok)
	}

	if !v.Running || v.State != opsRunning {
		t.Errorf("REST reports relaunched k1 as running %v, state %q", v.Running, v.State)
	}

	if !ok {
		// minimega: after QEMU exits, ron's serial re-dial loop gives up on the first failed dial (internal/ron/server.go:254-257), and KvmVM.Connect does not dial a relaunched VM again (cmd/minimega/kvm.go:1160-1170).
		t.Skip("minimega: k1 has no cc client after it was started again from QUIT")
	}

	t.Logf("C2 of k1 is back (hostname %q) after its relaunches from QUIT", host)
}

// ---------------------------------------------------------------------------
// C2

// opsExec runs a bash script in VM vm over C2 and returns its trimmed stdout,
// failing the test when no answer arrives.
func opsExec(t *testing.T, vm, script string) string {
	t.Helper()

	out, ok := opsTryExec(t, vm, opsExecWait, script)
	if !ok {
		t.Fatalf("%s did not answer C2 within %v", vm, opsExecWait)
	}

	return out
}

// opsTryExec runs a bash script in VM vm over minimega's cc and returns its
// trimmed stdout, or false when no answer arrives within wait. The command is
// deleted once answered or given up on: minimega sends every command it still
// holds to a client that connects again (after a reboot or a restore), and no
// script here may run twice. The answer is read from its response file,
// which minimega writes after it counts the response, so every script must
// end its output with a newline.
func opsTryExec(t *testing.T, vm string, wait time.Duration, script string) (string, bool) {
	t.Helper()

	uuid := strings.ToLower(vmInfo(t, opsExp)[vm]["uuid"])
	if uuid == "" {
		t.Fatalf("VM %s not found in namespace %s", vm, opsExp)
	}

	mm(t, opsExp, "cc filter uuid="+uuid)

	resp, err := mmRun(opsExp, "cc exec "+quoteArgs(guestScript(script)))

	mm(t, opsExp, "clear cc filter")

	if err != nil || len(resp) == 0 {
		t.Fatalf("cc exec in %s: %v", vm, err)
	}

	id := strings.TrimSpace(string(resp[0].Data))
	if _, err := strconv.Atoi(id); err != nil {
		t.Fatalf("cc exec in %s: command id %q: %v", vm, id, err)
	}

	defer mm(t, opsExp, "cc delete command "+id)

	// minimega keeps responses in <filepath>/<namespace>/miniccc_responses/<id>/<client uuid>/;
	// the filter lets only this VM's client answer.
	pattern := filepath.Join(cfg.imagesDir, opsExp, "miniccc_responses", id, "*", "stdout")

	for deadline := time.Now().Add(wait); time.Now().Before(deadline); time.Sleep(time.Second) {
		files, _ := filepath.Glob(pattern)

		for _, stdout := range files {
			if data, err := os.ReadFile(stdout); err == nil && bytes.HasSuffix(data, []byte("\n")) {
				return strings.TrimSpace(string(data)), true
			}
		}
	}

	return "", false
}

// opsClients maps the lowercase UUID of every cc client of the experiment to
// the hostname it reports.
func opsClients(t *testing.T) map[string]string {
	t.Helper()

	out := make(map[string]string)

	for _, row := range mmTable(t, opsExp, "cc clients") {
		out[strings.ToLower(row["uuid"])] = row["hostname"]
	}

	return out
}

// opsWaitClient waits until a cc client with the given UUID registers.
func opsWaitClient(t *testing.T, uuid string) {
	t.Helper()

	eventually(t, guestBootTimeout, 2*time.Second, "a cc client with UUID "+uuid, func() (bool, string) {
		clients := opsClients(t)
		_, ok := clients[uuid]

		return ok, fmt.Sprintf("have %v", clients)
	})
}

// opsPing pings k2 from k1 and reports reachable or unreachable. It keeps
// trying for a few seconds, or with retry for a minute, for a path that is
// coming up; the loop is bounded by time because a ping without ARP can take
// longer than its -W.
func opsPing(t *testing.T, retry bool) string {
	t.Helper()

	seconds := opsPingTry
	if retry {
		seconds = opsPingRetry
	}

	return opsExec(t, "k1", fmt.Sprintf(
		"timeout %d sh -c 'until ping -c 1 -W 1 %s >/dev/null 2>&1; do sleep 0.2; done' "+
			"&& echo reachable || echo unreachable",
		seconds, opsK2Addr,
	))
}

// How long, in seconds, opsPing tries without and with retry.
const (
	opsPingTry   = 4
	opsPingRetry = 60
)

// opsResume resumes vm when a check that paused it ends, also when it ends
// early. Resuming a running VM does nothing.
func opsResume(t *testing.T, vm string) {
	t.Helper()

	// minimega refuses to resume a VM that is already running.
	if vmInfo(t, opsExp)[vm]["state"] != "PAUSED" {
		return
	}

	if out, err := phenixRun("vm", "resume", opsExp, vm); err != nil {
		t.Errorf("resuming %s: %v\n%s", vm, err, out)
	}
}

// ---------------------------------------------------------------------------
// minimega and Open vSwitch

// opsWaitState waits until minimega reports vm in state and returns its row.
func opsWaitState(t *testing.T, vm, state string) map[string]string {
	t.Helper()

	var row map[string]string

	eventually(t, opsStateWait, time.Second, vm+" "+state, func() (bool, string) {
		row = vmInfo(t, opsExp)[vm]

		return row["state"] == state, fmt.Sprintf("state %q", row["state"])
	})

	return row
}

// opsCaptures returns minimega's capture rows for the interfaces of vm.
func opsCaptures(t *testing.T, vm string) []map[string]string {
	t.Helper()

	var rows []map[string]string

	for _, row := range mmTable(t, opsExp, "capture") {
		if strings.HasPrefix(row["interface"], vm+":") {
			rows = append(rows, row)
		}
	}

	return rows
}

// opsH2Tags returns the tags minimega reports for h2.
func opsH2Tags(t *testing.T) map[string]string {
	t.Helper()

	raw := vmInfo(t, opsExp)["h2"]["tags"]

	var tags map[string]string
	if err := json.Unmarshal([]byte(raw), &tags); err != nil {
		t.Fatalf("minimega tags of h2 %q: %v", raw, err)
	}

	return tags
}

// opsWantTapBridge checks that the tap of a vm info row is a port of the
// experiment's default bridge.
func opsWantTapBridge(t *testing.T, row map[string]string) {
	t.Helper()

	tap := strings.Trim(row["tap"], "[]")

	if br, err := ovsVsctl(t, "port-to-br", tap); err != nil || br != opsBridge {
		t.Errorf("%s tap %s is on bridge %q (%v), want %s", row["name"], tap, br, err, opsBridge)
	}
}

// opsCheckRedeployed checks that k2 was relaunched as a new VM with
// opsK2CPUs vCPUs and the given memory on the network it had, and returns its
// new row.
func opsCheckRedeployed(t *testing.T, before map[string]string, memory string) map[string]string {
	t.Helper()

	after := opsWaitState(t, "k2", opsRunning)

	if after["id"] == before["id"] {
		t.Errorf("redeployed k2 kept id %s, want a new VM", after["id"])
	}

	t.Logf("redeployed k2: id %s (was %s), UUID %s (was %s)", after["id"], before["id"], after["uuid"], before["uuid"])

	for col, want := range map[string]string{
		"vcpus":  opsK2CPUs,
		"memory": memory,
		"vlan":   before["vlan"],
		"bridge": before["bridge"],
		"type":   "kvm",
	} {
		if after[col] != want {
			t.Errorf("redeployed k2 %s = %q, want %q", col, after[col], want)
		}
	}

	opsWantTapBridge(t, after)

	return after
}

// opsWantK2Snapshot checks that k2 still boots from the injection snapshot
// phenix made for it at start.
func opsWantK2Snapshot(t *testing.T, row map[string]string, how string) {
	t.Helper()

	if !strings.Contains(row["disks"], "_"+opsExp+"_k2_snapshot") {
		t.Errorf("after a redeploy through %s, k2 boots %q, want its injection snapshot", how, row["disks"])
	}
}

// ---------------------------------------------------------------------------
// REST

// opsVMPath is the REST path of a VM of the experiment plus suffix.
func opsVMPath(vm, suffix string) string {
	return "/experiments/" + opsExp + "/vms/" + vm + suffix
}

// opsGetVM returns the REST view of vm.
func opsGetVM(t *testing.T, vm string) opsVM {
	t.Helper()

	var v opsVM

	apiOK(t, http.MethodGet, opsVMPath(vm, ""), nil, &v)

	return v
}

// opsWantListed checks that GET /experiments/{exp}/files lists a non-empty
// file at the top of the experiment's files directory.
func opsWantListed(t *testing.T, name string) {
	t.Helper()

	var files struct {
		Files []struct {
			Name string `json:"name"`
			Path string `json:"path"`
			Size int64  `json:"size"`
		} `json:"files"`
	}

	apiOK(t, http.MethodGet, "/experiments/"+opsExp+"/files", nil, &files)

	for _, f := range files.Files {
		if f.Name == name && f.Path == name && f.Size > 0 {
			return
		}
	}

	t.Errorf("GET /experiments/%s/files does not list %s: %+v", opsExp, name, files.Files)
}

// opsNode returns the topology node of vm as the experiment stores it. It
// reads the experiment through t: the subtests that call it must not fail the
// test that created the experiment, which would skip every later subtest.
func opsNode(t *testing.T, exp *experiment, vm string) (map[string]any, bool) {
	t.Helper()

	topo, _ := (&experiment{t: t, name: exp.name}).doc().Spec["topology"].(map[string]any)
	nodes, _ := topo["nodes"].([]any)

	for _, n := range nodes {
		node, _ := n.(map[string]any)
		general, _ := node["general"].(map[string]any)

		if general["hostname"] == vm {
			return node, true
		}
	}

	return nil, false
}

// opsNodeLabels returns the labels the experiment stores for vm.
func opsNodeLabels(t *testing.T, exp *experiment, vm string) map[string]string {
	t.Helper()

	node, ok := opsNode(t, exp, vm)
	if !ok {
		t.Fatalf("experiment %s has no node %s", exp.name, vm)
	}

	raw, _ := node["labels"].(map[string]any)
	labels := make(map[string]string, len(raw))

	for k, v := range raw {
		labels[k] = fmt.Sprint(v)
	}

	return labels
}

// ---------------------------------------------------------------------------
// files and images

// opsImageInfo is the part of `qemu-img info --output json` the tests read.
type opsImageInfo struct {
	Format          string `json:"format"`
	BackingFilename string `json:"backing-filename"`
}

// opsQemuImg returns qemu-img's view of an image, which may be in use.
func opsQemuImg(t *testing.T, path string) opsImageInfo {
	t.Helper()

	out, err := exec.Command("qemu-img", "info", "--output", "json", "-U", path).Output() //nolint:gosec // test
	if err != nil {
		t.Fatalf("qemu-img info %s: %v", path, err)
	}

	var info opsImageInfo
	if err := json.Unmarshal(out, &info); err != nil {
		t.Fatalf("qemu-img info %s: %v", path, err)
	}

	return info
}

// opsInspectDisk mounts partition 1 of a qcow2 image and calls fn with the
// mount point. Unlike inspectDisk it exports the image through a throwaway
// qemu-nbd overlay, so the kernel can replay the ext4 journal of a guest that
// was killed or is still running; the image itself is only read.
func opsInspectDisk(t *testing.T, image string, fn func(root string)) {
	t.Helper()

	mountDisk(t, image, 1, "--snapshot", "ro", fn)
}

// opsELF checks that path is an ELF core of at least minMiB MiB.
func opsELF(t *testing.T, path string, minMiB int) {
	t.Helper()

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("memory dump: %v", err)
	}
	defer f.Close()

	magic := make([]byte, len(opsELFMagic))
	if _, err := io.ReadFull(f, magic); err != nil || string(magic) != opsELFMagic {
		t.Errorf("%s starts with %q (%v), want the ELF magic", path, magic, err)
	}

	st, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}

	if st.Size() < int64(minMiB)*opsMiB {
		t.Errorf("%s is %d MiB, want at least %d MiB of guest memory", path, st.Size()/opsMiB, minMiB)
	}
}

// opsELFMagic starts every ELF file.
const opsELFMagic = "\x7fELF"

// opsScan reports which needles occur in a file, reading it in chunks that
// overlap by the longest needle.
func opsScan(t *testing.T, path string, needles ...string) map[string]bool {
	t.Helper()

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	longest := 0
	for _, n := range needles {
		longest = max(longest, len(n))
	}

	var (
		found = make(map[string]bool, len(needles))
		buf   = make([]byte, 4*opsMiB)
		tail  []byte
	)

	for {
		n, err := f.Read(buf)
		if n > 0 {
			chunk := append(tail, buf[:n]...) //nolint:gocritic // tail is a copy

			for _, needle := range needles {
				if !found[needle] && bytes.Contains(chunk, []byte(needle)) {
					found[needle] = true
				}
			}

			tail = slices.Clone(chunk[len(chunk)-min(longest-1, len(chunk)):])
		}

		if errors.Is(err, io.EOF) {
			return found
		}

		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
	}
}

// opsNonEmpty checks that a file exists and is not empty.
func opsNonEmpty(t *testing.T, path string) {
	t.Helper()

	if st, err := os.Stat(path); err != nil || st.Size() == 0 {
		t.Errorf("%s is missing or empty (%v)", path, err)
	}
}

// opsFileSince returns what was appended to a file after offset.
func opsFileSince(path string, offset int64) string {
	data, err := os.ReadFile(path)
	if err != nil || int64(len(data)) < offset {
		return ""
	}

	return string(data[offset:])
}

// opsMemTotalMiB returns the host's total memory in MiB from /proc/meminfo.
func opsMemTotalMiB(t *testing.T) int {
	t.Helper()

	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		t.Fatal(err)
	}

	for line := range strings.SplitSeq(string(data), "\n") {
		if fields := strings.Fields(line); len(fields) >= 2 && fields[0] == "MemTotal:" {
			kb, err := strconv.Atoi(fields[1])
			if err != nil {
				t.Fatalf("/proc/meminfo: %q: %v", line, err)
			}

			return kb / opsKiBPerMiB
		}
	}

	t.Fatal("/proc/meminfo has no MemTotal")

	return 0
}

// opsKiBPerMiB converts /proc/meminfo's kB to MiB.
const opsKiBPerMiB = 1024

// ---------------------------------------------------------------------------
// small helpers

// opsPNGSize returns the dimensions of a PNG image.
func opsPNGSize(t *testing.T, data []byte) (int, int) {
	t.Helper()

	img, err := png.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("not a PNG (%v): %.32q", err, data)
	}

	return img.Width, img.Height
}

// opsBase64Reader decodes standard base64.
func opsBase64Reader(s string) io.Reader {
	return base64.NewDecoder(base64.StdEncoding, strings.NewReader(s))
}

// opsInOrder reports whether want occurs in events as a subsequence.
func opsInOrder(events []string, want ...string) bool {
	i := 0

	for _, e := range events {
		if i < len(want) && e == want[i] {
			i++
		}
	}

	return i == len(want)
}
