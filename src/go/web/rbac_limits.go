package web

import (
	"fmt"

	"github.com/mitchellh/mapstructure"

	ifaces "phenix/types/interfaces"
	v1 "phenix/types/version/v1"
	"phenix/web/rbac"
)

// checkVMHardwareLimits validates requested vcpus/memoryMB (0 meaning "no
// change/not requested") against the role's configured resource limits. It
// returns nil when either value is 0 or when the role has no limits set.
func checkVMHardwareLimits(role rbac.Role, vcpus, memoryMB int) error {
	if vcpus != 0 {
		if err := role.CheckVCPULimit(vcpus); err != nil {
			return err
		}
	}

	if memoryMB != 0 {
		if err := role.CheckMemoryLimit(memoryMB); err != nil {
			return err
		}
	}

	return nil
}

// checkTopologyResourceLimits validates every node's hardware, along with the
// total node (VM) count, in the given topology against the role's configured
// resource limits.
func checkTopologyResourceLimits(role rbac.Role, topo ifaces.TopologySpec) error {
	nodes := topo.Nodes()

	if err := role.CheckVMCountLimit(len(nodes)); err != nil {
		return err
	}

	for _, node := range nodes {
		hw := node.Hardware()

		if err := checkVMHardwareLimits(role, hw.VCPU(), hw.Memory()); err != nil {
			return fmt.Errorf("node %s: %w", node.General().Hostname(), err)
		}
	}

	return nil
}

// checkRawTopologyResourceLimits decodes a raw topology spec (as submitted by
// the builder UI, i.e. a map[string]any destined for a store.Config's Spec
// field) and validates it via checkTopologyResourceLimits.
func checkRawTopologyResourceLimits(role rbac.Role, spec any) error {
	var topo v1.TopologySpec

	if err := mapstructure.Decode(spec, &topo); err != nil {
		return fmt.Errorf("decoding topology for resource limit validation: %w", err)
	}

	return checkTopologyResourceLimits(role, &topo)
}
