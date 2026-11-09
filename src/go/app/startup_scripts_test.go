package app

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	v1 "phenix/types/version/v1"
	v2 "phenix/types/version/v2"
)

// TestStartupLinuxScriptsCanRunAgain is a regression test: the Linux startup
// scripts run at every boot, yet a second run renamed a hostname containing
// "phenix" again in /etc/hosts (phenix-web became phenix-web-web), appended
// every DNS server to /etc/resolv.conf again, and tried to join the domain
// again. Each script runs twice here against stand-in files and commands.
func TestStartupLinuxScriptsCanRunAgain(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}

	node := newStartupTestNode(t, "phenix-web", osLinux, true, 1)
	node.NetworkF = &v1.Network{InterfacesF: []*v1.Interface{
		{NameF: "IF0", VLANF: "LAN", ProtoF: "static", AddressF: "10.0.0.1", MaskF: 24, DNSF: []string{"10.0.0.53", "10.0.0.54"}},
	}}

	scenario := &v2.ScenarioSpec{AppsF: []*v2.ScenarioApp{{
		NameF: "startup",
		HostsF: []*v2.ScenarioAppHost{{
			HostnameF: "phenix-web",
			MetadataF: map[string]any{"domain_controller": map[string]any{
				"username": "admin", "password": "password", "domain": "EXAMPLE.test",
			}},
		}},
	}}}

	exp, mmDir := newStartupTestExperiment(t, node, scenario)
	runStartupPreStart(t, exp, mmDir)

	var (
		startupDir = filepath.Join(exp.Spec.BaseDir(), "startup")
		root       = t.TempDir()
		bin        = filepath.Join(root, "bin")
		hosts      = filepath.Join(root, "hosts")
		resolv     = filepath.Join(root, "resolv.conf")
		realms     = filepath.Join(root, "realms")
		joins      = filepath.Join(root, "joins")
	)

	// realm lists joined realms in lower case, as realmd does.
	fakes := map[string]string{
		"hostnamectl": "#!/bin/sh\nexit 0\n",
		"realm": `#!/bin/sh
case "$1" in
list) tr 'A-Z' 'a-z' < "$REALMS" 2>/dev/null ;;
join) cat >/dev/null; echo "$3" >> "$REALMS"; echo "$3" >> "$JOINS" ;;
esac
`,
	}

	if err := os.MkdirAll(bin, 0o750); err != nil {
		t.Fatal(err)
	}

	for name, script := range fakes {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o700); err != nil { //nolint:gosec // test stand-in
			t.Fatal(err)
		}
	}

	if err := os.WriteFile(hosts, []byte("127.0.0.1 localhost\n127.0.1.1 phenix\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	read := func(path string) string {
		t.Helper()

		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}

		return string(data)
	}

	// The interfaces script also reconfigures every interface; only its DNS
	// lines run here.
	var dns []string

	for line := range strings.SplitSeq(read(filepath.Join(startupDir, "phenix-web-interfaces.sh")), "\n") {
		if strings.Contains(line, "/etc/resolv.conf") {
			dns = append(dns, line)
		}
	}

	if len(dns) == 0 {
		t.Fatal("the interfaces script writes no DNS server to /etc/resolv.conf")
	}

	scripts := map[string]string{
		"hostname":   strings.ReplaceAll(read(filepath.Join(startupDir, "phenix-web-hostname.sh")), "/etc/hosts", hosts),
		"interfaces": strings.ReplaceAll(strings.Join(dns, "\n"), "/etc/resolv.conf", resolv),
		"domain":     read(filepath.Join(startupDir, "phenix-web-domain.sh")),
	}

	for run := 1; run <= 2; run++ {
		for name, script := range scripts {
			cmd := exec.CommandContext(t.Context(), bash, "-c", script)
			cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "REALMS="+realms, "JOINS="+joins)

			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("run %d of the %s script: %v\n%s", run, name, err, out)
			}
		}
	}

	if got, want := read(hosts), "127.0.0.1 localhost\n127.0.1.1 phenix-web\n"; got != want {
		t.Errorf("/etc/hosts after two runs = %q, want %q", got, want)
	}

	if got, want := read(resolv), "nameserver 10.0.0.53\nnameserver 10.0.0.54\n"; got != want {
		t.Errorf("/etc/resolv.conf after two runs = %q, want %q", got, want)
	}

	if got, want := read(joins), "EXAMPLE.test\n"; got != want {
		t.Errorf("realm joins after two runs = %q, want one join of EXAMPLE.test", got)
	}
}
