package vm

import (
	"testing"

	"phenix/store"
	"phenix/types"
	v1 "phenix/types/version/v1"
)

func TestListConfiguredKeepsVMsOfRunningExperiment(t *testing.T) {
	nodes := make([]*v1.Node, 0, 2)
	for _, name := range []string{"vm1", "vm2"} {
		nodes = append(nodes, &v1.Node{
			TypeF:     "VirtualMachine",
			GeneralF:  &v1.General{HostnameF: name},
			HardwareF: &v1.Hardware{},
			NetworkF:  &v1.Network{},
		})
	}

	exp := types.Experiment{
		Metadata: store.ConfigMetadata{Name: "exp"},
		Spec: &v1.ExperimentSpec{
			ExperimentNameF: "exp",
			TopologyF:       &v1.TopologySpec{NodesF: nodes},
			SchedulesF:      map[string]string{"vm1": "host1"},
		},
		// running, so List would drop VMs minimega does not report
		Status: &v1.ExperimentStatus{StartTimeF: "2026-09-26T00:00:00Z"},
	}

	vms := ListConfigured(exp)
	if len(vms) != 2 {
		t.Fatalf("got %d VMs, want 2", len(vms))
	}

	if vms[0].Name != "vm1" || vms[0].Host != "host1" || vms[1].Name != "vm2" {
		t.Errorf("unexpected VMs: %+v", vms)
	}
}
