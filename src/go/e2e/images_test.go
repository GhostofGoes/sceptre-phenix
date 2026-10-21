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

// firefoxSkipsTermsOfUse holds for the images with Firefox in root's desktop
// session. Firefox 138 and later cover the page with a Terms of Use dialog at
// first launch unless the SkipTermsOfUse policy is true, and Firefox reads
// /etc/firefox/policies/policies.json in place of the policies in its
// distribution directory. Firefox ignores a file that is not JSON and keeps the
// last of duplicate keys, as json.load does, so the check prints true only for
// a file that Firefox applies.
var firefoxSkipsTermsOfUse = imageCheck{ //nolint:gochecknoglobals // test table
	name: "Firefox policy skips the Terms of Use dialog",
	script: `python3 -c 'import json; print(json.dumps(` +
		`json.load(open("/etc/firefox/policies/policies.json"))["policies"]["SkipTermsOfUse"]))'`,
	want: "true",
}

// plainUbuntuChecks hold for the images the images repository's
// scripts/ubuntu.sh builds (ubuntu and miniccc): the build upgraded them from
// the noble-updates and noble-security pockets, left only the upgraded kernel
// for grub to boot, and masked the wait for an online network.
var plainUbuntuChecks = []imageCheck{ //nolint:gochecknoglobals // test table
	{
		name:   "security pockets",
		script: "sed -n 's/^Suites: //p' /etc/apt/sources.list.d/ubuntu.sources",
		want:   "noble noble-updates noble-security",
	},
	{
		// noble's release kernel is 6.8.0-31; the build replaces it.
		name: "only the upgraded kernel, booted",
		script: `test "$(ls /boot/vmlinuz-*)" = "/boot/vmlinuz-$(uname -r)" && ` +
			`dpkg --compare-versions "$(dpkg-query -W -f '${Version}' linux-image-generic)" gt 6.8.0-31.31 && echo ok`,
		want: "ok",
	},
	{
		name:   "boot does not wait for the network",
		script: "systemctl is-enabled systemd-networkd-wait-online.service",
		want:   "masked",
	},
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
			[]imageCheck{
				commandsPresent("top 10 tools", "nmap", "sqlmap", "hydra", "john"),
				firefoxSkipsTermsOfUse,
			},
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
			firefoxSkipsTermsOfUse,
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
	// The wind turbine topology injects the configs this image's Grafana and
	// Elasticsearch run with; booted as is, both run with their package
	// defaults.
	"grafana": {
		vcpus: 2, memory: 4096,
		// Elasticsearch takes up to about a minute to answer after boot.
		checksWait: 3 * time.Minute,
		checks: []imageCheck{
			{
				name: "Grafana health",
				script: "timeout 90 sh -c 'until curl -fs -o /dev/null --max-time 5 http://localhost:3000/api/health; do sleep 2; done'; " +
					`curl -fs --max-time 5 http://localhost:3000/api/health | sed -n 's/.*"database": *"\([a-z]*\)".*/\1/p'`,
				want: "ok",
			},
			{name: "grafana service", script: "systemctl is-active grafana-server.service", want: "active"},
			{name: "Grafana 12", script: "dpkg-query -W -f '${Version}' grafana | cut -d. -f1", want: "12"},
			{
				name: "Elasticsearch listening",
				script: `timeout 120 sh -c 'until ss -Hltn "sport = :9200" | grep -q .; do sleep 3; done'; ` +
					`ss -Hltn "sport = :9200" | grep -q . && echo listening`,
				want: "listening",
			},
			{name: "elasticsearch service", script: "systemctl is-active elasticsearch.service", want: "active"},
			commandsPresent("loki installed", "loki"),
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
				// The caldera app replaces the image's policies file with one
				// that sets SkipTermsOfUse too.
				firefoxSkipsTermsOfUse,
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
	// The helloworld topology boots this image on 512 MB guests: the ubuntu
	// user from scripts/atomic/ubuntu-user.sh, and sshd from ssh.socket.
	"ubuntu": {
		vcpus: 1, memory: 512,
		checks: append(
			[]imageCheck{
				{
					name:   "ubuntu user with passwordless sudo",
					script: "runuser -u ubuntu -- sudo -n true && echo ok",
					want:   "ok",
				},
				{name: "ssh socket enabled", script: "systemctl is-enabled ssh.socket", want: "enabled"},
				{
					// ssh.socket starts sshd for this connection.
					name:   "sshd answers",
					script: "ssh-keyscan -T 5 -t ed25519 127.0.0.1 | cut -d' ' -f2",
					want:   "ssh-ed25519",
				},
				commandsPresent("base tools", "ping", "sudo", "less", "nano", "vim"),
			},
			plainUbuntuChecks...,
		),
	},
	// The wind plant soar topology's placeholder soar node boots this image
	// only to run miniccc, which the generic checks cover.
	"miniccc": {vcpus: 1, memory: 512, checks: plainUbuntuChecks},
	// The wind plant topologies boot this image with the
	// /etc/default/protonuke the protonuke app injects at pre-start. Booted
	// without one, as here, the recipe's drop-in skips protonuke.service
	// instead of failing it; the later checks write nuke-pns's config in the
	// app's format and start the service.
	"protonuke": {
		vcpus: 1, memory: 1024,
		// Each server check waits up to 30 s for its port.
		checksWait: 3 * time.Minute,
		checks: []imageCheck{
			{name: "protonuke installed", script: "test -x /usr/local/bin/protonuke && echo ok", want: "ok"},
			{name: "protonuke service enabled", script: "systemctl is-enabled protonuke.service", want: "enabled"},
			{
				// The unit inject-miniexe installs puts ConditionPathExists
				// under [Service], where systemd ignores it; the recipe's
				// drop-in checks it under [Unit].
				name:   "protonuke service skipped without its config",
				script: "systemctl is-failed protonuke.service",
				want:   "inactive",
			},
			{
				name:   "boot does not wait for the network",
				script: "systemctl is-enabled systemd-networkd-wait-online.service",
				want:   "masked",
			},
			{
				// The app writes "PROTONUKE_ARGS = <args>", spaces included.
				name: "protonuke service runs with the app's config",
				script: "echo 'PROTONUKE_ARGS = -logfile /var/log/protonuke.log -level info -http -https -smtp -serve' " +
					"> /etc/default/protonuke && systemctl start protonuke.service && systemctl is-active protonuke.service",
				want: "active",
			},
			{
				name: "protonuke HTTP server",
				script: "timeout 30 sh -c 'until curl -fs -o /dev/null --max-time 3 http://localhost/; do sleep 1; done'; " +
					"curl -fs -o /dev/null -D - --max-time 5 http://localhost/ | grep -io '^server: protonuke'",
				want: "Server: protonuke",
			},
			{
				name: "protonuke HTTPS server",
				script: "timeout 30 sh -c 'until curl -fsk -o /dev/null --max-time 3 https://localhost/; do sleep 1; done'; " +
					"curl -fsk -o /dev/null -D - --max-time 5 https://localhost/ | grep -io '^server: protonuke'",
				want: "Server: protonuke",
			},
			{
				name: "protonuke SMTP server",
				script: `timeout 30 sh -c 'until ss -Hltn "sport = :25" | grep -q .; do sleep 1; done'; ` +
					"timeout 5 bash -c 'exec 3<>/dev/tcp/127.0.0.1/25; head -c 13 <&3'",
				want: "220 protonuke",
			},
			{
				name:   "protonuke logs to the file the app's args name",
				script: "test -s /var/log/protonuke.log && echo ok",
				want:   "ok",
			},
		},
	},
	// The wind plant topologies (base, hids, nids, soar) boot this image as
	// guard-cli and guard-jh. The phenix wireguard app injects wg0.conf into
	// both and has guard-jh enable wg-quick@wg0; guard-jh forwards the VPN
	// clients to the plant networks. hids and soar inject a Wazuh agent
	// ossec.conf into guard-jh, but nothing starts the agent, so the image
	// ships it disabled.
	"wireguard": {
		vcpus: 1, memory: 512,
		checks: []imageCheck{
			commandsPresent("WireGuard and network tools", "wg", "wg-quick", "ip", "ping", "tcpdump"),
			{
				name:   "wg-quick@ unit the wireguard app enables",
				script: "systemctl cat wg-quick@.service >/dev/null && echo ok",
				want:   "ok",
			},
			{name: "forwarding enabled", script: "sysctl -n net.ipv4.ip_forward", want: "1"},
			{
				// What the app's startup script does on guard-jh, with a
				// throwaway key: the kernel module, wg-quick and the unit
				// bring up a listening interface.
				name: "wg-quick@ brings an interface up",
				script: `install -d -m 0700 /etc/wireguard && umask 077 && ` +
					`printf '[Interface]\nPrivateKey = %s\nListenPort = 51820\n' "$(wg genkey)" > /etc/wireguard/smoke.conf && ` +
					`systemctl start wg-quick@smoke.service && wg show smoke listen-port; ` +
					`systemctl stop wg-quick@smoke.service; rm -f /etc/wireguard/smoke.conf`,
				want: "51820",
			},
			{
				// The recipe pins the exact version the wazuh-manager recipe
				// pins; the line is enough here.
				name:   "Wazuh agent 4.14",
				script: "dpkg-query -W -f '${Version}' wazuh-agent | cut -d. -f1,2",
				want:   "4.14",
			},
			{
				name:   "Wazuh agent not started at boot",
				script: "systemctl is-enabled wazuh-agent.service",
				want:   "disabled",
			},
			{
				name:   "boot does not wait for the network",
				script: "systemctl is-enabled systemd-networkd-wait-online.service",
				want:   "masked",
			},
		},
	},
	// The wind plant nids and soar topologies inject Filebeat's filebeat.yml,
	// Zeek's networks.cfg and a filebeat setup startup script, and the mirror
	// app's setupOVS injects the script that adds the OVS GRE port whose
	// gre_sys device Zeek sniffs. Booted as is, Filebeat runs with its package
	// config and zeek.service retries zeekctl deploy every 15 s until gre_sys
	// exists, so the checks add that port with setup-ovs.tmpl's two commands.
	"zeek": {
		vcpus: 2, memory: 2048,
		// After the port appears, zeek.service's next deploy (up to about a
		// minute later on 2 vCPUs) starts Zeek. The checks' own waits add up
		// to about 6 minutes, and they are given the time to say which check
		// failed.
		checksWait: 7 * time.Minute,
		checks: []imageCheck{
			{
				name:   "zeek and zeekctl installed",
				script: "test -x /opt/zeek/bin/zeek && test -x /opt/zeek/bin/zeekctl && echo ok",
				want:   "ok",
			},
			{name: "Zeek LTS 8.0", script: "dpkg-query -W -f '${Version}' zeek-lts-core | cut -d. -f1,2", want: "8.0"},
			{
				name:   "site policy parses",
				script: "cd /tmp && /opt/zeek/bin/zeek -a /opt/zeek/share/zeek/site/local.zeek && echo ok",
				want:   "ok",
			},
			{
				name:   "Zeek sniffs the mirror's GRE device",
				script: "sed -n 's/^interface=//p' /opt/zeek/etc/node.cfg",
				want:   "gre_sys",
			},
			{name: "zeek service enabled", script: "systemctl is-enabled zeek.service", want: "enabled"},
			{
				name:   "boot does not wait for the network",
				script: "systemctl is-enabled systemd-networkd-wait-online.service",
				want:   "masked",
			},
			commandsPresent("capture tools", "ovs-vsctl", "tcpdump"),
			{
				// The mirror app's setup-ovs.tmpl, which setupOVS injects as
				// /etc/phenix/startup/99-setup-ovs.sh. Run early so that
				// zeek.service retries while the Filebeat checks run.
				name: "flow-based GRE port creates gre_sys",
				script: "ovs-vsctl --timeout=30 add-br phenix && " +
					"ovs-vsctl --timeout=30 add-port phenix sniffer -- set interface sniffer type=gre options:remote_ip=flow && " +
					"timeout 30 sh -c 'until ip link show gre_sys >/dev/null 2>&1; do sleep 1; done' && echo gre_sys",
				want: "gre_sys",
			},
			{name: "Filebeat 8.19.23", script: "dpkg-query -W -f '${Version}' filebeat", want: "8.19.23"},
			{name: "Filebeat config", script: "filebeat test config", want: "Config OK"},
			{
				name:   "Filebeat Zeek module enabled",
				script: "filebeat modules list | sed -n '/^Enabled:/,/^$/p' | grep -x zeek",
				want:   "zeek",
			},
			{name: "filebeat service", script: "systemctl is-active filebeat.service", want: "active"},
			{
				name: "Zeek runs on gre_sys",
				script: "timeout 180 sh -c 'until systemctl is-active -q zeek.service; do sleep 5; done'; " +
					`/opt/zeek/bin/zeekctl status | awk '$1 == "zeek" { print $4 }'`,
				want: "running",
			},
			{
				// local.zeek loads misc/loaded-scripts, which logs at startup,
				// and json-logs, which the Filebeat Zeek module needs.
				name: "Zeek writes JSON logs",
				script: "timeout 60 sh -c 'until test -s /opt/zeek/logs/current/loaded_scripts.log; do sleep 2; done'; " +
					"head -c 1 /opt/zeek/logs/current/loaded_scripts.log",
				want: "{",
			},
		},
	},
	// The wind plant topology's hids, nids and soar experiments boot this
	// image with no injections, so the Wazuh manager, Filebeat and
	// Elasticsearch run with the configuration the recipe bakes in.
	"wazuh-manager": {
		// The topologies' CI runner scales this node to about 3 GB.
		vcpus: 2, memory: 3072,
		// Elasticsearch takes a minute or two to answer at this size, and
		// Filebeat ships the manager's first alerts once it does; the checks'
		// own waits add up to 9 minutes.
		checksWait: 10 * time.Minute,
		checks: []imageCheck{
			{
				name: "Elasticsearch listening",
				script: `timeout 180 sh -c 'until ss -Hltn "sport = :9200" | grep -q .; do sleep 3; done'; ` +
					`ss -Hltn "sport = :9200" | grep -q . && echo listening`,
				want: "listening",
			},
			{
				// The ids node's Filebeat and the test harness send plain HTTP
				// without credentials.
				name: "Elasticsearch 8.19.23 over plain HTTP without credentials",
				script: "timeout 30 sh -c 'until curl -fs -o /dev/null --max-time 5 http://127.0.0.1:9200/; do sleep 2; done'; " +
					"curl -fs --max-time 5 http://127.0.0.1:9200/ | jq -r .version.number",
				want: "8.19.23",
			},
			{
				name: "Elasticsearch heap sized for the shared guest",
				script: "curl -fs --max-time 5 http://127.0.0.1:9200/_nodes/_local/jvm | " +
					"jq -r '.nodes[].jvm.input_arguments[]' | grep -x -- -Xmx768m",
				want: "-Xmx768m",
			},
			{name: "wazuh-manager enabled", script: "systemctl is-enabled wazuh-manager.service", want: "enabled"},
			{name: "elasticsearch enabled", script: "systemctl is-enabled elasticsearch.service", want: "enabled"},
			{name: "filebeat enabled", script: "systemctl is-enabled filebeat.service", want: "enabled"},
			{
				// The unit starts after phenix.service and may take a while
				// beside Elasticsearch.
				name: "wazuh-manager running",
				script: "timeout 120 sh -c 'until systemctl is-active -q wazuh-manager.service; do sleep 3; done'; " +
					"systemctl is-active wazuh-manager.service",
				want: "active",
			},
			{
				// The agents' ossec.conf sends events over UDP; agents enroll
				// with authd.
				name: "agent ports: 1514 over TCP and UDP, enrollment on 1515",
				script: `timeout 30 sh -c 'until ss -Hlun "sport = :1514" | grep -q .; do sleep 2; done'; ` +
					`ss -Hltn "sport = :1514" | grep -q . && echo -n "tcp/1514 "; ` +
					`ss -Hlun "sport = :1514" | grep -q . && echo -n "udp/1514 "; ` +
					`ss -Hltn "sport = :1515" | grep -q . && echo -n tcp/1515`,
				want: "tcp/1514 udp/1514 tcp/1515",
			},
			{
				name:   "indexer client disabled in ossec.conf",
				script: `sed -n '/<indexer>/,/<\/indexer>/p' /var/ossec/etc/ossec.conf | grep -c '<enabled>no</enabled>'`,
				want:   "1",
			},
			{
				name:   "remote protocol in ossec.conf",
				script: `sed -n '/<remote>/,/<\/remote>/p' /var/ossec/etc/ossec.conf | grep -o '<protocol>[^<]*</protocol>'`,
				want:   "<protocol>tcp,udp</protocol>",
			},
			{
				// The manager raises alerts of its own as it starts (Wazuh
				// server started, its first scans), which Filebeat ships into
				// Wazuh's 4.x alert indices.
				name: "manager alerts in Elasticsearch",
				script: `timeout 180 sh -c 'until curl -fs --max-time 5 "http://127.0.0.1:9200/wazuh-alerts-4.x-*/_count" | ` +
					`jq -e ".count > 0" >/dev/null; do sleep 5; done'; ` +
					`curl -fs --max-time 5 'http://127.0.0.1:9200/wazuh-alerts-4.x-*/_count' | jq '.count > 0'`,
				want: "true",
			},
			{
				// Without Wazuh's template, Windows event alerts would exceed
				// Elasticsearch's default limit of 1000 fields.
				name: "Wazuh's index template on the alert indices",
				script: `curl -fs --max-time 5 'http://127.0.0.1:9200/wazuh-alerts-4.x-*/_settings/index.mapping.total_fields.limit' | ` +
					`jq -r '[.[].settings.index.mapping.total_fields.limit] | unique | join(" ")'`,
				want: "10000",
			},
			{
				name:   "pinned versions",
				script: `for p in wazuh-manager elasticsearch filebeat; do dpkg-query -W -f '${Version} ' "$p"; done`,
				want:   "4.14.9-1 8.19.23 8.19.23",
			},
			{
				name:   "boot does not wait for the network",
				script: "systemctl is-enabled systemd-networkd-wait-online.service",
				want:   "masked",
			},
		},
	},
	// soh's packet capture boots this image as its Elastic server and
	// injects elasticsearch.yml and kibana.yml; booted as is, Elasticsearch
	// and Kibana run with the image's own single-node, security-off configs.
	// The checks cover what soh relies on whatever it injects: Elasticsearch
	// over plain HTTP without credentials, query-flows.sh, the overrides that
	// hold over soh's Elastic 7 configs, and one pinned release.
	"elasticsearch": {
		// soh's default size for the server.
		vcpus: 4, memory: 4096,
		// Elasticsearch answers about a minute after boot, Kibana a few
		// minutes later, and Packetbeat's dashboards load after that. On a
		// server that never answers, the checks below take about 12 minutes,
		// and they get the time to say which check failed.
		checksWait: 14 * time.Minute,
		checks: append(
			[]imageCheck{
				{
					// GET / pretty-prints ("number" : "...") unless the request
					// has a pretty parameter. The body that ends the wait is the
					// one parsed, so a blip after the wait cannot empty it.
					name: "Elasticsearch 8.19.23 without credentials",
					script: "timeout 180 sh -c 'until body=$(curl -fs --max-time 5 " +
						`"http://localhost:9200/?filter_path=version.number&pretty=false"); do sleep 3; done; ` +
						`printf "%s\n" "$body"' | ` +
						`sed -n 's/.*"number" *: *"\([^"]*\)".*/\1/p'`,
					want: "8.19.23",
				},
				{name: "elasticsearch service", script: "systemctl is-active elasticsearch.service", want: "active"},
				{
					// soh's default VM holds Elasticsearch, Kibana and the desktop.
					name: "1 GB heap from jvm.options.d",
					script: `curl -fs --max-time 5 'http://localhost:9200/_nodes/jvm?filter_path=nodes.*.jvm.mem.heap_max_in_bytes' | ` +
						`grep -o '"heap_max_in_bytes":[0-9]*' | cut -d: -f2`,
					want: "1073741824",
				},
				{
					// soh's getFlows runs it over C2 by name, with no shell.
					name:   "query-flows.sh executable on the PATH",
					script: `test -x "$(command -v query-flows.sh)" && command -v query-flows.sh`,
					want:   "/usr/local/bin/query-flows.sh",
				},
				{
					// soh parses stdout as one search response; nothing has
					// shipped flows yet, so it finds none. These two checks
					// need Elasticsearch, which the first check waited for, and
					// fail at once without it rather than wait in query-flows.sh.
					name: "query-flows.sh prints a search response",
					script: "curl -fs -o /dev/null --max-time 5 http://localhost:9200/ && " +
						`query-flows.sh | grep -o '^{.*"hits":{"total":{"value":[0-9]*' | grep -o '[0-9]*$'`,
					want: "0",
				},
				{
					// minimega puts a command's stderr ahead of its stdout.
					name: "query-flows.sh writes nothing to stderr",
					script: "curl -fs -o /dev/null --max-time 5 http://localhost:9200/ && " +
						"query-flows.sh 2>&1 >/dev/null | wc -c",
					want: "0",
				},
				{
					name: "Kibana available",
					script: "timeout 300 sh -c " +
						"'until curl -fs -o /dev/null --max-time 5 http://localhost:5601/api/status; do sleep 5; done'; " +
						"curl -s -o /dev/null --max-time 5 -w '%{http_code}' http://localhost:5601/api/status",
					want: "200",
				},
				{name: "kibana service", script: "systemctl is-active kibana.service", want: "active"},
				{
					// The soh monitors skip the dashboards; the server loads them.
					name: "Packetbeat's data view loaded into Kibana",
					script: "timeout 180 sh -c " +
						"'until curl -fs --max-time 5 http://localhost:5601/api/data_views | grep -q packetbeat; do sleep 10; done'; " +
						`curl -fs --max-time 5 http://localhost:5601/api/data_views | grep -o '"title":"packetbeat-\*"' | head -1`,
					want: `"title":"packetbeat-*"`,
				},
				{
					// Kibana refuses an older Elasticsearch; the monitors run
					// the same Packetbeat release.
					name:   "one Elastic release for all three packages",
					script: "dpkg-query -W -f '${Version} ' elasticsearch kibana packetbeat",
					want:   "8.19.23 8.19.23 8.19.23",
				},
				{
					name: "services enabled at boot",
					script: "systemctl is-enabled elasticsearch.service kibana.service phenix-packetbeat-dashboards.service " +
						"| sort -u",
					want: "enabled",
				},
				{
					name:   "the server runs no Packetbeat of its own",
					script: "systemctl is-enabled packetbeat.service",
					want:   "disabled",
				},
				{
					// Elasticsearch refuses to start on soh's elasticsearch.yml
					// while the keystore holds the install's TLS passwords.
					name:   "keystore without the install's TLS passwords",
					script: "ES_PATH_CONF=/etc/elasticsearch /usr/share/elasticsearch/bin/elasticsearch-keystore list",
					want:   "keystore.seed",
				},
				{
					name:   "security off over any elasticsearch.yml",
					script: "systemctl show -p ExecStart elasticsearch.service | grep -o 'xpack.security.enabled=false'",
					want:   "xpack.security.enabled=false",
				},
				{
					// Kibana 8 exits on three settings soh's kibana.yml sets.
					// systemctl prints the command's path and its argv; only
					// argv is matched.
					name:   "Kibana cleans soh's kibana.yml before it starts",
					script: `systemctl show -p ExecStartPre kibana.service | grep -o 'argv\[\]=/usr/local/sbin/phenix-kibana-config'`,
					want:   "argv[]=/usr/local/sbin/phenix-kibana-config",
				},
				{
					// minimega's disk inject keeps these when soh writes over
					// the files, and the services must be able to read them.
					name:   "config owners and modes the services can read",
					script: "stat -c '%U:%G %a' /etc/elasticsearch/elasticsearch.yml /etc/kibana/kibana.yml | tr '\\n' ' '",
					want:   "root:elasticsearch 660 root:kibana 660",
				},
				{
					name:   "boot does not wait for the network",
					script: "systemctl is-enabled systemd-networkd-wait-online.service",
					want:   "masked",
				},
				{
					// The Kibana autostart entry runs firefox from the PATH.
					name:   "Mozilla's Firefox, not Ubuntu's snap wrapper",
					script: `readlink -f "$(command -v firefox)"`,
					want:   "/usr/lib/firefox/firefox",
				},
				firefoxSkipsTermsOfUse,
				{
					name: "root desktop session for the Kibana autostart entry",
					script: "for try in $(seq 10); do " +
						"pgrep -u root -x xfce4-session >/dev/null && echo running && break; sleep 2; done",
					want: "running",
				},
			},
			desktopChecks...,
		),
	},
	// soh's packet capture boots this image as its monitors and injects
	// packetbeat.yml; booted as is, Packetbeat sniffs with the package's
	// config and retries its Elasticsearch output, which nothing answers.
	"packetbeat": {
		// soh's size for a monitor.
		vcpus: 1, memory: 512,
		checks: []imageCheck{
			{name: "Packetbeat 8.19.23", script: "dpkg-query -W -f '${Version}' packetbeat", want: "8.19.23"},
			{name: "packetbeat service enabled", script: "systemctl is-enabled packetbeat.service", want: "enabled"},
			{name: "packetbeat service", script: "systemctl is-active packetbeat.service", want: "active"},
			{
				name:   "Packetbeat config check",
				script: "packetbeat test config -c /etc/packetbeat/packetbeat.yml",
				want:   "Config OK",
			},
			{
				// The interfaces script takes eth1 down and up again, which
				// ends a capture already running.
				name:   "starts after phenix's startup scripts",
				script: "systemctl show -p After packetbeat.service | grep -o phenix.service",
				want:   "phenix.service",
			},
			{
				// soh's packetbeat.yml loads dashboards before capturing, and
				// Packetbeat exits while Kibana does not answer.
				name:   "skips Kibana dashboards over soh's packetbeat.yml",
				script: "systemctl show -p Environment packetbeat.service | grep -o 'setup.dashboards.enabled=false'",
				want:   "setup.dashboards.enabled=false",
			},
			{
				name:   "no start limit",
				script: "systemctl show -p StartLimitIntervalUSec --value packetbeat.service",
				want:   "0",
			},
			{
				name:   "boot does not wait for the network",
				script: "systemctl is-enabled systemd-networkd-wait-online.service",
				want:   "masked",
			},
		},
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
