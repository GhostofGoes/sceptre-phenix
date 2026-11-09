package tmpl

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// renameHosts runs the /etc/hosts edit of the hostname script rendered for
// hostname on a copy of hosts, as many times as runs, and returns the result.
func renameHosts(t *testing.T, hostname, hosts string, runs int) string {
	t.Helper()

	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Fatalf("the hostname script test needs bash: %v", err)
	}

	var buf bytes.Buffer

	if err := GenerateFromTemplate("linux_hostname.tmpl", hostname, &buf); err != nil {
		t.Fatalf("GenerateFromTemplate() error = %v", err)
	}

	var edit string

	for line := range strings.SplitSeq(buf.String(), "\n") {
		if strings.HasPrefix(line, "sed ") {
			edit = line
		}
	}

	if !strings.HasSuffix(edit, " /etc/hosts") {
		t.Fatalf("hostname script has no sed edit of /etc/hosts:\n%s", buf.String())
	}

	file := filepath.Join(t.TempDir(), "hosts")
	if err := os.WriteFile(file, []byte(hosts), 0o600); err != nil {
		t.Fatalf("writing hosts file: %v", err)
	}

	edit = strings.TrimSuffix(edit, "/etc/hosts") + "'" + file + "'"

	for range runs {
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)

		out, err := exec.CommandContext(ctx, bash, "-c", edit).CombinedOutput()

		cancel()

		if err != nil {
			t.Fatalf("running %q: %v\n%s", edit, err, out)
		}
	}

	got, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("reading hosts file: %v", err)
	}

	return string(got)
}

// TestLinuxHostnameRenamesEveryPhenixName checks the /etc/hosts edit of the
// hostname script: it renames every standalone "phenix" name, adjacent ones and
// the first label of an FQDN alias such as phenix.localdomain included, and
// leaves names that merely contain it, and the result of an earlier run, alone.
func TestLinuxHostnameRenamesEveryPhenixName(t *testing.T) {
	for _, tc := range []struct {
		name, hostname, hosts, want string
		runs                        int
	}{
		{name: "single", hostname: "web", hosts: "127.0.1.1 phenix\n", want: "127.0.1.1 web\n"},
		{name: "tab", hostname: "web", hosts: "127.0.1.1\tphenix\n", want: "127.0.1.1\tweb\n"},
		{name: "adjacent", hostname: "web", hosts: "127.0.1.1 phenix phenix\n", want: "127.0.1.1 web web\n"},
		{
			name: "three adjacent", hostname: "web",
			hosts: "127.0.1.1 phenix phenix phenix\n", want: "127.0.1.1 web web web\n",
		},
		{
			name: "FQDN alias", hostname: "web",
			hosts: "127.0.1.1 phenix.localdomain phenix\n", want: "127.0.1.1 web.localdomain web\n",
		},
		{
			name: "other names", hostname: "web",
			hosts: "127.0.0.1 localhost phenix-web myphenix\n", want: "127.0.0.1 localhost phenix-web myphenix\n",
		},
		{
			name: "rerun", hostname: "phenix-web", runs: 2,
			hosts: "127.0.1.1 phenix.localdomain phenix\n", want: "127.0.1.1 phenix-web.localdomain phenix-web\n",
		},
		{name: "hostname phenix", hostname: "phenix", runs: 2, hosts: "127.0.1.1 phenix\n", want: "127.0.1.1 phenix\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := renameHosts(t, tc.hostname, tc.hosts, max(tc.runs, 1)); got != tc.want {
				t.Errorf("hosts = %q, want %q", got, tc.want)
			}
		})
	}
}
