package disk

import (
	"reflect"
	"testing"

	"phenix/store"
	"phenix/types"
	v1 "phenix/types/version/v1"
)

func useTestExperiment(t *testing.T, name, startTime string, images ...string) types.Experiment {
	t.Helper()

	drives := make([]*v1.Drive, 0, len(images))
	for _, image := range images {
		drives = append(drives, &v1.Drive{ImageF: image})
	}

	node := &v1.Node{
		TypeF:     "VirtualMachine",
		GeneralF:  &v1.General{HostnameF: "host"},
		HardwareF: &v1.Hardware{DrivesF: drives},
	}

	spec := &v1.ExperimentSpec{
		ExperimentNameF: name,
		TopologyF:       &v1.TopologySpec{NodesF: []*v1.Node{node}},
	}

	return types.Experiment{
		Metadata: store.ConfigMetadata{Name: name},
		Spec:     spec,
		Status:   &v1.ExperimentStatus{StartTimeF: startTime},
	}
}

func TestAddExperimentUses(t *testing.T) {
	details := map[string]Details{
		"child.qc2":  {Name: "child.qc2", BackingImages: []string{"base.qc2"}},
		"base.qc2":   {Name: "base.qc2"},
		"unused.qc2": {Name: "unused.qc2"},
	}

	experiments := []types.Experiment{
		useTestExperiment(t, "running", "2026-09-26T00:00:00Z", "/phenix/images/child.qc2"),
		useTestExperiment(t, "stopped", "", "base.qc2", "missing.qc2"),
	}

	addExperimentUses(experiments, details)

	want := map[string][]ExperimentUse{
		"child.qc2": {{Name: "running", Running: true}},
		"base.qc2":  {{Name: "running", Running: true}, {Name: "stopped", Running: false}},
	}

	for name, image := range details {
		if !reflect.DeepEqual(image.Experiments, want[name]) {
			t.Errorf("%s: experiments = %+v, want %+v", name, image.Experiments, want[name])
		}
	}
}
