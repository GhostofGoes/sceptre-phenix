package tmpl_test

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	"phenix/tmpl"
	v1 "phenix/types/version/v1"
)

// TestMinimegaScriptVLANRange proves the script passes a VLAN range to
// minimega unchanged, so minimega assigns from [min, max), and renders no range
// unless both ends are set.
func TestMinimegaScriptVLANRange(t *testing.T) {
	tests := []struct {
		name     string
		min, max int
		want     []string // the script's `vlans range` lines
	}{
		{name: "both ends", min: 300, max: 305, want: []string{"vlans range 300 305"}},
		{name: "only the min", min: 320, want: nil},
		{name: "only the max", max: 3000, want: nil},
		{name: "no range", want: nil},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			spec := &v1.ExperimentSpec{
				ExperimentNameF: "exp",
				VLANsF: &v1.VLANSpec{
					AliasesF: map[string]int{"SW1": 0},
					MinF:     test.min,
					MaxF:     test.max,
				},
			}

			var buf bytes.Buffer

			if err := tmpl.GenerateFromTemplate("minimega_script.tmpl", spec, &buf); err != nil {
				t.Fatalf("GenerateFromTemplate() error = %v", err)
			}

			var got []string

			for line := range strings.Lines(buf.String()) {
				if line = strings.TrimSpace(line); strings.HasPrefix(line, "vlans range") {
					got = append(got, line)
				}
			}

			if !slices.Equal(got, test.want) {
				t.Errorf("vlans range lines = %q, want %q\n%s", got, test.want, buf.String())
			}
		})
	}
}

// TestMinimegaScriptRefusesInjectionWithoutSource is a regression test: the
// script rendered an injection whose source an app left empty or null as
// "<base dir>/", so minimega copied the experiment's whole base directory into
// the VM. The script fails to render, naming the node and destination.
func TestMinimegaScriptRefusesInjectionWithoutSource(t *testing.T) {
	const baseDir = "/phenix/experiments/exp"

	for _, test := range []struct {
		name    string
		inject  *v1.Injection
		want    string // the script's disk inject line holds it
		wantErr string
	}{
		{
			name:   "relative source",
			inject: &v1.Injection{SrcF: "motd", DstF: "/etc/motd"},
			want:   `files "` + baseDir + `/motd":"/etc/motd"`,
		},
		{
			name:    "empty source",
			inject:  &v1.Injection{DstF: "/etc/ot-sim/ca.pem"},
			wantErr: `node rtu-1: injection to "/etc/ot-sim/ca.pem" has no source`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			snapshot, dnb := true, false

			spec := &v1.ExperimentSpec{
				ExperimentNameF: "exp",
				BaseDirF:        baseDir,
				VLANsF:          &v1.VLANSpec{AliasesF: map[string]int{}},
				TopologyF: &v1.TopologySpec{NodesF: []*v1.Node{{
					TypeF:       "VirtualMachine",
					GeneralF:    &v1.General{HostnameF: "rtu-1", VMTypeF: "kvm", SnapshotF: &snapshot, DoNotBootF: &dnb},
					HardwareF:   &v1.Hardware{OSTypeF: "linux", DrivesF: []*v1.Drive{{ImageF: "base.qc2"}}},
					InjectionsF: []*v1.Injection{test.inject},
				}}},
			}

			var buf bytes.Buffer

			err := tmpl.GenerateFromTemplate("minimega_script.tmpl", spec, &buf)

			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("GenerateFromTemplate() error = %v, want one with %q", err, test.wantErr)
				}

				if strings.Contains(buf.String(), baseDir+`/"`) {
					t.Errorf("the script injects the base directory:\n%s", buf.String())
				}

				return
			}

			if err != nil {
				t.Fatalf("GenerateFromTemplate() error = %v", err)
			}

			if !strings.Contains(buf.String(), test.want) {
				t.Errorf("the script has no %q:\n%s", test.want, buf.String())
			}
		})
	}
}

// TestMinimegaScriptEscapesDiskPaths is a regression test: injection and
// deletion paths were rendered raw, so a double quote or a line break in one
// changed the minimega script, and a deletion with ".." made minimega delete
// the host's own files.
func TestMinimegaScriptEscapesDiskPaths(t *testing.T) {
	render := func(t *testing.T, deletion string) (string, error) {
		t.Helper()

		snapshot, dnb := true, false

		spec := &v1.ExperimentSpec{
			ExperimentNameF: "exp",
			BaseDirF:        "/phenix/experiments/exp",
			VLANsF:          &v1.VLANSpec{AliasesF: map[string]int{}},
			TopologyF: &v1.TopologySpec{NodesF: []*v1.Node{{
				TypeF:       "VirtualMachine",
				GeneralF:    &v1.General{HostnameF: "web", VMTypeF: "kvm", SnapshotF: &snapshot, DoNotBootF: &dnb},
				HardwareF:   &v1.Hardware{OSTypeF: "linux", DrivesF: []*v1.Drive{{ImageF: "base.qc2"}}},
				InjectionsF: []*v1.Injection{{SrcF: `/files/say "hi"`, DstF: `/etc/say "hi"`}},
				DeletionsF:  []*v1.Deletion{{PathF: deletion}},
			}}},
		}

		var buf bytes.Buffer

		err := tmpl.GenerateFromTemplate("minimega_script.tmpl", spec, &buf)

		return buf.String(), err
	}

	script, err := render(t, `/etc/a"b`)
	if err != nil {
		t.Fatalf("GenerateFromTemplate() error = %v", err)
	}

	for _, want := range []string{
		`files "/files/say \"hi\"":"/etc/say \"hi\""`,
		`delete files "/etc/a\"b"`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("the script has no %q:\n%s", want, script)
		}
	}

	if _, err := render(t, "/../../etc/passwd"); err == nil || !strings.Contains(err.Error(), "node web: deletion path") {
		t.Errorf("GenerateFromTemplate() of a deletion with '..' = %v, want it refused", err)
	}
}

// TestMinimegaScriptQuotesOverridesAndRefusesBrokenAdvancedSettings is a
// regression test: qemu-override values were rendered raw between quotes, so
// a quote or a backslash in one failed the script or split the value, and an
// advanced setting with a line break added its own minimega command, such as
// `shell`, to the script.
func TestMinimegaScriptQuotesOverridesAndRefusesBrokenAdvancedSettings(t *testing.T) {
	render := func(t *testing.T, advanced map[string]string) (string, error) {
		t.Helper()

		snapshot, dnb := false, false

		spec := &v1.ExperimentSpec{
			ExperimentNameF: "exp",
			BaseDirF:        "/phenix/experiments/exp",
			VLANsF:          &v1.VLANSpec{AliasesF: map[string]int{}},
			TopologyF: &v1.TopologySpec{NodesF: []*v1.Node{{
				TypeF:      "VirtualMachine",
				GeneralF:   &v1.General{HostnameF: "web", VMTypeF: "kvm", SnapshotF: &snapshot, DoNotBootF: &dnb},
				HardwareF:  &v1.Hardware{OSTypeF: "linux", DrivesF: []*v1.Drive{{ImageF: "base.qc2"}}},
				OverridesF: map[string]string{`-drive "a"`: `-drive C:\b`},
				AdvancedF:  advanced,
			}}},
		}

		var buf bytes.Buffer

		err := tmpl.GenerateFromTemplate("minimega_script.tmpl", spec, &buf)

		return buf.String(), err
	}

	script, err := render(t, map[string]string{"qemu-append": "-vga qxl -smp 2"})
	if err != nil {
		t.Fatalf("GenerateFromTemplate() error = %v", err)
	}

	for _, want := range []string{
		`vm config qemu-override "-drive \"a\"" "-drive C:\\b"`,
		"vm config qemu-append -vga qxl -smp 2",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("the script has no %q:\n%s", want, script)
		}
	}

	_, err = render(t, map[string]string{"qemu-append": "-vga qxl\nshell touch /tmp/pwned"})
	if err == nil || !strings.Contains(err.Error(), `node web: advanced setting "qemu-append" contains a control character`) {
		t.Errorf("GenerateFromTemplate() of an advanced setting with a line break = %v, want it refused", err)
	}
}

// TestMinimegaScriptRefusesValuesMinimegaWouldSplit is a regression test: the
// script rendered VLAN aliases, schedule hosts, hostnames, VM types, CPUs,
// drives and interfaces raw, so a line break in one added its own minimega
// command, such as `shell`, to the script, which `read` then ran on the head
// node. The script fails to render, naming the node and the field.
func TestMinimegaScriptRefusesValuesMinimegaWouldSplit(t *testing.T) {
	const evil = "x\nshell touch /tmp/pwned"

	newSpec := func() *v1.ExperimentSpec {
		snapshot, dnb := true, false

		return &v1.ExperimentSpec{
			ExperimentNameF: "exp",
			BaseDirF:        "/phenix/experiments/exp",
			VLANsF:          &v1.VLANSpec{AliasesF: map[string]int{"LAN": 300}},
			SchedulesF:      map[string]string{"web": "compute-0"},
			TopologyF: &v1.TopologySpec{NodesF: []*v1.Node{{
				TypeF:    "VirtualMachine",
				GeneralF: &v1.General{HostnameF: "web", VMTypeF: "kvm", SnapshotF: &snapshot, DoNotBootF: &dnb},
				HardwareF: &v1.Hardware{
					OSTypeF: "linux", CPUF: "Broadwell",
					DrivesF: []*v1.Drive{{ImageF: "base.qc2", IfaceF: "virtio"}, {ImageF: "data.qc2"}},
				},
				NetworkF: &v1.Network{InterfacesF: []*v1.Interface{{
					NameF: "eth0", VLANF: "LAN", BridgeF: "phenix", MACF: "00:11:22:33:44:55", DriverF: "e1000",
				}}},
				CommandsF: []string{`exec bash -c "echo hi"`},
			}}},
		}
	}

	// render fails the test if the rendered script has its own shell line.
	render := func(t *testing.T, name string, spec *v1.ExperimentSpec, script *string) error {
		t.Helper()

		var (
			buf  bytes.Buffer
			data any = spec
		)

		if name == "minimega_cc_script.tmpl" {
			data = spec.TopologyF.Nodes()
		}

		err := tmpl.GenerateFromTemplate(name, data, &buf)

		for line := range strings.Lines(buf.String()) {
			if strings.HasPrefix(strings.TrimSpace(line), "shell") {
				t.Errorf("the %s has its own shell line %q:\n%s", name, line, buf.String())
			}
		}

		if script != nil {
			*script = buf.String()
		}

		return err
	}

	var script string

	if err := render(t, "minimega_script.tmpl", newSpec(), &script); err != nil {
		t.Fatalf("GenerateFromTemplate() error = %v", err)
	}

	for _, want := range []string{
		"vlans add LAN 300",
		"vm config schedule compute-0",
		"vm config cpu Broadwell",
		"vm config disk " + newSpec().SnapshotName("web") + ",virtio,writeback data.qc2",
		"vm config net phenix,LAN,00:11:22:33:44:55,e1000",
		"vm launch kvm web",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("the script has no %q:\n%s", want, script)
		}
	}

	node := func(s *v1.ExperimentSpec) *v1.Node { return s.TopologyF.NodesF[0] }

	for _, test := range []struct {
		name    string
		tmpl    string
		set     func(*v1.ExperimentSpec)
		wantErr string
	}{
		{
			name: "VLAN alias",
			set: func(s *v1.ExperimentSpec) {
				s.VLANsF.AliasesF = map[string]int{"LAN 300\nshell touch /tmp/pwned\nvlans add LAN2": 301}
			},
			wantErr: "VLAN alias: ",
		},
		{
			name:    "schedule host",
			set:     func(s *v1.ExperimentSpec) { s.SchedulesF["web"] = evil },
			wantErr: "node web: schedule host: ",
		},
		{
			name:    "hostname",
			set:     func(s *v1.ExperimentSpec) { node(s).GeneralF.HostnameF = evil },
			wantErr: "hostname: ",
		},
		{
			name:    "VM type",
			set:     func(s *v1.ExperimentSpec) { node(s).GeneralF.VMTypeF = evil },
			wantErr: "node web: VM type: ",
		},
		{
			name:    "CPU",
			set:     func(s *v1.ExperimentSpec) { node(s).HardwareF.CPUF = evil },
			wantErr: "node web: CPU: ",
		},
		{
			name:    "first drive image",
			set:     func(s *v1.ExperimentSpec) { node(s).HardwareF.DrivesF[0].ImageF = evil },
			wantErr: "node web: drive 1 image: ",
		},
		{
			name: "drive image without a snapshot",
			set: func(s *v1.ExperimentSpec) {
				snapshot := false
				node(s).GeneralF.SnapshotF = &snapshot
				node(s).HardwareF.DrivesF[0].ImageF = evil
			},
			wantErr: "node web: drive 1 image: ",
		},
		{
			name:    "second drive image",
			set:     func(s *v1.ExperimentSpec) { node(s).HardwareF.DrivesF[1].ImageF = "data.qc2 #" },
			wantErr: "node web: drive 2 image: ",
		},
		{
			name:    "drive cache mode",
			set:     func(s *v1.ExperimentSpec) { node(s).HardwareF.DrivesF[1].CacheModeF = evil },
			wantErr: "node web: drive 2 cache mode: ",
		},
		{
			name:    "interface VLAN",
			set:     func(s *v1.ExperimentSpec) { node(s).NetworkF.InterfacesF[0].VLANF = evil },
			wantErr: "node web: interface eth0 VLAN: ",
		},
		{
			name:    "interface bridge",
			set:     func(s *v1.ExperimentSpec) { node(s).NetworkF.InterfacesF[0].BridgeF = `phenix"` },
			wantErr: "node web: interface eth0 bridge: ",
		},
		{
			name:    "interface driver",
			set:     func(s *v1.ExperimentSpec) { node(s).NetworkF.InterfacesF[0].DriverF = evil },
			wantErr: "node web: interface eth0 driver: ",
		},
		{
			name:    "C2 command",
			tmpl:    "minimega_cc_script.tmpl",
			set:     func(s *v1.ExperimentSpec) { node(s).CommandsF = []string{"exec id\nshell touch /tmp/pwned"} },
			wantErr: `node web: command 1 "exec id\nshell touch /tmp/pwned" contains a control character`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			name := test.tmpl
			if name == "" {
				name = "minimega_script.tmpl"
			}

			if err := render(t, name, newSpec(), nil); err != nil {
				t.Fatalf("GenerateFromTemplate(%s) of the valid spec: error = %v", name, err)
			}

			spec := newSpec()
			test.set(spec)

			err := render(t, name, spec, nil)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("GenerateFromTemplate(%s) error = %v, want one with %q", name, err, test.wantErr)
			}
		})
	}
}

// TestPacketCaptureScriptRefusesValuesMinimegaWouldSplit is a regression
// test: the packet capture script rendered a capture node's CPU, drives and
// interfaces raw, like the minimega script.
func TestPacketCaptureScriptRefusesValuesMinimegaWouldSplit(t *testing.T) {
	render := func(cpu string) (string, error) {
		spec := &v1.ExperimentSpec{
			ExperimentNameF: "exp",
			BaseDirF:        "/phenix/experiments/exp",
			TopologyF: &v1.TopologySpec{NodesF: []*v1.Node{{
				LabelsF:   map[string]string{"soh-monitor-node": "true"},
				GeneralF:  &v1.General{HostnameF: "mon", VMTypeF: "kvm"},
				HardwareF: &v1.Hardware{CPUF: cpu, DrivesF: []*v1.Drive{{ImageF: "base.qc2"}}},
				NetworkF:  &v1.Network{InterfacesF: []*v1.Interface{{NameF: "eth0", VLANF: "MGMT", BridgeF: "phenix"}}},
			}}},
		}

		data := struct {
			Exp *v1.ExperimentSpec
			Mon map[string][]string
		}{Exp: spec}

		var buf bytes.Buffer

		err := tmpl.GenerateFromTemplate("packet_capture_script.tmpl", data, &buf)

		return buf.String(), err
	}

	script, err := render("Broadwell")
	if err != nil {
		t.Fatalf("GenerateFromTemplate() error = %v", err)
	}

	if !strings.Contains(script, "vm config cpu Broadwell") || !strings.Contains(script, "vm config net phenix,MGMT") {
		t.Errorf("the script has no CPU or net line:\n%s", script)
	}

	script, err = render("x\nshell touch /tmp/pwned")
	if err == nil || !strings.Contains(err.Error(), "node mon: CPU: ") {
		t.Errorf("GenerateFromTemplate() of a CPU with a line break = %v, want it refused", err)
	}

	if strings.Contains(script, "\nshell") {
		t.Errorf("the script has its own shell line:\n%s", script)
	}
}
