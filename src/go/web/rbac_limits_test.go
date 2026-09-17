package web

import (
	"errors"
	"testing"

	"phenix/web/rbac"

	v1 "phenix/types/version/v1"
)

func roleWithLimits(l *v1.ResourceLimits) rbac.Role {
	return rbac.Role{Spec: &v1.RoleSpec{Name: "test", ResourceLimits: l}} //nolint:exhaustruct // partial initialization
}

func TestCheckVMHardwareLimits(t *testing.T) {
	role := roleWithLimits(&v1.ResourceLimits{MaxVCPUs: 2, MaxMemoryMB: 2048}) //nolint:exhaustruct // partial initialization

	if err := checkVMHardwareLimits(role, 2, 2048); err != nil {
		t.Fatalf("expected no error at limit, got %v", err)
	}

	if err := checkVMHardwareLimits(role, 4, 0); !errors.Is(err, rbac.ErrVCPULimitExceeded) {
		t.Fatalf("expected ErrVCPULimitExceeded, got %v", err)
	}

	if err := checkVMHardwareLimits(role, 0, 4096); !errors.Is(err, rbac.ErrMemoryLimitExceeded) {
		t.Fatalf("expected ErrMemoryLimitExceeded, got %v", err)
	}

	// zero values mean "not requested" and should never be rejected
	if err := checkVMHardwareLimits(role, 0, 0); err != nil {
		t.Fatalf("expected no error for unset fields, got %v", err)
	}
}

func newTestTopology(nodeVCPUs ...int) *v1.TopologySpec {
	nodes := make([]*v1.Node, len(nodeVCPUs))

	for i, vcpu := range nodeVCPUs {
		nodes[i] = &v1.Node{ //nolint:exhaustruct // partial initialization
			GeneralF: &v1.General{HostnameF: "node"}, //nolint:exhaustruct // partial initialization
			HardwareF: &v1.Hardware{ //nolint:exhaustruct // partial initialization
				VCPUF: vcpu,
			},
		}
	}

	return &v1.TopologySpec{NodesF: nodes} //nolint:exhaustruct // partial initialization
}

func TestCheckTopologyResourceLimits(t *testing.T) {
	role := roleWithLimits(&v1.ResourceLimits{MaxVCPUs: 2, MaxVMsPerExperiment: 2}) //nolint:exhaustruct // partial initialization

	if err := checkTopologyResourceLimits(role, newTestTopology(1, 2)); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	if err := checkTopologyResourceLimits(role, newTestTopology(1, 3)); !errors.Is(err, rbac.ErrVCPULimitExceeded) {
		t.Fatalf("expected ErrVCPULimitExceeded, got %v", err)
	}

	if err := checkTopologyResourceLimits(role, newTestTopology(1, 1, 1)); !errors.Is(err, rbac.ErrVMCountLimitExceeded) {
		t.Fatalf("expected ErrVMCountLimitExceeded, got %v", err)
	}
}

func TestCheckRawTopologyResourceLimits(t *testing.T) {
	role := roleWithLimits(&v1.ResourceLimits{MaxVCPUs: 2}) //nolint:exhaustruct // partial initialization

	spec := map[string]any{
		"nodes": []map[string]any{
			{"hardware": map[string]any{"vcpus": 4}},
		},
	}

	if err := checkRawTopologyResourceLimits(role, spec); !errors.Is(err, rbac.ErrVCPULimitExceeded) {
		t.Fatalf("expected ErrVCPULimitExceeded, got %v", err)
	}
}
