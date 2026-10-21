package mm

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/activeshadow/libminimega/minicli"
)

// registerCCExecPattern registers minimega's `namespace` and `cc exec`
// patterns (cmd/minimega/namespace_cli.go, cc_cli.go) with minicli for the
// rest of the test.
func registerCCExecPattern(t *testing.T) {
	t.Helper()

	minicli.Reset()
	t.Cleanup(minicli.Reset)

	noop := func(*minicli.Command, chan<- minicli.Responses) {}

	minicli.MustRegister(&minicli.Handler{Patterns: []string{"namespace <name> (command)"}, Call: noop})
	minicli.MustRegister(&minicli.Handler{Patterns: []string{"cc <exec,> <command>..."}, Call: noop})
}

// TestMinicliArgsMatchesMinicli checks minicliArgs against minicli's own
// lexer: both must read the same arguments, or both refuse the text.
func TestMinicliArgsMatchesMinicli(t *testing.T) {
	registerCCExecPattern(t)

	for _, in := range []string{
		"echo hi",
		`echo "a b" c`,
		`echo 'a "b" c'`,
		`echo a"b c"d`,
		`echo a\"b`,
		`echo "a\"b"`,
		`echo "" x`,
		`echo a\#b`,
		"echo a#b c",
		`echo "a#b"`,
		"echo\ttab  spaces",
		`echo "line\none"`,
		`echo "open`,
		`echo a\`,
		`echo a\qb`,
		"echo é",
	} {
		got, gotErr := minicliArgs("cc exec " + in)

		cmd, wantErr := minicli.Compile("cc exec " + in)
		if (gotErr == nil) != (wantErr == nil) {
			t.Errorf("minicliArgs(%q) error = %v, minicli error = %v", in, gotErr, wantErr)

			continue
		}

		if wantErr != nil {
			continue
		}

		if want := append([]string{"cc", "exec"}, cmd.ListArgs["command"]...); !slices.Equal(got, want) {
			t.Errorf("minicliArgs(%q) = %q, minicli read %q", in, got, want)
		}
	}
}

// TestCheckNamespacedCommandRefusesWhatMinimegaCannotRebuild is a regression
// test: an argument with a quote, backslash or # but no whitespace passes
// minimega's first compile of a namespaced command, is copied raw into the
// command minimega rebuilds for each namespace host, and then fails
// MustCompile, which exits minimega for every experiment. Each refused
// command is shown to break minimega's second compile, and each accepted one
// to survive it verbatim.
func TestCheckNamespacedCommandRefusesWhatMinimegaCannotRebuild(t *testing.T) {
	registerCCExecPattern(t)

	// rebuild compiles cmd as minimega does in a namespace, then compiles the
	// command minicli rebuilt from it, and fails unless both read the same
	// arguments.
	rebuild := func(cmd string) error {
		outer, err := minicli.Compile(`namespace "exp" ` + cmd)
		if err != nil {
			return err
		}

		inner, err := minicli.Compile(outer.Subcommand.Original)
		if err != nil {
			return err
		}

		if !slices.Equal(inner.ListArgs["command"], outer.Subcommand.ListArgs["command"]) {
			return errors.New("the rebuilt command reads different arguments")
		}

		return nil
	}

	for _, cmd := range []string{
		`cc exec echo a\"b`,
		`cc exec echo "a\"b"`,
		`cc exec echo "x y" 1\"2`,
		`cc exec echo it\'s`,
		`cc exec echo a\\b`,
		`cc exec echo a\#b`,
		`cc exec echo "" x`,
		"cc exec echo \"x \x01y\"",
	} {
		if err := rebuild(cmd); err == nil {
			t.Errorf("minimega rebuilds %q intact, so CheckNamespacedCommand need not refuse it", cmd)
		}

		if err := CheckNamespacedCommand(cmd); !errors.Is(err, ErrUnsafeArgument) {
			t.Errorf("CheckNamespacedCommand(%q) = %v, want ErrUnsafeArgument", cmd, err)
		}
	}

	// minicli ends a command at an unquoted #, so minimega would run less
	// than the command, or send another file.
	for _, cmd := range []string{
		"cc exec echo a # comment",
		"cc exec bash /tmp/miniccc/files/exp/vm1-Check_#1",
		"cc send exp/vm1-Check_#1",
		"cc tunnel vm1 50000 127.0.0.1 22 #x",
		"cc exec echo a #",
	} {
		if err := CheckNamespacedCommand(cmd); !errors.Is(err, ErrUnsafeArgument) || !strings.Contains(err.Error(), "#") {
			t.Errorf("CheckNamespacedCommand(%q) = %v, want ErrUnsafeArgument naming what minimega drops", cmd, err)
		}
	}

	for _, cmd := range []string{
		"cc exec pgrep -f ntpd",
		`cc exec powershell -command "Get-Process ntp -ErrorAction SilentlyContinue"`,
		`cc exec bash -c "echo \"a b\" | wc -c"`,
		`cc exec ss -lntu 'sport = 22'`,
		`cc exec echo "a#b c"`,
	} {
		if err := rebuild(cmd); err != nil {
			t.Errorf("minimega cannot rebuild %q: %v", cmd, err)
		}

		if err := CheckNamespacedCommand(cmd); err != nil {
			t.Errorf("CheckNamespacedCommand(%q) = %v, want nil", cmd, err)
		}
	}
}

// emptyMinimega answers every command without error, and lists the VM h1 on
// the host head for `vm info`.
func emptyMinimega(t *testing.T) *fakeMinimega {
	t.Helper()

	fake := &fakeMinimega{answer: func(cmd string) minicli.Responses {
		if strings.HasSuffix(cmd, "vm info") {
			return minicli.Responses{{Host: "head", Header: []string{"host"}, Tabular: [][]string{{"head"}}}}
		}

		return minicli.Responses{{Host: "head"}}
	}}
	fake.serve(t)

	return fake
}

// TestUnsafeArgumentsNeverReachMinimega is a regression test: a C2 command
// from an SOH configuration, the size or VM name of a REST screenshot, the
// VLAN of a REST VM update, and the host of a REST port forward went
// unchecked into a namespaced command, so a quote in them made minimega exit.
func TestUnsafeArgumentsNeverReachMinimega(t *testing.T) {
	tests := []struct {
		name string
		call func() error
		sent string // a command the fake must not get
	}{
		{
			name: "C2 command",
			call: func() error {
				_, err := Minimega{}.ExecC2Command(
					C2NS("exp"), C2VM("h1"), C2Command(`echo a\"b`), C2SkipActiveClientCheck(true),
				)

				return err
			},
			sent: "cc ",
		},
		{
			name: "C2 test-conn",
			call: func() error {
				_, err := Minimega{}.ExecC2Command(
					C2NS("exp"), C2VM("h1"), C2TestConn(`tcp a"b 22 wait 5s`), C2SkipActiveClientCheck(true),
				)

				return err
			},
			sent: "cc ",
		},
		{
			name: "screenshot size",
			call: func() error {
				_, err := Minimega{}.GetVMScreenshot(NS("exp"), VMName("h1"), ScreenshotSize(`"1\"2"`))

				return err
			},
			sent: "vm screenshot",
		},
		{
			name: "screenshot VM name",
			call: func() error {
				_, err := Minimega{}.GetVMScreenshot(NS("exp"), VMName(`h1\'`), ScreenshotSize("215"))

				return err
			},
			sent: "vm screenshot",
		},
		{
			name: "VLAN of vm net connect",
			call: func() error {
				return Minimega{}.ConnectVMInterface(
					NS("exp"), VMName("h1"), ConnectInterface(0), ConnectVLAN(`a\"b`), Bridge("phenix"),
				)
			},
			sent: "vm net",
		},
		{
			name: "VLAN of vm net connect with a space",
			call: func() error {
				return Minimega{}.ConnectVMInterface(
					NS("exp"), VMName("h1"), ConnectInterface(0), ConnectVLAN("100 evil"), Bridge("phenix"),
				)
			},
			sent: "vm net",
		},
		{
			name: "bridge of vm net connect",
			call: func() error {
				return Minimega{}.ConnectVMInterface(
					NS("exp"), VMName("h1"), ConnectInterface(0), ConnectVLAN("100"), Bridge(`br"0`),
				)
			},
			sent: "vm net",
		},
		{
			name: "tunnel host with whitespace",
			call: func() error {
				return Minimega{}.CreateTunnel(
					NS("exp"), VMName("h1"), TunnelSourcePort(50000),
					TunnelDestinationHost("a bind: address already in use"), TunnelDestinationPort(22),
				)
			},
			sent: "vm info",
		},
		{
			name: "tunnel host with #",
			call: func() error {
				return Minimega{}.CreateTunnel(
					NS("exp"), VMName("h1"), TunnelSourcePort(50000), TunnelDestinationHost("10.0.0.1#x"), TunnelDestinationPort(22),
				)
			},
			sent: "cc tunnel",
		},
		{
			name: "tunnel host",
			call: func() error {
				return Minimega{}.CreateTunnel(
					NS("exp"), VMName("h1"), TunnelSourcePort(50000), TunnelDestinationHost(`"a\"b"`), TunnelDestinationPort(22),
				)
			},
			sent: "cc tunnel",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := emptyMinimega(t)

			if err := test.call(); !errors.Is(err, ErrUnsafeArgument) {
				t.Errorf("got %v, want ErrUnsafeArgument", err)
			}

			if sent := fake.sent(); slices.ContainsFunc(sent, func(c string) bool { return strings.Contains(c, test.sent) }) {
				t.Errorf("minimega got the command: %q", sent)
			}
		})
	}
}

func TestCheckArgument(t *testing.T) {
	for _, s := range []string{"h1", "web-01", "C2.node_3", "$HOME", "é"} {
		if err := CheckArgument(s); err != nil {
			t.Errorf("CheckArgument(%q) = %v, want nil", s, err)
		}
	}

	for _, s := range []string{"", "a b", `"h1"`, `a"b`, `h1\'`, "a#b", "h1 #", "\xff"} {
		if err := CheckArgument(s); !errors.Is(err, ErrUnsafeArgument) {
			t.Errorf("CheckArgument(%q) = %v, want ErrUnsafeArgument", s, err)
		}
	}
}

// TestCheckVMName is a regression test: the VM routes authorize the VM name
// as written, and minimega read web1,db1, web[1-3], all and 7 as other VMs
// than that name, so a role scoped to web* stopped or killed db1.
func TestCheckVMName(t *testing.T) {
	for _, s := range []string{"h1", "web-01", "C2.node_3", "ALL", "all1", "web1x"} {
		if err := CheckVMName(s); err != nil {
			t.Errorf("CheckVMName(%q) = %v, want nil", s, err)
		}
	}

	for _, s := range []string{"web1,db1", "web[1-3]", "web]", "all", "7", "+7", "-1", `a"b`, "a b", ""} {
		if err := CheckVMName(s); !errors.Is(err, ErrUnsafeArgument) {
			t.Errorf("CheckVMName(%q) = %v, want ErrUnsafeArgument", s, err)
		}
	}
}

// TestVMTargetsNeverReachMinimega is a regression test: vm start, stop and
// kill took a list, range, ID or the wildcard all as the name of one VM.
func TestVMTargetsNeverReachMinimega(t *testing.T) {
	calls := map[string]func(...Option) error{
		"start": Minimega{}.StartVM,
		"stop":  Minimega{}.StopVM,
		"kill":  Minimega{}.KillVM,
	}

	for verb, call := range calls {
		t.Run(verb, func(t *testing.T) {
			fake := emptyMinimega(t)

			for _, name := range []string{"web1,db1", "web[1-2]", "all", "0"} {
				if err := call(NS("exp"), VMName(name)); !errors.Is(err, ErrUnsafeArgument) {
					t.Errorf("vm %s %s: got %v, want ErrUnsafeArgument", verb, name, err)
				}
			}

			if sent := fake.sent(); len(sent) != 0 {
				t.Errorf("minimega got %q", sent)
			}
		})
	}
}

// TestConnectVMInterfaceSendsVLANAsWritten checks that a VLAN alias the
// checks accept reaches minimega as written, with preprocessing off.
func TestConnectVMInterfaceSendsVLANAsWritten(t *testing.T) {
	fake := emptyMinimega(t)

	err := Minimega{}.ConnectVMInterface(NS("exp"), VMName("h1"), ConnectInterface(0), ConnectVLAN("EXP_1"), Bridge("phenix"))
	if err != nil {
		t.Fatal(err)
	}

	want := `.preprocess false namespace "exp" vm net connect h1 0 EXP_1 phenix`
	if sent := fake.sent(); !slices.Contains(sent, want) {
		t.Errorf("minimega got %q, want %q", sent, want)
	}
}

// TestCheckTunnelHost checks which tunnel destinations reach minimega, and
// that a refusal does not repeat the host, whose text a caller could take for
// a minimega answer such as `bind: address already in use`.
func TestCheckTunnelHost(t *testing.T) {
	for _, host := range []string{"127.0.0.1", "::1", "fe80::1", "localhost", "web-01.lab", "db_1"} {
		if err := checkTunnelHost(host); err != nil {
			t.Errorf("checkTunnelHost(%q) = %v, want nil", host, err)
		}
	}

	for _, host := range []string{"", "a bind: address already in use", "x\"y", "10.0.0.1#x", "-h", "a,b", "$HOME", "file:x"} {
		err := checkTunnelHost(host)
		if !errors.Is(err, ErrUnsafeArgument) {
			t.Errorf("checkTunnelHost(%q) = %v, want ErrUnsafeArgument", host, err)
		}

		if host != "" && err != nil && strings.Contains(err.Error(), host) {
			t.Errorf("checkTunnelHost(%q) = %v, which repeats the host", host, err)
		}
	}
}
