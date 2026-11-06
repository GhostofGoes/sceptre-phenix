package mm

import "testing"

func TestVMIsFirecracker(t *testing.T) {
	tests := []struct {
		vmType string
		want   bool
	}{
		{vmType: "firecracker", want: true},
		{vmType: "kvm", want: false},
		{vmType: "container", want: false},
		{vmType: "", want: false},
	}

	for _, test := range tests {
		t.Run(test.vmType, func(t *testing.T) {
			if got := (VM{VMType: test.vmType}).IsFirecracker(); got != test.want {
				t.Errorf("VM{VMType: %q}.IsFirecracker() = %t, want %t", test.vmType, got, test.want)
			}
		})
	}
}
