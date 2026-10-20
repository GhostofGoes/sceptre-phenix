package app

import (
	"context"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/hashicorp/go-multierror"
	"inet.af/netaddr"

	"phenix/types"
	"phenix/util"
	"phenix/util/mm"
	"phenix/util/tap"
)

const (
	tapNameRandomLength = 8

	// tapStagePreStart runs tap creation during the experiment's pre-start stage.
	tapStagePreStart = "pre-start"
	// tapStagePostStart runs tap creation during the experiment's post-start
	// stage. This is the default to preserve backwards compatibility.
	tapStagePostStart = "post-start"
)

func init() { //nolint:gochecknoinits // app registration
	err := RegisterUserApp("tap", func() App { return new(Tap) })
	if err != nil {
		panic(err)
	}
}

type TapAppMetadata struct {
	// Stage controls which experiment lifecycle stage the taps are created in.
	// Valid values are "pre-start" and "post-start". Defaults to "post-start"
	// when unset.
	Stage string     `mapstructure:"stage"`
	Taps  []*tap.Tap `mapstructure:"taps"`
}

type TapAppStatus struct {
	Host string     `mapstructure:"host" structs:"host"`
	Taps []*tap.Tap `mapstructure:"taps" structs:"taps"`
}

type Tap struct {
	options Options
}

func (t *Tap) Init(opts ...Option) error {
	t.options = NewOptions(opts...)

	return nil
}

func (Tap) Name() string {
	return "tap"
}

func (Tap) Configure(ctx context.Context, exp *types.Experiment) error {
	return nil
}

func (t *Tap) PreStart(ctx context.Context, exp *types.Experiment) error {
	return t.createTaps(exp, tapStagePreStart)
}

func (t *Tap) PostStart(ctx context.Context, exp *types.Experiment) error {
	return t.createTaps(exp, tapStagePostStart)
}

// createTaps creates the configured host taps if the app's configured stage
// matches the given stage. The stage defaults to "post-start" when unset in the
// app metadata, preserving the original behavior.
func (t *Tap) createTaps(exp *types.Experiment, stage string) error {
	app := exp.App(t.Name())
	if app == nil {
		// this should never happen...
		return fmt.Errorf("%s app not defined in experiment scenario", t.Name())
	}

	var amd TapAppMetadata
	if err := app.ParseMetadata(&amd); err != nil {
		return fmt.Errorf("decoding %s app metadata: %w", t.Name(), err)
	}

	configuredStage := amd.Stage
	if configuredStage == "" {
		configuredStage = tapStagePostStart
	}

	switch configuredStage {
	case tapStagePreStart, tapStagePostStart:
	default:
		return fmt.Errorf(
			"invalid stage %q for %s app: must be %q or %q",
			amd.Stage, t.Name(), tapStagePreStart, tapStagePostStart,
		)
	}

	// Only create taps during the stage the app is configured to run in.
	if configuredStage != stage {
		return nil
	}

	if err := validateTaps(amd.Taps); err != nil {
		return err
	}

	// A dry run has no VMs to tap, and asking minimega for the experiment's
	// hosts would create its namespace.
	if t.options.DryRun {
		return nil
	}

	// A trigger runs the stage again while the taps it created before still
	// exist; delete them, since the new status replaces the one recording them.
	if err := t.deleteRecordedTaps(exp); err != nil {
		return fmt.Errorf("deleting the host taps created before: %w", err)
	}

	exp.Status.SetAppStatus(t.Name(), nil)

	hosts, err := mm.GetNamespaceHosts(exp.Metadata.Name)
	if err != nil {
		return fmt.Errorf("getting list of experiment hosts: %w", err)
	}

	// The host list is empty when minimega cannot be queried.
	if len(hosts) == 0 {
		return fmt.Errorf("no minimega hosts found for experiment %s to create host taps on", exp.Metadata.Name)
	}

	rng := rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), 1)) //nolint:gosec // weak random number generator

	var (
		host  = hosts[rng.IntN(len(hosts))].Name
		pairs = t.discoverUsedPairs()
	)

	status := TapAppStatus{Host: host} //nolint:exhaustruct // partial initialization
	appName := t.Name()

	for _, t := range amd.Taps {
		opts := []tap.Option{tap.Experiment(exp.Metadata.Name), tap.UsedPairs(pairs)}

		if subnet, err := netaddr.ParseIPPrefix(t.Subnet); err == nil {
			opts = append(opts, tap.PairSubnet(subnet))
		}

		t.Init(exp.Spec.DefaultBridge(), opts...)

		// Tap name is random, yet descriptive to the fact that it's a "tapapp" tap.
		t.Name = util.RandomString(tapNameRandomLength) + "-tapapp"

		pair, err := t.Create(host)
		if err != nil {
			err = fmt.Errorf("creating host tap for VLAN %s: %w", t.VLAN, err)

			// No status is recorded for a failed stage, so nothing else would
			// delete the taps already created; record the ones left, if any.
			if left, delErr := deleteTaps(exp, host, status.Taps); delErr != nil {
				exp.Status.SetAppStatus(appName, TapAppStatus{Host: host, Taps: left})

				return multierror.Append(err, delErr)
			}

			return err
		}

		if !pair.IsZero() {
			// Include pair just created for this tap to list of used pairs in case
			// more than one tap is being created for this experiment.
			pairs = append(pairs, pair)
		}

		status.Taps = append(status.Taps, t)
	}

	exp.Status.SetAppStatus(t.Name(), status)

	return nil
}

func (Tap) Running(ctx context.Context, exp *types.Experiment) error {
	return nil
}

func (t *Tap) Cleanup(ctx context.Context, exp *types.Experiment) error {
	return t.deleteRecordedTaps(exp)
}

// deleteRecordedTaps deletes the taps the app's status records.
func (t *Tap) deleteRecordedTaps(exp *types.Experiment) error {
	if _, ok := exp.Status.AppStatus()[t.Name()]; !ok {
		// No status was ever recorded for this app, meaning its taps were never
		// created. This happens, for example, when the app is configured to run
		// in the post-start stage but the experiment is stopped before delayed
		// VMs finish starting, so post-start (and thus createTaps) never runs.
		// There's nothing to clean up in that case.
		return nil
	}

	var status TapAppStatus

	err := exp.Status.ParseAppStatus(t.Name(), &status)
	if err != nil {
		return fmt.Errorf("getting experiment status for %s app: %w", t.Name(), err)
	}

	// minimega fails to delete a tap that is gone, so a status that still
	// listed the deleted taps would fail every later trigger and cleanup.
	left, err := deleteTaps(exp, status.Host, status.Taps)
	if err != nil {
		status.Taps = left
		exp.Status.SetAppStatus(t.Name(), status)

		return err
	}

	exp.Status.SetAppStatus(t.Name(), nil)

	return nil
}

func (t Tap) discoverUsedPairs() []netaddr.IPPrefix {
	var pairs []netaddr.IPPrefix

	running, err := types.Experiments(true)
	if err != nil {
		return nil
	}

	for _, exp := range running {
		var status TapAppStatus

		err := exp.Status.ParseAppStatus(t.Name(), &status)
		if err == nil {
			for _, tap := range status.Taps {
				if pair, err := netaddr.ParseIPPrefix(tap.Subnet); err == nil {
					pairs = append(pairs, pair)
				}
			}
		}
	}

	return pairs
}

// validateTaps checks every configured tap before any is created, so a refused
// configuration leaves nothing on the host.
func validateTaps(taps []*tap.Tap) error {
	vlans := make(map[string]struct{}, len(taps))

	for i, tp := range taps {
		if tp == nil {
			return fmt.Errorf("taps[%d] of the tap app is empty", i)
		}

		if _, ok := vlans[tp.VLAN]; ok {
			return fmt.Errorf("tap already created for VLAN %s", tp.VLAN)
		}

		vlans[tp.VLAN] = struct{}{}

		if err := tp.Validate(); err != nil {
			return err
		}
	}

	return nil
}

// deleteTaps deletes the given taps from host. It returns the taps it failed
// to delete and every failure.
func deleteTaps(exp *types.Experiment, host string, taps []*tap.Tap) ([]*tap.Tap, error) {
	var (
		left []*tap.Tap
		errs error
	)

	for _, tp := range taps {
		tp.Init(exp.Spec.DefaultBridge(), tap.Experiment(exp.Metadata.Name))

		err := tp.Delete(host)
		if err != nil {
			left = append(left, tp)
			errs = multierror.Append(
				errs,
				fmt.Errorf("deleting host tap for VLAN %s: %w", tp.VLAN, err),
			)
		}
	}

	return left, errs
}
