package scorchexe

import (
	"context"
	"errors"
	"fmt"

	"github.com/hashicorp/go-multierror"

	"phenix/api/scorch/scorchmd"
	"phenix/app"
	"phenix/types"
	ifaces "phenix/types/interfaces"
)

// ErrAlreadyRunning is the refusal of a run while a SCORCH run, or the scorch
// app's running stage, executes for the experiment.
var ErrAlreadyRunning = errors.New("the Scorch app is currently already running")

// Check returns why Execute would refuse to start the given run of the
// experiment now: the experiment has no scorch app or no such run, the run
// names a component the app does not define, the experiment is not running,
// or it already executes a SCORCH run (ErrAlreadyRunning).
func Check(exp *types.Experiment, run int) error {
	var config ifaces.ScenarioApp

	for _, app := range exp.Apps() {
		if app.Name() == "scorch" {
			config = app

			break
		}
	}

	if config == nil {
		return fmt.Errorf("experiment %s doesn't include a Scorch configuration", exp.Metadata.Name)
	}

	if !exp.Running() {
		return fmt.Errorf("experiment %s is not running", exp.Metadata.Name)
	}

	if running := exp.Status.AppRunning()["scorch"]; running {
		return ErrAlreadyRunning
	}

	md, err := scorchmd.DecodeMetadata(exp)
	if err != nil {
		return fmt.Errorf("decoding Scorch configuration of experiment %s: %w", exp.Metadata.Name, err)
	}

	if run < 0 || run >= len(md.Runs) {
		return fmt.Errorf("invalid Scorch run ID for experiment %s", exp.Metadata.Name)
	}

	// Experiments created before Configure validated the runs can still name
	// undefined components, which Running refuses.
	return md.ValidateRun(run)
}

func Execute(ctx context.Context, exp *types.Experiment, run int) error {
	if err := Check(exp, run); err != nil {
		return err
	}

	// Check read exp's copy of the running flag; the flag is checked again,
	// and set, in one store update, so a run started from another process
	// since exp was read, or meanwhile, is refused. Running records the run
	// in status.apps.scorch and clears it.
	err := exp.UpdateStatus(app.MarkRunningStage("scorch"))
	if errors.Is(err, app.ErrAppAlreadyRunning) {
		return ErrAlreadyRunning
	}

	if err != nil {
		return fmt.Errorf("error updating store with experiment %s: %w", exp.Metadata.Name, err)
	}

	scorch := app.GetApp("scorch")
	_ = scorch.Init()

	var errors error

	ctx = SetRunID(ctx, run)

	err = scorch.Running(ctx, exp)
	if err != nil {
		errors = multierror.Append(
			errors,
			fmt.Errorf("running Scorch for experiment %s: %w", exp.Metadata.Name, err),
		)
	}

	// Only the flag is stored: components such as soh store the experiment's
	// status during the run, and a run of an experiment that was stopped, or
	// started again, meanwhile must not write to the new run.
	err = exp.UpdateStatus(func(status ifaces.ExperimentStatus) error {
		status.SetAppRunning("scorch", false)

		return nil
	})
	if err != nil {
		errors = multierror.Append(
			errors,
			fmt.Errorf("error updating store with experiment %s: %w", exp.Metadata.Name, err),
		)
	}

	return errors
}
