package scorch

import (
	"context"
	"fmt"

	"github.com/mitchellh/mapstructure"
	"inet.af/netaddr"

	"phenix/api/scorch/scorchmd"
	"phenix/app"
	"phenix/types"
	ifaces "phenix/types/interfaces"
	"phenix/util"
	"phenix/util/mm"
	"phenix/util/tap"
)

const tapCompSuffixLen = 7

type Tap struct {
	options Options
}

func (t *Tap) Init(opts ...Option) error {
	t.options = NewOptions(opts...)

	return nil
}

func (Tap) Type() string {
	return "tap"
}

func (Tap) Configure(context.Context) error {
	return nil
}

func (t Tap) Start(ctx context.Context) error {
	exp := t.options.Exp.Metadata.Name

	var tp *tap.Tap

	err := mapstructure.Decode(t.options.Meta, &tp)
	if err != nil {
		return fmt.Errorf("decoding tap component metadata: %w", err)
	}

	// Read the status that records the tap before creating it, so a missing
	// status cannot leave an unrecorded tap behind.
	var status scorchmd.ScorchStatus

	err = t.options.Exp.Status.ParseAppStatus("scorch", &status)
	if err != nil {
		return fmt.Errorf("getting experiment status for scorch app: %w", err)
	}

	pairs := discoverUsedPairs()
	tp.Init(t.options.Exp.Spec.DefaultBridge(), tap.Experiment(exp), tap.UsedPairs(pairs))

	// backwards compatibility (doesn't support external access firewall rules)
	if v, ok := tp.Other["internetAccess"]; ok {
		enabled, _ := v.(bool)
		tp.External.Enabled = enabled
	}

	// The VLAN, the bridge and the address reach minimega and the host.
	if err := tp.Validate(); err != nil {
		return fmt.Errorf("tap component %s: %w", t.options.Name, err)
	}

	// tap names cannot be longer than 15 characters
	// (dictated by max length of Linux interface names)
	tp.Name = util.RandomString(tapCompSuffixLen) + "-tapcomp"

	if _, createErr := tp.Create(mm.Headnode()); createErr != nil {
		return fmt.Errorf("setting up tap: %w", createErr)
	}

	if status.Taps == nil {
		status.Taps = make(map[string]*tap.Tap)
	}

	status.Taps[t.options.Name] = tp

	// The stop stage finds the tap in the status shared by the run's
	// components even if this write fails.
	t.options.Exp.Status.SetAppStatus("scorch", status)

	if err := storeRunRecord(t.options.Exp); err != nil {
		return fmt.Errorf("recording tap %s in experiment status: %w", tp.Name, err)
	}

	return nil
}

// storeRunRecord writes the run's record, status.apps.scorch, from the status
// the run's components share to the stored experiment, and only that: the
// shared status is the copy the run read when it began, so writing all of it
// back would revert what was stored since, such as a soh component's results.
// UpdateStatus refuses a run that was stopped, or started again, meanwhile.
func storeRunRecord(exp types.Experiment) error {
	record := exp.Status.AppStatus()["scorch"]

	err := exp.UpdateStatus(func(status ifaces.ExperimentStatus) error {
		status.SetAppStatus("scorch", record)

		return nil
	})
	if err != nil {
		return fmt.Errorf("saving the Scorch run record of experiment %s: %w", exp.Metadata.Name, err)
	}

	return nil
}

func (t Tap) Stop(ctx context.Context) error {
	exp := t.options.Exp.Metadata.Name

	var status scorchmd.ScorchStatus

	err := t.options.Exp.Status.ParseAppStatus("scorch", &status)
	if err != nil {
		return fmt.Errorf("getting experiment status for scorch app: %w", err)
	}

	tp, ok := status.Taps[t.options.Name]
	if ok {
		tp.Init(t.options.Exp.Spec.DefaultBridge(), tap.Experiment(exp))

		err = tp.Delete(mm.Headnode())
		if err != nil {
			return fmt.Errorf("deleting host tap for VLAN %s: %w", tp.VLAN, err)
		}
	}

	return nil
}

func (Tap) Cleanup(context.Context) error {
	return nil
}

func discoverUsedPairs() []netaddr.IPPrefix {
	var pairs []netaddr.IPPrefix

	running, err := types.Experiments(true)
	if err != nil {
		return nil
	}

	for _, exp := range running {
		var scorch scorchmd.ScorchStatus

		err = exp.Status.ParseAppStatus("scorch", &scorch)
		if err == nil {
			for _, tap := range scorch.Taps {
				if pair, parseErr := netaddr.ParseIPPrefix(tap.Subnet); parseErr == nil {
					pairs = append(pairs, pair)
				}
			}
		}

		var tap app.TapAppStatus

		err = exp.Status.ParseAppStatus("tap", &tap)
		if err == nil {
			for _, tap := range tap.Taps {
				if pair, parseErr := netaddr.ParseIPPrefix(tap.Subnet); parseErr == nil {
					pairs = append(pairs, pair)
				}
			}
		}
	}

	return pairs
}
