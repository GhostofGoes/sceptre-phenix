//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// snapshotPath is where phenix asks minimega to put a VM's injection snapshot.
func snapshotPath(t *testing.T, exp, vm string) string {
	t.Helper()

	return filepath.Join(cfg.imagesDir, fmt.Sprintf("%s_%s_%s_snapshot", headnode(t), exp, vm))
}

// lifecycleVMRows parses the table `phenix vm info <experiment>` prints into
// rows keyed by the NAME column, each mapping the upper-case column headers to
// their cells. Continuation lines of multi-line cells have no name and are
// skipped.
func lifecycleVMRows(out string) map[string]map[string]string {
	var (
		header []string
		rows   = make(map[string]map[string]string)
	)

	for line := range strings.SplitSeq(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "|") {
			continue
		}

		cells := strings.Split(strings.Trim(line, "|"), "|")
		for i := range cells {
			cells[i] = strings.TrimSpace(cells[i])
		}

		if header == nil {
			header = cells

			continue
		}

		row := make(map[string]string, len(header))

		for i, h := range header {
			if i < len(cells) {
				row[h] = cells[i]
			}
		}

		if row["NAME"] != "" {
			rows[row["NAME"]] = row
		}
	}

	return rows
}

var vlanColumn = regexp.MustCompile(`^\[?([A-Za-z0-9_-]+) \((\d+)\)\]?$`)

// TestCoreLifecycleCLI runs a two-guest experiment through the phenix
// CLI and checks every layer phenix touches: the rendered minimega script, the
// VM rows minimega reports, the snapshot and injection pipeline, the Open
// vSwitch data plane, C2 into the guests, and teardown.
func TestCoreLifecycleCLI(t *testing.T) { //nolint:gocyclo,cyclop,maintidx // one experiment checked layer by layer
	requireImage(t, cfg.guest)

	const name = "e2e-life"

	topo := topology(
		node("guest1", cfg.guest, "LAN", "10.20.0.1"),
		node("guest2", cfg.guest, "LAN", "10.20.0.2"),
		`
  - type: VirtualMachine
    general:
      hostname: parked
      do_not_boot: true
    hardware:
      vcpus: 1
      memory: 512
      os_type: linux
      drives:
      - image: `+cfg.guest+`
    network:
      interfaces:
      - name: eth0
        type: ethernet
        proto: static
        vlan: LAN
        address: 10.20.0.3
        mask: 24`,
	)

	exp := newExperiment(t, name, topo, "", "--vlan-min", "300", "--vlan-max", "310")

	// The teardown waits only for the QEMU processes this experiment started.
	qemu := coreQEMUBaseline(t)

	start := time.Now()
	exp.start()
	t.Logf("experiment start returned after %v", time.Since(start).Round(time.Millisecond))

	t.Run("rendered minimega script", func(t *testing.T) {
		script := exp.with(t).mmScript()

		for _, want := range []string{
			"vlans range 300 310", // minimega assigns 300 to 309
			"vm launch kvm guest1",
			"vm launch kvm guest2",
			"disk snapshot " + cfg.guest,
			"/etc/phenix/startup/1_hostname-start.sh",
			"/etc/phenix/startup/3_interfaces-start.sh",
		} {
			if !strings.Contains(script, want) {
				t.Errorf("rendered script is missing %q", want)
			}
		}

		if strings.Contains(script, "vm launch kvm parked") {
			t.Error("rendered script launches the do_not_boot node")
		}
	})

	t.Run("minimega reports the booted VMs", func(t *testing.T) {
		exp.with(t).waitRunning(30*time.Second, "guest1", "guest2")

		info := vmInfo(t, name)
		if len(info) != 2 {
			t.Fatalf("vm info lists %d VMs, want 2 (guest1, guest2): %v", len(info), info)
		}

		// The other shards check that QEMU exited by counting with
		// qemuProcesses, so it must see these two.
		if n := qemuProcesses(t); n < 2 {
			t.Errorf("qemuProcesses counts %d QEMU processes under %s with guest1 and guest2 running, want at least 2", n, cfg.mmBase)
		}

		// This teardown checks it with coreWaitNoQEMU, so that must see
		// these two, and not as processes that ran before the start.
		running := resilienceQEMU(t)

		for _, vm := range []string{"guest1", "guest2"} {
			uuid := info[vm]["uuid"]
			if _, ok := running[uuid]; !ok || qemu[uuid] {
				t.Errorf("%s (uuid %s) is not among the QEMU processes under %s the teardown waits for", vm, uuid, cfg.mmBase)
			}
		}

		// vm info has no host column (the harness fills it from the answering
		// daemon), so where phenix scheduled the VMs is checked in status.
		for _, vm := range []string{"guest1", "guest2"} {
			row := info[vm]

			m := vlanColumn.FindStringSubmatch(row["vlan"])
			if m == nil || m[1] != "LAN" {
				t.Fatalf("%s vlan = %q, want LAN (<id>)", vm, row["vlan"])
			}

			if id, _ := strconv.Atoi(m[2]); id < 300 || id >= 310 {
				t.Errorf("%s VLAN id %d is outside 300-309, which the experiment's range 300-310 gives", vm, id)
			}

			tap := strings.Trim(row["tap"], "[]")

			if br, err := ovsVsctl(t, "port-to-br", tap); err != nil || br != "phenix" {
				t.Errorf("%s tap %q is on bridge %q (%v), want phenix", vm, tap, br, err)
			}

			if row["snapshot"] != "true" {
				t.Errorf("%s snapshot = %q, want true", vm, row["snapshot"])
			}
		}
	})

	t.Run("experiment status", func(t *testing.T) {
		doc := exp.with(t).doc()

		if doc.Status.StartTime == "" {
			t.Error("status.startTime is empty for a running experiment")
		}

		if id := doc.Status.VLANs["LAN"]; id < 300 || id >= 310 {
			t.Errorf("status.vlans.LAN = %d, want 300-309", id)
		}

		host := headnode(t)

		for _, vm := range []string{"guest1", "guest2"} {
			if doc.Status.Schedules[vm] != host {
				t.Errorf("status.schedules[%s] = %q, want %q", vm, doc.Status.Schedules[vm], host)
			}
		}
	})

	t.Run("injection snapshots are backed by the image", func(t *testing.T) {
		for _, vm := range []string{"guest1", "guest2"} {
			snap := snapshotPath(t, name, vm)

			out, err := exec.Command("qemu-img", "info", "--output", "json", "-U", snap).Output() //nolint:gosec // test
			if err != nil {
				t.Fatalf("qemu-img info %s: %v", snap, err)
			}

			var info struct {
				BackingFilename string `json:"backing-filename"`
			}

			if err := json.Unmarshal(out, &info); err != nil {
				t.Fatal(err)
			}

			if filepath.Base(info.BackingFilename) != cfg.guest {
				t.Errorf("%s backing file = %q, want %s", snap, info.BackingFilename, cfg.guest)
			}
		}
	})

	t.Run("phenix vm info", func(t *testing.T) {
		out, err := phenixStdout("vm", "info", name)
		if err != nil {
			t.Fatalf("phenix vm info %s: %v\n%s", name, err, out)
		}

		rows := lifecycleVMRows(out)
		host := headnode(t)

		for _, vm := range []string{"guest1", "guest2"} {
			row, ok := rows[vm]
			if !ok {
				t.Errorf("phenix vm info does not list %s:\n%s", vm, out)

				continue
			}

			if row["RUNNING"] != "true" || row["HOST"] != host {
				t.Errorf("phenix vm info lists %s running %q on host %q, want true on %q:\n%s", vm, row["RUNNING"], row["HOST"], host, out)
			}
		}
	})

	t.Run("guests boot, run injected startup scripts, and talk C2", func(t *testing.T) {
		waitCCClients(t, name, "guest1", "guest2")
		t.Logf("both guests registered with C2 %v after start", time.Since(start).Round(time.Second))

		if got := strings.TrimSpace(ccExec(t, name, "guest1", "hostname")); got != "guest1" {
			t.Errorf("guest1 hostname = %q, want guest1", got)
		}

		addrs := ccExec(t, name, "guest1", "ip", "-4", "-o", "addr", "show")
		if !strings.Contains(addrs, "10.20.0.1/24") {
			t.Errorf("guest1 does not have 10.20.0.1/24 from the injected interface script:\n%s", addrs)
		}
	})

	t.Run("guests reach each other over the experiment VLAN", func(t *testing.T) {
		out := guestShell(t, name, "guest1", "ping -c 3 -W 2 10.20.0.2 >/dev/null && echo reachable || echo unreachable")
		if strings.TrimSpace(out) != "reachable" {
			t.Errorf("guest1 cannot ping guest2 over VLAN LAN: %q", out)
		}
	})

	t.Run("stop tears everything down", func(t *testing.T) {
		exp.with(t).stop()

		// phenix clears the namespace on stop; anything that queries it later
		// re-creates it empty, which leaves stale namespaces in minimega.
		if ns, ok := mmNamespaces(t)[name]; ok {
			t.Errorf("namespace %s exists after stop: %v", name, ns)
		}

		coreWaitNoQEMU(t, qemu)

		for _, vm := range []string{"guest1", "guest2"} {
			if _, err := os.Stat(snapshotPath(t, name, vm)); !os.IsNotExist(err) {
				t.Errorf("injection snapshot for %s was not deleted on stop (stat: %v)", vm, err)
			}
		}

		if st := exp.with(t).doc().Status.StartTime; st != "" {
			t.Errorf("status.startTime = %q after stop, want empty", st)
		}
	})
}
