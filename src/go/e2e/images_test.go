//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

// imageCheck is a check run inside a guest booted from one image recipe.
type imageCheck struct {
	name   string
	script string // bash, run through C2
	want   string // stdout, trimmed
}

// imageProfile is how one recipe's image is booted and what is checked inside
// it. The checks are what makes each recipe worth building.
type imageProfile struct {
	vcpus  int
	memory int // MB

	// nodeType and osType default to a Linux VirtualMachine.
	nodeType string
	osType   string

	// partition is the root partition phenix injects into; 0 means the first.
	partition int

	// boot overrides imageBootTimeout for slow-booting images.
	boot time.Duration

	// checksWait overrides ccWait for checks that can run longer.
	checksWait time.Duration

	// notGeneric skips the checks every phenix-built Linux image passes, for
	// images phenix's builder does not produce.
	notGeneric bool

	checks []imageCheck
}

// genericImageChecks hold for every Linux image phenix builds: the phenix
// startup service ran the scripts phenix injected, and miniccc runs as the
// systemd service inject-miniexe installs.
func genericImageChecks(hostname, address string) []imageCheck {
	return []imageCheck{
		{name: "hostname from the startup injection", script: "hostname", want: hostname},
		{
			name:   "address from the startup injection",
			script: fmt.Sprintf("ip -4 -o addr show | grep -q ' %s/24 ' && echo ok", address),
			want:   "ok",
		},
		{
			name:   "timezone from the startup injection",
			script: "timedatectl show -p Timezone --value",
			want:   "Etc/UTC",
		},
		{name: "phenix startup service", script: "systemctl is-active phenix.service", want: "active"},
		{name: "miniccc service", script: "systemctl is-active miniccc.service", want: "active"},
		{
			// The builder's base unit points at a path that does not exist in
			// the image; inject-miniexe must have replaced it.
			name:   "miniccc unit from inject-miniexe",
			script: "systemctl show -p ExecStart miniccc.service | grep -o '/usr/local/bin/miniccc -serial /dev/virtio-ports/cc'",
			want:   "/usr/local/bin/miniccc -serial /dev/virtio-ports/cc",
		},
		{
			name:   "startup scripts staged",
			script: "ls /etc/phenix/startup | sort | tr '\\n' ' '",
			want:   "1_hostname-start.sh 2_timezone-start.sh 3_interfaces-start.sh",
		},
		{name: "phenix base files", script: "grep -c 'Built with phenix image' /etc/motd", want: "1"},
	}
}

// desktopChecks hold for the mingui images.
var desktopChecks = []imageCheck{ //nolint:gochecknoglobals // test table
	{name: "boots to the desktop target", script: "systemctl get-default", want: "graphical.target"},
	{name: "display manager", script: "systemctl is-active lightdm.service", want: "active"},
}

// commandsPresent checks that every named command is on the guest's PATH. The
// script names each command literally: minimega would expand a shell variable
// such as $c to nothing before the guest saw it.
func commandsPresent(name string, commands ...string) imageCheck {
	var script strings.Builder

	for _, c := range commands {
		fmt.Fprintf(&script, "command -v %s >/dev/null || echo missing %s; ", c, c)
	}

	script.WriteString("echo done")

	return imageCheck{name: name, script: script.String(), want: "done"}
}

// pythonImports checks that every named module imports in the guest's python3.
func pythonImports(name string, modules ...string) imageCheck {
	return imageCheck{
		name:   name,
		script: fmt.Sprintf("python3 -c 'import %s' && echo ok", strings.Join(modules, ", ")),
		want:   "ok",
	}
}

// sandcatAgent checks that the Caldera server in the guest serves the Sandcat
// agent for platform as the caldera app's agent scripts fetch it, with a POST
// to /file/download that names the file and the platform in headers. It prints
// the file name the Linux script saves the agent under and the executable's
// magic (ELF, or MZ for Windows).
func sandcatAgent(platform, magic string) imageCheck {
	return imageCheck{
		name: "Sandcat agent for " + platform,
		script: fmt.Sprintf(
			`curl -fsS --max-time 10 -X POST -H 'file:sandcat.go' -H 'platform:%[1]s' -D /tmp/sandcat-%[1]s.headers `+
				`-o /tmp/sandcat-%[1]s http://localhost:8888/file/download && `+
				`echo "$(grep -i '^content-disposition:' /tmp/sandcat-%[1]s.headers | grep -io 'filename=.*' | cut -d= -f2 | tr -d '"\r')`+
				` $(head -c 4 /tmp/sandcat-%[1]s | tr -dc A-Z)"`,
			platform,
		),
		want: fmt.Sprintf("sandcat.go-%s %s", platform, magic),
	}
}

// calderaAPI returns a curl of path in the guest's Caldera REST API, with the
// red API key of the config the server runs, read as the SCORCH caldera
// component reads it.
func calderaAPI(path string) string {
	return `curl -fs --max-time 3 -H "KEY: $(grep api_key_red /opt/caldera/conf/default.yml | cut -d' ' -f2)" ` +
		"http://localhost:8888/api/v2/" + path
}

// calderaPhenixPlanner is the phenix plugin's planner, which the SCORCH caldera
// component (PHENIX_PLANNER_ID) runs operations with unless told otherwise.
const calderaPhenixPlanner = "d3810025-f28d-49a0-9021-73e9dac8e8e4"

// imageProfiles holds the boot settings and checks for each recipe in
// sceptre-phenix-images. A recipe without an entry boots with the default
// profile and only the generic checks, and the test says so.
var imageProfiles = map[string]imageProfile{ //nolint:gochecknoglobals // test table
	"ntp": {
		vcpus: 1, memory: 1024,
		checks: []imageCheck{
			commandsPresent("ntp installed", "ntpd", "ntpq"),
			{
				// Either daemon: is-active prints a state per unit, so only
				// its exit status combines across the two.
				name:   "ntp service",
				script: "systemctl is-active -q ntp.service || systemctl is-active -q ntpsec.service && echo active",
				want:   "active",
			},
		},
	},
	"minirouter": {
		vcpus: 1, memory: 1024,
		checks: []imageCheck{
			{name: "minirouter installed", script: "test -x /usr/local/bin/minirouter && echo ok", want: "ok"},
			{name: "minirouter service", script: "systemctl is-active minirouter.service", want: "active"},
			{
				name:   "minirouter control socket",
				script: "test -S /tmp/minirouter/minirouter && echo ok",
				want:   "ok",
			},
			{name: "forwarding enabled", script: "sysctl -n net.ipv4.ip_forward", want: "1"},
			commandsPresent(
				"routing packages", "bird", "dnsmasq", "ovs-vsctl", "iptables", "nft", "dhclient",
			),
		},
	},
	"e2e": {
		vcpus: 1, memory: 1024,
		checks: []imageCheck{
			commandsPresent(
				"test tools",
				"ntpd", "ntpq", "socat", "dhclient", "ping", "iperf3", "hping3",
				"ettercap", "snort", "nc", "wg", "python3",
			),
			{
				name:   "iperf3 server idle timeout",
				script: "iperf3 --help | grep -c -- --idle-timeout",
				want:   "1",
			},
			{
				name:   "ntp configuration link",
				script: "readlink /etc/ntpsec/ntp.conf",
				want:   "/etc/ntp.conf",
			},
			{
				name:   "boot does not wait for the network",
				script: "systemctl is-enabled systemd-networkd-wait-online.service",
				want:   "masked",
			},
		},
	},
	"bookworm": {vcpus: 2, memory: 2048, checks: desktopChecks},
	"jammy":    {vcpus: 2, memory: 2048, checks: desktopChecks},
	"noble":    {vcpus: 2, memory: 2048, checks: desktopChecks},
	"kali": {
		vcpus: 2, memory: 3072,
		checks: append(
			[]imageCheck{commandsPresent("top 10 tools", "nmap", "sqlmap", "hydra", "john")},
			desktopChecks...,
		),
	},
	"kali-harmonie": {
		vcpus: 2, memory: 2048,
		checks: []imageCheck{
			pythonImports("python packages", "scapy", "helics"),
			{
				name:   "ntp configuration link",
				script: "readlink /etc/ntpsec/ntp.conf",
				want:   "/etc/ntp.conf",
			},
		},
	},
	"bennu": {
		vcpus: 1, memory: 2048,
		checks: []imageCheck{
			commandsPresent(
				"bennu tools",
				"bennu-field-device", "bennu-probe", "pybennu-probe", "pybennu-power-solver",
			),
			pythonImports("pybennu", "pybennu", "zmq"),
		},
	},
	"bennu-dev": {
		vcpus: 2, memory: 2048,
		checks: []imageCheck{
			commandsPresent("bennu tools", "bennu-field-device", "pybennu-probe"),
		},
	},
	"docker-hello-world": {
		vcpus: 1, memory: 1024,
		checks: []imageCheck{commandsPresent("docker installed", "docker", "dockerd")},
	},
	"ubuntu-soaptools": {
		vcpus: 2, memory: 2048,
		checks: append(
			[]imageCheck{{name: "zeek installed", script: "test -x /opt/zeek/bin/zeek && echo ok", want: "ok"}},
			desktopChecks...,
		),
	},
	"ot-sim": {
		vcpus: 1, memory: 2048,
		checks: []imageCheck{
			commandsPresent("ot-sim installed", "ot-sim-cpu-module", "ot-sim-modbus-module"),
			{
				name:   "ot-sim service enabled",
				script: "systemctl is-enabled ot-sim.service",
				want:   "enabled",
			},
		},
	},
	// The phenix caldera app boots this image with its own conf/default.yml,
	// opens the server in Firefox from root's desktop session, and has its
	// agent scripts download Sandcat from the server; the SCORCH caldera
	// component runs operations there through the REST API.
	"caldera": {
		vcpus: 2, memory: 4096,
		// The checks take about 15 s on a healthy image. A server that takes
		// connections and never answers holds them for about 160 s (the
		// timeouts of the web UI wait, the curls and the polls below), and
		// they are given the time to say which check failed.
		checksWait: 4 * time.Minute,
		checks: append(
			[]imageCheck{
				{
					// caldera.service starts with miniccc at boot, and a first
					// start loads every plugin's abilities, so the server gets
					// up to a minute to answer.
					name: "Caldera web UI",
					script: "timeout 60 sh -c 'until curl -fs -o /dev/null --max-time 5 http://localhost:8888/; do sleep 2; done'; " +
						"curl -s -o /dev/null --max-time 5 -w '%{http_code}' http://localhost:8888/",
					want: "200",
				},
				{name: "caldera service", script: "systemctl is-active caldera.service", want: "active"},
				sandcatAgent("linux", "ELF"),
				sandcatAgent("windows", "MZ"),
				{
					// Caldera loads plugin data after it starts answering.
					name: "phenix planner for the SCORCH caldera component",
					script: "for try in $(seq 10); do " + calderaAPI("planners/"+calderaPhenixPlanner) +
						" && break; sleep 2; done | " +
						`python3 -c 'import json, sys; print(json.load(sys.stdin)["name"])'`,
					want: "phenix",
				},
				{
					name: "plugins the caldera app enables beyond Caldera's own",
					script: calderaAPI("plugins") + " | python3 -c 'import json, sys; " +
						`print(*sorted(p["name"] for p in json.load(sys.stdin) ` +
						`if p["enabled"] and p["name"] in ("dnp3", "modbus", "ot", "phenix")))'`,
					want: "dnp3 modbus ot phenix",
				},
				{
					// The app's autostart entry runs firefox from the PATH.
					name:   "Mozilla's Firefox, not Ubuntu's snap wrapper",
					script: `readlink -f "$(command -v firefox)"`,
					want:   "/usr/lib/firefox/firefox",
				},
				{name: "Firefox runs", script: "firefox --version | grep -o '^Mozilla Firefox'", want: "Mozilla Firefox"},
				{
					// lightdm logs root in as the desktop comes up, around the
					// time miniccc registers.
					name: "root desktop session for the autostart entry",
					script: "for try in $(seq 10); do " +
						"pgrep -u root -x xfce4-session >/dev/null && echo running && break; sleep 2; done",
					want: "running",
				},
			},
			desktopChecks...,
		),
	},
	// VyOS is built by its own script rather than the phenix builder, boots as
	// a router, and roots on its third partition.
	"vyos": {
		vcpus: 2, memory: 2048,
		nodeType: "Router", osType: "vyos", partition: 3,
		boot:       10 * time.Minute,
		notGeneric: true,
		checks: []imageCheck{
			{name: "miniccc service", script: "systemctl is-active miniccc.service", want: "active"},
		},
	},
}

// defaultImageProfile boots a recipe imageProfiles does not know.
var defaultImageProfile = imageProfile{vcpus: 1, memory: 1024} //nolint:gochecknoglobals // test default

var nonHostname = regexp.MustCompile(`[^a-z0-9-]+`)

// imageBatchSize bounds how many images boot at once on a runner with 15 GiB
// of memory and 4 vCPUs.
const imageBatchSize = 4

// imageBootTimeout allows for desktop (mingui) images, which boot slower than
// the general-purpose guest.
const imageBootTimeout = 8 * time.Minute

// TestImagesSmoke boots every image named in E2E_SMOKE_IMAGES, a few side by
// side per experiment as phenix users run them, and checks each guest from the
// inside.
func TestImagesSmoke(t *testing.T) {
	images := strings.Fields(os.Getenv("E2E_SMOKE_IMAGES"))
	if len(images) == 0 {
		t.Fatal("E2E_SMOKE_IMAGES names no images to smoke test")
	}

	for _, image := range images {
		requireImage(t, image+".qc2")
	}

	for start := 0; start < len(images); start += imageBatchSize {
		batch := images[start:min(start+imageBatchSize, len(images))]

		t.Run(strings.Join(batch, "+"), func(t *testing.T) {
			smokeImageBatch(t, fmt.Sprintf("e2e-images-%d", start/imageBatchSize), batch)
		})
	}
}

// smokeImage is one guest booted by TestImagesSmoke.
type smokeImage struct {
	image, hostname, address string
	profile                  imageProfile
}

// smokeNode renders a topology node for one smoke-tested guest.
func smokeNode(g smokeImage) string {
	nodeType := g.profile.nodeType
	if nodeType == "" {
		nodeType = "VirtualMachine"
	}

	osType := g.profile.osType
	if osType == "" {
		osType = "linux"
	}

	inject := ""
	if g.profile.partition != 0 {
		inject = fmt.Sprintf("\n        inject_partition: %d", g.profile.partition)
	}

	// Same layout as node, which topology joins without separators.
	return fmt.Sprintf(`
  - type: %s
    general:
      hostname: %s
    hardware:
      vcpus: %d
      memory: %d
      os_type: %s
      drives:
      - image: %s.qc2%s
    network:
      interfaces:
      - name: eth0
        type: ethernet
        proto: static
        vlan: IMG
        address: %s
        mask: 24`, nodeType, g.hostname, g.profile.vcpus, g.profile.memory, osType, g.image, inject, g.address)
}

func smokeImageBatch(t *testing.T, name string, images []string) {
	t.Helper()

	guests := make([]smokeImage, 0, len(images))
	nodes := make([]string, 0, len(images))
	boot := imageBootTimeout

	for i, image := range images {
		profile, ok := imageProfiles[image]
		if !ok {
			t.Logf("no profile is defined for %s; it boots with the defaults and only the generic checks run", image)

			profile = defaultImageProfile
		}

		boot = max(boot, profile.boot)

		g := smokeImage{
			image:    image,
			hostname: "img-" + strings.Trim(nonHostname.ReplaceAllString(strings.ToLower(image), "-"), "-"),
			address:  fmt.Sprintf("10.30.0.%d", i+1),
			profile:  profile,
		}

		guests = append(guests, g)
		nodes = append(nodes, smokeNode(g))
	}

	exp := newExperiment(t, name, topology(nodes...), "")
	exp.start()

	hostnames := make([]string, len(guests))
	for i, g := range guests {
		hostnames[i] = g.hostname
	}

	began := time.Now()

	waitCCClientsWithin(t, exp.name, boot, hostnames...)
	t.Logf("%v registered with C2 %v after start", images, time.Since(began).Round(time.Second))

	for _, g := range guests {
		t.Run(g.image, func(t *testing.T) {
			var checks []imageCheck
			if !g.profile.notGeneric {
				checks = genericImageChecks(g.hostname, g.address)
			}

			checks = append(checks, g.profile.checks...)

			wait := g.profile.checksWait
			if wait == 0 {
				wait = ccWait
			}

			got := runImageChecks(t, exp.name, g.hostname, wait, checks)

			for i, c := range checks {
				if got[i] != c.want {
					t.Errorf("%s: got %q, want %q", c.name, got[i], c.want)
				}
			}

			// miniccc reports its hostname and addresses with every heartbeat,
			// so the heartbeat that showed the new hostname can predate the
			// interfaces script. The heartbeat that carried the checks' answer
			// was sent after they ran, and minimega updates the client before
			// it records the answer, so cc clients now shows the addresses the
			// guest had when the checks ran.
			ips := ""

			for _, row := range mmTable(t, exp.name, "cc clients") {
				if row["hostname"] == g.hostname {
					ips = row["ip"]
				}
			}

			if !strings.Contains(ips, g.address) {
				t.Errorf("cc clients reports %q for %s, want it to include %s", ips, g.hostname, g.address)
			}

			if t.Failed() {
				t.Logf(
					"failed units and boot time on %s:\n%s", g.hostname,
					guestShell(t, exp.name, g.hostname,
						"systemctl --failed --no-legend --plain; systemd-analyze 2>&1 || true"),
				)
			}
		})
	}
}

// runImageChecks runs every check in one C2 command, since each C2 round trip
// costs a heartbeat, waits up to wait for their output, and returns each
// check's trimmed stdout in order.
func runImageChecks(t *testing.T, ns, vm string, wait time.Duration, checks []imageCheck) []string {
	t.Helper()

	var script strings.Builder

	for i, c := range checks {
		// Each check runs in its own subshell; its stderr is dropped so only
		// its answer is compared.
		fmt.Fprintf(&script, "echo '@@check %d@@'; ( %s ) 2>/dev/null; echo; ", i, c.script)
	}

	out := guestShellWithin(t, ns, vm, wait, script.String())
	results := make([]string, len(checks))

	for i := range checks {
		marker := fmt.Sprintf("@@check %d@@", i)

		_, after, found := strings.Cut(out, marker)
		if !found {
			results[i] = "(no output)"

			continue
		}

		if next := strings.Index(after, "@@check "); next >= 0 {
			after = after[:next]
		}

		results[i] = strings.TrimSpace(after)
	}

	return results
}
