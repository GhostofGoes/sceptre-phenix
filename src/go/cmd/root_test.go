package cmd

import (
	"errors"
	"net"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/spf13/viper"
)

func TestSudoRanPhenix(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("unable to determine test executable: %v", err)
	}

	exeName := filepath.Base(exe)

	tests := []struct {
		name        string
		sudoCommand string
		want        bool
	}{
		{
			name:        "empty SUDO_COMMAND",
			sudoCommand: "",
			want:        false,
		},
		{
			name:        "sudo ran phenix directly",
			sudoCommand: "/usr/local/bin/phenix config list",
			want:        true,
		},
		{
			name:        "sudo ran current test binary",
			sudoCommand: exe + " -test.run TestSudoRanPhenix",
			want:        true,
		},
		{
			name:        "sudo ran su (root shell escalation)",
			sudoCommand: "/bin/su",
			want:        false,
		},
		{
			name:        "sudo ran su dash (root shell escalation)",
			sudoCommand: "/bin/su -",
			want:        false,
		},
		{
			name:        "sudo ran bash",
			sudoCommand: "/bin/bash",
			want:        false,
		},
		{
			name:        "sudo ran phenix through setsid and nohup",
			sudoCommand: "/usr/bin/setsid nohup phenix ui",
			want:        true,
		},
		{
			name:        "sudo ran phenix by path through setsid with an option",
			sudoCommand: "/usr/bin/setsid -f /usr/local/bin/phenix ui --listen-endpoint 127.0.0.1:3080",
			want:        true,
		},
		{
			name:        "sudo ran phenix through env with options and variables",
			sudoCommand: "/usr/bin/env -i PHENIX_LOG_LEVEL=debug /usr/local/bin/phenix ui",
			want:        true,
		},
		{
			name:        "sudo ran a shell through setsid",
			sudoCommand: "/usr/bin/setsid bash -c phenix",
			want:        false,
		},
		{
			name:        "sudo ran env with a variable naming phenix",
			sudoCommand: "/usr/bin/env PATH=/opt/phenix /bin/bash",
			want:        false,
		},
		{
			name:        "sudo ran a wrapper without a command",
			sudoCommand: "/usr/bin/nohup",
			want:        false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SUDO_COMMAND", tc.sudoCommand)

			if got := sudoRanPhenix(); got != tc.want {
				t.Errorf("sudoRanPhenix() with SUDO_COMMAND=%q (exe=%s) = %v, want %v", tc.sudoCommand, exeName, got, tc.want)
			}
		})
	}
}

// TestGetCurrentUserInfoUnderSudo checks that the CLI and a UI that sudo
// started, directly or detached with setsid and nohup, resolve the same user,
// and so the same default store and config directory. It needs root, as
// getCurrentUserInfo trusts SUDO_USER only then.
func TestGetCurrentUserInfoUnderSudo(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("getCurrentUserInfo reads SUDO_USER only when run as root")
	}

	invoker, err := user.Lookup("nobody")
	if err != nil {
		t.Skipf("no user to stand in for the sudo invoker: %v", err)
	}

	self, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		sudoCommand string
		want        *user.User
	}{
		{sudoCommand: "/usr/local/bin/phenix config create t.yml", want: invoker},
		{sudoCommand: "/usr/bin/setsid nohup /usr/local/bin/phenix ui", want: invoker},
		{sudoCommand: "/usr/bin/nohup phenix ui --unix-socket /tmp/phenix.sock", want: invoker},
		{sudoCommand: "/bin/bash", want: self},
	}

	for _, tc := range tests {
		t.Run(tc.sudoCommand, func(t *testing.T) {
			t.Setenv("SUDO_USER", invoker.Username)
			t.Setenv("SUDO_COMMAND", tc.sudoCommand)

			uid, home := getCurrentUserInfo()
			if uid != tc.want.Uid || home != tc.want.HomeDir {
				t.Errorf("getCurrentUserInfo() = (%q, %q), want (%q, %q)", uid, home, tc.want.Uid, tc.want.HomeDir)
			}
		})
	}
}

// TestReadConfig checks where phenix reads its config file from and where it
// creates one when there is none. A root process, which `sudo phenix` is,
// searches the invoking user's config directory but creates the file only in
// the system directory, so it never leaves a root-owned config in that
// user's home to shadow the system config.
func TestReadConfig(t *testing.T) {
	tests := []struct {
		name     string
		root     bool
		existing string // "user", "system", or "" for no config file
		want     string // directory whose config.yaml is used
		absent   string // directory that must not exist afterwards
	}{
		{name: "root creates the config in the system directory", root: true, want: "system", absent: "user"},
		{name: "user creates the config in the user directory", root: false, want: "user", absent: "system"},
		{name: "root reads the invoking user's config", root: true, existing: "user", want: "user", absent: "system"},
		{name: "root reads the system config", root: true, existing: "system", want: "system", absent: "user"},
		{name: "user reads the system config", root: false, existing: "system", want: "system", absent: "user"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tmp := t.TempDir()
			dirs := map[string]string{
				"user":   filepath.Join(tmp, "home", ".config", "phenix"),
				"system": filepath.Join(tmp, "etc", "phenix"),
			}

			if tc.existing != "" {
				if err := os.MkdirAll(dirs[tc.existing], 0o750); err != nil {
					t.Fatal(err)
				}

				file := filepath.Join(dirs[tc.existing], "config.yaml")
				if err := os.WriteFile(file, []byte("log:\n  level: debug\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			v := viper.New()
			v.SetConfigName("config")
			v.AddConfigPath(dirs["user"])
			v.AddConfigPath(dirs["system"])

			if !readConfig(v, tc.root, dirs["user"], dirs["system"]) {
				t.Fatal("readConfig() = false, want true")
			}

			if got, want := v.ConfigFileUsed(), filepath.Join(dirs[tc.want], "config.yaml"); got != want {
				t.Errorf("ConfigFileUsed() = %q, want %q", got, want)
			}

			if _, err := os.Stat(dirs[tc.absent]); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("%s exists (stat error %v); readConfig must not create it", dirs[tc.absent], err)
			}

			if tc.existing != "" && v.GetString("log.level") != "debug" {
				t.Errorf("log.level = %q, want the existing file's %q", v.GetString("log.level"), "debug")
			}
		})
	}
}

// TestServerOptionsClient checks that the root command's request for the
// server's options has the preflight bound: against a socket whose listener
// accepts every connection and never answers, the request ends within the
// bound, lowered here, with a timeout. The root command ignores that error,
// as it does for a server that is not there.
func TestServerOptionsClient(t *testing.T) {
	lowerPreflightTimeout(t, 200*time.Millisecond)

	socket := shortSocketPath(t)

	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listening on %s: %v", socket, err)
	}

	var (
		mu    sync.Mutex
		conns []net.Conn
	)

	t.Cleanup(func() {
		_ = listener.Close()

		mu.Lock()
		defer mu.Unlock()

		for _, conn := range conns {
			_ = conn.Close()
		}
	})

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}

			mu.Lock()
			conns = append(conns, conn)
			mu.Unlock()
		}
	}()

	done := make(chan error, 1)

	go func() {
		resp, err := serverOptionsClient(socket).Get("http://unix/api/v1/options")
		if err == nil {
			_ = resp.Body.Close()
		}

		done <- err
	}()

	select {
	case err := <-done:
		var urlErr *url.Error
		if !errors.As(err, &urlErr) || !urlErr.Timeout() {
			t.Fatalf("Get() error = %v, want a timeout", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the request for the server's options had no answer after 5s; want it to end within the preflight bound")
	}
}
