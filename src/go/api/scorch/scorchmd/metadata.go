package scorchmd

import (
	"errors"
	"fmt"

	"github.com/mitchellh/mapstructure"

	"phenix/types"
	"phenix/util"
)

var (
	ErrScorchNotConfigured = errors.New("scorch not configured for experiment")
	ErrUndefinedComponent  = errors.New("component is not defined in the scorch app's components")
)

func DecodeMetadata(exp *types.Experiment) (ScorchMetadata, error) {
	var (
		ms map[string]any
		md ScorchMetadata
	)

	for _, app := range exp.Apps() {
		if app.Name() == "scorch" {
			ms = util.CopyableMap(app.Metadata()).DeepCopy()

			break
		}
	}

	if ms == nil {
		return md, ErrScorchNotConfigured
	}

	err := mapstructure.Decode(ms, &md)
	if err != nil {
		return md, fmt.Errorf("decoding app metadata: %w", err)
	}

	md.components = make(ComponentSpecMap)

	for _, c := range md.Components {
		md.components[c.Name] = c
	}

	for _, run := range md.Runs {
		ensureCount(run)
	}

	return md, nil
}

// Validate checks every run with ValidateRun.
func (m ScorchMetadata) Validate() error {
	for id := range m.Runs {
		if err := m.ValidateRun(id); err != nil {
			return err
		}
	}

	return nil
}

// ValidateRun checks that each component the given run, or one of its nested
// loops, names in a stage is defined in the app's components. Without it, an
// undefined name runs as an external component of no type.
func (m ScorchMetadata) ValidateRun(id int) error {
	defined := make(map[string]struct{}, len(m.Components))

	for _, c := range m.Components {
		defined[c.Name] = struct{}{}
	}

	depth := 0

	for loop := m.Runs[id]; loop != nil; loop = loop.Loop {
		stages := []struct {
			name  string
			names []string
		}{
			{"configure", loop.Configure},
			{"start", loop.Start},
			{"stop", loop.Stop},
			{"cleanup", loop.Cleanup},
		}

		for _, stage := range stages {
			for _, name := range stage.names {
				if _, ok := defined[name]; ok {
					continue
				}

				where := fmt.Sprintf("scorch run %d", id)
				if depth > 0 {
					where += fmt.Sprintf(" loop %d", depth)
				}

				return fmt.Errorf(
					"%s %s stage names component %q: %w",
					where,
					stage.name,
					name,
					ErrUndefinedComponent,
				)
			}
		}

		depth++
	}

	return nil
}

// Ensure missing run loop counts default to 1.
func ensureCount(run *Loop) {
	if run.Count == 0 {
		run.Count = 1
	}

	if run.Loop != nil {
		ensureCount(run.Loop)
	}
}
