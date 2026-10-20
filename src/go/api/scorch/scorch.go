package scorch

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"phenix/api/config"
	"phenix/api/experiment"
	"phenix/store"
	"phenix/util/plog"
	"phenix/web/scorch"
)

// backgroundStopTimeout bounds how long a stop or cleanup stage waits for the
// backgrounded stage it ends to exit. A user component gets SIGTERM and is
// killed 10 seconds later (util/shell), so only a stage whose output an
// orphaned child process holds open takes longer.
const backgroundStopTimeout = 30 * time.Second

var errBackgroundStopTimeout = fmt.Errorf("it did not exit within %v of being stopped", backgroundStopTimeout)

// backgroundTask is a configure or start stage of a component that runs in
// the background until the component's cleanup or stop stage ends it, or the
// loop that started it ends.
type backgroundTask struct {
	stage   Action
	options Options // the options the stage was started with

	cancel context.CancelFunc
	done   chan struct{} // closed once the stage has returned
	err    error         // why the stage failed before it was canceled; set before done is closed
}

var (
	// backgroundedMu guards backgrounded, which the runs of different
	// experiments update concurrently in phenix ui.
	backgroundedMu sync.Mutex //nolint:gochecknoglobals // global lock

	// backgrounded holds the backgrounded stages by stage and name; a stage
	// that lists a component twice backgrounds it twice under one name, and
	// the component's stop or cleanup stage ends both.
	backgrounded = map[Action]map[string][]*backgroundTask{ //nolint:gochecknoglobals // global state
		ActionConfigure: make(map[string][]*backgroundTask),
		ActionStart:     make(map[string][]*backgroundTask),
		ActionStop:      make(map[string][]*backgroundTask),
		ActionCleanup:   make(map[string][]*backgroundTask),
		ActionDone:      make(map[string][]*backgroundTask),
		ActionLoop:      make(map[string][]*backgroundTask),
	}
)

// backgroundName names a backgrounded stage by its experiment, run, loop and
// count too, so the stop or cleanup stage of the same loop iteration finds
// it, and a nested loop that backgrounds the same component does not
// replace it.
func backgroundName(stage Action, options Options) string {
	return fmt.Sprintf(
		"%s/%d/%d/%d/%s/%s",
		options.Exp.Spec.ExperimentName(),
		options.Run,
		options.Loop,
		options.Count,
		stage,
		options.Name,
	)
}

// runInBackground runs the given stage of a component in a goroutine, under a
// context that the component's matching stop or cleanup stage cancels.
func runInBackground(
	ctx context.Context,
	stage Action,
	options Options,
	run func(context.Context) error,
) {
	ctx, cancel := context.WithCancel(ctx)

	task := &backgroundTask{
		stage:   stage,
		options: options,
		cancel:  cancel,
		done:    make(chan struct{}),
		err:     nil,
	}

	name := backgroundName(stage, options)

	backgroundedMu.Lock()
	backgrounded[stage][name] = append(backgrounded[stage][name], task)
	backgroundedMu.Unlock()

	go func() {
		defer close(task.done)

		err := run(ctx)

		// A stage that exits once it is canceled, by its stop or cleanup stage
		// or with its pipeline, did what it was asked; only a failure before
		// that is the component's.
		if err == nil || ctx.Err() != nil {
			return
		}

		task.err = err

		plog.Error(
			plog.TypeScorch,
			"backgrounded component stage failed",
			"exp", options.Exp.Spec.ExperimentName(),
			"component", options.Name,
			"stage", stage,
			"err", err,
		)

		task.updatePipeline(statusFailure)
	}()
}

// endTasks cancels the given stages and waits for them to exit, up to
// backgroundStopTimeout in all and while ctx lasts, so what the stages write
// as they stop is in place before the run goes on. It returns the errors the
// stages failed with before they were canceled.
func endTasks(ctx context.Context, tasks []*backgroundTask) error {
	for _, task := range tasks {
		task.cancel()
	}

	ctx, cancel := context.WithTimeoutCause(ctx, backgroundStopTimeout, errBackgroundStopTimeout)
	defer cancel()

	errs := make([]error, 0, len(tasks))

	for _, task := range tasks {
		errs = append(errs, task.wait(ctx))
	}

	return errors.Join(errs...)
}

// wait waits for the canceled stage to exit while ctx lasts. It returns the
// error the stage failed with before it was canceled.
func (t *backgroundTask) wait(ctx context.Context) error {
	select {
	case <-t.done:
	case <-ctx.Done():
	}

	var err error

	select {
	case <-t.done:
		if t.err != nil {
			err = fmt.Errorf(
				"backgrounded %s stage of component %s failed: %w",
				t.stage,
				t.options.Name,
				t.err,
			)
		}
	default:
		err = fmt.Errorf(
			"backgrounded %s stage of component %s: %w",
			t.stage,
			t.options.Name,
			context.Cause(ctx),
		)
	}

	if err != nil {
		t.updatePipeline(statusFailure)
	} else {
		t.updatePipeline(statusSuccess)
	}

	return err
}

func (t *backgroundTask) updatePipeline(status string) {
	update := scorch.ComponentUpdate{ //nolint:exhaustruct // partial update
		Exp:     t.options.Exp.Spec.ExperimentName(),
		Run:     t.options.Run,
		Loop:    t.options.Loop,
		Count:   t.options.Count,
		Stage:   string(t.stage),
		CmpType: t.options.Type,
		CmpName: t.options.Name,
		Status:  status,
	}

	_ = scorch.UpdatePipeline(update)
	scorch.UpdateComponent(update)
}

// handleBackgrounded ends the backgrounded start (for the stop stage) or
// configure (for the cleanup stage) stage of the given component, if there is
// one, waiting for it while ctx lasts. It reports whether there was such a
// stage, and returns the error the stage failed with before it was ended.
func handleBackgrounded(ctx context.Context, stage Action, options Options) (bool, error) {
	var bgStage Action

	switch stage {
	case ActionStop:
		bgStage = ActionStart
	case ActionCleanup:
		bgStage = ActionConfigure
	case ActionConfigure, ActionStart, ActionDone, ActionLoop:
		return false, nil
	default:
		return false, nil
	}

	name := backgroundName(bgStage, options)

	backgroundedMu.Lock()
	tasks, ok := backgrounded[bgStage][name]
	delete(backgrounded[bgStage], name)
	backgroundedMu.Unlock()

	if !ok {
		return false, nil
	}

	return true, endTasks(ctx, tasks)
}

// endBackgrounded ends the stages that the given run, loop and count
// backgrounded and that no stop or cleanup stage ended, so that none outlives
// the loop that started it, waiting for them while ctx lasts. It returns the
// errors of stages that failed before they were ended.
func endBackgrounded(ctx context.Context, options Options) error {
	var left []*backgroundTask

	backgroundedMu.Lock()

	for _, stageTasks := range backgrounded {
		for name, tasks := range stageTasks {
			// The tasks under one name share their experiment, run, loop and
			// count.
			o := tasks[0].options

			if o.Exp.Spec.ExperimentName() == options.Exp.Spec.ExperimentName() &&
				o.Run == options.Run && o.Loop == options.Loop && o.Count == options.Count {
				left = append(left, tasks...)
				delete(stageTasks, name)
			}
		}
	}

	backgroundedMu.Unlock()

	for _, task := range left {
		plog.Warn(
			plog.TypeScorch,
			"ending backgrounded component stage that no stop or cleanup stage ended",
			"exp", options.Exp.Spec.ExperimentName(),
			"component", task.options.Name,
			"stage", task.stage,
		)
	}

	return endTasks(ctx, left)
}

func init() { //nolint:gochecknoinits // config hook
	config.RegisterConfigHook("Experiment", func(stage string, c *store.Config) error {
		switch stage {
		case "update", "delete":
			scorch.DeletePipeline(c.Metadata.Name, -1, -1, false)
		}

		return nil
	})

	experiment.RegisterHook("start", func(_ string, name string) { // clear SCORCH pipeline on experiment start
		scorch.DeletePipeline(name, -1, -1, false)
	})

	experiment.RegisterHook("stop", func(_ string, name string) { dropRunRecord(name) })
}

// dropRunRecord drops the record of a SCORCH run, status.apps.scorch, from
// the stored experiment. An experiment stop reloads the experiment once its
// stopping hook has ended the SCORCH runs, but a run still going when that
// hook stops waiting for it is still recorded in what the stop writes back,
// so the stop's "stop" hook drops the record.
func dropRunRecord(name string) {
	exp, err := experiment.Get(name)
	if err != nil {
		plog.Error(plog.TypeScorch, "reading stopped experiment to drop its Scorch run record", "exp", name, "err", err)

		return
	}

	if _, ok := exp.Status.AppStatus()["scorch"]; !ok {
		return
	}

	exp.Status.SetAppStatus("scorch", nil)

	if err := exp.WriteToStore(true); err != nil {
		plog.Error(plog.TypeScorch, "dropping Scorch run record of stopped experiment", "exp", name, "err", err)
	}
}
