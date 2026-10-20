package v1

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	ifaces "phenix/types/interfaces"
)

func boolPtr(b bool) *bool { return &b }

func nodeWithGateways(external *bool, gateways ...string) Node {
	ifaces := make([]*Interface, len(gateways))
	for i, gw := range gateways {
		ifaces[i] = &Interface{
			NameF:    "eth" + string(rune('0'+i)),
			GatewayF: gw,
		}
	}

	return Node{
		GeneralF:  &General{HostnameF: "test-node"},
		ExternalF: external,
		NetworkF:  &Network{InterfacesF: ifaces},
	}
}

func nodeWithIfaces(ifaces ...*Interface) Node {
	return Node{
		GeneralF: &General{HostnameF: "test-node"},
		NetworkF: &Network{InterfacesF: ifaces},
	}
}

func TestNodeValidate(t *testing.T) {
	tests := []struct {
		name    string
		node    Node
		wantErr bool
		// substr, when set, must appear in the returned error message.
		substr string
	}{
		{
			name: "internal node, single gateway",
			node: nodeWithGateways(nil, "10.0.0.1", ""),
		},
		{
			name: "internal node, no gateways",
			node: nodeWithGateways(nil, "", ""),
		},
		{
			name: "internal node, nil network",
			node: Node{GeneralF: &General{HostnameF: "test-node"}},
		},
		{
			name:    "internal node, two gateways",
			node:    nodeWithGateways(nil, "10.0.0.1", "10.0.1.1"),
			wantErr: true,
			substr:  "more than one gateway",
		},
		{
			name:    "internal node, same gateway on two interfaces",
			node:    nodeWithGateways(nil, "10.0.0.1", "10.0.0.1"),
			wantErr: true,
			substr:  "more than one gateway",
		},
		{
			name: "external node with two gateways is exempt",
			node: nodeWithGateways(boolPtr(true), "10.0.0.1", "10.0.1.1"),
		},
		{
			name:    "external key explicitly false is rejected",
			node:    nodeWithGateways(boolPtr(false), "10.0.0.1"),
			wantErr: true,
			substr:  "external key should not be included",
		},
		{
			name: "internal node, duplicate interface names",
			node: nodeWithIfaces(
				&Interface{NameF: "eth0"},
				&Interface{NameF: "eth0"},
			),
			wantErr: true,
			substr:  "defined more than once",
		},
		{
			name: "internal node, gateway on dhcp interface",
			node: nodeWithIfaces(
				&Interface{NameF: "eth0", ProtoF: "dhcp", GatewayF: "10.0.0.1"},
			),
			wantErr: true,
			substr:  "the gateway is ignored",
		},
		{
			name: "internal node, gateway on manual interface",
			node: nodeWithIfaces(
				&Interface{NameF: "eth0", ProtoF: "manual", GatewayF: "10.0.0.1"},
			),
			wantErr: true,
			substr:  "the gateway is ignored",
		},
		{
			name: "internal node, injection with an empty source",
			node: Node{
				GeneralF:    &General{HostnameF: "test-node"},
				InjectionsF: []*Injection{{SrcF: "motd", DstF: "/etc/motd"}, {DstF: "/etc/ca.pem"}},
			},
			wantErr: true,
			substr:  `injection to "/etc/ca.pem" has no source`,
		},
		{
			name: "internal node, null injection",
			node: Node{
				GeneralF:    &General{HostnameF: "test-node"},
				InjectionsF: []*Injection{{SrcF: "motd", DstF: "/etc/motd"}, nil},
			},
			wantErr: true,
			substr:  "injection 2 is null",
		},
		{
			name: "external node with an injection without a source is exempt",
			node: Node{
				GeneralF:    &General{HostnameF: "test-node"},
				ExternalF:   boolPtr(true),
				InjectionsF: []*Injection{{DstF: "/etc/ca.pem"}},
			},
		},
		{
			name: "internal node, gateway on static interface is allowed",
			node: nodeWithIfaces(
				&Interface{
					NameF: "eth0", ProtoF: "static",
					AddressF: "10.0.0.2", MaskF: 24, GatewayF: "10.0.0.1",
				},
			),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.node.validate()

			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got nil")
				}

				if tt.substr != "" && !strings.Contains(err.Error(), tt.substr) {
					t.Fatalf("error %q does not contain %q", err.Error(), tt.substr)
				}

				return
			}

			if err != nil {
				t.Fatalf("expected no error, got %v", err)
			}
		})
	}
}

func TestNodeValidateDelayTimer(t *testing.T) {
	tests := []struct {
		timer   string
		wantErr bool
	}{
		{timer: "", wantErr: false},
		{timer: "30s", wantErr: false},
		{timer: "5m", wantErr: false},
		{timer: "5", wantErr: true},
		{timer: "soon", wantErr: true},
		{timer: "-10s", wantErr: true},
	}

	for _, test := range tests {
		node := Node{
			GeneralF: &General{HostnameF: "test-node"},
			DelayF:   &Delay{TimerF: test.timer},
		}

		err := node.validate()
		if (err != nil) != test.wantErr {
			t.Errorf("validate() with timer %q = %v, want error %v", test.timer, err, test.wantErr)
		}
	}
}

func TestNodeDelayedReportsTheEnforcedDelay(t *testing.T) {
	tests := []struct {
		name  string
		delay *Delay
		want  string
	}{
		{name: "none", delay: nil, want: ""},
		{name: "timer", delay: &Delay{TimerF: "5s"}, want: "timer:5s"},
		{name: "user wins over timer", delay: &Delay{TimerF: "5s", UserF: true}, want: "user"},
		{name: "c2 wins over timer", delay: &Delay{TimerF: "5s", C2F: []C2Delay{{HostnameF: "a"}}}, want: "cc:a"},
		{name: "user wins over c2", delay: &Delay{UserF: true, C2F: []C2Delay{{HostnameF: "a"}}}, want: "user"},
		{name: "empty c2 hostnames leave the timer", delay: &Delay{TimerF: "5m", C2F: []C2Delay{{}}}, want: "timer:5m"},
		{name: "empty c2 hostname alone", delay: &Delay{C2F: []C2Delay{{}}}, want: ""},
		{name: "empty c2 hostnames are left out", delay: &Delay{C2F: []C2Delay{{}, {HostnameF: "a"}}}, want: "cc:a"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := (Node{DelayF: test.delay}).Delayed(); got != test.want {
				t.Fatalf("Delayed() = %q, want %q", got, test.want)
			}
		})
	}
}

// TestInjectPairsSetsTheModeOfTheResolvedSource is a regression test: the
// start-time script resolved a relative injection source against the
// experiment's base directory but set the injection's permissions on the path
// relative to phenix's working directory, so the injected file kept its mode
// and a file of the same name under the working directory was changed.
func TestInjectPairsSetsTheModeOfTheResolvedSource(t *testing.T) {
	var (
		baseDir = t.TempDir()
		workDir = t.TempDir()
	)

	t.Chdir(workDir)

	for _, dir := range []string{baseDir, workDir} {
		if err := os.WriteFile(filepath.Join(dir, "motd"), []byte("v1\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	injections := []ifaces.NodeInjection{
		&Injection{SrcF: "motd", DstF: "/etc/motd", PermissionsF: "0640"},
		&Injection{SrcF: "/abs/file", DstF: "/etc/file"},
	}

	got, err := InjectPairs(baseDir, injections)
	if err != nil {
		t.Fatalf("InjectPairs() = %v", err)
	}

	want := []string{`"` + baseDir + `/motd":"/etc/motd"`, `"/abs/file":"/etc/file"`}
	if !slices.Equal(got, want) {
		t.Errorf("InjectPairs() = %q, want %q", got, want)
	}

	for dir, mode := range map[string]os.FileMode{baseDir: 0o640, workDir: 0o600} {
		fi, err := os.Stat(filepath.Join(dir, "motd"))
		if err != nil {
			t.Fatal(err)
		}

		if fi.Mode().Perm() != mode {
			t.Errorf("%s/motd has mode %o, want %o", dir, fi.Mode().Perm(), mode)
		}
	}
}

// TestInjectPairsRefusesInjectionWithoutSource is a regression test: an
// injection with an empty or null source, such as one an app returned with
// src: null, was rendered as "<base dir>/", so minimega copied the
// experiment's whole base directory into the VM.
func TestInjectPairsRefusesInjectionWithoutSource(t *testing.T) {
	const baseDir = "/phenix/experiments/exp"

	for _, tc := range []struct {
		name       string
		injections []*Injection
		want       string
	}{
		{
			name:       "empty source",
			injections: []*Injection{{SrcF: "/abs/file", DstF: "/etc/file"}, {DstF: "/etc/ot-sim/ca.pem"}},
			want:       `node rtu-1: injection to "/etc/ot-sim/ca.pem" has no source`,
		},
		{
			name:       "null injection",
			injections: []*Injection{nil},
			want:       "node rtu-1: injection 1 is null",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			node := Node{GeneralF: &General{HostnameF: "rtu-1"}, InjectionsF: tc.injections}

			if pairs, err := InjectPairs(baseDir, node.Injections()); err == nil {
				t.Errorf("InjectPairs() = %q, want an error", pairs)
			}

			got, err := node.FileInjects(baseDir)
			if err == nil || err.Error() != tc.want {
				t.Errorf("FileInjects() = %q, %v, want the error %q", got, err, tc.want)
			}

			if strings.Contains(got, baseDir+`/"`) {
				t.Errorf("FileInjects() rendered the base directory as a source: %q", got)
			}
		})
	}
}

// TestTopologyInitRefusesInjectionWithoutSource checks that an experiment
// whose topology injects a file without a source is refused when it is created
// or updated, naming the node and the destination.
func TestTopologyInitRefusesInjectionWithoutSource(t *testing.T) {
	topo := TopologySpec{NodesF: []*Node{
		{
			GeneralF:    &General{HostnameF: "rtu-1"},
			HardwareF:   &Hardware{},
			InjectionsF: []*Injection{{DstF: "/etc/ot-sim/ca.pem"}},
		},
	}}

	err := topo.Init("phenix")

	want := `validating node rtu-1: injection to "/etc/ot-sim/ca.pem" has no source`
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("Init() = %v, want an error with %q", err, want)
	}
}

// TestInjectPairsEscapesPathsForMinimega is a regression test: a source or
// destination with a double quote ended the quoted path early, so the rest of
// the line became more src:dst pairs, and a backslash failed the minimega
// script with "unexpected escaped character".
func TestInjectPairsEscapesPathsForMinimega(t *testing.T) {
	injections := []ifaces.NodeInjection{
		&Injection{SrcF: `/files/a"b`, DstF: `/etc/a" "/etc/shadow`},
		&Injection{SrcF: `/files/c\d`, DstF: "/Program Files/c d"},
	}

	got, err := InjectPairs("/base", injections)
	if err != nil {
		t.Fatalf("InjectPairs() = %v", err)
	}

	want := []string{`"/files/a\"b":"/etc/a\" \"/etc/shadow"`, `"/files/c\\d":"/Program Files/c d"`}
	if !slices.Equal(got, want) {
		t.Errorf("InjectPairs() = %q, want %q", got, want)
	}
}

// TestCheckInjectionsRefusesPathsMinimegaMisreads is a regression test: a
// line break in a path ended the minimega script's line, so the rest ran as a
// minimega command such as `shell`, and minimega expanded a '$', split the
// pair at a second ':' and followed a destination's ".." out of the VM's disk
// into the host's files.
func TestCheckInjectionsRefusesPathsMinimegaMisreads(t *testing.T) {
	for _, tc := range []struct {
		name     string
		src, dst string
		want     string // empty when the injection is accepted
	}{
		{name: "plain", src: "files/motd", dst: "/etc/motd"},
		{name: "quote and space", src: `/files/a"b c`, dst: "/opt/a b"},
		{name: "dots in names", src: "../shared/motd", dst: "/etc/..motd"},
		{name: "line break in source", src: "x\nshell touch /tmp/pwned", dst: "/etc/x", want: "control character"},
		{name: "tab in destination", src: "x", dst: "/etc/\tx", want: "control character"},
		{name: "dollar", src: "$HOME/x", dst: "/etc/x", want: "'$'"},
		{name: "colon in source", src: "/files/a:b", dst: "/etc/x", want: "':'"},
		{name: "colon in destination", src: "x", dst: "C:/x", want: "':'"},
		{name: "destination leaves the disk", src: "x", dst: "/../../etc/cron.d/x", want: "'..'"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckInjections([]ifaces.NodeInjection{&Injection{SrcF: tc.src, DstF: tc.dst}})

			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("CheckInjections() = %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("CheckInjections() = %v, want an error with %s", err, tc.want)
			}
		})
	}
}

// TestFileDeletionsRefusesAndEscapesPaths is a regression test: deletion
// paths were rendered raw, so a quote or a line break broke the minimega
// script, a ',' split a path, and a ".." made minimega delete the host's own
// files, as root, instead of the VM's.
func TestFileDeletionsRefusesAndEscapesPaths(t *testing.T) {
	node := Node{
		GeneralF:   &General{HostnameF: "web"},
		DeletionsF: []*Deletion{{PathF: `/etc/a"b`}, {PathF: `/opt/c\d e`}},
	}

	got, err := node.FileDeletions()
	if want := `"/etc/a\"b","/opt/c\\d e"`; err != nil || got != want {
		t.Errorf("FileDeletions() = %q, %v, want %q", got, err, want)
	}

	for _, tc := range []struct {
		deletions []*Deletion
		want      string
	}{
		{deletions: []*Deletion{nil}, want: "node web: deletion 1 is null"},
		{deletions: []*Deletion{{PathF: "/etc/x"}, {}}, want: "node web: deletion 2 has no path"},
		{deletions: []*Deletion{{PathF: "/etc/a,/etc/b"}}, want: "','"},
		{deletions: []*Deletion{{PathF: "/../../etc/passwd"}}, want: "'..'"},
		{deletions: []*Deletion{{PathF: "/etc/x\nshell id"}}, want: "control character"},
		{deletions: []*Deletion{{PathF: "/home/$USER"}}, want: "'$'"},
	} {
		node.DeletionsF = tc.deletions

		if got, err := node.FileDeletions(); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("FileDeletions() of %+v = %q, %v, want an error with %s", tc.deletions, got, err, tc.want)
		}
	}
}

// TestNodeValidationRefusesDeletionsMinimegaMisreads checks that topology
// validation refuses the deletions the minimega script refuses.
func TestNodeValidationRefusesDeletionsMinimegaMisreads(t *testing.T) {
	topo := TopologySpec{NodesF: []*Node{{
		TypeF:      "VirtualMachine",
		GeneralF:   &General{HostnameF: "web"},
		HardwareF:  &Hardware{OSTypeF: "linux", DrivesF: []*Drive{{ImageF: "a.qc2"}}},
		DeletionsF: []*Deletion{{PathF: "/../etc/passwd"}},
	}}}

	err := topo.Init("phenix")
	if err == nil || !strings.Contains(err.Error(), `validating node web: deletion path "/../etc/passwd" contains '..'`) {
		t.Fatalf("Init() = %v, want the deletion refused", err)
	}
}

// TestInjectPairsLeavesHostFilesModesAlone is a regression test: phenix,
// running as root, set the mode of whatever file an injection with
// permissions named, so `src: /etc/shadow, permissions: "0777"` made the
// host's shadow file world-writable. A source outside the experiment's base
// directory is injected from a copy with the permissions instead.
func TestInjectPairsLeavesHostFilesModesAlone(t *testing.T) {
	var (
		baseDir = t.TempDir()
		host    = t.TempDir()
		shadow  = filepath.Join(host, "shadow")
	)

	if err := os.WriteFile(shadow, []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.Symlink(shadow, filepath.Join(baseDir, "link")); err != nil {
		t.Fatal(err)
	}

	rel, err := filepath.Rel(baseDir, shadow)
	if err != nil {
		t.Fatal(err)
	}

	for _, src := range []string{shadow, "link", rel} {
		t.Run(src, func(t *testing.T) {
			pairs, err := InjectPairs(baseDir, []ifaces.NodeInjection{
				&Injection{SrcF: src, DstF: "/etc/shadow", PermissionsF: "0777"},
			})
			if err != nil {
				t.Fatalf("InjectPairs() = %v", err)
			}

			if fi, err := os.Stat(shadow); err != nil || fi.Mode().Perm() != 0o600 {
				t.Fatalf("the host file has mode %v (%v), want it left at 600", fi.Mode().Perm(), err)
			}

			staged, ok := strings.CutPrefix(pairs[0], `"`+filepath.Join(baseDir, "injections")+"/")
			if !ok || !strings.HasSuffix(staged, `-0777-shadow":"/etc/shadow"`) {
				t.Fatalf("InjectPairs() = %q, want a copy in the experiment's injections directory", pairs)
			}

			copied := filepath.Join(baseDir, "injections", strings.TrimSuffix(staged, `":"/etc/shadow"`))

			fi, err := os.Stat(copied)
			if err != nil || fi.Mode().Perm() != 0o777 {
				t.Fatalf("the copy %s has mode %v (%v), want 777", copied, fi.Mode().Perm(), err)
			}

			if got, _ := os.ReadFile(copied); string(got) != "secret\n" {
				t.Errorf("the copy holds %q, want the source's content", got)
			}
		})
	}
}

func TestInjectPairsPermissions(t *testing.T) {
	var (
		baseDir = t.TempDir()
		host    = t.TempDir()
	)

	for _, tc := range []struct {
		name string
		src  string
		perm string
		want string // an error with it, or empty
	}{
		{name: "missing source", src: "/no/such/file", perm: "0644"},
		{name: "directory outside the base directory", src: host, perm: "0755", want: "not a regular file"},
		{name: "not octal", src: "/no/such/file", perm: "rw-r--r--", want: `permissions "rw-r--r--"`},
		{name: "beyond the permission bits", src: "/no/such/file", perm: "4755", want: `permissions "4755"`},
		{name: "too long", src: "/no/such/file", perm: "00644", want: `permissions "00644"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pairs, err := InjectPairs(baseDir, []ifaces.NodeInjection{
				&Injection{SrcF: tc.src, DstF: "/etc/x", PermissionsF: tc.perm},
			})

			if tc.want == "" {
				if want := `"` + tc.src + `":"/etc/x"`; err != nil || len(pairs) != 1 || pairs[0] != want {
					t.Fatalf("InjectPairs() = %q, %v, want %q", pairs, err, want)
				}

				return
			}

			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("InjectPairs() = %q, %v, want an error with %s", pairs, err, tc.want)
			}
		})
	}

	err := CheckInjections([]ifaces.NodeInjection{&Injection{SrcF: "x", DstF: "/etc/x", PermissionsF: "0x1ff"}})
	if err == nil || !strings.Contains(err.Error(), `injection to "/etc/x": permissions "0x1ff"`) {
		t.Errorf("CheckInjections() = %v, want the permissions refused", err)
	}
}

func TestNodeValidationRefusesAdvancedSettingsWithControlCharacters(t *testing.T) {
	topo := TopologySpec{NodesF: []*Node{{
		TypeF:     "VirtualMachine",
		GeneralF:  &General{HostnameF: "web"},
		HardwareF: &Hardware{OSTypeF: "linux", DrivesF: []*Drive{{ImageF: "a.qc2"}}},
		AdvancedF: map[string]string{"qemu-append": "-vga qxl", "serial": "1\nshell id"},
	}}}

	err := topo.Init("phenix")
	if err == nil || !strings.Contains(err.Error(), `validating node web: advanced setting "serial" contains a control character`) {
		t.Fatalf("Init() = %v, want the advanced setting refused", err)
	}
}

// TestNodeValidationRefusesValuesTheScriptsWriteUnquoted is a regression test:
// a line break in a CPU, drive, interface or C2 command of a node passed
// validation and added its own minimega command to the scripts.
func TestNodeValidationRefusesValuesTheScriptsWriteUnquoted(t *testing.T) {
	const evil = "x\nshell id"

	for _, test := range []struct {
		name string
		set  func(*Node)
		want string
	}{
		{name: "CPU", set: func(n *Node) { n.HardwareF.CPUF = evil }, want: "CPU: "},
		{name: "VM type", set: func(n *Node) { n.GeneralF.VMTypeF = "kvm x" }, want: "VM type: "},
		{name: "drive image", set: func(n *Node) { n.HardwareF.DrivesF[0].ImageF = evil }, want: "drive 1 image: "},
		{name: "drive interface", set: func(n *Node) { n.HardwareF.DrivesF[0].IfaceF = `virtio"` }, want: "drive 1 interface: "},
		{name: "interface VLAN", set: func(n *Node) { n.NetworkF.InterfacesF[0].VLANF = evil }, want: "interface eth0 VLAN: "},
		{name: "interface bridge", set: func(n *Node) { n.NetworkF.InterfacesF[0].BridgeF = "br #0" }, want: "interface eth0 bridge: "},
		{name: "interface MAC", set: func(n *Node) { n.NetworkF.InterfacesF[0].MACF = evil }, want: "interface eth0 MAC: "},
		{name: "interface driver", set: func(n *Node) { n.NetworkF.InterfacesF[0].DriverF = evil }, want: "interface eth0 driver: "},
		{name: "C2 command", set: func(n *Node) { n.CommandsF = []string{"exec id", evil} }, want: `command 2 "x\nshell id" contains a control character`},
	} {
		t.Run(test.name, func(t *testing.T) {
			newTopo := func() TopologySpec {
				return TopologySpec{NodesF: []*Node{{
					TypeF:     "VirtualMachine",
					GeneralF:  &General{HostnameF: "web", VMTypeF: "kvm"},
					HardwareF: &Hardware{OSTypeF: "linux", CPUF: "Broadwell", DrivesF: []*Drive{{ImageF: "a.qc2", IfaceF: "virtio"}}},
					NetworkF: &Network{InterfacesF: []*Interface{{
						NameF: "eth0", VLANF: "LAN", BridgeF: "phenix", MACF: "00:11:22:33:44:55", DriverF: "e1000",
					}}},
					CommandsF: []string{`exec bash -c "echo hi"`},
				}}}
			}

			if topo := newTopo(); topo.Init("phenix") != nil {
				t.Fatalf("Init() of the valid topology = %v", topo.Init("phenix"))
			}

			topo := newTopo()
			test.set(topo.NodesF[0])

			err := topo.Init("phenix")
			if err == nil || !strings.Contains(err.Error(), "validating node web: "+test.want) {
				t.Fatalf("Init() = %v, want one with %q", err, "validating node web: "+test.want)
			}
		})
	}
}

// TestNodeValidationAcceptsTheLineBreakABlockScalarEndsWith is a regression
// test: the refusal of control characters in advanced settings and C2
// commands also refused the line break a YAML block scalar (`|` or `>`) ends
// with, which the minimega script line ends anyway.
func TestNodeValidationAcceptsTheLineBreakABlockScalarEndsWith(t *testing.T) {
	var node Node

	if err := yaml.Unmarshal([]byte(`
type: VirtualMachine
general: {hostname: web}
hardware: {os_type: linux, drives: [{image: a.qc2}]}
advanced:
  qemu-append: >
    -vga qxl
commands:
  - |
    exec df -h
`), &node); err != nil {
		t.Fatal(err)
	}

	if node.AdvancedF["qemu-append"] != "-vga qxl\n" || node.CommandsF[0] != "exec df -h\n" {
		t.Fatalf("YAML decoded advanced %q and commands %q, want values ending in a line break", node.AdvancedF, node.CommandsF)
	}

	topo := TopologySpec{NodesF: []*Node{&node}}
	if err := topo.Init("phenix"); err != nil {
		t.Fatalf("Init() = %v, want the trailing line breaks accepted", err)
	}

	advanced, err := node.AdvancedConfig()
	if err != nil || advanced["qemu-append"] != "-vga qxl" {
		t.Errorf("AdvancedConfig() = %q, %v, want the value without its line break", advanced, err)
	}

	commands, err := node.CCCommands()
	if err != nil || !slices.Equal(commands, []string{"exec df -h"}) {
		t.Errorf("CCCommands() = %q, %v, want the command without its line break", commands, err)
	}

	node.AdvancedF["qemu-append"] = "-vga qxl\nshell id\n"

	const want = `advanced setting "qemu-append" contains a control character`

	if err := topo.Init("phenix"); err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("Init() = %v, want an interior line break refused", err)
	}
}

// TestInjectPairsRefusesTwoModesForOneBaseDirSource is a regression test: two
// injections of one source in the base directory with different permissions
// both set them on that file, so both injected it with the mode set last.
func TestInjectPairsRefusesTwoModesForOneBaseDirSource(t *testing.T) {
	var (
		baseDir = t.TempDir()
		host    = filepath.Join(t.TempDir(), "host")
	)

	for _, f := range []string{filepath.Join(baseDir, "f"), host} {
		if err := os.WriteFile(f, []byte("data"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if err := os.Symlink(filepath.Join(baseDir, "f"), filepath.Join(baseDir, "link")); err != nil {
		t.Fatal(err)
	}

	inject := func(src, dst, perms string) ifaces.NodeInjection {
		return &Injection{SrcF: src, DstF: dst, PermissionsF: perms}
	}

	for _, tc := range []struct {
		name    string
		second  ifaces.NodeInjection
		refused bool
	}{
		{name: "another mode", second: inject("f", "/b", "0644"), refused: true},
		{name: "another mode through a link", second: inject("link", "/b", "0644"), refused: true},
		{name: "the same mode", second: inject(filepath.Join(baseDir, "f"), "/b", "0700")},
		{name: "another source", second: inject(host, "/b", "0644")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := InjectPairs(baseDir, []ifaces.NodeInjection{inject("f", "/a", "0700"), tc.second})

			if tc.refused != (err != nil) {
				t.Fatalf("InjectPairs() = %v, want refused %t", err, tc.refused)
			}

			if tc.refused && !strings.Contains(err.Error(), `the injection to "/a" and the injection to "/b"`) {
				t.Errorf("InjectPairs() = %v, want both injections named", err)
			}
		})
	}
}

// TestCheckInjectionModesAcrossNodes is a regression test: injections of one
// source in the base directory into two nodes with different permissions both
// set them on that file as the minimega script was rendered, so both nodes got
// the mode set last. A node the script does not inject into is left out.
func TestCheckInjectionModesAcrossNodes(t *testing.T) {
	baseDir := t.TempDir()

	if err := os.WriteFile(filepath.Join(baseDir, "f"), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}

	node := func(hostname, perms string, dnb bool) ifaces.NodeSpec {
		return &Node{
			GeneralF:    &General{HostnameF: hostname, SnapshotF: boolPtr(true), DoNotBootF: boolPtr(dnb)},
			HardwareF:   &Hardware{DrivesF: []*Drive{{ImageF: "base.qc2"}}},
			InjectionsF: []*Injection{{SrcF: "f", DstF: "/etc/x", PermissionsF: perms}},
		}
	}

	err := CheckInjectionModes(baseDir, []ifaces.NodeSpec{node("a", "0700", false), node("b", "0644", false)})
	if err == nil || !strings.Contains(err.Error(), `node a's injection to "/etc/x" and node b's injection to "/etc/x"`) {
		t.Errorf("CheckInjectionModes() = %v, want both nodes' injections named", err)
	}

	err = CheckInjectionModes(baseDir, []ifaces.NodeSpec{node("a", "0700", false), node("b", "0644", true)})
	if err != nil {
		t.Errorf("CheckInjectionModes() with a do-not-boot node = %v, want no refusal", err)
	}
}
