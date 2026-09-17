package rbac

import (
	"errors"
	"testing"

	v1 "phenix/types/version/v1"
)

func roleWithLimits(l *v1.ResourceLimits) Role {
	return Role{Spec: &v1.RoleSpec{Name: "test", ResourceLimits: l}} //nolint:exhaustruct // partial initialization
}

func TestCheckVCPULimit(t *testing.T) {
	role := roleWithLimits(&v1.ResourceLimits{MaxVCPUs: 4}) //nolint:exhaustruct // partial initialization

	if err := role.CheckVCPULimit(4); err != nil {
		t.Fatalf("expected no error at limit, got %v", err)
	}

	if err := role.CheckVCPULimit(5); !errors.Is(err, ErrVCPULimitExceeded) {
		t.Fatalf("expected ErrVCPULimitExceeded, got %v", err)
	}

	unlimited := roleWithLimits(nil)
	if err := unlimited.CheckVCPULimit(1024); err != nil {
		t.Fatalf("expected no limit enforced, got %v", err)
	}
}

func TestCheckMemoryLimit(t *testing.T) {
	role := roleWithLimits(&v1.ResourceLimits{MaxMemoryMB: 8192}) //nolint:exhaustruct // partial initialization

	if err := role.CheckMemoryLimit(8192); err != nil {
		t.Fatalf("expected no error at limit, got %v", err)
	}

	if err := role.CheckMemoryLimit(8193); !errors.Is(err, ErrMemoryLimitExceeded) {
		t.Fatalf("expected ErrMemoryLimitExceeded, got %v", err)
	}
}

func TestCheckVMCountLimit(t *testing.T) {
	role := roleWithLimits(&v1.ResourceLimits{MaxVMsPerExperiment: 2}) //nolint:exhaustruct // partial initialization

	if err := role.CheckVMCountLimit(2); err != nil {
		t.Fatalf("expected no error at limit, got %v", err)
	}

	if err := role.CheckVMCountLimit(3); !errors.Is(err, ErrVMCountLimitExceeded) {
		t.Fatalf("expected ErrVMCountLimitExceeded, got %v", err)
	}
}

func TestCheckDiskSizeLimit(t *testing.T) {
	role := roleWithLimits(&v1.ResourceLimits{MaxDiskGB: 100}) //nolint:exhaustruct // partial initialization

	cases := []struct {
		size    string
		wantErr bool
	}{
		{"100G", false},
		{"100000M", false},
		{"101G", true},
		{"1T", true},
		{"+50G", false},
		{"+150G", true},
	}

	for _, c := range cases {
		err := role.CheckDiskSizeLimit(c.size)
		if c.wantErr && !errors.Is(err, ErrDiskLimitExceeded) {
			t.Errorf("size %q: expected ErrDiskLimitExceeded, got %v", c.size, err)
		}
		if !c.wantErr && err != nil {
			t.Errorf("size %q: expected no error, got %v", c.size, err)
		}
	}
}

func TestParseSizeToGB(t *testing.T) {
	cases := []struct {
		size string
		want float64
	}{
		{"1G", 1},
		{"1024M", 1},
		{"1T", 1024},
		{"1048576K", 1},
		{"1073741824", 1},
	}

	for _, c := range cases {
		got, err := parseSizeToGB(c.size)
		if err != nil {
			t.Fatalf("size %q: unexpected error: %v", c.size, err)
		}

		if got != c.want {
			t.Errorf("size %q: expected %v GB, got %v GB", c.size, c.want, got)
		}
	}
}
