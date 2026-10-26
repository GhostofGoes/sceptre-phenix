package scorch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/activeshadow/structs"
	"github.com/hashicorp/go-multierror"
	"github.com/mitchellh/mapstructure"

	"phenix/api/scorch/scorchexe"
	"phenix/api/scorch/scorchmd"
	"phenix/app"
	"phenix/store"
	"phenix/types"
	ifaces "phenix/types/interfaces"
	"phenix/util"
	"phenix/util/mm/mmcli"
	"phenix/util/plog"
	"phenix/util/shell"
	"phenix/util/tap"
	"phenix/version"
	"phenix/web/scorch"
)

const (
	statusRunning  = "running"
	statusSuccess  = "success"
	statusFailure  = "failure"
	statusUnstable = "unstable"
	scorchInfoName = "name"

	filebeatStartupDelay = 2 * time.Second
	filebeatScanDelay    = 7 * time.Second
	filebeatMaxHarvest   = 5 * time.Minute
	filebeatHarvestTick  = 5 * time.Second
	filebeatStopDelay    = 10 * time.Second
)

func init() { //nolint:gochecknoinits // app registration
	_ = app.RegisterUserApp("scorch", func() app.App { return newScorch() })
}

type Scorch struct {
	md scorchmd.ScorchMetadata

	options app.Options
}

func newScorch() *Scorch {
	return new(Scorch)
}

func (s *Scorch) Init(opts ...app.Option) error {
	s.options = app.NewOptions(opts...)

	return nil
}

func (Scorch) Name() string {
	return "scorch"
}

func (Scorch) Configure(ctx context.Context, exp *types.Experiment) error {
	var app ifaces.ScenarioApp

	for _, app = range exp.Apps() {
		if app.Name() == "scorch" {
			break // this will keep `app` set to SCORCH app
		}
	}

	var md scorchmd.ScorchMetadata

	err := mapstructure.Decode(app.Metadata(), &md)
	if err != nil {
		return fmt.Errorf("decoding app metadata: %w", err)
	}

	if err := md.Validate(); err != nil {
		return err
	}

	// Ensure type is set for each component.
	for idx, c := range md.Components {
		if c.Type == "" {
			c.Type = c.Name
			md.Components[idx] = c
		}
	}

	body := structs.MapDefaultCase(md, structs.CASESNAKE)
	app.SetMetadata(body)

	return nil
}

func (Scorch) PreStart(context.Context, *types.Experiment) error {
	return nil
}

func (s Scorch) PostStart(ctx context.Context, exp *types.Experiment) error {
	return nil
}

func (s *Scorch) Running(ctx context.Context, exp *types.Experiment) error {
	var err error

	if s.md, err = scorchmd.DecodeMetadata(exp); err != nil {
		return err
	}

	runID := scorchexe.MustRunID(ctx)

	if runID < 0 || runID >= len(s.md.Runs) {
		return fmt.Errorf("invalid Scorch run ID for experiment %s", exp.Metadata.Name)
	}

	// Experiments created before Configure validated the runs can still name
	// undefined components.
	if err := s.md.ValidateRun(runID); err != nil {
		return err
	}

	// status.apps.scorch records the run and the taps its tap and break
	// components create. Running sets it for each caller: scorchexe.Execute,
	// and the app framework's running stage (`phenix experiment trigger
	// running` and runPeriodically), which has no run ID and runs run 0.
	// Only the record is stored, so what others stored since exp was read is
	// kept.
	if err := exp.UpdateStatus(func(status ifaces.ExperimentStatus) error {
		status.SetAppStatus("scorch", scorchmd.ScorchStatus{RunID: runID, Taps: make(map[string]*tap.Tap)})

		return nil
	}); err != nil {
		return fmt.Errorf(
			"recording Scorch run %d in the status of experiment %s: %w",
			runID,
			exp.Metadata.Name,
			err,
		)
	}

	errs := s.run(ctx, exp, runID)

	// Components such as soh write the experiment's status as they run, so
	// only the run's record is cleared.
	if err := exp.UpdateStatus(func(status ifaces.ExperimentStatus) error {
		status.SetAppStatus("scorch", nil)

		return nil
	}); err != nil {
		errs = multierror.Append(
			errs,
			fmt.Errorf("clearing Scorch status of experiment %s: %w", exp.Metadata.Name, err),
		)
	}

	return errs
}

// run executes the given run once status.apps.scorch records it.
func (s *Scorch) run(ctx context.Context, exp *types.Experiment, runID int) error {
	// One teardown budget covers every loop count of the run.
	ctx, release := withTeardown(ctx)
	defer release()

	var (
		runDir = filepath.Join(exp.FilesDir(), "scorch", fmt.Sprintf("run-%d", runID))
		start  = time.Now().UTC()
	)

	if err := os.RemoveAll(runDir); err != nil {
		return fmt.Errorf("removing existing contents of run directory at %s: %w", runDir, err)
	}

	var (
		cmd  *exec.Cmd
		port int
	)

	if s.md.FilebeatEnabled(runID) {
		inputs, err := createFilebeatConfig(s.md, exp.Spec.ExperimentName(), runID, runDir, start)
		if err != nil {
			return fmt.Errorf("creating Filebeat config: %w", err)
		}

		// Filebeat outlives a cancel to ship what the stop and cleanup stages
		// write, until the run's teardown budget runs out.
		if inputs > 0 {
			cmd, port, err = s.startFilebeat(teardownContext(ctx), runID, runDir)
			if err != nil {
				return fmt.Errorf("starting Filebeat: %w", err)
			}
		}
	}

	var (
		errors error
		run    = s.md.Runs[runID]
		opts   = []Option{Experiment(*exp), RunID(runID), StartTime(start.Format(time.RubyDate))}
	)

	for i := range run.Count {
		loopOpts := append([]Option(nil), opts...)
		loopOpts = append(loopOpts, LoopCount(i))

		err := executor(ctx, s.md.ComponentSpecs(), run, loopOpts...)
		if err != nil {
			errors = multierror.Append(
				errors,
				fmt.Errorf("executing Scorch for run %d, count %d: %w", runID, i, err),
			)

			break
		}
	}

	update := scorch.ComponentUpdate{ //nolint:exhaustruct // partial update
		Exp:   exp.Metadata.Name,
		Run:   runID,
		Loop:  0,
		Stage: string(ActionDone),
	}

	update.Status = statusRunning
	_ = scorch.UpdatePipeline(update)

	if cmd != nil {
		s.stopFilebeat(teardownContext(ctx), cmd, port)
	}

	if err := s.recordInfo(runID, runDir, exp.Metadata, start); err != nil {
		errors = multierror.Append(errors, err)
	}

	if _, err := os.Stat(runDir); err == nil {
		archive := filepath.Join(
			exp.FilesDir(),
			fmt.Sprintf("scorch-run-%d_%s.tgz", runID, start.Format("2006-01-02T15-04-05Z0700")),
		)

		err := util.CreateArchive(runDir, archive)
		if err != nil {
			errors = multierror.Append(
				errors,
				fmt.Errorf("archiving data generated for run %d: %w", runID, err),
			)
		}
	}

	update.Status = statusSuccess
	_ = scorch.UpdatePipeline(update)

	return errors
}

// Cleanup leaves the record of a SCORCH run, status.apps.scorch, alone: a
// cleanup stage triggered on a running experiment runs it while a run may
// execute. The hook dropRunRecord drops the record once a stop is done.
func (Scorch) Cleanup(context.Context, *types.Experiment) error {
	return nil
}

func (s Scorch) startFilebeat(
	ctx context.Context,
	runID int,
	runDir string,
) (*exec.Cmd, int, error) {
	var (
		stdOut bytes.Buffer
		stdErr bytes.Buffer
	)

	// TODO: don't rely on `shell.ProcessExists` since it's not working correctly
	// for detecting zombie (defunct) processes. For example, when filebeat fails
	// to start because of an issue w/ the config the current code still thinks
	// it's running. Instead, call `cmd.Wait` in a Goroutine and use a channel or
	// atomic int to determine later on if the command has finished.

	// FilebeatEnabled honors a run-level filebeat setting, which overrides the
	// global one.
	if !s.md.FilebeatEnabled(runID) {
		return nil, 0, nil
	}

	if !shell.CommandExists("filebeat") {
		plog.Warn(
			plog.TypePhenixApp,
			"Filebeat is enabled for this Scorch run but filebeat is not in PATH; not starting it",
			"app",
			"scorch",
			"run",
			runID,
		)

		return nil, 0, nil
	}

	var (
		data = filepath.Join(runDir, "filebeat", "data")
		cfg  = filepath.Join(runDir, "filebeat", "filebeat.yml")
	)

	port, err := util.GetFreePort("127.0.0.1")
	if err != nil {
		return nil, 0, fmt.Errorf(
			"unable to determine free port for Filebeat httpprof: %w",
			err,
		)
	}

	// We include the httpprof server so we can query for running harvesters
	// when stopping Filebeat below.
	httpprof := fmt.Sprintf("127.0.0.1:%d", port)
	cmd := exec.CommandContext(
		ctx,
		"filebeat",
		"-c",
		cfg,
		"--path.data",
		data,
		"--httpprof",
		httpprof,
	)

	cmd.Stdin = nil
	cmd.Stdout = &stdOut
	cmd.Stderr = &stdErr

	// When ctx ends, let Filebeat shut down as stopFilebeat asks it to.
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = filebeatStopDelay

	if err := cmd.Start(); err != nil {
		return nil, 0, err
	}

	// Give Filebeat time to start up or die.
	time.Sleep(filebeatStartupDelay)

	if !shell.ProcessExists(cmd.Process.Pid) {
		err := cmd.Wait()
		if err != nil {
			return nil, 0, err
		}
	}

	return cmd, port, nil
}

//nolint:funlen // complex logic
func (s Scorch) stopFilebeat(ctx context.Context, cmd *exec.Cmd, port int) {
	if cmd == nil {
		return
	}

	if !shell.ProcessExists(cmd.Process.Pid) {
		err := cmd.Wait()
		if err != nil {
			plog.Error(
				plog.TypePhenixApp,
				"the Filebeat process terminated early (logs may be missing)",
				"err",
				err,
				"app",
				"scorch",
			)
		}

		return
	}

	// Waiting for 7s here since we have Filebeat configured with a scan
	// frequency of 5s for inputs in the config file.
	select {
	case <-time.After(filebeatScanDelay):
	case <-ctx.Done():
	}

	var (
		maxTimer = time.NewTimer(filebeatMaxHarvest)
		tick     = time.NewTicker(filebeatHarvestTick)

		metrics filebeatMetrics
	)

	defer maxTimer.Stop()
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			// The end of ctx interrupted Filebeat (startFilebeat).
			_ = cmd.Wait()

			return
		case <-maxTimer.C:
			plog.Warn(
				plog.TypePhenixApp,
				"max amount of time for Filebeat to harvest inputs reached",
				"max",
				maxTimer,
				"app",
				"scorch",
			)

			_ = cmd.Process.Signal(os.Interrupt)
			_ = cmd.Wait()

			return
		case <-tick.C:
			req, err := http.NewRequestWithContext(
				ctx,
				http.MethodGet,
				fmt.Sprintf("http://localhost:%d/debug/vars", port),
				nil,
			)
			if err != nil {
				plog.Error(
					plog.TypePhenixApp,
					"unable to create request to get number of active harvesters from Filebeat",
					"err",
					err,
					"app",
					"scorch",
				)

				_ = cmd.Process.Signal(os.Interrupt)
				_ = cmd.Wait()

				return
			}
			resp, err := http.DefaultClient.Do(req) //nolint:gosec // SSRF via taint analysis
			if err != nil {
				plog.Error(
					plog.TypePhenixApp,
					"unable to get number of active harvesters from Filebeat",
					"err",
					err,
					"app",
					"scorch",
				)

				_ = cmd.Process.Signal(os.Interrupt)
				_ = cmd.Wait()

				return
			}
			defer resp.Body.Close()

			body, err := io.ReadAll(resp.Body)
			if err != nil {
				plog.Error(
					plog.TypePhenixApp,
					"unable to get number of active harvesters from Filebeat",
					"err",
					err,
					"app",
					"scorch",
				)

				_ = cmd.Process.Signal(os.Interrupt)
				_ = cmd.Wait()

				return
			}

			prev := metrics

			if err := json.Unmarshal(body, &metrics); err != nil {
				plog.Error(
					plog.TypePhenixApp,
					"unmarshaling Filebeat harvester metrics",
					"err",
					err,
					"app",
					"scorch",
				)

				continue
			}

			// Reset the max timer if progress is being made.
			if metrics.Progress(prev) {
				// stop and drain the max timer before resetting it
				if !maxTimer.Stop() {
					<-maxTimer.C
				}

				maxTimer.Reset(1 * time.Minute)

				// Skip the check below so we don't kill Filebeat prematurely if
				// progress is being made.
				continue
			}

			// NOTE: This might cause Filebeat to be killed prematurely if the
			// number of started harvesters doesn't yet match the number of total
			// files generated that need to be harvested. However, I'm not sure it's
			// a good idea to assume started must equal the number of inputs defined
			// in the Scorch config since 1) there's no guarantee all of them will
			// actually be generated, and 2) the input paths defined in the Filebeat
			// config have wildcards in them for run loops and counts.
			if metrics.Done() {
				plog.Info(
					plog.TypePhenixApp,
					"Filebeat has completed harvesting inputs",
					"app",
					"scorch",
				)

				_ = cmd.Process.Signal(os.Interrupt)
				_ = cmd.Wait()

				return
			}
		}
	}
}

func (s Scorch) recordInfo(
	runID int,
	runDir string,
	md store.ConfigMetadata,
	startTime time.Time,
) error {
	c := mmcli.NewCommand()
	c.Command = "version"

	mmVersion, err := mmcli.SingleResponse(mmcli.Run(c))
	if err != nil {
		return fmt.Errorf("getting minimega version: %w", err)
	}

	info := map[string]any{
		"experiment": map[string]string{
			scorchInfoName: md.Name,
			"tags":         md.Annotations["phenix.workflow/tags"],
		},
		"run": map[string]any{
			scorchInfoName: s.md.RunName(runID),
			"index":        runID,
		},
		"start": startTime.Format(time.RFC3339),
		"end":   time.Now().UTC().Format(time.RFC3339),
		"phenix_version": map[string]string{
			"commit": version.Commit,
			"tag":    version.Tag,
			"date":   version.Date,
		},
		"minimega_version": mmVersion,
	}

	fileName := fmt.Sprintf(
		"%s-scorch-run-%d-%s.json",
		md.Name,
		runID,
		startTime.Format("2006-01-02T15-04-05Z0700"),
	)

	if err := os.MkdirAll(runDir, 0o750); err != nil {
		return fmt.Errorf("creating %s directory for scorch run %d: %w", runDir, runID, err)
	}

	body, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return fmt.Errorf("marshalling scorch run info: %w", err)
	}

	if err := os.WriteFile(filepath.Join(runDir, fileName), body, 0o600); err != nil {
		return fmt.Errorf("writing scorch information file (%s): %w", fileName, err)
	}

	return nil
}

// teardownTimeout bounds the stop and cleanup stages of a canceled run, from
// the cancel, at every loop level together.
var teardownTimeout = scorchexe.TeardownTimeout //nolint:gochecknoglobals // tests shorten it

// errTeardownExpired is the failure of the stop and cleanup stages that
// teardownTimeout ended.
var errTeardownExpired = errors.New("the stop and cleanup stages of the canceled run ran out of time")

type (
	teardownKey struct{}
	pipelineKey struct{}
)

// withTeardown returns ctx carrying the context that the stop and cleanup
// stages of the run run under, unless ctx carries one already, and a function
// that releases it. That context is not canceled with ctx, so those stages
// release what configure and start set up even after a cancel, but it ends
// teardownTimeout after ctx is canceled, before or during those stages, so a
// hung component cannot hold up the run.
func withTeardown(ctx context.Context) (context.Context, func()) {
	if _, ok := ctx.Value(teardownKey{}).(context.Context); ok {
		return ctx, func() {}
	}

	tctx, cancel := context.WithCancelCause(context.WithoutCancel(ctx))

	var (
		mu       sync.Mutex
		released bool
		timer    *time.Timer
	)

	stop := context.AfterFunc(ctx, func() {
		mu.Lock()
		defer mu.Unlock()

		if !released {
			timer = time.AfterFunc(teardownTimeout, func() { cancel(errTeardownExpired) })
		}
	})

	release := func() {
		stop()

		mu.Lock()
		released = true

		if timer != nil {
			timer.Stop()
		}
		mu.Unlock()

		cancel(nil)
	}

	tctx = context.WithValue(tctx, pipelineKey{}, ctx)

	return context.WithValue(ctx, teardownKey{}, tctx), release
}

// teardownContext returns the context that the stop and cleanup stages of
// the run ctx belongs to run under.
func teardownContext(ctx context.Context) context.Context {
	if tctx, ok := ctx.Value(teardownKey{}).(context.Context); ok {
		return tctx
	}

	return context.WithoutCancel(ctx)
}

// pipelineContext returns the context of the run that the context of one of
// its stop or cleanup stages belongs to, which a cancel ends at once, and any
// other context as is. Components that release nothing, such as pause and
// break, run under it in every stage.
func pipelineContext(ctx context.Context) context.Context {
	if pctx, ok := ctx.Value(pipelineKey{}).(context.Context); ok {
		return pctx
	}

	return ctx
}

// markCanceled records the error of a component that ran under a canceled
// context as the context's error, whatever the component reported, such as
// the exit status of a process the cancel terminated.
func markCanceled(ctx context.Context, err error) error {
	if ctx.Err() == nil || errors.Is(err, ctx.Err()) {
		return err
	}

	// Only the context's error is wrapped, so the result reads as a cancel.
	return fmt.Errorf("%w: %v", ctx.Err(), err) //nolint:errorlint // see above
}

// executor runs the given loop, then ends the stages it backgrounded that its
// stop and cleanup stages did not end.
func executor(
	ctx context.Context,
	components scorchmd.ComponentSpecMap,
	exe *scorchmd.Loop,
	opts ...Option,
) error {
	ctx, release := withTeardown(ctx)
	defer release()

	err := runLoop(ctx, components, exe, opts...)

	if endErr := endBackgrounded(teardownContext(ctx), NewOptions(opts...)); endErr != nil {
		err = multierror.Append(err, endErr)
	}

	return err
}

//nolint:funlen,maintidx // complex logic
func runLoop(
	ctx context.Context,
	components scorchmd.ComponentSpecMap,
	exe *scorchmd.Loop,
	opts ...Option,
) error {
	options := NewOptions(opts...)

	loopReplacements, err := scorchmd.ResolveReplacements(exe.Replace)
	if err != nil {
		return fmt.Errorf("resolving replacements: %w", err)
	}

	mergedReplacements := scorchmd.MergeReplacements(options.Replacements, loopReplacements)
	plog.Info(
		plog.TypePhenixApp,
		"Resolved replacements for Scorch loop",
		"run_id",
		options.Run,
		"loop_idx",
		options.Loop,
		"replacements",
		mergedReplacements,
		"app",
		"scorch",
	)

	options.Replacements = mergedReplacements
	opts = append(opts, Replacements(mergedReplacements))

	var (
		exp        = options.Exp.Spec.ExperimentName()
		loopPrefix = fmt.Sprintf(
			"[RUN: %d - LOOP: %d - COUNT: %d]",
			options.Run,
			options.Loop,
			options.Count,
		)
	)

	logger := plog.LoggerFromContext(ctx, plog.TypeScorch)

	if options.Loop == 0 {
		scorch.DeletePipeline(exp, options.Run, -1, true)
	}

	update := scorch.ComponentUpdate{ //nolint:exhaustruct // partial update
		Exp:   exp,
		Run:   options.Run,
		Loop:  options.Loop,
		Count: options.Count,
	}

	logger.Info("starting scorch", "run", loopPrefix)

	runStage := func(ctx context.Context, stage Action, names []string, failFast bool) error {
		update.Stage = string(stage)

		if len(names) == 0 {
			update.CmpType = ""
			update.CmpName = ""
			update.Status = statusSuccess
			_ = scorch.UpdatePipeline(update)

			return nil
		}

		var errors error

		logger.Info("running scorch stage", "stage", stage)

		for _, name := range names {
			typ := components[name].Type

			update.CmpType = typ
			update.CmpName = name
			update.Status = "start"

			scorch.UpdateComponent(update)

			meta := scorchmd.ApplyReplacements(components[name].Metadata, options.Replacements)
			cmpOpts := opts
			cmpOpts = append(cmpOpts, Name(name), Type(typ), Stage(stage), Metadata(meta))

			status := statusRunning

			if components[name].Background && failFast {
				cmpOpts = append(cmpOpts, Background())
				status = "background"
			}

			update.Status = status
			scorch.UpdateComponent(update)
			_ = scorch.UpdatePipeline(update)

			logger.Debug("running scorch stage component", "stage", stage, "component", name)

			err := ExecuteComponent(ctx, cmpOpts...)
			if err != nil {
				err = markCanceled(ctx, err)

				update.Status = statusFailure
				scorch.UpdateComponent(update)
				_ = scorch.UpdatePipeline(update)

				logger.Error(
					"[✗] failed scorch stage component",
					"stage",
					stage,
					"component",
					name,
					"err",
					err,
				)

				err = fmt.Errorf(
					"%s %s component %s for experiment %s: %w",
					loopPrefix,
					stage,
					name,
					exp,
					err,
				)

				if failFast {
					return err
				}

				errors = multierror.Append(errors, err)
			} else if !components[name].Background || !failFast {
				update.Status = statusSuccess
				scorch.UpdateComponent(update)
				_ = scorch.UpdatePipeline(update)

				logger.Debug(
					"[✓] completed scorch stage component",
					"stage",
					stage,
					"component",
					name,
				)
			}
		}

		return errors
	}

	// Stop and cleanup run even when the pipeline was canceled: they release
	// what configure and start set up (captures, QoS rules, taps, processes in
	// guests). They get the run's teardown context, which a cancel does not
	// end but bounds (withTeardown).
	teardown := func(stage Action, names []string) error {
		tctx := teardownContext(ctx)

		err := runStage(tctx, stage, names, false)
		if err != nil && errors.Is(context.Cause(tctx), errTeardownExpired) {
			// Not wrapped, so the failure does not read as the cancel.
			return fmt.Errorf("%w after %v: %v", errTeardownExpired, teardownTimeout, err) //nolint:errorlint // see above
		}

		return err
	}

	configure := func() error { return runStage(ctx, ActionConfigure, exe.Configure, true) }
	start := func() error { return runStage(ctx, ActionStart, exe.Start, true) }
	stop := func() error { return teardown(ActionStop, exe.Stop) }
	cleanup := func() error { return teardown(ActionCleanup, exe.Cleanup) }

	if err := configure(); err != nil {
		errors := multierror.Append(nil, err)

		err := cleanup()
		if err != nil {
			errors = multierror.Append(errors, err)
		}

		return errors
	}

	if err := start(); err != nil {
		errors := multierror.Append(nil, err)

		err := stop()
		if err != nil {
			errors = multierror.Append(errors, err)
		}

		err = cleanup()
		if err != nil {
			errors = multierror.Append(errors, err)
		}

		return errors
	}

	var errors error

	if exe.Loop != nil {
		update := scorch.ComponentUpdate{ //nolint:exhaustruct // partial update
			Exp:   exp,
			Run:   options.Run,
			Loop:  options.Loop,
			Stage: string(ActionLoop),
		}

		update.Status = statusRunning
		_ = scorch.UpdatePipeline(update)

		for i := range exe.Loop.Count {
			loopOpts := append([]Option(nil), opts...)
			loopOpts = append(loopOpts, CurrentLoop(options.Loop+1), LoopCount(i))

			err := executor(ctx, components, exe.Loop, loopOpts...)
			if err != nil {
				errors = multierror.Append(errors, err)

				break
			}
		}

		if errors != nil {
			update.Status = statusFailure
		} else {
			update.Status = statusSuccess
		}

		_ = scorch.UpdatePipeline(update)
	}

	if err := stop(); err != nil {
		errors = multierror.Append(errors, err)
	}

	if err := cleanup(); err != nil {
		errors = multierror.Append(errors, err)
	}

	if update.Loop != 0 {
		update.CmpType = ""
		update.CmpName = ""
		update.Stage = string(ActionDone)
		update.Status = statusSuccess

		_ = scorch.UpdatePipeline(update)
	}

	// A run canceled during its stop or cleanup stage was still canceled.
	if errors == nil {
		errors = ctx.Err()
	}

	return errors
}
