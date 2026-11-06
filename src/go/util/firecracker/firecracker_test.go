package firecracker_test

import (
	"errors"
	"testing"

	"phenix/util/firecracker"
)

func TestParseSize(t *testing.T) {
	tests := []struct {
		size    string
		want    int64
		invalid bool
	}{
		{size: "", want: 0},
		{size: "0", want: 0},
		{size: "512", want: 512},
		{size: "4096", want: 4096},
		{size: "64M", want: 64 << 20},
		{size: "64m", want: 64 << 20},
		{size: "1K", want: 1 << 10},
		{size: "1G", want: 1 << 30},
		{size: "2T", want: 2 << 40},
		{size: "1000", invalid: true},
		{size: "12x", invalid: true},
		{size: "M", invalid: true},
		{size: "-512", invalid: true},
		{size: " 64M", invalid: true},
		{size: "1.5G", invalid: true},
		{size: "9999999999T", invalid: true},
	}

	for _, tt := range tests {
		t.Run(tt.size, func(t *testing.T) {
			got, err := firecracker.ParseSize(tt.size)

			if tt.invalid {
				if !errors.Is(err, firecracker.ErrInvalidSize) {
					t.Errorf("ParseSize(%q) = %d, %v, want an error wrapping ErrInvalidSize", tt.size, got, err)
				}

				return
			}

			if err != nil || got != tt.want {
				t.Errorf("ParseSize(%q) = %d, %v, want %d, nil", tt.size, got, err, tt.want)
			}
		})
	}
}

func TestDevicesScratch(t *testing.T) {
	tests := []struct {
		name    string
		devices firecracker.Devices
		count   int
		fits    bool
		text    string
	}{
		{
			name:    "17 devices with a scratch drive",
			devices: firecracker.NewDevices(1, 13, true, "", ""),
			count:   17,
			fits:    true,
			text:    "17 Firecracker devices (1 disk, scratch, 13 NICs, vsock, entropy); the limit is 17 without firecracker-pci",
		},
		{
			name:    "18 devices with a scratch drive",
			devices: firecracker.NewDevices(1, 14, true, "", ""),
			count:   18,
			fits:    false,
			text:    "18 Firecracker devices (1 disk, scratch, 14 NICs, vsock, entropy); the limit is 17 without firecracker-pci",
		},
		{
			name:    "no scratch drive",
			devices: firecracker.NewDevices(2, 1, false, "false", "true"),
			count:   4,
			fits:    true,
			text:    "4 Firecracker devices (2 disks, 1 NIC, vsock); the limit is 31 with firecracker-pci",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.devices.Count(); got != tt.count {
				t.Errorf("Count() = %d, want %d", got, tt.count)
			}

			if got := tt.devices.Fits(); got != tt.fits {
				t.Errorf("Fits() = %t, want %t", got, tt.fits)
			}

			if got := tt.devices.String(); got != tt.text {
				t.Errorf("String() = %q, want %q", got, tt.text)
			}
		})
	}
}

func TestKernelArgsReadOnlyRoot(t *testing.T) {
	tests := []struct {
		args     string
		readOnly bool
		want     string
	}{
		{args: "", readOnly: false, want: "root=/dev/vda1 rw"},
		{args: "", readOnly: true, want: "root=/dev/vda ro"},
		{args: "console=ttyS0", readOnly: true, want: "console=ttyS0 root=/dev/vda ro"},
		{args: "root=/dev/vdb ro", readOnly: true, want: "root=/dev/vdb ro"},
	}

	for _, tt := range tests {
		if got := firecracker.KernelArgs(tt.args, tt.readOnly); got != tt.want {
			t.Errorf("KernelArgs(%q, %t) = %q, want %q", tt.args, tt.readOnly, got, tt.want)
		}
	}
}
