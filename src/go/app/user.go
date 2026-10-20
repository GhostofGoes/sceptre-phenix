package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"phenix/scheduler"
	"phenix/types"
	ifaces "phenix/types/interfaces"
	v1 "phenix/types/version/v1"
	"phenix/util"
	"phenix/util/common"
	"phenix/util/mm"
	"phenix/util/plog"
	"phenix/util/shell"
)

const (
	ExitSchedule int = 101
)

var (
	UserAppPrefix      = "phenix-app-" //nolint:gochecknoglobals // global constant
	ErrUserAppNotFound = errors.New("user app not found")
)

type UserApp struct {
	options Options
}

// PopulateRuntime adds the current minimega cluster hosts to an experiment
// and, if withVMs is true, the details of the VMs in its namespace. Pass false
// when the namespace cannot hold the experiment's VMs: minimega creates a
// namespace for any command sent to it, so asking would leave an empty
// namespace behind.
func PopulateRuntime(exp *types.Experiment, withVMs bool) error {
	cluster, err := mm.GetClusterHosts(true)
	if err != nil {
		return fmt.Errorf("getting cluster hosts: %w", err)
	}

	exp.Hosts = cluster
	exp.VMs = nil

	if withVMs {
		exp.VMs = mm.GetVMInfo(mm.NS(exp.Spec.ExperimentName()))
	}

	return nil
}

// stageHasVMs reports whether the experiment's VMs can exist in minimega
// during the given stage. They are launched after pre-start, and a dry run
// never launches them.
func stageHasVMs(action Action, dryRun bool) bool {
	if dryRun {
		return false
	}

	switch action {
	case ActionPostStart, ActionRunning, ActionCleanup:
		return true
	case ActionConfigure, ActionPreStart:
		return false
	}

	return false
}

func (u *UserApp) Init(opts ...Option) error {
	u.options = NewOptions(opts...)

	return nil
}

func (u UserApp) Name() string {
	return u.options.Name
}

func (u UserApp) Configure(ctx context.Context, exp *types.Experiment) error {
	err := u.shellOut(ctx, ActionConfigure, exp)
	if err != nil {
		return fmt.Errorf("running user app: %w", err)
	}

	return nil
}

func (u UserApp) PreStart(ctx context.Context, exp *types.Experiment) error {
	err := u.shellOut(ctx, ActionPreStart, exp)
	if err != nil {
		return fmt.Errorf("running user app: %w", err)
	}

	return nil
}

func (u UserApp) PostStart(ctx context.Context, exp *types.Experiment) error {
	err := u.shellOut(ctx, ActionPostStart, exp)
	if err != nil {
		return fmt.Errorf("running user app: %w", err)
	}

	return nil
}

func (u UserApp) Running(ctx context.Context, exp *types.Experiment) error {
	err := u.shellOut(ctx, ActionRunning, exp)
	if err != nil {
		return fmt.Errorf("running user app: %w", err)
	}

	return nil
}

func (u UserApp) Cleanup(ctx context.Context, exp *types.Experiment) error {
	err := u.shellOut(ctx, ActionCleanup, exp)
	if err != nil {
		return fmt.Errorf("running user app: %w", err)
	}

	return nil
}

func (u UserApp) shellOut(ctx context.Context, action Action, exp *types.Experiment) error {
	cmdName := UserAppPrefix + u.options.Name

	if !shell.CommandExists(cmdName) {
		return fmt.Errorf(
			"external user app %s does not exist in your path: %w",
			cmdName,
			ErrUserAppNotFound,
		)
	}

	// A stage triggered for a stopped experiment finds no VMs either, and
	// neither does the cleanup of a start refused after a stored dry run.
	hasVMs := stageHasVMs(action, u.options.DryRun) && exp.Running() && !exp.DryRun()
	if err := PopulateRuntime(exp, hasVMs); err != nil {
		return err
	}

	data, err := json.Marshal(exp)
	if err != nil {
		return fmt.Errorf("marshaling experiment to JSON: %w", err)
	}

	stderrChan := make(chan []byte)
	go plog.ProcessStderrLogs(
		stderrChan,
		plog.TypePhenixApp,
		"app",
		u.options.Name,
		"action",
		action,
		"exp",
		exp.Metadata.Name,
	)

	opts := []shell.Option{
		shell.Command(cmdName),
		shell.Args(string(action)),
		shell.Stdin(data),
		shell.SplitBytes(),
		shell.Env(
			"PHENIX_DIR="+common.PhenixBase,
			"PHENIX_FILES_DIR="+exp.FilesDir(),
			"PHENIX_LOG_LEVEL="+util.GetEnv("PHENIX_LOG_LEVEL", "DEBUG"),
			"PHENIX_LOG_FILE=stderr",
			"PHENIX_DRYRUN="+strconv.FormatBool(u.options.DryRun),
			"PHENIX_STORE_ENDPOINT="+common.StoreEndpoint,
		),
		shell.StreamStderr(stderrChan),
	}

	stdOut, _, err := shell.ExecCommand(ctx, opts...)
	if err != nil {
		var exitErr *exec.ExitError

		// The user app returned a non-zero exit status, so see if it matches any of
		// our special exit codes and handle accordingly.
		if errors.As(err, &exitErr) && exitErr.ExitCode() == ExitSchedule {
			sched := strings.TrimSpace(string(stdOut))

			err := scheduler.Schedule(sched, exp.Spec)
			if err != nil {
				return fmt.Errorf("scheduling experiment with %s: %w", sched, err)
			}

			return u.shellOut(ctx, action, exp)
		}

		return fmt.Errorf("user app %s command %s failed: %w", u.options.Name, cmdName, err)
	}

	// If we make it to this point, then the user app exited with a 0 exit code.
	// If the user app didn't make any modifications, then we don't require it to
	// output an experiment config. So, if there's nothing on STDOUT then just
	// return immediately without error.
	if len(stdOut) == 0 {
		return nil
	}

	result := types.NewExperiment(exp.Metadata)

	if err := json.Unmarshal(stdOut, &result); err != nil {
		return fmt.Errorf("unmarshaling experiment from JSON: %w", err)
	}

	return u.adoptResult(action, exp, result)
}

// adoptResult takes into exp what the app's stage may change of the
// experiment it returned.
func (u UserApp) adoptResult(action Action, exp, result *types.Experiment) error {
	switch action {
	case ActionConfigure, ActionPreStart:
		if err := checkInjections(result.Spec); err != nil {
			return fmt.Errorf("user app %s returned an experiment phenix refuses: %w", u.options.Name, err)
		}

		exp.SetSpec(result.Spec)
	case ActionPostStart, ActionRunning:
		if metadata, ok := result.Status.AppStatus()[u.options.Name]; ok {
			exp.Status.SetAppStatus(u.options.Name, metadata)
		}
	case ActionCleanup:
		// Nothing renders the spec of a stopping experiment, and failing here
		// would skip the cleanup of every app after this one; the next
		// configure or start refuses the spec instead.
		if err := checkInjections(result.Spec); err != nil {
			plog.Warn(
				plog.TypePhenixApp,
				"user app returned an experiment the next configure or start refuses",
				"app", u.options.Name, "exp", exp.Metadata.Name, "err", err,
			)
		}

		exp.SetSpec(result.Spec)

		if metadata, ok := result.Status.AppStatus()[u.options.Name]; ok {
			exp.Status.SetAppStatus(u.options.Name, metadata)
		}
	}

	return nil
}

// checkInjections refuses an experiment spec a user app returned when a node
// minimega launches has an injection, a deletion or an advanced setting
// phenix refuses, such as an injection that names no source, which the
// minimega script would render as the experiment's base directory. External
// nodes are never injected into or launched.
func checkInjections(spec ifaces.ExperimentSpec) error {
	if spec == nil {
		return nil
	}

	for _, node := range spec.Topology().Nodes() {
		if node.External() {
			continue
		}

		if err := v1.CheckInjections(node.Injections()); err != nil {
			return fmt.Errorf("node %s: %w", node.General().Hostname(), err)
		}

		if err := v1.CheckDeletions(node.Deletions()); err != nil {
			return fmt.Errorf("node %s: %w", node.General().Hostname(), err)
		}

		if err := v1.CheckAdvanced(node.Advanced()); err != nil {
			return fmt.Errorf("node %s: %w", node.General().Hostname(), err)
		}
	}

	return nil
}
