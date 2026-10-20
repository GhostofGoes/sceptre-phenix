package mm

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"phenix/util/common"
)

func TestConnectVMInterfaceCommand(t *testing.T) {
	tests := []struct {
		name string
		opts []Option
		want string
	}{
		{
			name: "with bridge",
			opts: []Option{
				VMName("test-vm"),
				ConnectInterface(1),
				ConnectVLAN("EXP_1"),
				Bridge("phenix"),
			},
			want: "vm net connect test-vm 1 EXP_1 phenix",
		},
		{
			name: "without bridge",
			opts: []Option{
				VMName("test-vm"),
				ConnectInterface(1),
				ConnectVLAN("EXP_1"),
			},
			want: "vm net connect test-vm 1 EXP_1",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := connectVMInterfaceCommand(NewOptions(test.opts...)); got != test.want {
				t.Fatalf("connectVMInterfaceCommand returned %q, want %q", got, test.want)
			}
		})
	}
}

func TestFirstDiskPath(t *testing.T) {
	tests := []struct {
		name   string
		column string
		want   string
	}{
		{name: "no disk", column: "", want: ""},
		{name: "container", column: "N/A", want: ""},
		{name: "path only", column: "/phenix/images/a.qc2", want: "/phenix/images/a.qc2"},
		{name: "with cache", column: "host_exp_vm_snapshot,writeback", want: "host_exp_vm_snapshot"},
		{name: "with interface and cache", column: "a.qc2,virtio,writeback", want: "a.qc2"},
		{name: "two disks", column: "a.qc2,writeback b.qc2,writeback", want: "a.qc2"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := firstDiskPath(test.column); got != test.want {
				t.Fatalf("firstDiskPath(%q) = %q, want %q", test.column, got, test.want)
			}
		})
	}
}

func TestRedeployLaunchCommand(t *testing.T) {
	tests := []struct {
		vmType string
		want   string
	}{
		{vmType: "kvm", want: "vm launch kvm vm1"},
		{vmType: "container", want: "vm launch container vm1"},
		{vmType: "", want: "vm launch kvm vm1"},
	}

	for _, test := range tests {
		if got := redeployLaunchCommand(test.vmType, "vm1"); got != test.want {
			t.Errorf("redeployLaunchCommand(%q) = %q, want %q", test.vmType, got, test.want)
		}
	}
}

func TestParseC2Response(t *testing.T) {
	const (
		a = "8f2f8e1e-d781-4fd7-99c6-f8ce41df531a"
		b = "83028c26-d868-4032-9dd2-6bc0fd47d12a"
	)

	resp := "7/" + a + "/stderr:\n" +
		"warning\n" +
		"\n" +
		"7/" + a + "/stdout:\n" +
		"7/x/stdout: poison\n" +
		"line two\n" +
		"\n" +
		"7/" + b + "/stdout:\n" +
		"other vm\n"

	tests := []struct {
		name string
		uuid string
		typ  C2ResponseType
		want string
	}{
		{name: "guest output that looks like a header is output", uuid: a, typ: C2ResponseStdout, want: "7/x/stdout: poison\nline two\n"},
		{name: "stderr block", uuid: a, typ: C2ResponseStderr, want: "warning\n"},
		{name: "other client", uuid: b, typ: C2ResponseStdout, want: "other vm"},
		{name: "missing block", uuid: b, typ: C2ResponseStderr, want: ""},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := parseC2Response(resp, test.uuid, test.typ); got != test.want {
				t.Fatalf("parseC2Response() = %q, want %q", got, test.want)
			}
		})
	}

	t.Run("a line over 1 MiB", func(t *testing.T) {
		long := strings.Repeat("x", 2<<20)
		resp := "5/" + a + "/stdout:\nfirst\n" + long + "\nlast\n"

		if got, want := parseC2Response(resp, a, C2ResponseStdout), "first\n"+long+"\nlast"; got != want {
			t.Fatalf("parseC2Response() returned %d bytes, want all %d: the long line cut the output short", len(got), len(want))
		}
	})

	t.Run("CRLF line ends", func(t *testing.T) {
		resp := "5/" + a + "/stdout:\r\nC:\\> dir\r\n\r\n5/" + b + "/stdout:\r\nother\r\n"

		if got := parseC2Response(resp, a, C2ResponseStdout); got != "C:\\> dir\n" {
			t.Fatalf("parseC2Response() = %q, want %q", got, "C:\\> dir\n")
		}
	})

	t.Run("an empty block does not swallow the next one", func(t *testing.T) {
		resp := "3/" + a + "/stdout:\n" +
			"3/" + a + "/stderr:\n" +
			"boom\n"

		if got := parseC2Response(resp, a, C2ResponseStdout); got != "" {
			t.Fatalf("parseC2Response() = %q, want empty stdout", got)
		}
	})
}

func TestCountRangedNames(t *testing.T) {
	tests := map[string]int{
		"":                     0,
		"web":                  1,
		"a,b,c":                3,
		"vm[1-3]":              3,
		"node[0-8],n[0-4,6-9]": 18,
		"foo2.bar[1-2,5],foo[3-4].bar[1-2],foo[0-1].bar[1-3,5],foo[0-4].car": 20,
	}

	for in, want := range tests {
		if got := countRangedNames(in); got != want {
			t.Errorf("countRangedNames(%q) = %d, want %d", in, got, want)
		}
	}
}

// TestClusterHostsKeepsMinimegaHostNames is a regression test: the cluster
// hosts carried names with the hostname suffixes trimmed, so schedulers wrote
// names such as e2e-head for a minimega running as e2e-head-minimega, and
// minimega rejected the start with `VM scheduled on unknown host`.
func TestClusterHostsKeepsMinimegaHostNames(t *testing.T) {
	head := Host{Name: "e2e-head-minimega"}

	tests := []struct {
		name      string
		compute   Hosts
		schedOnly bool
		want      Hosts
	}{
		{
			name:    "single node",
			compute: Hosts{{Name: "e2e-head-minimega"}},
			want:    Hosts{{Name: "e2e-head-minimega", Schedulable: true, Headnode: true}},
		},
		{
			name:      "single node, schedulable only",
			compute:   Hosts{{Name: "e2e-head-minimega"}},
			schedOnly: true,
			want:      Hosts{{Name: "e2e-head-minimega", Schedulable: true, Headnode: true}},
		},
		{
			name:    "compute nodes",
			compute: Hosts{{Name: "compute1-minimega"}, {Name: "compute2-phenix"}},
			want: Hosts{
				{Name: "compute1-minimega", Schedulable: true},
				{Name: "compute2-phenix", Schedulable: true},
				{Name: "e2e-head-minimega", Headnode: true},
			},
		},
		{
			name:      "compute nodes, schedulable only",
			compute:   Hosts{{Name: "compute1-minimega"}},
			schedOnly: true,
			want:      Hosts{{Name: "compute1-minimega", Schedulable: true}},
		},
	}

	original := common.HostnameSuffixes
	t.Cleanup(func() { common.HostnameSuffixes = original }) //nolint:reassign // restore configuration

	common.HostnameSuffixes = "-minimega,-phenix" //nolint:reassign // configure suffixes

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := clusterHosts(head, tt.compute, tt.schedOnly)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("clusterHosts() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestScreenshotError checks how minimega's `vm screenshot` errors are mapped:
// an unknown VM to ErrVMNotFound, a VM that is not running, such as a paused
// one, to ErrVMNotRunning, and any other error to nil.
func TestScreenshotError(t *testing.T) {
	for msg, want := range map[string]error{
		"vm not found: h2":            ErrVMNotFound,
		"vm not running: 5":           ErrVMNotRunning,
		"unable to connect to QMP: x": nil,
	} {
		if got := screenshotError(msg); !errors.Is(got, want) {
			t.Errorf("screenshotError(%q) = %v, want %v", msg, got, want)
		}
	}
}

// withoutMinimega points mmcli at a directory where no minimega listens for
// the rest of the test.
func withoutMinimega(t *testing.T) {
	t.Helper()

	original := common.MinimegaBase
	t.Cleanup(func() { common.MinimegaBase = original }) //nolint:reassign // restore configuration

	common.MinimegaBase = t.TempDir() //nolint:reassign // no minimega listens here
}

// TestGetVMInfoErrReportsAnUnreachableMinimega is a regression test: a failed
// vm info was logged and dropped, so with minimega down a namespace read as
// having no VMs, and so did a running experiment in vm.List.
func TestGetVMInfoErrReportsAnUnreachableMinimega(t *testing.T) {
	withoutMinimega(t)

	vms, err := Minimega{}.GetVMInfoErr(NS("exp"))
	if err == nil {
		t.Fatalf("GetVMInfoErr with minimega down returned %d VMs and no error", len(vms))
	}

	for _, want := range []string{"running vm info in namespace exp", "unable to dial"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("GetVMInfoErr error = %q, want it to contain %q", err, want)
		}
	}

	if len(vms) != 0 {
		t.Errorf("GetVMInfoErr with minimega down returned VMs %+v", vms)
	}

	if got := (Minimega{}).GetVMInfo(NS("exp")); len(got) != 0 {
		t.Errorf("GetVMInfo with minimega down returned VMs %+v", got)
	}
}

// TestGetClusterHostsReportsAnUnreachableMinimega is a regression test: a
// failed `host` was logged and dropped, so GET /hosts answered only `no
// cluster hosts found` when minimega could not be reached.
func TestGetClusterHostsReportsAnUnreachableMinimega(t *testing.T) {
	withoutMinimega(t)

	hosts, err := Minimega{}.GetClusterHosts(false)
	if err == nil {
		t.Fatalf("GetClusterHosts with minimega down returned hosts %+v and no error", hosts)
	}

	for _, want := range []string{"listing cluster hosts: running host in namespace minimega", "unable to dial"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("GetClusterHosts error = %q, want it to contain %q", err, want)
		}
	}
}
