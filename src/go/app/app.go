package app

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"phenix/store"
	"phenix/types"
	ifaces "phenix/types/interfaces"
	"phenix/util"
	"phenix/util/notes"
	"phenix/util/plog"
	"phenix/util/pubsub"
	"phenix/util/shell"
)

// Action represents the different experiment lifecycle hooks.
type Action string

// AppFactory is a function that returns a new app struct.
type AppFactory func() App

// CreateTunnelTopic is the pubsub topic the startup app publishes CreateTunnel
// requests to. Only phenix ui subscribes to it.
const CreateTunnelTopic = "create-tunnel"

type CreateTunnel struct {
	Experiment string
	VM         string
	Sport      string
	Dhost      string
	Dport      string
	User       string
}

type TriggerPublication struct {
	Experiment string
	Verb       string
	App        string
	Resource   string
	State      string
	Error      error
}

const (
	ActionConfigure Action = "configure"
	ActionPreStart  Action = "pre-start"
	ActionPostStart Action = "post-start"
	ActionRunning   Action = "running"
	ActionCleanup   Action = "cleanup"

	defaultAppsAnnotation = "phenix/default-apps"
	osWindows             = "windows"
	osLinux               = "linux"
	osMinirouter          = "minirouter"
)

// Actions returns the supported experiment app lifecycle actions.
func Actions() []Action {
	return []Action{
		ActionConfigure,
		ActionPreStart,
		ActionPostStart,
		ActionRunning,
		ActionCleanup,
	}
}

// ParseAction parses a lifecycle action or its shorthand.
func ParseAction(value string) (Action, bool) {
	switch value {
	case string(ActionConfigure), "config":
		return ActionConfigure, true
	case string(ActionPreStart), "pre":
		return ActionPreStart, true
	case string(ActionPostStart), "post":
		return ActionPostStart, true
	case string(ActionRunning), "run":
		return ActionRunning, true
	case string(ActionCleanup), "clean":
		return ActionCleanup, true
	default:
		return "", false
	}
}

// Shorthand returns the shorthand for the action.
func (a Action) Shorthand() string {
	switch a {
	case ActionConfigure:
		return "config"
	case ActionPreStart:
		return "pre"
	case ActionPostStart:
		return "post"
	case ActionRunning:
		return "run"
	case ActionCleanup:
		return "clean"
	default:
		return ""
	}
}

// Valid reports whether the action is a supported experiment app lifecycle action.
func (a Action) Valid() bool {
	switch a {
	case ActionConfigure, ActionPreStart, ActionPostStart, ActionRunning, ActionCleanup:
		return true
	default:
		return false
	}
}

var (
	apps = make(map[string]AppFactory) //nolint:gochecknoglobals // global registry

	defaultApps = map[string]struct{}{ //nolint:gochecknoglobals // global constant
		appNameNTP:     {},
		appNameSerial:  {},
		appNameStartup: {},
		"vrouter":      {},
	}
)

var ErrUserAppAlreadyRegistered = errors.New("user app already registered")

func init() { //nolint:gochecknoinits // app registration
	// Default apps (always run)
	apps[appNameNTP] = func() App { return new(NTP) }
	apps[appNameSerial] = func() App { return new(Serial) }
	apps[appNameStartup] = func() App { return new(Startup) }
	apps["vrouter"] = func() App { return new(Vrouter) }

	// External user apps
	apps["user-shell"] = func() App { return new(UserApp) }
}

func RegisterUserApp(name string, factory AppFactory) error {
	if _, ok := apps[name]; ok {
		return ErrUserAppAlreadyRegistered
	}

	apps[name] = factory

	return nil
}

// List returns a list of non-default phenix applications.
func List() []string {
	var names []string

	for name := range apps {
		// Don't include app that wraps external user apps.
		if name == "user-shell" {
			continue
		}

		// Don't include default apps in the list since they always get applied.
		if _, ok := defaultApps[name]; ok {
			continue
		}

		names = append(names, name)
	}

	names = append(names, shell.FindCommandsWithPrefix(UserAppPrefix)...)

	return names
}

// GetApp returns the phenix app with the given name. Preference is given to a
// user app with the given name to allow users to override internal apps.
func GetApp(name string) App { //nolint:ireturn // factory
	cmdName := UserAppPrefix + name

	// Default to shelling out to a user app with the given name so internal apps
	// can be overridden by users.
	if shell.CommandExists(cmdName) {
		return apps["user-shell"]()
	}

	app, ok := apps[name]
	if !ok {
		app = apps["user-shell"]
	}

	return app()
}

// DefaultApps returns the names of the default phenix apps in the order every
// lifecycle stage runs them: sorted by name, as the documentation lists them.
func DefaultApps() []string {
	return slices.Sorted(maps.Keys(defaultApps))
}

// ExperimentApps returns the names of all apps (default and scenario) that
// are part of the given experiment.
func ExperimentApps(exp *types.Experiment) []string {
	names := DefaultApps()

	if exp.Spec.Scenario() != nil {
		for _, a := range exp.Spec.Scenario().Apps() {
			names = append(names, a.Name())
		}
	}

	return names
}

// StageApplicable returns whether the given lifecycle stage is applicable for
// the named app. Default apps run automatically as part of the other
// lifecycle stages and don't support being triggered for the running stage.
func StageApplicable(stage Action, name string) bool {
	if stage == ActionRunning {
		if _, ok := defaultApps[name]; ok {
			return false
		}
	}

	return true
}

// defaultAppsEnabled reports whether the default apps apply to the node. An
// annotation defaultAppsSetting cannot read keeps them enabled; ApplyApps
// refuses it in the configure and pre-start stages.
func defaultAppsEnabled(node ifaces.NodeSpec) bool {
	enabled, err := defaultAppsSetting(node)

	return err != nil || enabled
}

// defaultAppsSetting reads the node's phenix/default-apps annotation: a
// boolean, a string [strconv.ParseBool] accepts in any case, such as "false" or
// "0", or a number, where 0 is false. An absent annotation means true.
func defaultAppsSetting(node ifaces.NodeSpec) (bool, error) {
	value, ok := node.GetAnnotation(defaultAppsAnnotation)
	if !ok {
		return true, nil
	}

	switch v := value.(type) {
	case bool:
		return v, nil
	case string:
		if enabled, err := strconv.ParseBool(strings.ToLower(strings.TrimSpace(v))); err == nil {
			return enabled, nil
		}
	case int:
		return v != 0, nil
	case int64:
		return v != 0, nil
	case uint64:
		return v != 0, nil
	case float64:
		return v != 0, nil
	}

	shown := fmt.Sprint(value)
	if v, ok := value.(string); ok {
		shown = strconv.Quote(v)
	}

	return true, fmt.Errorf(
		"node %s: annotation %s is %s, want true or false",
		node.General().Hostname(), defaultAppsAnnotation, shown,
	)
}

// checkDefaultAppsSettings returns an error naming every node whose
// phenix/default-apps annotation defaultAppsSetting cannot read.
func checkDefaultAppsSettings(exp *types.Experiment) error {
	var errs []error

	for _, node := range exp.Spec.Topology().Nodes() {
		if _, err := defaultAppsSetting(node); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

// App is the interface that identifies all the required functionality for a
// phenix app. Each experiment lifecycle hook function is passed a pointer to
// the experiment the app is being applied to, and the lifecycle hook function
// should modify the experiment as necessary. Not all lifecycle hook functions
// have to be implemented. If one (or more) isn't needed for an app, it should
// simply return nil.
type App interface {
	// Init is used to initialize a phenix app with options generic to all apps.
	Init(...Option) error

	// Name returns the name of the phenix app.
	Name() string

	// Configure is called for an app at the `configure` experiment lifecycle
	// phase.
	Configure(context.Context, *types.Experiment) error

	// Start is called for an app at the `pre-start` experiment lifecycle phase.
	PreStart(context.Context, *types.Experiment) error

	// PostStart is called for an app at the `post-start` experiment lifecycle
	// phase.
	PostStart(context.Context, *types.Experiment) error

	// Running can be called for an app while an experiment is running. It will
	// only be called on-demand by a user or the web UI. The code that implements
	// this function should always be idempotent.
	Running(context.Context, *types.Experiment) error

	// Cleanup is called for an app at the `cleanup` experiment lifecycle
	// phase.
	Cleanup(context.Context, *types.Experiment) error
}

// ApplyApps applies all the default phenix apps and any configured user apps to
// the given experiment for the given lifecycle phase. It returns any errors
// encountered while applying the apps.
//
//nolint:cyclop,funlen,gocyclo,maintidx // complex lifecycle dispatch logic
func ApplyApps(ctx context.Context, exp *types.Experiment, opts ...Option) error {
	var (
		options = NewOptions(opts...)
		err     error
	)

	if options.Stage == ActionPreStart && !options.Trigger {
		// Reset status.apps for experiment. Note that this will get rid of any app
		// status from previous experiment deployments. We do this in the pre-start
		// stage instead of the post-start stage to ensure there's no lingering app
		// status from previous deployments if we have to wait for the post-start
		// stage to run due to delayed start VMs. The state-of-health app was the
		// main driver for this.
		exp.Status.ResetAppStatus()
	}

	// Publish triggered app events so web broker can propagate the publish out to
	// web clients. This was initially setup to help convey SOH status in the UI.
	publish := func(app, state string, err error) {
		pubsub.Publish("trigger-app", TriggerPublication{ //nolint:exhaustruct // partial initialization
			Experiment: exp.Metadata.Name,
			App:        app,
			State:      state,
			Error:      err,
		})
	}

	matchesFilter := func(name string) bool {
		if len(options.Filter) == 0 {
			return true
		}

		_, ok := options.Filter[name]
		return ok
	}

	// setCleanupPending records whether the named app's cleanup stage is
	// pending. A triggered stage also stores it at once, since its caller
	// stores only the spec afterwards (see experiment.Trigger), so that what
	// others store during the stage is kept.
	setCleanupPending := func(name string, pending bool) {
		exp.Status.SetAppCleanup(name, pending)

		if options.Trigger {
			_ = updateStoredStatus(exp, func(status ifaces.ExperimentStatus) error {
				status.SetAppCleanup(name, pending)

				return nil
			})
		}
	}

	queueCleanup := func(name string) {
		if !options.Trigger {
			return
		}

		switch options.Stage {
		case ActionConfigure, ActionPreStart, ActionPostStart:
			setCleanupPending(name, true)
		case ActionRunning, ActionCleanup:
			// Running stage: apps handle cleanup at experiment stop time.
			// Cleanup stage: the cleanup queue is consumed, not added to.
		}
	}

	// Refuse an annotation the default apps would otherwise read as enabled
	// when the experiment is created, configured or started.
	if options.Stage == ActionConfigure || options.Stage == ActionPreStart {
		if err := checkDefaultAppsSettings(exp); err != nil {
			return fmt.Errorf("applying default apps for action %s: %w", options.Stage, err)
		}
	}

	for _, name := range DefaultApps() {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		if !matchesFilter(name) {
			continue
		}

		// A stop from another process cannot cancel ctx; the scenario apps
		// notice it as they mark themselves running (see runMarked).
		if options.Stage == ActionPostStart && !options.Trigger {
			if err := checkRunUnchanged(exp); err != nil {
				return fmt.Errorf("applying default app %s for action %s: %w", name, options.Stage, err)
			}
		}

		a := GetApp(name)
		_ = a.Init(Name(name), DryRun(options.DryRun))

		queueCleanup(a.Name())
		publish(a.Name(), "start", nil)

		switch options.Stage {
		case ActionConfigure:
			err = a.Configure(ctx, exp)
		case ActionPreStart:
			err = a.PreStart(ctx, exp)
		case ActionPostStart:
			err = a.PostStart(ctx, exp)
		case ActionRunning:
			continue // silently ignore running stage for default apps
		case ActionCleanup:
			err = a.Cleanup(ctx, exp)
		}

		if err != nil {
			publish(a.Name(), "error", err)

			plog.Error(
				plog.TypePhenixApp,
				fmt.Sprintf("[✗] '%s' default app (%s)", a.Name(), options.Stage),
			)

			return fmt.Errorf(
				"applying default app %s for action %s: %w",
				a.Name(),
				options.Stage,
				err,
			)
		}

		if options.Stage == ActionCleanup {
			setCleanupPending(a.Name(), false)
		}

		publish(a.Name(), "success", nil)

		plog.Info(
			plog.TypePhenixApp,
			fmt.Sprintf("[✓] '%s' default app (%s)", a.Name(), options.Stage),
		)
	}

	if exp.Spec.Scenario() != nil {
		for _, app := range exp.Spec.Scenario().Apps() {
			if ctx.Err() != nil {
				return ctx.Err()
			}

			// Don't apply default apps again if configured via the Scenario.
			if _, ok := defaultApps[app.Name()]; ok {
				continue
			}

			if !matchesFilter(app.Name()) {
				plog.Debug(
					plog.TypePhenixApp,
					fmt.Sprintf(
						"Skipping '%s' experiment app (%s)",
						app.Name(),
						options.Stage,
					),
				)

				continue
			}

			// Running-stage triggers historically include disabled apps. Explicit
			// filters provide the same override for other lifecycle stages.
			cleanupPending := options.Stage == ActionCleanup &&
				exp.Status.AppCleanup()[app.Name()]
			hasExplicitFilter := len(options.Filter) > 0
			shouldSkipDisabledApp := app.Disabled() && options.Stage != ActionRunning &&
				!hasExplicitFilter && !cleanupPending
			if shouldSkipDisabledApp {
				continue
			}

			a := GetApp(app.Name())
			_ = a.Init(Name(app.Name()), DryRun(options.DryRun))

			queueCleanup(a.Name())
			publish(a.Name(), "start", nil)

			switch options.Stage {
			case ActionConfigure:
				err = runMarked(ctx, exp, app.Name(), options.Trigger, a.Configure)
			case ActionPreStart:
				err = runMarked(ctx, exp, app.Name(), options.Trigger, a.PreStart)
			case ActionPostStart:
				err = runMarked(ctx, exp, app.Name(), options.Trigger, a.PostStart)
			case ActionRunning:
				// Check to make sure this app isn't already running via an automatic
				// periodic execution. The check and the mark are one store update, so
				// a periodic run cannot start between them.
				storeErr := exp.UpdateStatus(MarkRunningStage(app.Name()))
				if errors.Is(storeErr, ErrAppAlreadyRunning) {
					notes.AddInfo(
						ctx,
						false,
						fmt.Sprintf(
							"app %s is currently already executing its running stage -- skipping",
							app.Name(),
						),
					)

					continue
				}

				if errors.Is(storeErr, types.ErrExperimentRunChanged) {
					err = storeErr

					break
				}

				if storeErr != nil {
					notes.AddErrors(
						ctx,
						false,
						fmt.Errorf(
							"error updating store with experiment (%s): %w",
							exp.Spec.ExperimentName(),
							storeErr,
						),
					)
				}

				before, hadStatus := appStatusSnapshot(exp, app.Name())

				// Assign to the function-level err to carry out
				err = a.Running(ctx, exp)

				storeErr = exp.UpdateStatus(runningStageDone(exp, app.Name(), before, hadStatus, err))
				if storeErr != nil {
					notes.AddErrors(
						ctx,
						false,
						fmt.Errorf(
							"error updating store with experiment (%s): %w",
							exp.Spec.ExperimentName(),
							storeErr,
						),
					)
				}
			case ActionCleanup:
				err = runMarked(ctx, exp, app.Name(), options.Trigger, a.Cleanup)
			}

			if err != nil {
				publish(a.Name(), "error", err)

				if errors.Is(err, ErrUserAppNotFound) {
					plog.Warn(
						plog.TypePhenixApp,
						fmt.Sprintf("[?] '%s' user app (%s)", a.Name(), options.Stage),
					)

					continue
				}

				plog.Error(
					plog.TypePhenixApp,
					fmt.Sprintf("[✗] '%s' user app (%s)", a.Name(), options.Stage),
				)

				return fmt.Errorf(
					"applying user app %s for action %s: %w",
					a.Name(),
					options.Stage,
					err,
				)
			}

			if options.Stage == ActionCleanup {
				setCleanupPending(a.Name(), false)
			}

			publish(a.Name(), "success", nil)

			plog.Info(
				plog.TypePhenixApp,
				fmt.Sprintf("[✓] '%s' user app (%s)", a.Name(), options.Stage),
			)
		}
	}

	if options.Stage == ActionConfigure || options.Stage == ActionPreStart {
		// just in case one of the apps added some nodes to the topology...
		_ = exp.Spec.Topology().Init(exp.Spec.DefaultBridge())
	}

	return nil
}

// checkRunUnchanged returns an error wrapping types.ErrExperimentRunChanged
// when the stored experiment was stopped, or started again, since exp was
// read: a start's post-start apps must not run for a run that ended, as the
// commands they send to minimega would create the cleared namespace again.
// An experiment that cannot be read is left for the stage to report.
func checkRunUnchanged(exp *types.Experiment) error {
	c, _ := store.NewConfig("experiment/" + exp.Metadata.Name)

	if err := store.Get(c); err != nil {
		return nil //nolint:nilerr // only a changed run ends the stage
	}

	if stored, _ := c.Status["startTime"].(string); stored != exp.Status.StartTime() {
		return fmt.Errorf(
			"%w since it was read: stored start time %q, read %q",
			types.ErrExperimentRunChanged,
			stored,
			exp.Status.StartTime(),
		)
	}

	return nil
}

// runMarked runs stage, a stage other than running of the named app, with the
// app marked running in the experiment's stored status, so that the app's
// running stage, a periodic tick or a SCORCH run refuses to start meanwhile
// (see MarkRunningStage). It stores only the mark and the app's own status
// entry, each in one store update, so what others store during the stage,
// such as another app's status, is kept; exp's other changes are left for the
// stage's caller to store. A stage of a start or a stop clears the mark when
// it ends. A triggered stage leaves a mark it found alone instead: the app's
// running stage, such as a SCORCH run, holds it and clears it as it ends.
//
// The stage does not run, and the error wraps types.ErrExperimentRunChanged,
// when the experiment was stopped, or started again, since exp was read: the
// commands the app sends to minimega would create the cleared namespace again.
func runMarked(
	ctx context.Context,
	exp *types.Experiment,
	name string,
	trigger bool,
	stage func(context.Context, *types.Experiment) error,
) error {
	before, hadStatus := appStatusSnapshot(exp, name)

	// held reports whether the app's running stage held the mark when this
	// stage began.
	var held bool

	err := updateStoredStatus(exp, func(status ifaces.ExperimentStatus) error {
		held = trigger && status.AppRunning()[name]
		status.SetAppRunning(name, true)

		return nil
	})
	if err == nil {
		err = stage(ctx, exp)
	}

	after, hasStatus := exp.Status.AppStatus()[name]
	changed := hasStatus != hadStatus || !reflect.DeepEqual(before, after)
	running := held

	// A stop from another process during the stage refuses this update too;
	// the stop cleared what the stage would store.
	_ = updateStoredStatus(exp, func(status ifaces.ExperimentStatus) error {
		if changed {
			status.SetAppStatus(name, after)
		}

		if !held {
			status.SetAppRunning(name, false)
		}

		running = status.AppRunning()[name]

		return nil
	})

	exp.Status.SetAppRunning(name, running)

	return err
}

// updateStoredStatus applies change to the experiment's stored status in one
// store update (see types.Experiment.UpdateStatus) and, unlike UpdateStatus,
// leaves exp's status as it is: the caller of a stage, such as a start,
// stores exp's other changes itself. It returns only an error wrapping
// types.ErrExperimentRunChanged; an experiment being created is not stored
// yet, so it has no status to update.
func updateStoredStatus(exp *types.Experiment, change func(ifaces.ExperimentStatus) error) error {
	status := exp.Status

	err := exp.UpdateStatus(change)

	exp.Status = status

	if errors.Is(err, types.ErrExperimentRunChanged) {
		return err
	}

	return nil
}

// ErrAppAlreadyRunning reports that an app already executes its running
// stage.
var ErrAppAlreadyRunning = errors.New("app is already executing its running stage")

// MarkRunningStage returns the status change that marks the named app as
// executing its running stage, or that fails with ErrAppAlreadyRunning when it
// already is. Applied with types.Experiment.UpdateStatus, the check and the
// mark are one store update, so no other run can start between them.
func MarkRunningStage(name string) func(ifaces.ExperimentStatus) error {
	return func(status ifaces.ExperimentStatus) error {
		if status.AppRunning()[name] {
			return ErrAppAlreadyRunning
		}

		status.SetAppRunning(name, true)

		return nil
	}
}

// runningStageDone returns the status change that ends the named app's
// running stage on exp: it clears the app's running flag and stores the
// app's status entry in exp only when the stage, which failed with stageErr,
// changed it from before (see appStatusSnapshot). Applied with
// Experiment.UpdateStatus, it keeps what others stored during the stage, such
// as another app's status or running flag, and a status the app itself
// stored in the store meanwhile.
func runningStageDone(
	exp *types.Experiment,
	name string,
	before any,
	hadStatus bool,
	stageErr error,
) func(ifaces.ExperimentStatus) error {
	after, hasStatus := exp.Status.AppStatus()[name]
	changed := stageErr == nil && (hasStatus != hadStatus || !reflect.DeepEqual(before, after))

	return func(status ifaces.ExperimentStatus) error {
		if changed {
			status.SetAppStatus(name, after)
		}

		status.SetAppRunning(name, false)

		return nil
	}
}

// appStatusSnapshot returns a copy of the status.apps entry of the named app,
// and whether it has one. Map values are copied deeply, so an app that changes
// its entry in place still compares as changed.
func appStatusSnapshot(exp *types.Experiment, name string) (any, bool) {
	status, ok := exp.Status.AppStatus()[name]

	switch v := status.(type) {
	case map[string]any:
		return util.CopyableMap(v).DeepCopy(), ok
	case []any:
		return util.CopyableSlice(v).DeepCopy(), ok
	default:
		return status, ok
	}
}

// PeriodicallyRunApps checks the configuration for each app in the scenario to
// see if it's configured to have its "running" stage run periodically. A
// Goroutine is scheduled for each applicable app.
func PeriodicallyRunApps(ctx context.Context, wg *sync.WaitGroup, exp *types.Experiment) error {
	if exp.Spec.Scenario() != nil {
		for _, app := range exp.Spec.Scenario().Apps() {
			// Don't consider default apps as candidates for running periodically.
			if _, ok := defaultApps[app.Name()]; ok {
				continue
			}

			if app.RunPeriodically() != "" {
				// A copy of a stopped experiment, such as one read just after a stop
				// from another process, has no run for the app to follow.
				if !exp.Running() {
					return fmt.Errorf(
						"experiment %s is not running, so app %s cannot run periodically",
						exp.Metadata.Name,
						app.Name(),
					)
				}

				// A period shorter than minPeriod would run the app almost without a
				// pause.
				duration, err := time.ParseDuration(app.RunPeriodically())
				if err != nil || duration < minPeriod {
					plog.Error(
						plog.TypePhenixApp,
						"[✗] invalid periodic duration for app",
						"app",
						app.Name(),
						"duration",
						app.RunPeriodically(),
						"minimum",
						minPeriod.String(),
					)

					continue
				}

				plog.Info(
					plog.TypePhenixApp,
					"[✓] scheduling 'running' stage for app",
					"app",
					app.Name(),
					"duration",
					app.RunPeriodically(),
				)

				run := periodicRun{
					name:      exp.Metadata.Name,
					start:     exp.Status.StartTime(),
					app:       app.Name(),
					frequency: app.RunPeriodically(),
				}

				wg.Add(1)

				go func() {
					defer wg.Done()

					runPeriodically(ctx, run, duration)
				}()
			}
		}
	}

	return nil
}
