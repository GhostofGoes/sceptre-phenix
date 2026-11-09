//go:build e2e

package e2e

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Experiments and images of the disk shard. Experiment names are unique in the
// package; the images are private copies this shard creates and removes.
const (
	diskHollowExp    = "e2e-disk"
	diskBlankExp     = "e2e-diskblank"
	diskNoAppsExp    = "e2e-disknoapps"
	diskRealExp      = "e2e-diskreal"
	diskNS1Image     = "e2e-disk-ns1.qc2"
	diskMissingImage = "e2e-disk-missing.qc2"
	diskDeleted      = "e2e-del-a"
	diskStale        = "e2e-stale"
	diskBridge       = "phenix"
	diskStartupDst   = "/etc/phenix/startup/"
	diskGuestMotd    = "/etc/e2e-motd"
	diskVMWait       = 30 * time.Second
	diskCCGoneWait   = 90 * time.Second
)

// diskStartupScripts are the startup app's Linux scripts: the generated file
// suffix and the destination name in the guest.
func diskStartupScripts() [][2]string {
	return [][2]string{
		{"hostname", "1_hostname-start.sh"},
		{"timezone", "2_timezone-start.sh"},
		{"interfaces", "3_interfaces-start.sh"},
	}
}

// ---------------------------------------------------------------------------
// topology

// diskVM returns a VirtualMachine topology node. general, drive and extra are
// appended verbatim under general (indent 6), the first drive (indent 8) and
// the node (indent 4); ifaces come from staticIface.
func diskVM(hostname, image, osType string, memory int, general, drive, extra string, ifaces ...string) string {
	return fmt.Sprintf(`
  - type: VirtualMachine
    general:
      hostname: %s%s
    hardware:
      vcpus: 1
      memory: %d
      os_type: %s
      drives:
      - image: %s%s
    network:
      interfaces:%s%s`, hostname, general, memory, osType, image, drive, strings.Join(ifaces, ""), extra)
}

// diskHollowTopology is the hollow experiment: h1 and h2 inject into
// partition 1 of the ext4 fixture (h1 also injects a user file and deletes
// two paths), h3 uses C2 delivery through inject_partition 0, o1 is os_type
// other, ns1 runs without a snapshot on a private image, and x1 names an image
// that does not exist yet.
func diskHollowTopology(motd string) string {
	h1 := fmt.Sprintf(`
    injections:
    - src: %[1]s
      dst: /etc/motd
      permissions: "0640"
    - src: %[1]s
      dst: /%[2]s
    deletions:
    - path: /%[3]s
    - path: /%[2]s`, motd, diskDeleted, ext4Marker)

	ns1 := fmt.Sprintf(`
    injections:
    - src: %s
      dst: /etc/motd`, motd)

	return topology(
		diskVM("h1", ext4Image, "linux", 256, "", "", h1, staticIface("eth0", "LAN", "10.0.0.1")),
		diskVM("h2", ext4Image, "linux", 256, "", "", "\n    labels:\n      role: web server",
			staticIface("eth0", "LAN", "10.0.0.2")),
		diskVM("h3", blankImage, "linux", 256, "", "\n        inject_partition: 0", "",
			staticIface("eth0", "LAN", "10.0.0.3"), staticIface("eth1", "WAN", "10.1.0.3")),
		diskVM("o1", blankImage, "other", 256, "", "", "", staticIface("eth0", "WAN", "10.1.0.4")),
		diskVM("ns1", diskNS1Image, "linux", 256, "\n      snapshot: false", "", ns1,
			staticIface("eth0", "LAN", "10.0.0.5")),
		diskVM("x1", diskMissingImage, "linux", 256, "", "", "", staticIface("eth0", "LAN", "10.0.0.9")),
	)
}

// diskRealTopology is the real-guest experiment: g1 gets its startup scripts
// and a user file by disk injection, g2 gets them over C2.
func diskRealTopology(motd string) string {
	g1 := fmt.Sprintf(`
    injections:
    - src: %s
      dst: %s`, motd, diskGuestMotd)

	g2 := `
    annotations:
      phenix/startup-via-cc: true`

	return topology(
		diskVM("g1", cfg.guest, "linux", 1024, "", "", g1, staticIface("eth0", "LAN", "10.40.0.1")),
		diskVM("g2", cfg.guest, "linux", 1024, "", "", g2, staticIface("eth0", "LAN", "10.40.0.2")),
	)
}

// ---------------------------------------------------------------------------
// helpers

// diskAbsSnapshot reports whether minimega runs with -abssnapshot, which makes
// `disk snapshot` record absolute backing paths.
func diskAbsSnapshot(t *testing.T) bool {
	t.Helper()

	rows := mmTable(t, "", "args")
	if len(rows) != 1 {
		t.Fatalf("minimega args returned %d rows, want 1", len(rows))
	}

	v, ok := rows[0]["abssnapshot"]
	if !ok {
		t.Fatalf("minimega args has no abssnapshot column: %v", rows[0])
	}

	abs, err := strconv.ParseBool(v)
	if err != nil {
		t.Fatalf("minimega args abssnapshot = %q: %v", v, err)
	}

	return abs
}

// diskSnapshotPath is where phenix asks minimega to put a VM's injection
// snapshot: <files dir>/<headnode>_<experiment>_<vm>_snapshot.
func diskSnapshotPath(head, exp, vm string) string {
	return filepath.Join(cfg.imagesDir, fmt.Sprintf("%s_%s_%s_snapshot", head, exp, vm))
}

// diskCleanupFiles removes what phenix and this shard leave in the minimega
// files directory for an experiment. Call it before newExperiment so it runs
// after the experiment is stopped and deleted.
func diskCleanupFiles(t *testing.T, head, exp string, images ...string) {
	t.Helper()

	t.Cleanup(func() {
		for _, pattern := range []string{
			filepath.Join(cfg.imagesDir, head+"_"+exp+"_*_snapshot"),
			filepath.Join(cfg.imagesDir, exp+"_*_disk-0.qcow2"),
		} {
			matches, _ := filepath.Glob(pattern)
			for _, m := range matches {
				_ = os.Remove(m)
			}
		}

		_ = os.RemoveAll(filepath.Join(cfg.imagesDir, exp))

		for _, image := range images {
			_ = os.Remove(filepath.Join(cfg.imagesDir, image))
		}
	})
}

// diskMMScript returns the minimega script phenix rendered for exp.
func diskMMScript(t *testing.T, exp string) string {
	t.Helper()

	return (&experiment{t: t, name: exp}).mmScript()
}

// diskCCScript returns the minimega cc script phenix rendered for exp.
func diskCCScript(t *testing.T, exp string) string {
	t.Helper()

	return readFile(t, filepath.Join("/phenix/experiments", exp, "mm_files", exp+"-cc.mm"))
}

// diskVMBlock returns the trimmed, non-empty lines a rendered phenix script
// holds for one VM: those after its "## VM: <vm> ##" header and before the
// next VM header.
func diskVMBlock(script, vm string) []string {
	var (
		header = "## VM: " + vm + " ##"
		lines  []string
		in     bool
	)

	for line := range strings.SplitSeq(script, "\n") {
		line = strings.TrimSpace(line)

		if strings.HasPrefix(line, "## VM: ") {
			in = line == header

			continue
		}

		if in && line != "" {
			lines = append(lines, line)
		}
	}

	return lines
}

// diskLineWithPrefix returns the first line that starts with prefix.
func diskLineWithPrefix(lines []string, prefix string) string {
	for _, line := range lines {
		if strings.HasPrefix(line, prefix) {
			return line
		}
	}

	return ""
}

// diskContainsAny reports whether any line contains substr.
func diskContainsAny(lines []string, substr string) bool {
	return slices.ContainsFunc(lines, func(line string) bool { return strings.Contains(line, substr) })
}

// diskStartupCCLines are the cc script lines the startup app renders for a
// Linux node that gets its startup scripts over C2.
func diskStartupCCLines(exp, vm string) []string {
	scripts := diskStartupScripts()
	lines := make([]string, 0, 1+2*len(scripts))
	lines = append(lines, "cc filter name="+vm)

	for _, s := range scripts {
		rel := fmt.Sprintf("%s/%s-%s.sh", exp, vm, s[0])
		lines = append(lines, "cc send "+rel, "cc exec bash /tmp/miniccc/files/"+rel)
	}

	return lines
}

// diskCheckCCDelivery checks the C2 delivery phenix set up for vm: the cc
// script lines and the scripts staged in the minimega files directory.
func diskCheckCCDelivery(t *testing.T, exp, vm string) {
	t.Helper()

	cc := diskCCScript(t, exp)
	want := diskStartupCCLines(exp, vm)

	got := diskVMBlock(cc, vm)
	if len(got) < len(want) || !slices.Equal(got[:len(want)], want) {
		t.Errorf("cc script block for %s = %q, want it to start with %q", vm, got, want)
	}

	lines := strings.Split(strings.TrimSpace(cc), "\n")
	if last := strings.TrimSpace(lines[len(lines)-1]); last != "clear cc filter" {
		t.Errorf("cc script ends with %q, want \"clear cc filter\"", last)
	}

	for _, s := range diskStartupScripts() {
		staged := filepath.Join(cfg.imagesDir, exp, fmt.Sprintf("%s-%s.sh", vm, s[0]))
		if _, err := os.Stat(staged); err != nil {
			t.Errorf("startup script for C2 delivery was not staged: %v", err)
		}
	}
}

// diskImageInfo is the part of `qemu-img info --output=json` the tests read.
type diskImageInfo struct {
	Filename            string `json:"filename"`
	BackingFilename     string `json:"backing-filename"`
	FullBackingFilename string `json:"full-backing-filename"`
}

// diskImgInfo returns qemu-img's view of an image, which may be in use.
func diskImgInfo(t *testing.T, image string) diskImageInfo {
	t.Helper()

	out, err := exec.Command("qemu-img", "info", "-U", "--output=json", image).Output()
	if err != nil {
		t.Fatalf("qemu-img info %s: %v", image, err)
	}

	var info diskImageInfo
	if err := json.Unmarshal(out, &info); err != nil {
		t.Fatalf("decoding qemu-img info %s: %v\n%s", image, err, out)
	}

	return info
}

// diskBackingChain returns qemu-img's view of an image and its backing files.
func diskBackingChain(t *testing.T, image string) []diskImageInfo {
	t.Helper()

	out, err := exec.Command("qemu-img", "info", "-U", "--backing-chain", "--output=json", image).Output()
	if err != nil {
		t.Fatalf("qemu-img info --backing-chain %s: %v", image, err)
	}

	var chain []diskImageInfo
	if err := json.Unmarshal(out, &chain); err != nil {
		t.Fatalf("decoding qemu-img info --backing-chain %s: %v\n%s", image, err, out)
	}

	return chain
}

// diskSameContent compares the guest-visible content of two qcow2 images.
func diskSameContent(t *testing.T, a, b string) bool {
	t.Helper()

	out, err := exec.Command("qemu-img", "compare", "-U", "-f", "qcow2", "-F", "qcow2", a, b).CombinedOutput()
	if err == nil {
		return true
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false
	}

	t.Fatalf("qemu-img compare %s %s: %v\n%s", a, b, err, out)

	return false
}

func diskSHA256(t *testing.T, path string) string {
	t.Helper()

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatalf("hashing %s: %v", path, err)
	}

	return hex.EncodeToString(h.Sum(nil))
}

// diskWaitState waits until minimega reports VM vm in the given state and
// returns its vm info row.
func diskWaitState(t *testing.T, ns, vm, state string) map[string]string {
	t.Helper()

	var row map[string]string

	eventually(t, diskVMWait, time.Second, fmt.Sprintf("%s %s", vm, state), func() (bool, string) {
		row = vmInfo(t, ns)[vm]

		return row["state"] == state, fmt.Sprintf("state %q", row["state"])
	})

	return row
}

// diskWaitRunning waits until minimega reports every named VM RUNNING.
func diskWaitRunning(t *testing.T, ns string, vms ...string) {
	t.Helper()

	(&experiment{t: t, name: ns}).waitRunning(diskVMWait, vms...)
}

// diskWaitCCUUID waits until the miniccc client of VM vm has registered with
// minimega, whatever hostname it reports.
func diskWaitCCUUID(t *testing.T, ns, vm string) {
	t.Helper()

	uuid := vmInfo(t, ns)[vm]["uuid"]
	if uuid == "" {
		t.Fatalf("VM %s not found in namespace %s", vm, ns)
	}

	eventually(t, guestBootTimeout, 2*time.Second, "cc client of "+vm, func() (bool, string) {
		var have []string

		for _, row := range mmTable(t, ns, "cc clients") {
			if strings.EqualFold(row["uuid"], uuid) {
				return true, ""
			}

			have = append(have, row["uuid"]+"="+row["hostname"])
		}

		return false, fmt.Sprintf("want %s, have %v", uuid, have)
	})
}

// diskWaitCCGone waits until no cc client of namespace ns reports hostname,
// which shows that the guest that registered with it went away.
func diskWaitCCGone(t *testing.T, ns, hostname string) {
	t.Helper()

	eventually(t, diskCCGoneWait, time.Second, "cc client "+hostname+" to go away", func() (bool, string) {
		for _, row := range mmTable(t, ns, "cc clients") {
			if row["hostname"] == hostname {
				return false, "still registered as " + row["uuid"]
			}
		}

		return true, ""
	})
}

// diskRESTVM is the part of a REST VM the tests read.
type diskRESTVM struct {
	Name            string `json:"name"`
	Disk            string `json:"disk"`
	Snapshot        bool   `json:"snapshot"`
	InjectPartition int    `json:"inject_partition"`
	DoNotBoot       bool   `json:"dnb"`
}

// diskRESTVMs returns the experiment's VMs from the phenix REST API by name.
func diskRESTVMs(t *testing.T, exp string) map[string]diskRESTVM {
	t.Helper()

	var body struct {
		VMs []diskRESTVM `json:"vms"`
	}

	apiOK(t, http.MethodGet, "/experiments/"+exp+"/vms", nil, &body)

	out := make(map[string]diskRESTVM, len(body.VMs))
	for _, vm := range body.VMs {
		out[vm.Name] = vm
	}

	return out
}

// diskSpecNode returns a node of the experiment config phenix stores.
func diskSpecNode(t *testing.T, exp, hostname string) map[string]any {
	t.Helper()

	doc := (&experiment{t: t, name: exp}).doc()

	topo, _ := doc.Spec["topology"].(map[string]any)
	nodes, _ := topo["nodes"].([]any)

	for _, n := range nodes {
		node, _ := n.(map[string]any)
		general, _ := node["general"].(map[string]any)

		if general["hostname"] == hostname {
			return node
		}
	}

	t.Fatalf("experiment %s config has no node %s", exp, hostname)

	return nil
}

// diskGuestFacts runs prelude and then a fixed set of probes in a real guest
// through C2 and returns their key=value results. It waits up to a minute for
// address to come up first, since startup scripts may still be running.
// ccExec deletes the probe's command once answered, so a guest that reboots
// under the same UUID does not run it, or the marker file it creates, again.
func diskGuestFacts(t *testing.T, ns, vm, address, prelude string) map[string]string {
	t.Helper()

	script := prelude + fmt.Sprintf(`
for i in $(seq 60); do ip -4 -o addr show | grep -q ' %[1]s/24 ' && break; sleep 1; done
if ip -4 -o addr show | grep -q ' %[1]s/24 '; then echo addr=ok; else echo addr=missing; fi
echo "hostname=$(hostname)"
echo "motd=$(head -n 1 %[3]s 2>/dev/null)"
echo "tz=$(date +%%Z)"
echo "startup=$(ls /etc/phenix/startup 2>/dev/null | sort | tr '\n' ' ')"
echo "ccfiles=$(ls /tmp/miniccc/files/%[2]s 2>/dev/null | sort | tr '\n' ' ')"
echo "marker=$(test -e /root/e2e-marker && echo present || echo absent)"
echo "bootid=$(cat /proc/sys/kernel/random/boot_id)"
`, address, ns, diskGuestMotd)

	out := guestShell(t, ns, vm, script)

	facts := make(map[string]string)

	for line := range strings.SplitSeq(out, "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			facts[k] = strings.TrimSpace(v)
		}
	}

	t.Logf("%s: %v", vm, facts)

	return facts
}

// diskWantFacts fails the test for every fact that differs from want.
func diskWantFacts(t *testing.T, vm string, facts, want map[string]string) {
	t.Helper()

	keys := make([]string, 0, len(want))
	for k := range want {
		keys = append(keys, k)
	}

	slices.Sort(keys)

	for _, k := range keys {
		if facts[k] != want[k] {
			t.Errorf("%s: %s = %q, want %q", vm, k, facts[k], want[k])
		}
	}
}

// ---------------------------------------------------------------------------
// F2 on hollow VMs

// diskHollow is the shared state of TestDiskSnapshotInjection.
type diskHollow struct {
	name     string
	head     string
	abs      bool
	motd     string
	startOut string
	sums     map[string]string
}

func (d *diskHollow) snap(vm string) string {
	return diskSnapshotPath(d.head, d.name, vm)
}

// TestDiskSnapshotInjection runs one hollow experiment through phenix's
// snapshot and injection pipeline (flows F2-01 to F2-06 and F2-08 to F2-11):
// the snapshots and backing chains phenix asks minimega for, what the
// startup app and user injections put on disk, C2 delivery for partition 0,
// snapshot: false, reset-disk, redeploy, cleanup on stop and restart, and a
// node whose image is missing.
func TestDiskSnapshotInjection(t *testing.T) {
	hollowImages(t)

	d := &diskHollow{name: diskHollowExp, head: trimHostSuffixes(headnode(t)), abs: diskAbsSnapshot(t)}
	t.Logf("minimega -abssnapshot=%v", d.abs)

	diskCleanupFiles(t, d.head, d.name, diskNS1Image, diskMissingImage)

	_ = os.Remove(filepath.Join(cfg.imagesDir, diskMissingImage))

	err := run("cp", filepath.Join(cfg.imagesDir, ext4Image), filepath.Join(cfg.imagesDir, diskNS1Image))
	if err != nil {
		t.Fatal(err)
	}

	d.motd = writeFile(t, "motd", "v1\n")
	d.sums = map[string]string{
		ext4Image:  diskSHA256(t, filepath.Join(cfg.imagesDir, ext4Image)),
		blankImage: diskSHA256(t, filepath.Join(cfg.imagesDir, blankImage)),
	}

	exp := newExperiment(t, d.name, diskHollowTopology(d.motd), "")
	d.startOut = exp.start()
	diskWaitRunning(t, d.name, "h1", "h2", "h3", "o1", "ns1")

	t.Run("F2-01 snapshots and backing chain", d.checkSnapshots)
	t.Run("F2-02 injected content", d.checkInjectedContent)
	t.Run("F2-04 C2 delivery with partition 0", d.checkPartitionZero)
	t.Run("F2-05 snapshot false", d.checkNoSnapshot)
	t.Run("F2-11 missing image becomes do-not-boot", d.checkMissingImage)
	t.Run("F2-08 reset-disk", d.checkResetDisk)
	t.Run("F2-09 redeploy sizing", d.checkRedeploySizing)
	t.Run("F2-10 redeploy re-injects", d.checkRedeployReinject)
	t.Run("F2-10 redeploy re-injects with partition 0", d.checkRedeployPartitionZero)
	t.Run("F2-10 redeploy recreates a snapshot without injections", d.checkRedeployWithoutInjections)
	t.Run("F2-06 snapshot cleanup and restart", d.checkCleanupAndRestart)
	t.Run("F2-03 base images untouched", d.checkBaseImages)
}

// checkSnapshots covers F2-01: each snapshot node gets a phenix snapshot
// backed by its image, minimega runs the VM on a per-instance snapshot of it,
// and the REST API reports the base image.
func (d *diskHollow) checkSnapshots(t *testing.T) {
	var (
		script = diskMMScript(t, d.name)
		info   = vmInfo(t, d.name)
		rest   = diskRESTVMs(t, d.name)
	)

	for _, c := range [][2]string{{"h1", ext4Image}, {"h2", ext4Image}, {"h3", blankImage}, {"o1", blankImage}} {
		var (
			vm, image = c[0], c[1]
			snap      = d.snap(vm)
			base      = filepath.Join(cfg.imagesDir, image)
			want      = image
		)

		if d.abs {
			want = base
		}

		si := diskImgInfo(t, snap)
		if si.BackingFilename != want {
			t.Errorf("%s snapshot backing-filename = %q, want %q", vm, si.BackingFilename, want)
		}

		if got := filepath.Clean(si.FullBackingFilename); got != base {
			t.Errorf("%s snapshot full-backing-filename = %q, want %q", vm, got, base)
		}

		row := info[vm]

		if row["snapshot"] != "true" {
			t.Errorf("%s vm info snapshot = %q, want true", vm, row["snapshot"])
		}

		if row["disks"] != snap+",writeback" {
			t.Errorf("%s vm info disks = %q, want %q", vm, row["disks"], snap+",writeback")
		}

		overlay := filepath.Join(cfg.mmBase, row["id"], "disk-0.qcow2")
		if got := filepath.Clean(diskImgInfo(t, overlay).FullBackingFilename); got != snap {
			t.Errorf("%s instance disk %s is backed by %q, want %q", vm, overlay, got, snap)
		}

		block := diskVMBlock(script, vm)

		for _, line := range []string{
			"disk snapshot " + image + " " + filepath.Base(snap),
			"vm config snapshot true",
			"vm config disk " + filepath.Base(snap) + ",writeback",
		} {
			if !slices.Contains(block, line) {
				t.Errorf("rendered script for %s is missing %q:\n%s", vm, line, strings.Join(block, "\n"))
			}
		}

		if got := filepath.Base(rest[vm].Disk); got != image {
			t.Errorf("REST disk of %s = %q, want the base image %s", vm, rest[vm].Disk, image)
		}

		if !rest[vm].Snapshot {
			t.Errorf("REST snapshot of %s = false, want true", vm)
		}
	}

	if got := rest["h1"].InjectPartition; got != 1 {
		t.Errorf("REST inject_partition of h1 = %d, want the default 1", got)
	}

	if got := rest["h3"].InjectPartition; got != 0 {
		t.Errorf("REST inject_partition of h3 = %d, want 0", got)
	}
}

// checkInjectedContent covers F2-02: the inject and delete lines phenix
// renders for h1, and what they leave in h1's snapshot.
func (d *diskHollow) checkInjectedContent(t *testing.T) {
	var (
		snap    = d.snap("h1")
		block   = diskVMBlock(diskMMScript(t, d.name), "h1")
		inject  = diskLineWithPrefix(block, "disk inject "+filepath.Base(snap)+":1 files ")
		startup = filepath.Join("/phenix/experiments", d.name, "startup")
	)

	if inject == "" {
		t.Errorf("rendered script has no disk inject line for h1:\n%s", strings.Join(block, "\n"))
	}

	// The /etc/motd injection has permissions and a source outside the
	// experiment's base directory, so phenix injects a copy with them and
	// leaves the host file's mode alone.
	copies := filepath.Join("/phenix/experiments", d.name, "injections")
	staged := regexp.MustCompile(`"(` + regexp.QuoteMeta(copies) + `/[0-9a-f]+-0640-motd)":"/etc/motd"`).FindStringSubmatch(inject)

	if staged == nil {
		t.Errorf("h1 disk inject line injects /etc/motd from no copy in %s:\n%s", copies, inject)
	} else if fi, err := os.Stat(staged[1]); err != nil {
		t.Errorf("the copy of the motd source: %v", err)
	} else if fi.Mode().Perm() != 0o640 {
		t.Errorf("the copy %s of the motd source has mode %o, want 640", staged[1], fi.Mode().Perm())
	}

	diskWantSourceMode(t, d.motd, 0o600)

	scripts := diskStartupScripts()
	pairs := make([]string, 0, 1+len(scripts))
	pairs = append(pairs, fmt.Sprintf("%q:%q", d.motd, "/"+diskDeleted))

	for _, s := range scripts {
		pairs = append(pairs, fmt.Sprintf("%q:%q", fmt.Sprintf("%s/h1-%s.sh", startup, s[0]), diskStartupDst+s[1]))
	}

	for _, pair := range pairs {
		if !strings.Contains(inject, pair) {
			t.Errorf("h1 disk inject line is missing %s:\n%s", pair, inject)
		}
	}

	// Two deletions render as one comma-separated argument, which minimega
	// splits again.
	del := fmt.Sprintf("disk inject %s:1 delete files %q,%q", filepath.Base(snap), "/"+ext4Marker, "/"+diskDeleted)
	if !slices.Contains(block, del) {
		t.Errorf("rendered script is missing %q:\n%s", del, strings.Join(block, "\n"))
	}

	inspectDisk(t, snap, func(root string) {
		for path, want := range map[string]string{
			"etc/phenix/startup/1_hostname-start.sh":   "hostnamectl set-hostname h1",
			"etc/phenix/startup/2_timezone-start.sh":   "timedatectl set-timezone Etc/UTC",
			"etc/phenix/startup/3_interfaces-start.sh": "ip addr add 10.0.0.1/24",
		} {
			if got := readGuestFile(t, root, path); !strings.Contains(got, want) {
				t.Errorf("%s does not contain %q:\n%s", path, want, got)
			}
		}

		if got := readGuestFile(t, root, "etc/motd"); got != "v1\n" {
			t.Errorf("injected /etc/motd = %q, want %q", got, "v1\n")
		}

		fi, err := os.Stat(filepath.Join(root, "etc/motd"))
		if err != nil {
			t.Fatal(err)
		}

		if perm := fi.Mode().Perm(); perm != 0o640 {
			t.Errorf("injected /etc/motd has mode %o, want the injection's 640", perm)
		}

		for _, gone := range []string{ext4Marker, diskDeleted} {
			if _, err := os.Stat(filepath.Join(root, gone)); !os.IsNotExist(err) {
				t.Errorf("/%s is still on h1's snapshot after the deletion (stat: %v)", gone, err)
			}
		}
	})
}

// diskWantSourceMode checks that phenix left an injection source outside the
// experiment's base directory at its own mode.
func diskWantSourceMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()

	if fi, err := os.Stat(path); err != nil {
		t.Error(err)
	} else if fi.Mode().Perm() != want {
		t.Errorf("injection source %s has mode %o, want its own %o: phenix changed a host file's mode", path, fi.Mode().Perm(), want)
	}
}

// checkPartitionZero covers F2-04: with inject_partition 0 the startup app
// stages its scripts in the files directory and queues C2 commands instead
// of injecting them.
func (d *diskHollow) checkPartitionZero(t *testing.T) {
	block := diskVMBlock(diskMMScript(t, d.name), "h3")

	if diskContainsAny(block, "disk inject") {
		t.Errorf("rendered script injects into h3, which has inject_partition 0:\n%s", strings.Join(block, "\n"))
	}

	if line := "disk snapshot " + blankImage + " " + filepath.Base(d.snap("h3")); !slices.Contains(block, line) {
		t.Errorf("rendered script is missing %q", line)
	}

	diskCheckCCDelivery(t, d.name, "h3")

	rows := mmTable(t, d.name, "cc commands")
	if len(rows) != 2*len(diskStartupScripts()) {
		t.Errorf("namespace has %d cc commands, want %d (send and exec per script): %v",
			len(rows), 2*len(diskStartupScripts()), rows)
	}

	for _, row := range rows {
		if row["responses"] != "0" {
			t.Errorf("cc command %s has %s responses from a hollow VM, want 0", row["id"], row["responses"])
		}
	}

	for _, s := range diskStartupScripts() {
		var (
			name  = fmt.Sprintf("h3-%s.sh", s[0])
			guest = fmt.Sprintf("/tmp/miniccc/files/%s/%s", d.name, name)
			sent  = slices.ContainsFunc(rows, func(r map[string]string) bool { return strings.Contains(r["sent"], name) })
			runs  = slices.ContainsFunc(rows, func(r map[string]string) bool {
				return strings.Contains(r["command"], guest) && r["once"] == "false"
			})
		)

		if !sent {
			t.Errorf("no cc command sends %s: %v", name, rows)
		}

		if !runs {
			t.Errorf("no cc exec command (not exec-once) runs %s: %v", guest, rows)
		}
	}
}

// checkNoSnapshot covers F2-05: a snapshot: false node boots its image
// directly, gets nothing injected, and cannot have its disk reset.
func (d *diskHollow) checkNoSnapshot(t *testing.T) {
	if _, err := os.Stat(d.snap("ns1")); !os.IsNotExist(err) {
		t.Errorf("phenix created a snapshot for ns1, which has snapshot: false (stat: %v)", err)
	}

	block := diskVMBlock(diskMMScript(t, d.name), "ns1")

	for _, line := range []string{"vm config snapshot false", "vm config disk " + diskNS1Image} {
		if !slices.Contains(block, line) {
			t.Errorf("rendered script for ns1 is missing %q:\n%s", line, strings.Join(block, "\n"))
		}
	}

	for _, cmd := range []string{"disk snapshot", "disk inject"} {
		if diskContainsAny(block, cmd) {
			t.Errorf("rendered script runs %s for ns1, which has snapshot: false", cmd)
		}
	}

	if inj, _ := diskSpecNode(t, d.name, "ns1")["injections"].([]any); len(inj) == 0 {
		t.Error("ns1 has no injections in the stored experiment, so the check proves nothing")
	}

	row := vmInfo(t, d.name)["ns1"]

	if row["snapshot"] != "false" {
		t.Errorf("ns1 vm info snapshot = %q, want false", row["snapshot"])
	}

	if want := filepath.Join(cfg.imagesDir, diskNS1Image); row["disks"] != want {
		t.Errorf("ns1 vm info disks = %q, want %q", row["disks"], want)
	}

	overlay := filepath.Join(cfg.mmBase, row["id"], "disk-0.qcow2")
	if _, err := os.Stat(overlay); !os.IsNotExist(err) {
		t.Errorf("minimega created an instance snapshot %s for ns1 (stat: %v)", overlay, err)
	}

	out := phenixFails(t, "vm", "reset-disk", d.name, "ns1")
	if !strings.Contains(out, "snapshot flag for ns1 was not set") {
		t.Errorf("reset-disk of ns1 failed without naming the snapshot flag:\n%s", out)
	}

	rest := diskRESTVMs(t, d.name)["ns1"]

	if rest.Snapshot {
		t.Error("REST snapshot of ns1 = true, want false")
	}

	if got := filepath.Base(rest.Disk); got != diskNS1Image {
		t.Errorf("REST disk of ns1 = %q, want %s", rest.Disk, diskNS1Image)
	}
}

// checkMissingImage covers the first half of F2-11: a node whose image does
// not exist is marked do-not-boot with a warning instead of failing the start.
func (d *diskHollow) checkMissingImage(t *testing.T) {
	for _, want := range []string{"disk image not found; node will not boot", diskMissingImage} {
		if !strings.Contains(d.startOut, want) {
			t.Errorf("experiment start output does not mention %q:\n%s", want, d.startOut)
		}
	}

	block := diskVMBlock(diskMMScript(t, d.name), "x1")

	if !slices.Contains(block, "## DoNotBoot: true ##") {
		t.Errorf("rendered script does not mark x1 do-not-boot:\n%s", strings.Join(block, "\n"))
	}

	for _, cmd := range []string{"vm launch", "disk snapshot"} {
		if diskContainsAny(block, cmd) {
			t.Errorf("rendered script runs %s for x1, whose image is missing", cmd)
		}
	}

	if row, ok := vmInfo(t, d.name)["x1"]; ok {
		t.Errorf("minimega lists x1, whose image is missing: %v", row)
	}

	general, _ := diskSpecNode(t, d.name, "x1")["general"].(map[string]any)
	if general["do_not_boot"] != true {
		t.Errorf("stored x1 general.do_not_boot = %v, want true", general["do_not_boot"])
	}

	if !diskRESTVMs(t, d.name)["x1"].DoNotBoot {
		t.Error("REST dnb of x1 = false, want true")
	}
}

// checkResetDisk covers F2-08: reset-disk replaces h1's instance snapshot
// with a fresh one on top of the phenix snapshot and restarts the same VM.
func (d *diskHollow) checkResetDisk(t *testing.T) {
	var (
		snap    = d.snap("h1")
		id      = vmInfo(t, d.name)["h1"]["id"]
		overlay = filepath.Join(cfg.mmBase, id, "disk-0.qcow2")
	)

	mm(t, d.name, "vm kill h1")
	diskWaitState(t, d.name, "h1", "QUIT")

	if err := run("qemu-io", "-f", "qcow2", "-c", "write -P 0xab 60M 4k", overlay); err != nil {
		t.Fatal(err)
	}

	if diskSameContent(t, overlay, snap) {
		t.Fatal("writing to h1's instance disk did not change its content")
	}

	// The reset snapshot must resolve its backing file with and without
	// -abssnapshot; the disk shard's minimega runs with it, and
	// TestResilienceRelativeSnapshots resets a disk without it.
	out, err := phenixRun("vm", "reset-disk", d.name, "h1")
	if err != nil {
		t.Fatalf("phenix vm reset-disk %s h1 (abssnapshot=%v): %v\n%s", d.name, d.abs, err, out)
	}

	row := diskWaitState(t, d.name, "h1", "RUNNING")
	if row["id"] != id {
		t.Errorf("h1 id = %s after reset-disk, want the same VM (%s)", row["id"], id)
	}

	if !diskSameContent(t, overlay, snap) {
		t.Error("h1's instance disk still differs from its phenix snapshot after reset-disk")
	}

	chain := diskBackingChain(t, overlay)
	if len(chain) != 3 {
		t.Fatalf("h1's instance disk has a backing chain of %d images, want 3: %+v", len(chain), chain)
	}

	if got := filepath.Clean(chain[0].FullBackingFilename); got != snap {
		t.Errorf("h1's reset instance disk is backed by %q, want %q", got, snap)
	}

	if got, want := filepath.Clean(chain[1].FullBackingFilename), filepath.Join(cfg.imagesDir, ext4Image); got != want {
		t.Errorf("h1's phenix snapshot is backed by %q, want %q", got, want)
	}
}

// checkRedeploySizing covers F2-09: redeploy resizes a VM and keeps its
// network and tags; without --cpu and --mem it keeps the VM's size.
func (d *diskHollow) checkRedeploySizing(t *testing.T) {
	var (
		before    = vmInfo(t, d.name)["h2"]
		tapBefore = strings.Trim(before["tap"], "[]")
		tagBefore = mustOVS(t, "get", "port", tapBefore, "tag")
	)

	phenix(t, "vm", "redeploy", d.name, "h2", "--cpu", "2", "--mem", "1024")

	after := diskWaitState(t, d.name, "h2", "RUNNING")

	for col, want := range map[string]string{"vcpus": "2", "memory": "1024", "vlan": before["vlan"]} {
		if after[col] != want {
			t.Errorf("redeployed h2 %s = %q, want %q", col, after[col], want)
		}
	}

	if after["id"] == before["id"] {
		t.Errorf("redeployed h2 kept id %s, want a new VM", after["id"])
	}

	var tags map[string]string
	if err := json.Unmarshal([]byte(after["tags"]), &tags); err != nil || tags["role"] != "web server" {
		t.Errorf("redeployed h2 tags = %s (%v), want role=web server", after["tags"], err)
	}

	tap := strings.Trim(after["tap"], "[]")

	if br := mustOVS(t, "port-to-br", tap); br != diskBridge {
		t.Errorf("redeployed h2 tap %s is on bridge %q, want %s", tap, br, diskBridge)
	}

	if tag := mustOVS(t, "get", "port", tap, "tag"); tag != tagBefore {
		t.Errorf("redeployed h2 tap %s has OVS tag %s, want %s", tap, tag, tagBefore)
	}

	// Without --cpu and --mem the redeployed VM keeps its size.
	phenix(t, "vm", "redeploy", d.name, "o1")

	o1 := diskWaitState(t, d.name, "o1", "RUNNING")
	if o1["memory"] != "256" || o1["vcpus"] != "1" {
		t.Errorf("o1 redeployed without flags has memory %s and vcpus %s, want its own 256 and 1",
			o1["memory"], o1["vcpus"])
	}
}

// checkRedeployReinject covers F2-10 for h1: redeploy -r rebuilds the phenix
// snapshot from the image and injects the current files again, with their
// permissions.
func (d *diskHollow) checkRedeployReinject(t *testing.T) {
	if err := os.WriteFile(d.motd, []byte("v2\n"), 0o640); err != nil {
		t.Fatal(err)
	}

	// The redeploy must inject the new content with the injection's 0640
	// again, from a fresh copy, and leave the source's own mode alone.
	if err := os.Chmod(d.motd, 0o600); err != nil {
		t.Fatal(err)
	}

	id := vmInfo(t, d.name)["h1"]["id"]

	phenix(t, "vm", "redeploy", d.name, "h1", "-r")

	row := diskWaitState(t, d.name, "h1", "RUNNING")
	if row["id"] == id {
		t.Errorf("redeployed h1 kept id %s, want a new VM", id)
	}

	diskWantSourceMode(t, d.motd, 0o600)

	if want := d.snap("h1") + ",writeback"; row["disks"] != want {
		t.Errorf("redeployed h1 disks = %q, want %q", row["disks"], want)
	}

	inspectDisk(t, d.snap("h1"), func(root string) {
		if got := readGuestFile(t, root, "etc/motd"); got != "v2\n" {
			t.Errorf("re-injected /etc/motd = %q, want %q", got, "v2\n")
		}

		if fi, err := os.Stat(filepath.Join(root, "etc/motd")); err != nil {
			t.Error(err)
		} else if perm := fi.Mode().Perm(); perm != 0o640 {
			t.Errorf("re-injected /etc/motd has mode %o, want the injection's 640", perm)
		}

		if got := readGuestFile(t, root, "etc/phenix/startup/1_hostname-start.sh"); !strings.Contains(got, "set-hostname h1") {
			t.Errorf("re-injected hostname script is wrong:\n%s", got)
		}

		for _, gone := range []string{ext4Marker, diskDeleted} {
			if _, err := os.Stat(filepath.Join(root, gone)); !os.IsNotExist(err) {
				t.Errorf("/%s is on h1's snapshot after redeploy -r, but the topology deletes it (stat: %v)", gone, err)
			}
		}
	})
}

// checkRedeployPartitionZero covers F2-10 for h3: redeploy -r of a node that
// gets its startup scripts over C2 recreates its snapshot from the image and
// injects nothing, as the start does.
func (d *diskHollow) checkRedeployPartitionZero(t *testing.T) {
	d.redeployBlankSnapshot(t, "h3")
}

// checkRedeployWithoutInjections covers F2-10 for o1, which has no injections
// and no deletions: redeploy -r recreates its snapshot from the image too,
// instead of relaunching it on the image.
func (d *diskHollow) checkRedeployWithoutInjections(t *testing.T) {
	d.redeployBlankSnapshot(t, "o1")
}

// redeployBlankSnapshot runs redeploy -r for vm, a node on the blank image
// that gets nothing injected, and checks that vm runs on its phenix snapshot,
// recreated from the blank image and left unchanged.
func (d *diskHollow) redeployBlankSnapshot(t *testing.T, vm string) {
	t.Helper()

	id := vmInfo(t, d.name)[vm]["id"]

	phenix(t, "vm", "redeploy", d.name, vm, "-r")

	row := diskWaitState(t, d.name, vm, "RUNNING")
	if row["id"] == id {
		t.Errorf("redeployed %s kept id %s, want a new VM", vm, id)
	}

	if want := d.snap(vm) + ",writeback"; row["disks"] != want {
		t.Errorf("redeployed %s disks = %q, want %q", vm, row["disks"], want)
	}

	blank := filepath.Join(cfg.imagesDir, blankImage)

	if got := filepath.Clean(diskImgInfo(t, d.snap(vm)).FullBackingFilename); got != blank {
		t.Errorf("%s's recreated snapshot is backed by %q, want %q", vm, got, blank)
	}

	// Nothing was injected: the snapshot still holds the blank image's content.
	if !diskSameContent(t, d.snap(vm), blank) {
		t.Errorf("%s's recreated snapshot differs from the blank image, so redeploy -r wrote to it", vm)
	}
}

// checkCleanupAndRestart covers F2-06 and the second half of F2-11: stop -K
// keeps the snapshots, the next start replaces them and clears stale C2
// responses, and a plain stop deletes them.
func (d *diskHollow) checkCleanupAndRestart(t *testing.T) {
	snapshotted := []string{"h1", "h2", "h3", "o1"}

	phenix(t, "experiment", "stop", d.name, "-K")

	if ns, ok := mmNamespaces(t)[d.name]; ok {
		t.Errorf("namespace %s exists after stop -K: %v", d.name, ns)
	}

	kept := make(map[string]time.Time, len(snapshotted))

	for _, vm := range snapshotted {
		fi, err := os.Stat(d.snap(vm))
		if err != nil {
			t.Errorf("stop -K did not keep %s's snapshot: %v", vm, err)

			continue
		}

		kept[vm] = fi.ModTime()
	}

	stale := filepath.Join(cfg.imagesDir, d.name, "miniccc_responses", diskStale)

	if err := os.MkdirAll(filepath.Dir(stale), 0o750); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(stale, []byte("stale\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := run("cp", filepath.Join(cfg.imagesDir, ext4Image), filepath.Join(cfg.imagesDir, diskMissingImage))
	if err != nil {
		t.Fatal(err)
	}

	phenix(t, "experiment", "start", d.name)
	diskWaitRunning(t, d.name, "h1", "h2", "h3", "o1", "ns1")

	for vm, before := range kept {
		fi, err := os.Stat(d.snap(vm))
		if err != nil || !fi.ModTime().After(before) {
			t.Errorf("start did not recreate %s's kept snapshot (stat: %v)", vm, err)
		}
	}

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("start left the previous run's C2 responses in place (stat: %v)", err)
	}

	if rows := mmTable(t, d.name, "cc commands"); len(rows) != 2*len(diskStartupScripts()) {
		t.Errorf("restart queued %d cc commands, want %d: startup C2 commands must not pile up across starts",
			len(rows), 2*len(diskStartupScripts()))
	}

	inject := diskLineWithPrefix(diskVMBlock(diskMMScript(t, d.name), "h1"), "disk inject ")
	if n := strings.Count(inject, diskStartupDst+"1_hostname-start.sh"); n != 1 {
		t.Errorf("restart renders the hostname injection %d times for h1, want 1:\n%s", n, inject)
	}

	// The do_not_boot the startup app set for x1's missing image is cleared
	// now that the image exists; a user's do_not_boot would be kept.
	if state := vmInfo(t, d.name)["x1"]["state"]; state != "RUNNING" {
		t.Errorf("x1 is %q after its image appeared and the experiment restarted, want RUNNING", state)
	}

	phenix(t, "experiment", "stop", d.name)

	if ns, ok := mmNamespaces(t)[d.name]; ok {
		t.Errorf("namespace %s exists after stop: %v", d.name, ns)
	}

	for _, pattern := range []string{d.head + "_" + d.name + "_*", d.name + "_*"} {
		left, _ := filepath.Glob(filepath.Join(cfg.imagesDir, pattern))
		if len(left) > 0 {
			t.Errorf("stop left snapshot files behind: %v", left)
		}
	}
}

// checkBaseImages covers F2-03: nothing phenix did wrote to the base images.
func (d *diskHollow) checkBaseImages(t *testing.T) {
	for image, sum := range d.sums {
		if got := diskSHA256(t, filepath.Join(cfg.imagesDir, image)); got != sum {
			t.Errorf("base image %s changed during the experiment", image)
		}
	}
}

// ---------------------------------------------------------------------------
// F2-07 default apps on a blank disk

// TestDiskDefaultAppsBlankDisk covers F2-07: the startup app injects into
// partition 1 of every Linux node, so a Linux node on an unpartitioned disk
// fails the start unless minimega errors are warnings or default apps are off.
func TestDiskDefaultAppsBlankDisk(t *testing.T) {
	hollowImages(t)

	head := trimHostSuffixes(headnode(t))

	t.Run("injection into an unpartitioned disk fails the start", func(t *testing.T) {
		diskCleanupFiles(t, head, diskBlankExp)

		exp := newExperiment(t, diskBlankExp,
			topology(diskVM("b1", blankImage, "linux", 256, "", "", "", staticIface("eth0", "LAN", "10.2.0.1"))), "")

		began := time.Now()
		out := phenixFails(t, "experiment", "start", exp.name)
		t.Logf("the failed start returned after %v", time.Since(began).Round(time.Second))

		for _, want := range []string{"no partitions found on image", filepath.Base(diskSnapshotPath(head, exp.name, "b1"))} {
			if !strings.Contains(out, want) {
				t.Errorf("failed start does not mention %q:\n%s", want, out)
			}
		}

		if ns, ok := mmNamespaces(t)[exp.name]; ok {
			t.Errorf("failed start left namespace %s behind: %v", exp.name, ns)
		}

		if exp.running() {
			t.Error("experiment is marked running after its start failed")
		}

		out = phenix(t, "experiment", "start", exp.name, "--treat-mm-errors-as-warnings")
		if !strings.Contains(out, "no partitions found on image") {
			t.Errorf("start with minimega errors as warnings does not warn about the failed injection:\n%s", out)
		}

		diskWaitRunning(t, exp.name, "b1")
		exp.stop()
	})

	t.Run("default apps off skip the injection", func(t *testing.T) {
		diskCleanupFiles(t, head, diskNoAppsExp)

		node := diskVM("b2", blankImage, "linux", 256, "", "", "\n    annotations:\n      phenix/default-apps: false",
			staticIface("eth0", "LAN", "10.2.0.2"))
		exp := newExperiment(t, diskNoAppsExp, topology(node), "")
		exp.start()

		if script := diskMMScript(t, exp.name); strings.Contains(script, "disk inject") {
			t.Errorf("rendered script injects into b2, which has default apps off:\n%s", script)
		}

		diskWaitRunning(t, exp.name, "b2")
		exp.stop()
	})
}

// ---------------------------------------------------------------------------
// F2-R1 to F2-R4 on real guests

// TestDiskRealGuest covers F2-R1 to F2-R4 in one experiment of two real
// guests: startup scripts and a user file delivered by disk injection (g1)
// and over C2 (g2), and a disk reset and a redeploy with a changed injection
// seen from inside g1.
func TestDiskRealGuest(t *testing.T) {
	requireImage(t, cfg.guest)

	head := trimHostSuffixes(headnode(t))
	diskCleanupFiles(t, head, diskRealExp)

	motd := writeFile(t, "motd", "v1\n")
	exp := newExperiment(t, diskRealExp, diskRealTopology(motd), "")

	began := time.Now()

	exp.start()
	exp.waitRunning(diskVMWait, "g1", "g2")

	t.Run("rendered delivery", func(t *testing.T) {
		script := diskMMScript(t, exp.name)

		snap := filepath.Base(diskSnapshotPath(head, exp.name, "g1"))

		g1 := diskLineWithPrefix(diskVMBlock(script, "g1"), "disk inject "+snap+":1 files ")
		if !strings.Contains(g1, fmt.Sprintf("%q:%q", motd, diskGuestMotd)) {
			t.Errorf("g1 disk inject line does not inject the motd: %q", g1)
		}

		if diskContainsAny(diskVMBlock(script, "g2"), "disk inject") {
			t.Errorf("rendered script injects into g2, which has phenix/startup-via-cc:\n%s", script)
		}

		if block := diskVMBlock(diskCCScript(t, exp.name), "g1"); len(block) > 0 {
			t.Errorf("cc script has commands for g1, which gets its startup scripts by injection: %q", block)
		}

		diskCheckCCDelivery(t, exp.name, "g2")
	})

	// g1 registers under its topology hostname once its injected startup
	// scripts ran. g2 is waited for by UUID, so a failed C2 delivery of its
	// hostname script fails F2-R2 alone.
	waitCCClients(t, exp.name, "g1")
	diskWaitCCUUID(t, exp.name, "g2")
	t.Logf("both guests registered with C2 %v after start", time.Since(began).Round(time.Second))

	t.Run("F2-R1 disk-inject delivery", func(t *testing.T) {
		diskWantFacts(t, "g1", diskGuestFacts(t, exp.name, "g1", "10.40.0.1", ""), map[string]string{
			"hostname": "g1",
			"motd":     "v1",
			"addr":     "ok",
			"tz":       "UTC",
			"startup":  "1_hostname-start.sh 2_timezone-start.sh 3_interfaces-start.sh",
			"ccfiles":  "",
		})
	})

	t.Run("F2-R2 C2 delivery", func(t *testing.T) {
		facts := diskGuestFacts(t, exp.name, "g2", "10.40.0.2", "")

		// The scripts arrive by cc send and nothing was injected.
		diskWantFacts(t, "g2", facts, map[string]string{
			"startup": "",
			"ccfiles": "g2-hostname.sh g2-interfaces.sh g2-timezone.sh",
		})

		// Record what minimega did with the startup app's exec commands, so a
		// failure below shows whether they ever reached g2.
		guestDir := "/tmp/miniccc/files/" + exp.name + "/g2-"
		for _, row := range mmTable(t, exp.name, "cc commands") {
			if strings.Contains(row["command"], guestDir) {
				t.Logf("cc command %s %s: once=%s issued=%s responses=%s",
					row["id"], row["command"], row["once"], row["issued"], row["responses"])
			}
		}

		diskWantFacts(t, "g2", facts, map[string]string{"hostname": "g2", "addr": "ok", "tz": "UTC"})
	})

	t.Run("F2-R3 reset-disk seen from the guest", func(t *testing.T) {
		before := diskGuestFacts(t, exp.name, "g1", "10.40.0.1", "touch /root/e2e-marker && sync\n")
		if before["marker"] != "present" {
			t.Fatalf("could not write a marker file in g1: %v", before)
		}

		id := vmInfo(t, exp.name)["g1"]["id"]

		phenix(t, "vm", "reset-disk", exp.name, "g1")

		if row := diskWaitState(t, exp.name, "g1", "RUNNING"); row["id"] != id {
			t.Errorf("g1 id = %s after reset-disk, want the same VM (%s)", row["id"], id)
		}

		diskWaitCCGone(t, exp.name, "g1")
		waitCCClients(t, exp.name, "g1")

		after := diskGuestFacts(t, exp.name, "g1", "10.40.0.1", "")
		if after["bootid"] == before["bootid"] {
			t.Fatalf("g1 did not reboot during reset-disk (boot id %s)", after["bootid"])
		}

		diskWantFacts(t, "g1", after, map[string]string{"marker": "absent", "motd": "v1", "hostname": "g1", "addr": "ok"})
	})

	t.Run("F2-R4 redeploy with a changed injection", func(t *testing.T) {
		// F2-R3's reset removed its marker, so write one for the redeploy to
		// discard.
		if before := diskGuestFacts(t, exp.name, "g1", "10.40.0.1", "touch /root/e2e-marker && sync\n"); before["marker"] != "present" {
			t.Fatalf("could not write a marker file in g1: %v", before)
		}

		if err := os.WriteFile(motd, []byte("v3\n"), 0o600); err != nil {
			t.Fatal(err)
		}

		id := vmInfo(t, exp.name)["g1"]["id"]

		phenix(t, "vm", "redeploy", exp.name, "g1", "-r")

		row := diskWaitState(t, exp.name, "g1", "RUNNING")
		if row["id"] == id || row["memory"] != "1024" {
			t.Errorf("redeployed g1 has id %s (was %s) and memory %s, want a new VM that keeps its 1024", row["id"], id, row["memory"])
		}

		diskWaitCCGone(t, exp.name, "g1")
		waitCCClients(t, exp.name, "g1")

		diskWantFacts(t, "g1", diskGuestFacts(t, exp.name, "g1", "10.40.0.1", ""), map[string]string{
			"motd":     "v3",
			"hostname": "g1",
			"addr":     "ok",
			"startup":  "1_hostname-start.sh 2_timezone-start.sh 3_interfaces-start.sh",
			"marker":   "absent",
		})
	})
}
