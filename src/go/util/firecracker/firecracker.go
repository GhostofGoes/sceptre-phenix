// Package firecracker holds the limits of minimega's firecracker VM type that
// phenix checks before it launches a node, so that topology validation and
// the Builder preflight checks apply the same rules.
package firecracker

import (
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	// VMType is the general.vm_type of a node that minimega runs as a
	// Firecracker microVM.
	VMType = "firecracker"

	// MaxVCPUs is the most vCPUs a Firecracker VM can have.
	MaxVCPUs = 32

	// MaxDevices is the most virtio devices a Firecracker VM can have on
	// x86_64 with the virtio-MMIO transport, and MaxDevicesPCI the most with
	// the PCI transport (firecracker-pci).
	MaxDevices    = 17
	MaxDevicesPCI = 31

	// EntropyKey and PCIKey are the minimega vm config settings, set through
	// a node's advanced section, that add or remove devices.
	EntropyKey = "firecracker-entropy"
	PCIKey     = "firecracker-pci"

	// DefaultRoot is what phenix adds to the kernel arguments of a node that
	// does not set root=, to boot the first partition of the first drive.
	DefaultRoot = "root=/dev/vda1 rw"

	// ReadOnlyRoot is what phenix adds instead when the first drive is read
	// only. A shared root image is usually a file system without partitions.
	ReadOnlyRoot = "root=/dev/vda ro"

	// sectorSize is the unit of a scratch drive size.
	sectorSize = 512

	// The sizes of the K, M, G and T suffixes of a scratch drive size.
	kibibyte = 1 << 10
	mebibyte = 1 << 20
	gibibyte = 1 << 30
	tebibyte = 1 << 40
)

// ErrInvalidSize is the error that [ParseSize] wraps for a size that minimega
// refuses.
var ErrInvalidSize = errors.New("invalid size")

// Devices is what a Firecracker VM attaches as virtio devices: its disks, the
// scratch drive minimega adds when firecracker-scratch is set, its NICs, the
// vsock device (always counted, because cc can use it), and the entropy device
// unless it is turned off. PCI tells which device limit applies.
type Devices struct {
	Disks   int
	Scratch bool
	NICs    int
	Entropy bool
	PCI     bool
}

// NewDevices returns the devices of a VM with the given disks and NICs, and a
// scratch drive when scratch is true. entropy and pci are the values of the
// firecracker-entropy and firecracker-pci settings, or "" when they are not
// set. The entropy device is on unless entropy is false, and the PCI transport
// is off unless pci is true.
func NewDevices(disks, nics int, scratch bool, entropy, pci string) Devices {
	devices := Devices{Disks: disks, Scratch: scratch, NICs: nics, Entropy: true, PCI: false}

	if on, err := strconv.ParseBool(strings.TrimSpace(entropy)); err == nil {
		devices.Entropy = on
	}

	if on, err := strconv.ParseBool(strings.TrimSpace(pci)); err == nil {
		devices.PCI = on
	}

	return devices
}

// Count returns how many virtio devices the VM needs.
func (d Devices) Count() int {
	count := d.Disks + d.NICs + 1

	if d.Scratch {
		count++
	}

	if d.Entropy {
		count++
	}

	return count
}

// Limit returns the most virtio devices the VM can have.
func (d Devices) Limit() int {
	if d.PCI {
		return MaxDevicesPCI
	}

	return MaxDevices
}

// Fits reports whether the VM's devices are within the limit.
func (d Devices) Fits() bool {
	return d.Count() <= d.Limit()
}

// String describes the devices and the limit, for example "20 Firecracker
// devices (2 disks, scratch, 15 NICs, vsock, entropy); the limit is 17 without
// firecracker-pci".
func (d Devices) String() string {
	parts := []string{plural(d.Disks, "disk", "disks")}

	if d.Scratch {
		parts = append(parts, "scratch")
	}

	parts = append(parts, plural(d.NICs, "NIC", "NICs"), "vsock")

	if d.Entropy {
		parts = append(parts, "entropy")
	}

	transport := "without " + PCIKey
	if d.PCI {
		transport = "with " + PCIKey
	}

	return fmt.Sprintf(
		"%d Firecracker devices (%s); the limit is %d %s",
		d.Count(), strings.Join(parts, ", "), d.Limit(), transport,
	)
}

// VCPUsFit reports whether a Firecracker VM can have vcpus vCPUs.
func VCPUsFit(vcpus int) bool {
	return vcpus >= 1 && vcpus <= MaxVCPUs
}

// IsQCOW2 reports whether image names a qcow2 file (.qcow2 or .qc2), which
// Firecracker can boot only from a copy that minimega converts.
func IsQCOW2(image string) bool {
	switch strings.ToLower(filepath.Ext(image)) {
	case ".qcow2", ".qc2":
		return true
	}

	return false
}

// KernelArgs returns the kernel command line for a node with the given
// kernel_args. When args sets no root=, it adds [DefaultRoot], or
// [ReadOnlyRoot] when the first drive of the node is read only.
func KernelArgs(args string, readOnlyRoot bool) string {
	args = strings.TrimSpace(args)

	for field := range strings.FieldsSeq(args) {
		if strings.HasPrefix(field, "root=") {
			return args
		}
	}

	root := DefaultRoot
	if readOnlyRoot {
		root = ReadOnlyRoot
	}

	if args == "" {
		return root
	}

	return args + " " + root
}

// ParseSize returns the bytes of a firecracker-scratch size, as minimega reads
// it: a number of bytes, with an optional K, M, G or T suffix (powers of
// 1024), that is a multiple of 512. An empty size is 0, which adds no scratch
// drive, as 0 does. The error wraps [ErrInvalidSize].
func ParseSize(size string) (int64, error) {
	if size == "" {
		return 0, nil
	}

	num, mult := size, int64(1)

	switch strings.ToUpper(size[len(size)-1:]) {
	case "K":
		mult = kibibyte
	case "M":
		mult = mebibyte
	case "G":
		mult = gibibyte
	case "T":
		mult = tebibyte
	}

	if mult > 1 {
		num = size[:len(size)-1]
	}

	n, err := strconv.ParseInt(num, 10, 64)
	if err != nil || n < 0 || n > math.MaxInt64/mult {
		return 0, fmt.Errorf("%w %q: use a number of bytes with an optional K, M, G or T suffix", ErrInvalidSize, size)
	}

	n *= mult
	if n%sectorSize != 0 {
		return 0, fmt.Errorf("%w %q: the size must be a multiple of %d bytes", ErrInvalidSize, size, sectorSize)
	}

	return n, nil
}

func plural(n int, one, many string) string {
	if n == 1 {
		return strconv.Itoa(n) + " " + one
	}

	return strconv.Itoa(n) + " " + many
}
