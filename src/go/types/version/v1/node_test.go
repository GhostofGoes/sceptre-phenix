package v1

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"pgregory.net/rapid"
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
			name: "internal node, delay timer that parses",
			node: Node{GeneralF: &General{HostnameF: "test-node"}, DelayF: &Delay{TimerF: "1h30m"}},
		},
		{
			name:    "internal node, delay timer without a unit",
			node:    Node{GeneralF: &General{HostnameF: "test-node"}, DelayF: &Delay{TimerF: "5"}},
			wantErr: true,
			substr:  "is not a duration",
		},
		{
			name:    "internal node, delay timer too long for a duration",
			node:    Node{GeneralF: &General{HostnameF: "test-node"}, DelayF: &Delay{TimerF: "9999999999h"}},
			wantErr: true,
			substr:  "is not a duration",
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

// genNodeInterface draws an interface, or none, from few names, protocols and gateways, so
// repeated names and gateways on dhcp or manual interfaces are common.
func genNodeInterface() *rapid.Generator[*Interface] {
	return rapid.Custom(func(t *rapid.T) *Interface {
		if rapid.IntRange(0, 7).Draw(t, "nil") == 0 {
			return nil
		}

		return &Interface{
			NameF:    rapid.SampledFrom([]string{"", "eth0", "eth1"}).Draw(t, "name"),
			ProtoF:   rapid.SampledFrom([]string{"static", "DHCP", "Manual", ""}).Draw(t, "proto"),
			GatewayF: rapid.SampledFrom([]string{"10.0.0.1", "", "10.0.1.1"}).Draw(t, "gateway"),
		}
	})
}

// nodeIfaceSummary writes each interface as name/proto/gateway.
func nodeIfaceSummary(ifaces []*Interface) []string {
	summary := make([]string, len(ifaces))

	for i, iface := range ifaces {
		summary[i] = "nil"
		if iface != nil {
			summary[i] = iface.NameF + "/" + iface.ProtoF + "/" + iface.GatewayF
		}
	}

	return summary
}

// An internal node is valid exactly when its named interfaces have distinct names, no dhcp
// or manual interface sets a gateway, and at most one interface does; an external one always is.
func TestNodeValidateProperties(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(t *rapid.T) {
		ifaces := rapid.SliceOfN(genNodeInterface(), rapid.IntRange(0, 2).Draw(t, "min"), 4).Draw(t, "ifaces")
		var external *bool
		if rapid.IntRange(0, 7).Draw(t, "external") == 0 {
			external = boolPtr(true)
		}

		var (
			names    = make(map[string]bool)
			gateways int
			valid    = true
		)

		for _, iface := range ifaces {
			if iface == nil {
				continue
			}

			if iface.NameF != "" {
				valid = valid && !names[iface.NameF]
				names[iface.NameF] = true
			}

			if iface.GatewayF != "" {
				gateways++
				valid = valid && !strings.EqualFold(iface.ProtoF, "dhcp") && !strings.EqualFold(iface.ProtoF, "manual")
			}
		}

		valid = external != nil || valid && gateways <= 1

		node := nodeWithIfaces(ifaces...)
		node.ExternalF = external

		if err := node.validate(); (err == nil) != valid {
			t.Fatalf("Node{interfaces %v, external %v}.validate() = %v, want valid %t",
				nodeIfaceSummary(ifaces), external != nil, err, valid)
		}
	})
}

// firecrackerTestNode returns a valid firecracker node web1 with one drive and
// nics interfaces. change edits it.
func firecrackerTestNode(nics int, change func(*Node)) Node {
	ifaces := make([]*Interface, nics)
	for i := range ifaces {
		ifaces[i] = &Interface{NameF: fmt.Sprintf("eth%d", i), VLANF: "EXP"}
	}

	n := Node{
		GeneralF: &General{HostnameF: "web1", VMTypeF: "firecracker"},
		HardwareF: &Hardware{
			OSTypeF: "linux", KernelF: "vmlinux", DrivesF: []*Drive{{ImageF: "debian.qc2"}},
		},
		NetworkF: &Network{InterfacesF: ifaces},
	}

	if change != nil {
		change(&n)
	}

	return n
}

// kvmVMType is the VM type of a node that is not a firecracker node.
const kvmVMType = "kvm"

func TestNodeValidateFirecracker(t *testing.T) {
	tests := []struct {
		name string
		node Node
		// substr is part of the error, or "" when the node is valid.
		substr string
	}{
		{name: "valid", node: firecrackerTestNode(1, nil)},
		{
			name: "unset os_type, vcpus and snapshot use the defaults",
			node: firecrackerTestNode(1, func(n *Node) { n.HardwareF.OSTypeF = "" }),
		},
		{
			name:   "windows",
			node:   firecrackerTestNode(1, func(n *Node) { n.HardwareF.OSTypeF = "windows" }),
			substr: "run only linux",
		},
		{
			name:   "no kernel",
			node:   firecrackerTestNode(1, func(n *Node) { n.HardwareF.KernelF = " " }),
			substr: "hardware.kernel is not set",
		},
		{name: "32 vcpus", node: firecrackerTestNode(1, func(n *Node) { n.HardwareF.VCPUF = 32 })},
		{
			name:   "33 vcpus",
			node:   firecrackerTestNode(1, func(n *Node) { n.HardwareF.VCPUF = 33 }),
			substr: "have 1 to 32 vCPUs",
		},
		{
			name:   "negative vcpus",
			node:   firecrackerTestNode(1, func(n *Node) { n.HardwareF.VCPUF = -1 }),
			substr: "have 1 to 32 vCPUs",
		},
		{
			name: "qcow2 without snapshot",
			node: firecrackerTestNode(1, func(n *Node) {
				n.GeneralF.SnapshotF = boolPtr(false)
				n.HardwareF.DrivesF[0].ImageF = "debian.QCOW2"
			}),
			substr: "need a raw image",
		},
		{
			name: "raw without snapshot",
			node: firecrackerTestNode(1, func(n *Node) {
				n.GeneralF.SnapshotF = boolPtr(false)
				n.HardwareF.DrivesF[0].ImageF = "debian.raw"
			}),
		},
		{
			name:   "serial interface",
			node:   firecrackerTestNode(1, func(n *Node) { n.NetworkF.InterfacesF[0].TypeF = "serial" }),
			substr: "is a serial interface",
		},
		{
			name:   "qinq interface",
			node:   firecrackerTestNode(1, func(n *Node) { n.NetworkF.InterfacesF[0].QinQF = true }),
			substr: "not supported for firecracker nodes yet",
		},
		{name: "17 devices", node: firecrackerTestNode(14, nil)},
		{
			name: "19 devices",
			node: firecrackerTestNode(15, func(n *Node) {
				n.HardwareF.DrivesF = append(n.HardwareF.DrivesF, &Drive{ImageF: "data.raw"})
			}),
			substr: "node web1 needs 19 Firecracker devices (2 disks, 15 NICs, vsock, entropy); " +
				"the limit is 17 without firecracker-pci",
		},
		{
			name: "no entropy device",
			node: firecrackerTestNode(15, func(n *Node) {
				n.AdvancedF = map[string]string{"firecracker-entropy": "false"}
			}),
		},
		{
			name: "31 devices with pci",
			node: firecrackerTestNode(28, func(n *Node) {
				n.AdvancedF = map[string]string{"firecracker-pci": "true"}
			}),
		},
		{
			name: "32 devices with pci",
			node: firecrackerTestNode(29, func(n *Node) {
				n.AdvancedF = map[string]string{"firecracker-pci": "true"}
			}),
			substr: "the limit is 31 with firecracker-pci",
		},
		{
			name: "overrides and a drive interface only warn",
			node: firecrackerTestNode(1, func(n *Node) {
				n.OverridesF = map[string]string{"a": "b"}
				n.HardwareF.DrivesF[0].IfaceF = "ide"
			}),
		},
		{
			name: "kvm node is not checked",
			node: firecrackerTestNode(1, func(n *Node) {
				n.GeneralF.VMTypeF = kvmVMType
				n.HardwareF.OSTypeF = "windows"
				n.HardwareF.KernelF = ""
			}),
		},
		{
			name: "read-only qcow2 without snapshot",
			node: firecrackerTestNode(1, func(n *Node) {
				n.GeneralF.SnapshotF = boolPtr(false)
				n.HardwareF.DrivesF[0].ReadOnlyF = true
			}),
		},
		{
			name: "scratch drive",
			node: firecrackerTestNode(1, func(n *Node) { n.HardwareF.ScratchF = "64M" }),
		},
		{
			name:   "scratch size with an unknown suffix",
			node:   firecrackerTestNode(1, func(n *Node) { n.HardwareF.ScratchF = "12x" }),
			substr: `hardware.scratch: invalid size "12x"`,
		},
		{
			name:   "scratch size that is not a multiple of 512",
			node:   firecrackerTestNode(1, func(n *Node) { n.HardwareF.ScratchF = "1000" }),
			substr: "multiple of 512 bytes",
		},
		{name: "17 devices with scratch", node: firecrackerTestNode(13, func(n *Node) { n.HardwareF.ScratchF = "1G" })},
		{
			name:   "18 devices with scratch",
			node:   firecrackerTestNode(14, func(n *Node) { n.HardwareF.ScratchF = "1G" }),
			substr: "needs 18 Firecracker devices (1 disk, scratch, 14 NICs, vsock, entropy)",
		},
		{
			name: "injections into a read-only first drive",
			node: firecrackerTestNode(1, func(n *Node) {
				n.HardwareF.DrivesF[0].ReadOnlyF = true
				n.InjectionsF = []*Injection{{SrcF: "a.sh", DstF: "/etc/a.sh"}}
			}),
			substr: "injections need a writable first drive",
		},
		{
			name: "deletions from a read-only first drive",
			node: firecrackerTestNode(1, func(n *Node) {
				n.HardwareF.DrivesF[0].ReadOnlyF = true
				n.DeletionsF = []*Deletion{{PathF: "/etc/old"}}
			}),
			substr: "injections need a writable first drive",
		},
		{
			name: "injections with a read-only first drive and inject partition 0",
			node: firecrackerTestNode(1, func(n *Node) {
				part := 0
				n.HardwareF.DrivesF[0].ReadOnlyF = true
				n.HardwareF.DrivesF[0].InjectPartitionF = &part
				n.InjectionsF = []*Injection{{SrcF: "a.sh", DstF: "/etc/a.sh"}}
			}),
		},
		{
			name: "injections with a read-only second drive",
			node: firecrackerTestNode(1, func(n *Node) {
				n.HardwareF.DrivesF = append(n.HardwareF.DrivesF, &Drive{ImageF: "tools.img", ReadOnlyF: true})
				n.InjectionsF = []*Injection{{SrcF: "a.sh", DstF: "/etc/a.sh"}}
			}),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.node.validate()

			if tt.substr == "" {
				if err != nil {
					t.Fatalf("validate() = %v, want nil", err)
				}

				return
			}

			if !errors.Is(err, ErrFirecrackerNode) {
				t.Fatalf("validate() = %v, want an error wrapping ErrFirecrackerNode", err)
			}

			if !strings.Contains(err.Error(), tt.substr) {
				t.Errorf("validate() = %q, want it to contain %q", err.Error(), tt.substr)
			}
		})
	}
}

func TestNodeSetDefaultsFirecrackerCPU(t *testing.T) {
	for vmType, want := range map[string]string{"kvm": "Broadwell", "firecracker": ""} {
		t.Run(vmType, func(t *testing.T) {
			n := firecrackerTestNode(1, func(n *Node) { n.GeneralF.VMTypeF = vmType })
			n.setDefaults("phenix")

			if got := n.HardwareF.CPUF; got != want {
				t.Errorf("setDefaults() cpu = %q, want %q", got, want)
			}
		})
	}
}

func TestHardwareFirecrackerAppend(t *testing.T) {
	for args, want := range map[string]string{
		"":                         "root=/dev/vda1 rw",
		"console=ttyS0":            "console=ttyS0 root=/dev/vda1 rw",
		" root=/dev/vda ro quiet ": "root=/dev/vda ro quiet",
		"quiet root=LABEL=rootfs":  "quiet root=LABEL=rootfs",
		"rootdelay=5":              "rootdelay=5 root=/dev/vda1 rw",
	} {
		t.Run(args, func(t *testing.T) {
			if got := (&Hardware{KernelArgsF: args}).FirecrackerAppend(); got != want {
				t.Errorf("FirecrackerAppend(%q) = %q, want %q", args, got, want)
			}
		})
	}
}

// A read-only first drive is usually a file system without partitions, so
// phenix boots it read only from the whole device.
func TestHardwareFirecrackerAppendReadOnlyRoot(t *testing.T) {
	for args, want := range map[string]string{
		"":                    "root=/dev/vda ro",
		"console=ttyS0":       "console=ttyS0 root=/dev/vda ro",
		"root=/dev/vda1 rw":   "root=/dev/vda1 rw",
		"init=/sbin/init.sh ": "init=/sbin/init.sh root=/dev/vda ro",
	} {
		t.Run(args, func(t *testing.T) {
			h := &Hardware{KernelArgsF: args, DrivesF: []*Drive{{ImageF: "root.squashfs", ReadOnlyF: true}, {ImageF: "data.raw"}}}
			if got := h.FirecrackerAppend(); got != want {
				t.Errorf("FirecrackerAppend(%q) = %q, want %q", args, got, want)
			}
		})
	}

	second := &Hardware{DrivesF: []*Drive{{ImageF: "debian.raw"}, {ImageF: "tools.img", ReadOnlyF: true}}}
	if got, want := second.FirecrackerAppend(), "root=/dev/vda1 rw"; got != want {
		t.Errorf("FirecrackerAppend() with a read-only second drive = %q, want %q", got, want)
	}
}

func TestHardwareDiskConfigReadOnly(t *testing.T) {
	tests := []struct {
		name     string
		drives   []*Drive
		snapshot string
		want     string
	}{
		{
			name:   "read-only drive",
			drives: []*Drive{{ImageF: "root.squashfs", ReadOnlyF: true}},
			want:   "root.squashfs,ro",
		},
		{
			name: "ro after the interface and the cache mode",
			drives: []*Drive{
				{ImageF: "root.raw", IfaceF: "virtio", CacheModeF: "unsafe", ReadOnlyF: true},
				{ImageF: "data.raw"},
			},
			want: "root.raw,virtio,unsafe,ro data.raw",
		},
		{
			name:     "snapshot of a writable drive",
			drives:   []*Drive{{ImageF: "base.qc2"}, {ImageF: "data.raw", IfaceF: "virtio"}},
			snapshot: "snap.qc2",
			want:     "snap.qc2,writeback data.raw,virtio,writeback",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := (Hardware{DrivesF: tt.drives}).DiskConfig(tt.snapshot); got != tt.want {
				t.Errorf("DiskConfig(%q) = %q, want %q", tt.snapshot, got, tt.want)
			}
		})
	}
}

// read_only and scratch are only for firecracker nodes, and minimega refuses
// a read-only drive for the other VM types.
func TestNodeValidateFirecrackerOnly(t *testing.T) {
	tests := []struct {
		name    string
		vmType  string
		change  func(*Node)
		wantErr bool
	}{
		{name: "kvm node without them", vmType: "kvm", change: func(*Node) {}},
		{
			name: "kvm node with a read-only drive", vmType: "kvm", wantErr: true,
			change: func(n *Node) { n.HardwareF.DrivesF[0].ReadOnlyF = true },
		},
		{
			name: "container node with a scratch drive", vmType: "container", wantErr: true,
			change: func(n *Node) { n.HardwareF.ScratchF = "64M" },
		},
		{
			name: "node without a VM type with a scratch drive", vmType: "", wantErr: true,
			change: func(n *Node) { n.HardwareF.ScratchF = "64M" },
		},
		{
			name: "firecracker node with both", vmType: "firecracker",
			change: func(n *Node) {
				n.HardwareF.DrivesF[0].ReadOnlyF = true
				n.HardwareF.ScratchF = "64M"
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			node := firecrackerTestNode(1, func(n *Node) {
				n.GeneralF.VMTypeF = tt.vmType
				tt.change(n)
			})

			err := node.validate()

			if got := errors.Is(err, ErrFirecrackerOnly); got != tt.wantErr {
				t.Errorf("validate() = %v, want an error wrapping ErrFirecrackerOnly: %t", err, tt.wantErr)
			}

			if !tt.wantErr && err != nil {
				t.Errorf("validate() = %v, want nil", err)
			}
		})
	}
}
