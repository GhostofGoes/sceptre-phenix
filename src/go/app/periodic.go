package app

import (
	"context"
	"errors"
	"time"

	"phenix/store"
	"phenix/types"
	ifaces "phenix/types/interfaces"
	"phenix/util/plog"
	"phenix/util/pubsub"
)

// stopPollInterval is how often a periodic app with a longer period checks
// whether its experiment run ended, so that it ends, and with it phenix
// experiment start --honor-run-periodically, soon after a stop from another
// process instead of at its next scheduled run.
var stopPollInterval = 5 * time.Second //nolint:gochecknoglobals // tests shorten it

// minPeriod is the shortest runPeriodically period phenix accepts. A shorter
// one, such as 1ms typed for 1m, would read and write the experiment and run
// the app, which may start a process, almost without a pause.
var minPeriod = time.Second //nolint:gochecknoglobals // tests shorten it

// periodicRun is the running stage of an app that runs periodically for the
// experiment run that started at start.
type periodicRun struct {
	name, start, app, frequency string
}

// errRunEnded reports that the experiment run a periodic app was scheduled for
// has ended.
var errRunEnded = errors.New("experiment run ended")

// current reads the experiment from the store, so that each tick runs the app
// on a fresh copy. It returns errRunEnded once the experiment was deleted,
// stopped or started again: a stop from another process, such as phenix
// experiment stop while phenix ui runs the app, cannot cancel the app's
// context.
func (r periodicRun) current() (*types.Experiment, error) {
	if r.start == "" {
		return nil, errRunEnded
	}

	exp := &types.Experiment{Metadata: store.ConfigMetadata{Name: r.name}} //nolint:exhaustruct // Reload reads the rest

	err := exp.Reload()
	if errors.Is(err, store.ErrNotExist) || (err == nil && exp.Status.StartTime() != r.start) {
		return nil, errRunEnded
	}

	if err != nil {
		return nil, err
	}

	return exp, nil
}

// update applies change to the stored status of exp's run (see
// types.Experiment.UpdateStatus), which keeps what other apps stored
// meanwhile, and reports whether the run goes on.
func (r periodicRun) update(exp *types.Experiment, change func(ifaces.ExperimentStatus) error) bool {
	err := exp.UpdateStatus(change)
	if errors.Is(err, types.ErrExperimentRunChanged) || errors.Is(err, store.ErrNotExist) {
		return false
	}

	if err != nil {
		plog.Error(plog.TypePhenixApp, "[✗] error updating store with experiment", "exp", r.name, "err", err)
	}

	return true
}

// setSchedule returns the status change that records the app's frequency,
// or that it no longer runs periodically when frequency is empty.
func (r periodicRun) setSchedule(frequency string) func(ifaces.ExperimentStatus) error {
	return func(status ifaces.ExperimentStatus) error {
		status.SetAppFrequency(r.app, frequency)
		status.SetAppRunning(r.app, false)

		return nil
	}
}

// runPeriodically runs the app's running stage every duration until ctx is
// canceled or the experiment run ends.
//
//nolint:funlen // the app's schedule and its status updates
func runPeriodically(ctx context.Context, r periodicRun, duration time.Duration) {
	ended := func() {
		plog.Info(
			plog.TypePhenixApp,
			"[✓] experiment run ended, no longer running app periodically",
			"exp", r.name,
			"app", r.app,
		)
	}

	exp, err := r.current()

	switch {
	case errors.Is(err, errRunEnded):
		ended()

		return
	case err != nil:
		// The ticks read the experiment again.
		plog.Error(plog.TypePhenixApp, "[✗] error reading experiment", "exp", r.name, "err", err)
	default:
		if !r.update(exp, r.setSchedule(r.frequency)) {
			ended()

			return
		}
	}

	timer := time.NewTimer(duration)
	defer timer.Stop()

	var poll <-chan time.Time // nil, which never fires, unless the period is longer

	if duration > stopPollInterval {
		ticker := time.NewTicker(stopPollInterval)
		defer ticker.Stop()

		poll = ticker.C
	}

	for {
		select {
		case <-ctx.Done():
			if exp, err := r.current(); err == nil {
				r.update(exp, r.setSchedule(""))
			}

			return
		case <-poll:
			if _, err := r.current(); errors.Is(err, errRunEnded) {
				ended()

				return
			}
		case <-timer.C:
			exp, err := r.current()
			if errors.Is(err, errRunEnded) {
				ended()

				return
			}

			if err != nil {
				plog.Error(plog.TypePhenixApp, "[✗] error reading experiment", "exp", r.name, "err", err)
				timer.Reset(duration)

				continue
			}

			// The app may have been triggered by hand since the last run; the
			// check and the mark are one store update, so the trigger and the tick
			// cannot both run the app.
			err = exp.UpdateStatus(MarkRunningStage(r.app))

			switch {
			case errors.Is(err, ErrAppAlreadyRunning):
				plog.Info(
					plog.TypePhenixApp,
					"[✓] app is currently already executing its running stage -- skipping",
					"app",
					r.app,
				)

				timer.Reset(duration)

				continue
			case errors.Is(err, types.ErrExperimentRunChanged), errors.Is(err, store.ErrNotExist):
				ended()

				return
			case err != nil:
				plog.Error(plog.TypePhenixApp, "[✗] error updating store with experiment", "exp", r.name, "err", err)
			}

			a := GetApp(r.app)
			_ = a.Init(Name(r.app), DryRun(exp.DryRun()))

			pubsub.Publish("trigger-app", TriggerPublication{ //nolint:exhaustruct // partial initialization
				Experiment: exp.Spec.ExperimentName(),
				App:        r.app,
				State:      "start",
			})

			before, hadStatus := appStatusSnapshot(exp, r.app)

			err = a.Running(ctx, exp)
			if err != nil {
				pubsub.Publish("trigger-app", TriggerPublication{ //nolint:exhaustruct // partial initialization
					Experiment: exp.Spec.ExperimentName(),
					App:        r.app,
					State:      "error",
					Error:      err,
				})

				plog.Error(
					plog.TypePhenixApp,
					"[✗] error periodically running app",
					"app",
					r.app,
					"err",
					err,
				)
			} else {
				pubsub.Publish("trigger-app", TriggerPublication{ //nolint:exhaustruct // partial initialization
					Experiment: exp.Spec.ExperimentName(),
					App:        r.app,
					State:      "success",
				})
			}

			if !r.update(exp, runningStageDone(exp, r.app, before, hadStatus, err)) {
				ended()

				return
			}

			timer.Reset(duration)
		}
	}
}
