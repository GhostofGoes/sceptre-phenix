package experiment

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/activeshadow/structs"
	"github.com/hashicorp/go-multierror"
	"github.com/mitchellh/mapstructure"

	"phenix/api/config"
	"phenix/app"
	"phenix/scheduler"
	"phenix/store"
	"phenix/tmpl"
	"phenix/types"
	"phenix/types/version"
	v1 "phenix/types/version/v1"
	"phenix/util/common"
	"phenix/util/file"
	"phenix/util/mm"
	"phenix/util/mm/mmcli"
	"phenix/util/notes"
	"phenix/util/plog"
	"phenix/util/pubsub"
)

const (
	// MaxBridgeNameLength is the longest default bridge name, in bytes: the
	// Linux interface name limit. With the auto bridge mode it bounds the
	// experiment name too.
	MaxBridgeNameLength = 15

	// ReservedName is the experiment name [Create] refuses in any letter case.
	ReservedName = "all"

	c2CheckInterval = 5 * time.Second
)

var (
	ErrExperimentNotFound   = errors.New("experiment not found")
	ErrExperimentNotRunning = errors.New("experiment not running")
)

func init() { //nolint:gochecknoinits // config hook
	config.RegisterConfigHook("Experiment", experimentConfigHook)
}

func experimentConfigHook(stage string, c *store.Config) error {
	exp, err := types.DecodeExperimentFromConfig(*c)
	if err != nil {
		return fmt.Errorf("decoding experiment from config: %w", err)
	}

	switch stage {
	case "startup":
		return hookStartup(exp, c)
	case "create":
		return hookCreate(exp, c)
	case "update":
		return hookUpdate(exp, c)
	case "delete":
		return hookDelete(exp)
	}

	return nil
}

func hookStartup(exp *types.Experiment, c *store.Config) error {
	// Experiments created before the default bridge option was added need to
	// be updated to have a default `phenix` bridge.
	if exp.Spec.DefaultBridge() == "" {
		exp.Spec.SetDefaultBridge(defaultBridgeName)

		err := exp.Spec.Init()
		if err != nil {
			return fmt.Errorf("re-initializing experiment with default bridge: %w", err)
		}

		c.Spec = structs.MapDefaultCase(exp.Spec, structs.CASESNAKE)
	}
	return nil
}

func hookCreate(exp *types.Experiment, c *store.Config) error {
	exp.Spec.SetExperimentName(c.Metadata.Name)

	err := exp.Spec.Init()
	if err != nil {
		return fmt.Errorf("initializing experiment: %w", err)
	}

	if common.BridgeMode == common.BridgeModeAuto {
		if len(c.Metadata.Name) > MaxBridgeNameLength {
			return fmt.Errorf(
				"experiment name must be %d characters or less when using auto bridge mode",
				MaxBridgeNameLength,
			)
		}

		exp.Spec.SetDefaultBridge(c.Metadata.Name)
	}

	if len(exp.Spec.DefaultBridge()) > MaxBridgeNameLength {
		return fmt.Errorf("default bridge name must be %d characters or less", MaxBridgeNameLength)
	}

	exp.Spec.SetUseGREMesh(exp.Spec.UseGREMesh() || common.UseGREMesh)

	existing, _ := types.Experiments(false)
	for _, other := range existing {
		if other.Metadata.Name == exp.Metadata.Name {
			continue
		}

		if exp.Spec.DefaultBridge() == defaultBridgeName {
			continue
		}

		if other.Spec.DefaultBridge() == exp.Spec.DefaultBridge() {
			return fmt.Errorf(
				"experiment %s already using default bridge %s",
				other.Metadata.Name,
				other.Spec.DefaultBridge(),
			)
		}
	}

	err = exp.Spec.VerifyScenario(context.Background())
	if err != nil {
		return fmt.Errorf("verifying experiment scenario: %w", err)
	}

	err = app.ApplyApps(context.Background(), exp, app.Stage(app.ActionConfigure))
	if err != nil {
		return fmt.Errorf("applying apps to experiment: %w", err)
	}

	c.Spec = structs.MapDefaultCase(exp.Spec, structs.CASESNAKE)
	return nil
}

func hookUpdate(exp *types.Experiment, c *store.Config) error {
	if exp.Running() {
		// Halt this update if the experiment is running.
		return errors.New("cannot update running experiment")
	}

	err := exp.Spec.Init()
	if err != nil {
		return fmt.Errorf("re-initializing experiment (after update): %w", err)
	}

	// Just in case the updated experiment reset the default bridge.
	if common.BridgeMode == common.BridgeModeAuto {
		if len(c.Metadata.Name) > MaxBridgeNameLength {
			return fmt.Errorf(
				"experiment name must be %d characters or less when using auto bridge mode",
				MaxBridgeNameLength,
			)
		}

		exp.Spec.SetDefaultBridge(c.Metadata.Name)
	}

	if len(exp.Spec.DefaultBridge()) > MaxBridgeNameLength {
		return fmt.Errorf("default bridge name must be %d characters or less", MaxBridgeNameLength)
	}

	exp.Spec.SetUseGREMesh(exp.Spec.UseGREMesh() || common.UseGREMesh)

	existing, _ := types.Experiments(false)
	for _, other := range existing {
		if other.Metadata.Name == exp.Metadata.Name {
			continue
		}

		if exp.Spec.DefaultBridge() == defaultBridgeName {
			continue
		}

		if other.Spec.DefaultBridge() == exp.Spec.DefaultBridge() {
			return fmt.Errorf(
				"experiment %s already using default bridge %s",
				other.Metadata.Name,
				other.Spec.DefaultBridge(),
			)
		}
	}

	if exp.Spec.ExperimentName() != c.Metadata.Name {
		if strings.Contains(exp.Spec.BaseDir(), exp.Spec.ExperimentName()) {
			// If the experiment's base directory contains the current experiment
			// name, replace it with the new name.
			dir := strings.ReplaceAll(
				exp.Spec.BaseDir(),
				exp.Spec.ExperimentName(),
				c.Metadata.Name,
			)
			exp.Spec.SetBaseDir(dir)
		}

		exp.Spec.SetExperimentName(c.Metadata.Name)
	}

	c.Spec = structs.MapDefaultCase(exp.Spec, structs.CASESNAKE)
	return nil
}

func hookDelete(exp *types.Experiment) error {
	var errors error

	// Delete any snapshot files created by this headnode for this experiment
	// after deleting the experiment.
	err := deleteC2AndSnapshots(exp)
	if err != nil {
		errors = multierror.Append(
			errors,
			fmt.Errorf("deleting experiment snapshots and CC responses: %w", err),
		)
	}

	err = os.RemoveAll(exp.Spec.BaseDir())
	if err != nil {
		errors = multierror.Append(
			errors,
			fmt.Errorf("deleting experiment base directory: %w", err),
		)
	}

	return errors //nolint:wrapcheck // returning multierror
}

// Hook is a function to be called during the different lifecycle stages of an
// experiment. The first argument is the experiment stage (create, start,
// stopping, stop, stop-end, delete), and the second argument is the
// experiment, name. Stopping hooks run first, before the cleanup apps and
// while the experiment's minimega namespace still exists; stop hooks run once
// the experiment has stopped. Stop-end hooks run last, whenever the stopping
// hooks began to run, also when the stop finds that another process stopped
// the experiment, or started it again, meanwhile, or panics, in a stopping
// hook too: they end what a stopping hook began for the stop, such as a
// refusal of new SCORCH runs, so they must also do no harm for a stop whose
// stopping hooks did not all run.
type Hook func(string, string)

var hooks = make(map[string][]Hook) //nolint:gochecknoglobals // package level registry

// RegisterHook registers a Hook for the given experiment stage.
func RegisterHook(stage string, hook Hook) {
	hooks[stage] = append(hooks[stage], hook)
}

// List collects experiments, each in a struct that references the latest
// versioned experiment spec and status. It returns a slice of experiments and
// any errors encountered while gathering and decoding them.
func List() ([]types.Experiment, error) {
	configs, err := store.List("Experiment")
	if err != nil {
		return nil, fmt.Errorf("getting list of experiment configs from store: %w", err)
	}

	var (
		experiments []types.Experiment
		errs        error
	)

	for _, c := range configs {
		exp, err2 := types.DecodeExperimentFromConfig(c)
		if err2 != nil {
			errs = multierror.Append(
				errs,
				fmt.Errorf("decoding experiment %s from config: %w", c.Metadata.Name, err2),
			)

			continue
		}

		experiments = append(experiments, *exp)
	}

	return experiments, errs //nolint:wrapcheck // returning multierror
}

// Get retrieves the experiment with the given name. It returns a pointer to a
// struct that references the latest versioned experiment spec and status for
// the given experiment, and any errors encountered while retrieving the
// experiment.
func Get(name string) (*types.Experiment, error) {
	if name == "" {
		return nil, errors.New("no experiment name provided")
	}

	c, err := store.NewConfig("experiment/" + name)
	if err != nil {
		return nil, fmt.Errorf("getting experiment: %w", err)
	}

	err = store.Get(c)
	if err != nil {
		return nil, fmt.Errorf("getting experiment %s from store: %w", name, err)
	}

	exp, err2 := types.DecodeExperimentFromConfig(*c)
	if err2 != nil {
		return nil, fmt.Errorf("decoding experiment from config: %w", err2)
	}

	return exp, nil
}

// Create uses the provided arguments to create a new experiment. The
// `scenarioName` argument can be an empty string, in which case no scenario is
// used for the experiment. The `baseDir` argument can be an empty string, in
// which case the default value of `/phenix/experiments/{name}` is used for the
// experiment base directory. It returns any errors encountered while creating
// the experiment.
func Create(ctx context.Context, opts ...CreateOption) error {
	o := newCreateOptions(opts...)

	if o.name == "" {
		return errors.New("no experiment name provided")
	}

	if strings.ToLower(o.name) == ReservedName {
		return fmt.Errorf("cannot use '%s' for experiment name", ReservedName)
	}

	if o.topology == "" {
		return errors.New("no topology name provided")
	}

	if len(o.defaultBridge) > MaxBridgeNameLength {
		return fmt.Errorf("default bridge name must be %d characters or less", MaxBridgeNameLength)
	}

	var (
		kind       = "Experiment"
		apiVersion = version.StoredVersion[kind]
	)

	topoC, _ := store.NewConfig("topology/" + o.topology)

	err := store.Get(topoC)
	if err != nil {
		return errors.New("topology doesn't exist")
	}

	topo, err2 := types.DecodeTopologyFromConfig(*topoC)
	if err2 != nil {
		return fmt.Errorf("decoding topology from config: %w", err2)
	}

	meta := store.ConfigMetadata{ //nolint:exhaustruct // partial initialization
		Name: o.name,
		Annotations: map[string]string{
			"topology": o.topology,
		},
	}

	specMap := map[string]any{
		"experimentName": o.name,
		"baseDir":        o.baseDir,
		"deployMode":     o.deployMode,
		"defaultBridge":  o.defaultBridge,
		"topology":       topo,
	}

	if len(o.disabledApps) > 0 {
		plog.Info(plog.TypePhenixApp, fmt.Sprintf("Got disabled applications: %v", o.disabledApps))
	}

	if err = processCreateScenario(o.scenario, o.topology, o.disabledApps, &meta, specMap); err != nil {
		return err
	}

	for k, v := range o.annotations {
		if _, ok := meta.Annotations[k]; !ok {
			meta.Annotations[k] = v
		}
	}

	c := &store.Config{ //nolint:exhaustruct // partial initialization
		Version:  store.APIGroup + "/" + apiVersion,
		Kind:     kind,
		Metadata: meta,
		Spec:     specMap,
	}

	exp, err2 := types.DecodeExperimentFromConfig(*c)
	if err2 != nil {
		return fmt.Errorf("decoding experiment from config: %w", err2)
	}

	err = exp.Spec.SetVLANRange(o.vlanMin, o.vlanMax, false)
	if err != nil {
		return fmt.Errorf("setting vlan range: %w", err)
	}

	exp.Spec.VLANs().SetAliases(o.vlanAliases)
	exp.Spec.SetSchedule(o.schedules)
	exp.Spec.SetUseGREMesh(o.useGREMesh)

	c.Spec = structs.MapDefaultCase(exp.Spec, structs.CASESNAKE)

	_, err = config.Create(
		config.CreateFromConfig(c),
		config.CreateWithValidation(),
	)
	if err != nil {
		return fmt.Errorf("creating experiment config: %w", err)
	}

	for _, hook := range hooks["create"] {
		hook("create", o.name)
	}

	return nil
}

func processCreateScenario(
	scenarioName, topologyName string,
	disabledApps []string,
	meta *store.ConfigMetadata,
	specMap map[string]any,
) error {
	if scenarioName == "" {
		return nil
	}

	scenarioC, _ := store.NewConfig("scenario/" + scenarioName)

	err := store.Get(scenarioC)
	if err != nil {
		return errors.New("scenario doesn't exist")
	}

	topo, ok := scenarioC.Metadata.Annotations["topology"]
	if !ok {
		return errors.New("topology annotation missing from scenario")
	}

	if !strings.Contains(topo, topologyName) {
		return fmt.Errorf("experiment/scenario topology mismatch for scenario %s", scenarioName)
	}

	plog.Info(plog.TypeSystem, "Creating custom scenario")

	scenario, err2 := types.MakeCustomScenarioFromConfig(*scenarioC, disabledApps)
	if err2 != nil {
		return fmt.Errorf("decoding scenario from config: %w", err2)
	}

	err = types.MergeScenariosForTopology(scenario, topologyName)
	if err != nil {
		return fmt.Errorf("merging scenarios: %w", err)
	}

	meta.Annotations["scenario"] = scenarioName
	specMap["scenario"] = scenario

	return nil
}

// Schedule applies the given scheduling algorithm to the experiment with the
// given name. It returns any errors encountered while scheduling the
// experiment.
func Schedule(opts ...ScheduleOption) error {
	var err error
	o := newScheduleOptions(opts...)

	c, _ := store.NewConfig("experiment/" + o.name)

	err = store.Get(c)
	if err != nil {
		return fmt.Errorf("getting experiment %s from store: %w", o.name, err)
	}

	exp, err2 := types.DecodeExperimentFromConfig(*c)
	if err2 != nil {
		return fmt.Errorf("decoding experiment from config: %w", err2)
	}

	if exp.Running() {
		return fmt.Errorf("experiment already running (started at: %s)", exp.Status.StartTime())
	}

	err = scheduler.Schedule(o.algorithm, exp.Spec)
	if err != nil {
		return fmt.Errorf("running scheduler algorithm: %w", err)
	}

	c.Spec = structs.MapDefaultCase(exp.Spec, structs.CASESNAKE)

	err = store.Update(c)
	if err != nil {
		return fmt.Errorf("updating experiment config: %w", err)
	}

	return nil
}

// Start starts the experiment with the given name. It returns any errors
// encountered while starting the experiment.
//
//nolint:cyclop,funlen,gocyclo,maintidx // complex logic
func Start(ctx context.Context, opts ...StartOption) error {
	o := newStartOptions(opts...)

	c, _ := store.NewConfig("experiment/" + o.name)

	err := store.Get(c)
	if err != nil {
		return fmt.Errorf("getting experiment %s from store: %w", o.name, err)
	}

	exp, err2 := types.DecodeExperimentFromConfig(*c)
	if err2 != nil {
		return fmt.Errorf("decoding experiment from config: %w", err2)
	}

	if exp.Running() {
		if !strings.HasSuffix(exp.Status.StartTime(), "-DRYRUN") {
			return fmt.Errorf("experiment already running (started at: %s)", exp.Status.StartTime())
		}
	}

	if o.vlanMin != 0 {
		exp.Spec.VLANs().SetMin(o.vlanMin)
	}

	if o.vlanMax != 0 {
		exp.Spec.VLANs().SetMax(o.vlanMax)
	}

	err = app.ApplyApps(
		ctx,
		exp,
		app.Stage(app.ActionPreStart),
		app.DryRun(o.dryrun),
	)
	if err != nil {
		return fmt.Errorf("applying apps to experiment: %w", err)
	}

	var (
		mmScript = fmt.Sprintf("%s/mm_files/%s.mm", exp.Spec.BaseDir(), exp.Spec.ExperimentName())
		ccScript = fmt.Sprintf(
			"%s/mm_files/%s-cc.mm",
			exp.Spec.BaseDir(),
			exp.Spec.ExperimentName(),
		)
	)

	err = tmpl.CreateFileFromTemplate("minimega_script.tmpl", exp.Spec, mmScript)
	if err != nil {
		return cleanUpApps(exp, o.dryrun, fmt.Errorf("generating minimega script: %w", err))
	}

	if exp.Spec.Topology().HasCommands() {
		err = tmpl.CreateFileFromTemplate(
			"minimega_cc_script.tmpl",
			exp.Spec.Topology().Nodes(),
			ccScript,
		)
		if err != nil {
			return cleanUpApps(exp, o.dryrun, fmt.Errorf("generating minimega cc script: %w", err))
		}
	}

	var (
		delays = make(map[string]time.Duration)
		c2s    = make(map[string]map[string]bool)
	)

	if o.dryrun {
		exp.Status.SetVLANs(exp.Spec.VLANs().Aliases())
	} else {
		// Delete any snapshot files created by this headnode for this experiment
		// previously before starting the experiment. This way, cluster nodes that VMs
		// get scheduled on will pull the most up-to-date snapshot files.
		err = deleteC2AndSnapshots(exp)
		if err != nil {
			return cleanUpApps(exp, o.dryrun, fmt.Errorf("deleting experiment snapshots and CC responses: %w", err))
		}

		err = mm.ReadScriptFromFile(exp.Spec.ExperimentName(), mmScript)
		if err != nil {
			if !o.mmErrAsWarn {
				return abortStart(exp, o.dryrun, fmt.Errorf("reading minimega script: %w", err))
			}

			merr := &multierror.Error{} //nolint:exhaustruct // library struct
			if errors.As(err, &merr) {
				notes.AddWarnings(ctx, false, merr.Errors...)
			} else {
				notes.AddWarnings(ctx, false, err)
			}
		}

		var (
			bootable = exp.Spec.Topology().BootableNodes()
			start    = make([]string, 0) // nil vs. slice makes a difference here
		)

		for _, node := range bootable {
			if node.External() {
				continue
			}

			hostname := node.General().Hostname()

			if node.Delay().User() {
				notes.AddInfo(
					ctx,
					true,
					fmt.Sprintf("VM %s delayed - to be started by user", hostname),
				)

				continue
			}

			if others := node.Delay().C2(); len(others) > 0 {
				var (
					c2    = make(map[string]bool)
					hosts = make([]string, len(others))
				)

				for i, other := range others {
					if other.Hostname() == "" {
						continue
					}

					c2[other.Hostname()] = other.UseUUID()
					hosts[i] = other.Hostname()
				}

				if len(c2) > 0 {
					c2s[hostname] = c2
					notes.AddInfo(
						ctx,
						true,
						fmt.Sprintf(
							"VM %s delayed - will be started after C2 for %v is active",
							hostname,
							hosts,
						),
					)

					continue
				}
			}

			if d := node.Delay().Timer(); d != 0 {
				delays[hostname] = d

				notes.AddInfo(
					ctx,
					true,
					fmt.Sprintf("VM %s delayed - will be started after %v", hostname, d),
				)

				continue
			}

			start = append(start, hostname)
		}

		if len(start) == len(bootable) {
			// Reset start slice so the call to mm.LaunchVMs results in `vm start all`
			// being used (to reduce calls to minimega). A nil slice vs. an empty
			// slice makes a difference here.
			start = nil
		}

		err = mm.LaunchVMs(exp.Spec.ExperimentName(), start...)
		if err != nil {
			if !o.mmErrAsWarn {
				return abortStart(exp, o.dryrun, fmt.Errorf("launching experiment VMs: %w", err))
			}

			merr := &multierror.Error{} //nolint:exhaustruct // library struct
			if errors.As(err, &merr) {
				notes.AddWarnings(ctx, false, merr.Errors...)
			} else {
				notes.AddWarnings(ctx, false, err)
			}
		}

		// Creating experiment bridge after launching VMs to ensure the bridge
		// already exists in minimega (and OVS) before creating GRE tunnels between
		// them. This cannot be done as part of the minimega script template since
		// the VM taps (and thus bridges) do not get created until the overall
		// minimega namespace is launched.
		if exp.Spec.UseGREMesh() {
			err = mm.CreateBridge(mm.NS(exp.Metadata.Name), mm.Bridge(exp.Spec.DefaultBridge()))
			if err != nil {
				if !o.mmErrAsWarn {
					return abortStart(exp, o.dryrun, fmt.Errorf("creating experiment bridge: %w", err))
				}

				merr := &multierror.Error{} //nolint:exhaustruct // library struct
				if errors.As(err, &merr) {
					notes.AddWarnings(ctx, false, merr.Errors...)
				} else {
					notes.AddWarnings(ctx, false, err)
				}
			}
		}

		var (
			vms      = mm.GetVMInfo(mm.NS(exp.Spec.ExperimentName()))
			schedule = make(map[string]string)
		)

		for _, vm := range vms {
			schedule[vm.Name] = vm.Host
		}

		// minimega accepts `vm launch` and `vm start` even when QEMU exits right
		// away (for example a missing QEMU module or disk image); the VM is then
		// left in the ERROR state with the reason in its error tag.
		launched := make([]string, 0, len(bootable))

		for _, node := range bootable {
			if !node.External() {
				launched = append(launched, node.General().Hostname())
			}
		}

		err = launchErrors(vms, launched)
		if err != nil {
			if !o.mmErrAsWarn {
				return abortStart(exp, o.dryrun, fmt.Errorf("launching experiment VMs: %w", err))
			}

			merr := &multierror.Error{} //nolint:exhaustruct // library struct
			if errors.As(err, &merr) {
				notes.AddWarnings(ctx, false, merr.Errors...)
			} else {
				notes.AddWarnings(ctx, false, err)
			}
		}

		exp.Status.SetSchedule(schedule)

		var vlans map[string]int
		vlans, err = mm.GetVLANs(mm.NS(exp.Spec.ExperimentName()))
		if err != nil {
			return abortStart(exp, o.dryrun, fmt.Errorf("processing experiment VLANs: %w", err))
		}

		exp.Status.SetVLANs(vlans)
	}

	start := time.Now().Format(time.RFC3339)

	if o.dryrun {
		start += "-DRYRUN"
	}

	prior := exp.Status.StartTime()
	exp.Status.SetStartTime(start)

	// Persist the start time before waiting for delayed VMs, so the experiment
	// is reported running while its VMs run, a failure or an interrupt from
	// here on is torn down by Stop, which also runs the cleanup apps for what
	// pre-start set up, and the delayed VMs' waits see a stop from another
	// process.
	err = saveExperiment(c, exp)
	if err != nil {
		// The cleanup apps write status only to the run the store holds.
		exp.Status.SetStartTime(prior)

		return abortStart(exp, o.dryrun, err)
	}

	if o.errChan == nil {
		err = finishStart(ctx, exp, ccScript, start, delays, c2s, o.dryrun)
		if err == nil {
			// Persist the spec the post-start apps changed, unless the run
			// ended. Each app's stage stored its own status changes, and the
			// whole status would revert what others stored meanwhile, such as
			// a SCORCH run's running flag.
			err = exp.WriteSpecToStore()
		}

		// A stop from another process already tore it down, and the
		// experiment may have been started again since.
		if stoppedElsewhere(err) {
			return stoppedWhileStarting(o.name)
		}

		if err != nil {
			errs := multierror.Append(nil, err)

			if err := stopFailedStart(o.name, start, nil); err != nil {
				errs = multierror.Append(errs, err)
			}

			return errs
		}
	} else {
		// Stop, in this process, cancels this context and waits for the
		// goroutine before it clears the namespace the goroutine sends to.
		runCtx, cancel := context.WithCancel(ctx)
		run := trackAsyncStart(o.name, cancel)

		go func() {
			// A Stop waiting for this goroutine proceeds once finish runs, by
			// which time the start has reported all of its errors.
			defer run.finish(o.name)
			defer close(o.errChan)

			// stop tears the start down after it failed with err. A canceled
			// start is already being stopped, by the Stop or the caller (e.g.
			// the web UI) that canceled it; stopping it again would race with
			// that stop and its cleanup. One stopped by another process is
			// torn down already.
			stop := func(err error) {
				if runCtx.Err() != nil || stoppedElsewhere(err) {
					return
				}

				if err := stopFailedStart(o.name, start, run); err != nil {
					o.errChan <- err
				}
			}

			if !o.dryrun {
				if exp.Spec.Topology().HasCommands() {
					err = mm.ReadScriptFromFile(exp.Spec.ExperimentName(), ccScript)
					if err != nil {
						o.errChan <- fmt.Errorf("reading minimega cc script: %w", err)

						stop(err)

						return
					}
				}

				err = handleDelayedVMs(runCtx, exp.Spec.ExperimentName(), start, delays, c2s)
				if err != nil {
					o.errChan <- fmt.Errorf("handling delayed VMs: %w", err)

					stop(err)

					return
				}
			}

			err = app.ApplyApps(runCtx, exp, app.Stage(app.ActionPostStart), app.DryRun(o.dryrun))
			if err != nil {
				o.errChan <- fmt.Errorf("applying apps to experiment: %w", err)

				stop(err)
			}
		}()
	}

	for _, hook := range hooks["start"] {
		hook("start", o.name)
	}

	return nil
}

// stopFailedStart stops the run of the named experiment that a failed start
// began at start; an asynchronous start passes itself as self. Once another
// process has stopped the experiment, and maybe started it again, that run is
// torn down already: the stop then leaves the experiment alone and returns an
// error wrapping errStoppedWhileStarting.
func stopFailedStart(name, start string, self *asyncStart) error {
	err := stop(name, newStopOptions(), start, self)
	if err == nil || errors.Is(err, errStoppedWhileStarting) {
		return err
	}

	return fmt.Errorf("stopping experiment: %w", err)
}

// stoppedElsewhere reports whether err reports that the experiment was
// stopped, or started again, from elsewhere while it was starting.
func stoppedElsewhere(err error) bool {
	return errors.Is(err, errStoppedWhileStarting) || errors.Is(err, types.ErrExperimentRunChanged)
}

// cleanUpApps runs the cleanup apps of a start that failed after its pre-start
// apps ran, for what those set up, such as the tap app's host taps, and
// returns err with any error from them.
func cleanUpApps(exp *types.Experiment, dryrun bool, err error) error {
	cleanupErr := app.ApplyApps(context.Background(), exp, app.Stage(app.ActionCleanup), app.DryRun(dryrun))
	if cleanupErr != nil {
		return multierror.Append(err, fmt.Errorf("cleaning up app experiments: %w", cleanupErr))
	}

	return err
}

// abortStart tears down what a failed start left before the experiment was
// stored as running: it runs the cleanup apps, then clears the namespace and
// deletes the snapshots the minimega script created, as Stop does, and
// returns err with any error from that. A dry run left nothing in minimega.
func abortStart(exp *types.Experiment, dryrun bool, err error) error {
	err = cleanUpApps(exp, dryrun, err)

	if dryrun {
		return err
	}

	if cleanupErr := stopVMsAndDeleteSnapshots(exp, false); cleanupErr != nil {
		return multierror.Append(err, fmt.Errorf("cleaning up the failed start: %w", cleanupErr))
	}

	return err
}

// finishStart runs what a synchronous start does after the VMs launched:
// reading the minimega cc script, starting the delayed VMs once their delays
// pass, and applying the post-start apps.
func finishStart(
	ctx context.Context,
	exp *types.Experiment,
	ccScript, start string,
	delays map[string]time.Duration,
	c2s map[string]map[string]bool,
	dryrun bool,
) error {
	if !dryrun {
		if exp.Spec.Topology().HasCommands() {
			err := mm.ReadScriptFromFile(exp.Spec.ExperimentName(), ccScript)
			if err != nil {
				return fmt.Errorf("reading minimega cc script: %w", err)
			}
		}

		err := handleDelayedVMs(ctx, exp.Spec.ExperimentName(), start, delays, c2s)
		if err != nil {
			return fmt.Errorf("handling delayed VMs: %w", err)
		}
	}

	err := app.ApplyApps(ctx, exp, app.Stage(app.ActionPostStart), app.DryRun(dryrun))
	if err != nil {
		return fmt.Errorf("applying apps to experiment: %w", err)
	}

	return nil
}

// saveExperiment stores the experiment's spec and status in its config.
func saveExperiment(c *store.Config, exp *types.Experiment) error {
	c.Spec = structs.MapDefaultCase(exp.Spec, structs.CASESNAKE)
	c.Status = structs.MapDefaultCase(exp.Status, structs.CASESNAKE)

	err := store.Update(c)
	if err != nil {
		return fmt.Errorf("updating experiment config: %w", err)
	}

	return nil
}

// Stop stops the experiment with the given name. Injection snapshots are
// deleted by default. It returns any errors encountered while stopping the
// experiment.
func Stop(name string, opts ...StopOption) error {
	return stop(name, newStopOptions(opts...), "", nil)
}

// stop stops the named experiment. A failed start passes the start time of its
// run, and self when it is asynchronous, so that it stops only its own run: it
// returns an error wrapping errStoppedWhileStarting once the experiment is not
// stored with that start time.
func stop(name string, o stopOptions, start string, self *asyncStart) error {
	// An asynchronous start in this process may still wait for delayed VMs or
	// run post-start apps; whatever it sends to minimega after the clear below
	// would create the namespace again.
	endAsyncStart(name, self)

	defer lockStop(name)()

	var err error
	c, _ := store.NewConfig("experiment/" + name)

	err = store.Get(c)
	if err != nil {
		return fmt.Errorf("getting experiment %s from store: %w", name, err)
	}

	exp, err2 := types.DecodeExperimentFromConfig(*c)
	if err2 != nil {
		return fmt.Errorf("decoding experiment from config: %w", err2)
	}

	if start != "" && exp.Status.StartTime() != start {
		return stoppedWhileStarting(name)
	}

	if !exp.Running() {
		return errors.New("experiment isn't running")
	}

	var (
		read   = exp.Status.StartTime() // the run this stop ends
		dryrun = strings.HasSuffix(read, "-DRYRUN")
		errors error
	)

	// Deferred before the stopping hooks, so that the stop-end hooks also run
	// when a stopping hook panics after an earlier one began something, such
	// as SCORCH's refusal of the experiment's runs.
	defer func() {
		for _, hook := range hooks["stop-end"] {
			hook("stop-end", name)
		}
	}()

	// minimega creates a namespace for any command sent to it, so whatever
	// still queries this one must let go of it before it is cleared. The
	// stopping SCORCH runs' stop and cleanup stages also need what the cleanup
	// apps tear down, such as the tap app's taps.
	for _, hook := range hooks["stopping"] {
		hook("stopping", name)
	}

	// The runs the hooks ended stored their status as they ended. The hooks
	// can wait minutes for those runs, and another process may have stopped
	// the experiment, and maybe started it again, meanwhile: then the stored
	// run is not the one this stop read, and the stop leaves it alone.
	if err := exp.Reload(); err != nil {
		errors = multierror.Append(errors, err)
	} else if exp.Status.StartTime() != read {
		return runChangedWhileStopping(name, start, exp.Status.StartTime(), read)
	}

	err = app.ApplyApps(
		context.Background(),
		exp,
		app.Stage(app.ActionCleanup),
		app.DryRun(dryrun),
	)
	if err != nil {
		errors = multierror.Append(errors, fmt.Errorf("cleaning up app experiments: %w", err))
	}

	if !dryrun {
		err = stopVMsAndDeleteSnapshots(exp, o.keepInjectionSnapshots)
		if err != nil {
			errors = multierror.Append(errors, err)
		}
	}

	exp.Status.SetStartTime("")

	err = store.Modify(c, func(c *store.Config) error {
		if stored, _ := c.Status["startTime"].(string); stored != read {
			return runChangedWhileStopping(name, start, stored, read)
		}

		c.Spec = structs.MapDefaultCase(exp.Spec, structs.CASESNAKE)
		c.Status = structs.MapDefaultCase(exp.Status, structs.CASESNAKE)

		return nil
	})
	if err != nil {
		if stoppedElsewhere(err) {
			// The stop hooks would end what the newer run uses, such as its
			// mounts and its SCORCH run record.
			return multierror.Append(errors, err)
		}

		errors = multierror.Append(errors, fmt.Errorf("updating experiment config: %w", err))
	}

	for _, hook := range hooks["stop"] {
		hook("stop", name)
	}

	return errors
}

// runChangedWhileStopping reports that the stored start time of the named
// experiment changed from read, the run a stop read, to stored while the stop
// ran. For the stop of a failed start, start is that run's start time.
func runChangedWhileStopping(name, start, stored, read string) error {
	if start != "" {
		return stoppedWhileStarting(name)
	}

	return fmt.Errorf(
		"experiment %s: %w while it was stopping: stored start time %q, read %q",
		name,
		types.ErrExperimentRunChanged,
		stored,
		read,
	)
}

func stopVMsAndDeleteSnapshots(exp *types.Experiment, keepSnapshots bool) error {
	err := mm.ClearNamespace(exp.Spec.ExperimentName())
	if err != nil {
		return fmt.Errorf("killing experiment VMs: %w", err)
	}

	if keepSnapshots {
		return nil
	}

	err = deleteSnapshots(exp)
	if err != nil {
		return fmt.Errorf("deleting experiment snapshots: %w", err)
	}

	return nil
}

func Status(name string) (*v1.ExperimentStatus, error) {
	c, _ := store.NewConfig("experiment/" + name)

	err := store.Get(c)
	if err != nil {
		return nil, fmt.Errorf("unable to get experiment status from store: %w", err)
	}

	var status v1.ExperimentStatus

	err = mapstructure.Decode(c.Status, &status)
	if err != nil {
		return nil, fmt.Errorf("unable to decode experiment status: %w", err)
	}

	return &status, nil
}

func Running(name string) bool {
	var err error
	c, _ := store.NewConfig("experiment/" + name)

	err = store.Get(c)
	if err != nil {
		return false
	}

	exp, err2 := types.DecodeExperimentFromConfig(*c)
	if err2 != nil {
		return false
	}

	return exp.Running()
}

func Save(opts ...SaveOption) error {
	o := newSaveOptions(opts...)

	if o.name == "" {
		return errors.New("experiment name required")
	}

	c, _ := store.NewConfig("experiment/" + o.name)

	err := store.Get(c)
	if err != nil {
		return fmt.Errorf("getting experiment %s from store: %w", o.name, err)
	}

	if o.spec == nil {
		if o.saveNilSpec {
			c.Spec = nil
		}
	} else {
		c.Spec = structs.MapDefaultCase(o.spec, structs.CASESNAKE)
	}

	if o.status == nil {
		if o.saveNilStatus {
			c.Status = nil
		}
	} else {
		c.Status = structs.MapDefaultCase(o.status, structs.CASESNAKE)
	}

	err = store.Update(c)
	if err != nil {
		return fmt.Errorf("saving experiment config: %w", err)
	}

	return nil
}

// Reconfigure executes the 'configure' stage for all apps the given experiment
// is configured to use. It returns any errors encountered while reconfiguring
// the experiment.
//
// Reconfigure is intended to be called after an experiment's stored topology,
// scenario, or deployment settings have changed (e.g. via the config or
// workflow APIs), not as a general on-demand debugging tool. Unlike
// Trigger(ctx, name, app.ActionConfigure), it: refuses to run against a
// running experiment; persists the experiment via config.Update so config
// validation and any registered config hooks run; and always applies to every
// app, without support for filtering to specific apps. Use Trigger for
// manually re-running a single app's (or apps') configure hook on demand, e.g.
// for debugging.
//
// Reconfigure sends nothing to minimega: the experiment is stopped, so its
// namespace is cleared, and any command sent to it would create it again.
func Reconfigure(name string) error {
	var err error
	c, _ := store.NewConfig("experiment/" + name)

	err = store.Get(c)
	if err != nil {
		return fmt.Errorf("getting experiment %s from store: %w", name, err)
	}

	exp, err2 := types.DecodeExperimentFromConfig(*c)
	if err2 != nil {
		return fmt.Errorf("decoding experiment from config: %w", err2)
	}

	if exp.Running() {
		return errors.New("experiment is running")
	}

	err = app.ApplyApps(context.Background(), exp, app.Stage(app.ActionConfigure))
	if err != nil {
		return fmt.Errorf("configuring apps for experiment: %w", err)
	}

	c.Spec = structs.MapDefaultCase(exp.Spec, structs.CASESNAKE)

	err = config.Update(c.FullName(), c)
	if err != nil {
		return fmt.Errorf("updating experiment config: %w", err)
	}

	return nil
}

// Trigger executes the given lifecycle stage for the selected apps in an
// experiment. If no apps are passed, the stage is triggered for all apps.
//
// Trigger is intended for manually invoking a lifecycle stage for a specific
// app (or apps) on demand, e.g. for debugging. For the 'configure' stage in
// particular, it is not a replacement for Reconfigure: Trigger doesn't
// validate the experiment config, run config hooks, or refuse to run against
// a running experiment. Use Reconfigure after an experiment's stored
// topology, scenario, or deployment settings have changed.
func Trigger(ctx context.Context, name string, stage app.Action, apps ...string) error {
	if !stage.Valid() {
		return fmt.Errorf("invalid app lifecycle stage %q", stage)
	}

	c, err := store.NewConfig("experiment/" + name)
	if err != nil {
		return fmt.Errorf("creating experiment config for %s: %w", name, err)
	}

	err = store.Get(c)
	if err != nil {
		return fmt.Errorf("getting experiment %s from store: %w", name, err)
	}

	exp, err2 := types.DecodeExperimentFromConfig(*c)
	if err2 != nil {
		return fmt.Errorf("decoding experiment from config: %w", err2)
	}

	if stage == app.ActionRunning && !exp.Running() {
		return errors.New("experiment is not running")
	}

	if err := validateTriggerApps(exp, stage, apps); err != nil {
		return err
	}

	err = app.ApplyApps(
		ctx,
		exp,
		app.Stage(stage),
		app.FilterApp(apps...),
		app.Trigger(),
		app.DryRun(exp.DryRun()),
	)
	if err != nil {
		return fmt.Errorf("triggering %s stage for experiment apps: %w", stage, err)
	}

	// Running-stage apps persist their status as they execute and reload the
	// experiment between apps.
	if stage == app.ActionRunning {
		return nil
	}

	// The apps stored their own status changes as they ran; writing back the
	// status read before the stage would lose what others stored meanwhile,
	// such as the end of a SCORCH run.
	if err := exp.WriteSpecToStore(); err != nil {
		return fmt.Errorf("saving experiment after triggering %s stage: %w", stage, err)
	}

	return nil
}

// TriggerRunning executes the 'running' stage for the given apps in the given
// experiment. If no apps are passed, then all experiment apps will have their
// 'running' stage triggered.
func TriggerRunning(ctx context.Context, name string, apps ...string) error {
	return Trigger(ctx, name, app.ActionRunning, apps...)
}

// validateTriggerApps ensures the given apps are part of the experiment and
// that the requested lifecycle stage is applicable to each of them. If no
// apps are given (meaning "all apps"), it instead ensures at least one app in
// the experiment supports the requested stage.
func validateTriggerApps(exp *types.Experiment, stage app.Action, apps []string) error {
	if len(apps) == 0 {
		for _, name := range app.ExperimentApps(exp) {
			if app.StageApplicable(stage, name) {
				return nil
			}
		}

		return fmt.Errorf(
			"the %s stage is not applicable for any app in experiment %s",
			stage,
			exp.Metadata.Name,
		)
	}

	known := make(map[string]struct{})
	for _, name := range app.ExperimentApps(exp) {
		known[name] = struct{}{}
	}

	for _, name := range apps {
		if _, ok := known[name]; !ok {
			return fmt.Errorf(
				"app %s is not part of experiment %s",
				name,
				exp.Metadata.Name,
			)
		}

		if !app.StageApplicable(stage, name) {
			return fmt.Errorf(
				"the %s stage is not applicable for app %s",
				stage,
				name,
			)
		}
	}

	return nil
}

func Delete(name string) error {
	if Running(name) {
		return errors.New("cannot delete a running experiment")
	}

	c, err := config.Get("experiment/" + name)
	if err != nil {
		return fmt.Errorf("getting experiment %s: %w", name, err)
	}

	err = config.Delete("experiment/" + name)
	if err != nil {
		return fmt.Errorf("deleting experiment %s: %w", name, err)
	}

	exp, err2 := types.DecodeExperimentFromConfig(*c)
	if err2 != nil {
		return fmt.Errorf("decoding experiment from config: %w", err2)
	}

	var errors error

	// Delete any snapshot files created by this headnode for this experiment
	// after deleting the experiment.
	err = deleteC2AndSnapshots(exp)
	if err != nil {
		errors = multierror.Append(
			errors,
			fmt.Errorf("deleting experiment snapshots and CC responses: %w", err),
		)
	}

	err = os.RemoveAll(exp.Spec.BaseDir())
	if err != nil {
		errors = multierror.Append(
			errors,
			fmt.Errorf("deleting experiment base directory: %w", err),
		)
	}

	for _, hook := range hooks["delete"] {
		hook("delete", name)
	}

	return errors //nolint:wrapcheck // returning multierror
}

// Captures returns the packet captures minimega runs in the given experiment.
// A stopped or dry-run experiment has none, so minimega isn't asked: it would
// create the experiment's cleared namespace again.
func Captures(name string) ([]mm.Capture, error) {
	exp, err := Get(name)
	if err != nil {
		return nil, err
	}

	if !exp.Running() || exp.DryRun() {
		return nil, nil
	}

	return mm.GetExperimentCaptures(mm.NS(name)), nil
}

func Files(name, filter string) (file.Files, error) {
	return file.GetExperimentFiles(name, filter) //nolint:wrapcheck // passthrough
}

func File(name, filePath string) ([]byte, error) {
	files, err := file.GetExperimentFiles(name, "")
	if err != nil {
		return nil, fmt.Errorf("getting list of experiment files: %w", err)
	}

	if filePath == "" {
		return nil, errors.New("no file path provided")
	}

	captures, err := Captures(name)
	if err != nil {
		return nil, fmt.Errorf("getting experiment captures: %w", err)
	}

	if captureInProgress(captures, filePath) {
		return nil, mm.ErrCaptureExists
	}

	for _, f := range files {
		if filePath == f.Path {
			headnode, _ := os.Hostname()

			_ = file.CopyFile(fmt.Sprintf("/%s/files/%s", name, f.Path), headnode, nil)

			path := fmt.Sprintf("%s/images/%s/files/%s", common.PhenixBase, name, f.Path)

			var data []byte
			data, err = os.ReadFile(path)
			if err != nil {
				return nil, fmt.Errorf("reading contents of file: %w", err)
			}

			return data, nil
		}
	}

	return nil, errors.New("file not found")
}

func deleteC2AndSnapshots(exp *types.Experiment) error {
	expName := exp.Metadata.Name
	c2Path := expName + "/miniccc_responses"

	err := file.DeleteFile(c2Path)
	if err != nil {
		return fmt.Errorf("deleting CC responses for experiment %s: %w", expName, err)
	}

	return deleteSnapshots(exp)
}

func deleteSnapshots(exp *types.Experiment) error {
	// Snapshot naming convention is as follows:
	//   {hostname}_{experiment_name}_{vm_name}_snapshot
	// Now, we *could* use {hostname}_{experiment_name}_*_snapshot as the deletion
	// filter to delete all the VMs for a given experiment on a given headnode,
	// but... if another experiment has a similar name but with an underscore in
	// it, we may accidentally end up deleting those snapshots as well. For
	// example, lets say we have two experiments in phenix, one named "foo" and
	// the other named "foo_bar". Using the deletion filter above, if we went to
	// delete snapshots for experiment "foo" we would also delete the snapshots
	// for experiment "foo_bar". As such, the safest bet is to loop through all
	// the VMs in an experiment and delete the snapshots one by one. Note that by
	// including the hostname we ensure that only snapshots created for
	// experiments by this headnode get deleted. This is important when multiple
	// headnodes exist for a single minimega cluster.
	var (
		expName  = exp.Metadata.Name
		headnode = mm.Headnode()
	)

	for _, node := range exp.Spec.Topology().Nodes() {
		if node.External() {
			continue
		}

		hostname := node.General().Hostname()
		snapshot := fmt.Sprintf("%s_%s_%s_snapshot", headnode, expName, hostname)

		err := file.DeleteFile(snapshot)
		if err != nil {
			return fmt.Errorf(
				"deleting snapshot file for VM %s in experiment %s: %w",
				hostname,
				expName,
				err,
			)
		}
	}

	return nil
}

// handleDelayedVMs starts the delayed VMs of the experiment ns, which was
// started at start, once their delays pass.
func handleDelayedVMs(
	ctx context.Context,
	ns, start string,
	delays map[string]time.Duration,
	c2s map[string]map[string]bool,
) error {
	if len(delays) == 0 && len(c2s) == 0 {
		return nil
	}

	notes.AddInfo(ctx, true, "Waiting for delayed VMs to be started...")

	var (
		wg      sync.WaitGroup
		errChan = make(chan error, len(delays)+len(c2s))
	)

	for host, delay := range delays {
		wg.Add(1)

		go func(host string, delay time.Duration) {
			defer wg.Done()
			if err := waitForTimeDelay(ctx, ns, start, host, delay); err != nil {
				errChan <- err
			}
		}(host, delay)
	}

	for host, others := range c2s {
		wg.Add(1)

		go func(host string, others map[string]bool) {
			defer wg.Done()
			if err := waitForC2(ctx, ns, start, host, others); err != nil {
				errChan <- err
			}
		}(host, others)
	}

	wg.Wait()
	close(errChan)

	var errs error
	for err := range errChan {
		errs = multierror.Append(errs, err)
	}

	// The namespace is left alone. A failed delayed start is torn down by the
	// caller's Stop, which runs the cleanup apps while the namespace still
	// exists and then clears it; clearing it here first would run those apps
	// against a cleared namespace, and any that asked minimega would create it
	// again. A canceled start is stopped by whoever canceled it (e.g. a user
	// who stopped the experiment while delayed VMs were still pending), and
	// clearing here too would race with that stop and can delete resources
	// (such as taps created by the tap app) out from under its cleanup. A stop
	// from another process already tore it down, and the experiment may have
	// run again since.
	return errs //nolint:wrapcheck // returning multierror
}

func waitForTimeDelay(ctx context.Context, ns, start, host string, delay time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(delay):
		if !stillStarted(ns, start) {
			return stoppedWhileStarting(ns)
		}

		cmd := mmcli.NewNamespacedCommand(ns)
		cmd.Command = "vm start " + host

		err := mmcli.ErrorResponse(mmcli.Run(cmd))
		if err != nil {
			return NewDelayedVMError(host, err, "starting VM %s", host)
		}

		notes.AddInfo(ctx, true, fmt.Sprintf("Time delayed VM %s started", host))
		pubsub.Publish("delayed-start", fmt.Sprintf("%s/%s", ns, host))
	}

	return nil
}

func waitForC2(
	ctx context.Context,
	ns, start, host string,
	others map[string]bool,
) error {
	ticker := time.NewTicker(c2CheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if !stillStarted(ns, start) {
				return stoppedWhileStarting(ns)
			}

			done := true

			for other, useUUID := range others {
				opts := []mm.C2Option{
					mm.C2Context(ctx),
					mm.C2NS(ns),
					mm.C2VM(other),
					mm.C2Timeout(1 * time.Second),
				}

				if useUUID {
					opts = append(opts, mm.C2IDClientsByUUID())
				}

				if mm.IsC2ClientActive(opts...) != nil {
					done = false

					break
				}
			}

			if done {
				cmd := mmcli.NewNamespacedCommand(ns)
				cmd.Command = "vm start " + host

				err := mmcli.ErrorResponse(mmcli.Run(cmd))
				if err != nil {
					return NewDelayedVMError(host, err, "starting VM %s", host)
				}

				notes.AddInfo(ctx, true, fmt.Sprintf("C2 delayed VM %s started", host))
				pubsub.Publish("delayed-start", fmt.Sprintf("%s/%s", ns, host))

				return nil
			}
		}
	}
}

// errStoppedWhileStarting reports that the experiment was stopped while its
// start was still waiting for delayed VMs.
var errStoppedWhileStarting = errors.New("stopped while it was starting")

// stoppedWhileStarting returns the error a start reports when the experiment
// was stopped while it was starting.
func stoppedWhileStarting(name string) error {
	return fmt.Errorf("experiment %s was %w", name, errStoppedWhileStarting)
}

// stillStarted reports whether the named experiment is still stored with the
// given start time, which a stop clears and a later start replaces. A stop from
// another process, such as phenix experiment stop while phenix ui starts the
// experiment, cannot cancel this process's start, and once the namespace is
// cleared a C2 check or VM start would make minimega create it again. If the
// store can't tell, it reports true.
func stillStarted(name, start string) bool {
	exp, err := Get(name)
	if err != nil {
		if errors.Is(err, store.ErrNotExist) {
			return false
		}

		plog.Warn(plog.TypeSystem, "reading experiment while its delayed VMs wait", "exp", name, "err", err)

		return true
	}

	return exp.Status != nil && exp.Status.StartTime() == start
}
