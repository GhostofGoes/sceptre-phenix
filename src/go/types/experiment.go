package types

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/activeshadow/structs"
	"github.com/mitchellh/mapstructure"

	"phenix/store"
	ifaces "phenix/types/interfaces"
	"phenix/types/version"
	"phenix/util/common"
	"phenix/util/mm"
)

type Experiment struct {
	Metadata store.ConfigMetadata    `json:"metadata" yaml:"metadata"` // experiment configuration metadata
	Spec     ifaces.ExperimentSpec   `json:"spec"     yaml:"spec"`     // reference to latest versioned experiment spec
	Status   ifaces.ExperimentStatus `json:"status"   yaml:"status"`   // reference to latest versioned experiment status

	// used for user apps
	Hosts mm.Hosts `json:"hosts,omitempty" yaml:"hosts,omitempty"` // cluster host details
	VMs   mm.VMs   `json:"vms,omitempty"   yaml:"vms,omitempty"`   // VM runtime details
}

func NewExperiment(md store.ConfigMetadata) *Experiment {
	ver := version.StoredVersion["Experiment"]

	spec, _ := version.GetVersionedSpecForKind("Experiment", ver)
	status, _ := version.GetVersionedStatusForKind("Experiment", ver)

	specSpec, _ := spec.(ifaces.ExperimentSpec)
	_ = specSpec.Init()
	statusStatus, _ := status.(ifaces.ExperimentStatus)
	_ = statusStatus.Init()

	return &Experiment{ //nolint:exhaustruct // partial initialization
		Metadata: md,
		Spec:     specSpec,
		Status:   statusStatus,
	}
}

func (e *Experiment) Reload() error {
	c, err := store.NewConfig("experiment/" + e.Metadata.Name)
	if err != nil {
		return fmt.Errorf("getting experiment: %w", err)
	}

	if err := store.Get(c); err != nil {
		return fmt.Errorf("getting experiment %s from store: %w", e.Metadata.Name, err)
	}

	exp, err := DecodeExperimentFromConfig(*c)
	if err != nil {
		return fmt.Errorf("decoding experiment %s: %w", e.Metadata.Name, err)
	}

	e.Metadata = exp.Metadata
	e.Spec = exp.Spec
	e.Status = exp.Status

	return nil
}

// ErrExperimentRunChanged reports that the stored experiment was stopped, or
// started again, since the copy being written was read.
var ErrExperimentRunChanged = errors.New("experiment was stopped or started again")

// WriteToStore stores the experiment's annotations and status, and its spec
// unless statusOnly is set. Only a start and a stop change an experiment's
// start time, so a copy read before the experiment was stopped, or started
// again, would write an ended run back: the write is then refused with an
// error wrapping ErrExperimentRunChanged.
func (e Experiment) WriteToStore(statusOnly bool) error {
	return e.write(!statusOnly, true)
}

// WriteSpecToStore stores the experiment's annotations and spec, and leaves
// its stored status alone: what apps stored since e was read, such as another
// app's status or running flag, is kept. Like WriteToStore, it refuses a run
// other than the one e was read for.
func (e Experiment) WriteSpecToStore() error {
	return e.write(true, false)
}

func (e Experiment) write(spec, status bool) error {
	name := e.Metadata.Name

	c, _ := store.NewConfig("experiment/" + name)

	err := store.Modify(c, func(c *store.Config) error {
		stored, _ := c.Status["startTime"].(string)

		if read := e.StartTime(); stored != read {
			return fmt.Errorf(
				"%w since it was read: stored start time %q, read %q",
				ErrExperimentRunChanged,
				stored,
				read,
			)
		}

		// limit metadata updates to annotations so name doesn't accidentally get changed
		c.Metadata.Annotations = e.Metadata.Annotations

		if spec {
			c.Spec = structs.MapDefaultCase(e.Spec, structs.CASESNAKE)
		}

		if status {
			c.Status = structs.MapDefaultCase(e.Status, structs.CASESNAKE)
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("saving experiment %s: %w", name, err)
	}

	return nil
}

// UpdateStatus applies change to the experiment's stored status in one
// atomic step (see store.Modify) and sets e's status to the result. Unlike
// WriteToStore, it keeps what others stored since e was read, such as another
// app's status or running flag. Like WriteToStore, it refuses, with an error
// wrapping ErrExperimentRunChanged, a run other than the one e was read for.
// When change returns an error, the stored status is left as it was.
func (e *Experiment) UpdateStatus(change func(ifaces.ExperimentStatus) error) error {
	name := e.Metadata.Name

	c, _ := store.NewConfig("experiment/" + name)

	var updated ifaces.ExperimentStatus

	err := store.Modify(c, func(c *store.Config) error {
		status, err := decodeStatus(*c)
		if err != nil {
			return err
		}

		if stored, read := status.StartTime(), e.StartTime(); stored != read {
			return fmt.Errorf(
				"%w since it was read: stored start time %q, read %q",
				ErrExperimentRunChanged,
				stored,
				read,
			)
		}

		if err := change(status); err != nil {
			return err
		}

		c.Status = structs.MapDefaultCase(status, structs.CASESNAKE)
		updated = status

		return nil
	})
	if err != nil {
		return fmt.Errorf("updating status of experiment %s: %w", name, err)
	}

	e.Status = updated

	return nil
}

func (e Experiment) StartTime() string {
	if e.Status == nil {
		return ""
	}

	return e.Status.StartTime()
}

func (e *Experiment) SetSpec(spec ifaces.ExperimentSpec) {
	e.Spec = spec
}

func (e Experiment) Apps() []ifaces.ScenarioApp {
	if e.Spec.Scenario() != nil {
		return e.Spec.Scenario().Apps()
	}

	return nil
}

func (e Experiment) App(name string) ifaces.ScenarioApp { //nolint:ireturn // interface
	if e.Spec.Scenario() == nil {
		return nil
	}

	for _, app := range e.Spec.Scenario().Apps() {
		if app.Name() == name {
			return app
		}
	}

	return nil
}

func (e Experiment) Running() bool {
	if e.Status == nil {
		return false
	}

	if e.Status.StartTime() == "" {
		return false
	}

	return true
}

func (e Experiment) DryRun() bool {
	if e.Status == nil {
		return false
	}

	if e.Status.StartTime() == "" {
		return false
	}

	return strings.Contains(e.Status.StartTime(), "DRYRUN")
}

func (e Experiment) FilesDir() string {
	return filepath.Join(common.PhenixBase, "images", e.Metadata.Name, "files")
}

func Experiments(running bool) ([]*Experiment, error) {
	configs, err := store.List("Experiment")
	if err != nil {
		return nil, fmt.Errorf("getting list of experiment configs from store: %w", err)
	}

	var experiments []*Experiment

	for _, c := range configs {
		exp, err := DecodeExperimentFromConfig(c)
		if err != nil {
			return nil, fmt.Errorf("decoding experiment %s from config: %w", c.Metadata.Name, err)
		}

		if running && !exp.Running() {
			continue
		}

		experiments = append(experiments, exp)
	}

	return experiments, nil
}

func DecodeExperimentFromConfig(c store.Config) (*Experiment, error) {
	iface, err := version.GetVersionedSpecForKind(c.Kind, c.APIVersion())
	if err != nil {
		return nil, fmt.Errorf("getting versioned spec for config: %w", err)
	}

	if err := mapstructure.Decode(c.Spec, &iface); err != nil {
		return nil, fmt.Errorf("decoding versioned spec for experiment %s: %w", c.Metadata.Name, err)
	}

	spec, ok := iface.(ifaces.ExperimentSpec)
	if !ok {
		return nil, errors.New("invalid spec in config")
	}

	status, err := decodeStatus(c)
	if err != nil {
		return nil, err
	}

	exp := &Experiment{ //nolint:exhaustruct // partial initialization
		Metadata: c.Metadata,
		Spec:     spec,
		Status:   status,
	}

	return exp, nil
}

// decodeStatus decodes the experiment status of config c. Unlike its spec,
// the status needs no other config, so it can be decoded within store.Modify.
func decodeStatus(c store.Config) (ifaces.ExperimentStatus, error) { //nolint:ireturn // versioned status
	iface, err := version.GetVersionedStatusForKind(c.Kind, c.APIVersion())
	if err != nil {
		return nil, fmt.Errorf("getting versioned status for config: %w", err)
	}

	if err := mapstructure.Decode(c.Status, &iface); err != nil {
		return nil, fmt.Errorf("decoding versioned status: %w", err)
	}

	status, ok := iface.(ifaces.ExperimentStatus)
	if !ok {
		return nil, errors.New("invalid status in config")
	}

	return status, nil
}
