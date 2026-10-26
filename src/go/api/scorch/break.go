package scorch

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/mitchellh/mapstructure"

	"phenix/api/scorch/scorchmd"
	"phenix/app"
	"phenix/util"
	"phenix/util/mm"
	"phenix/util/tap"
	"phenix/web/scorch"
)

const tapBreakSuffixLen = 8

type BreakMetadata struct {
	Tap    *tap.Tap `mapstructure:"tap"`
	Readme string   `mapstructure:"readme"`
}

type Break struct {
	options Options
}

func (b *Break) Init(opts ...Option) error {
	b.options = NewOptions(opts...)

	return nil
}

func (Break) Type() string {
	return "break"
}

func (b Break) Configure(ctx context.Context) error {
	return b.breakPoint(ctx, ActionConfigure)
}

func (b Break) Start(ctx context.Context) error {
	return b.breakPoint(ctx, ActionStart)
}

// Stop and Cleanup break under the run's own context, so a cancel ends the
// breakpoint, or skips it, in these stages too.
func (b Break) Stop(ctx context.Context) error {
	return b.breakPoint(pipelineContext(ctx), ActionStop)
}

func (b Break) Cleanup(ctx context.Context) error {
	return b.breakPoint(pipelineContext(ctx), ActionCleanup)
}

//nolint:funlen // complex logic
func (b Break) breakPoint(ctx context.Context, stage Action) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}

	exp := b.options.Exp.Spec.ExperimentName()

	var md BreakMetadata

	if err := mapstructure.Decode(b.options.Meta, &md); err != nil {
		return fmt.Errorf("decoding 'break' component metadata: %w", err)
	}

	if md.Tap != nil {
		// Read the status that records the tap before creating it, so a
		// missing status cannot leave an unrecorded tap behind.
		var status scorchmd.ScorchStatus

		err := b.options.Exp.Status.ParseAppStatus("scorch", &status)
		if err != nil {
			return fmt.Errorf("getting experiment status for scorch app: %w", err)
		}

		pairs := discoverUsedPairs()
		md.Tap.Init(b.options.Exp.Spec.DefaultBridge(), tap.Experiment(exp), tap.UsedPairs(pairs))

		// backwards compatibility (doesn't support external access firewall rules)
		if v, ok := md.Tap.Other["internetAccess"]; ok {
			enabled, _ := v.(bool)
			md.Tap.External.Enabled = enabled
		}

		// The VLAN, the bridge and the address reach minimega and the host.
		if err := md.Tap.Validate(); err != nil {
			return fmt.Errorf("tap of break component %s: %w", b.options.Name, err)
		}

		// tap names cannot be longer than 15 characters
		// (dictated by max length of Linux interface names)
		md.Tap.Name = util.RandomString(tapBreakSuffixLen) + "-tapbrk"

		if _, err := md.Tap.Create(mm.Headnode()); err != nil {
			return fmt.Errorf("setting up tap for break: %w", err)
		}

		defer func() { _ = md.Tap.Delete(mm.Headnode()) }()

		if status.Taps == nil {
			status.Taps = make(map[string]*tap.Tap)
		}

		status.Taps[b.options.Name] = md.Tap

		b.options.Exp.Status.SetAppStatus("scorch", status)

		if err := storeRunRecord(b.options.Exp); err != nil {
			return fmt.Errorf("recording tap %s in experiment status: %w", md.Tap.Name, err)
		}
	}

	var (
		cmd  string
		args []string
	)

	dir, err := os.MkdirTemp("", "*")
	if err != nil {
		return fmt.Errorf("creating temporary directory for break component: %w", err)
	}

	defer func() { _ = os.RemoveAll(dir) }()

	if md.Readme == "" {
		if md.Tap == nil {
			cmd = "/bin/bash"
		} else {
			// create shell in the network namespace created for the tap
			cmd = "ip"
			args = []string{"netns", "exec", md.Tap.Name, "/bin/bash"}
		}
	} else {
		readme := filepath.Join(dir, "README")

		err = os.WriteFile(readme, []byte(md.Readme), 0o600)
		if err != nil {
			return fmt.Errorf("writing break component README to file: %w", err)
		}

		cmd = "/usr/local/bin/glow"
		args = []string{"README"}
	}

	if app.IsContextTriggerCLI(ctx) {
		// this blocks until terminal is exited
		err = terminal(ctx, dir, cmd, args)
		if err != nil {
			return fmt.Errorf("starting bash terminal: %w", err)
		}
	} else if app.IsContextTriggerUI(ctx) {
		var done <-chan struct{}
		done, err = scorch.CreateWebTerminal(
			ctx,
			exp,
			b.options.Run,
			b.options.Loop,
			string(stage),
			b.options.Name,
			dir,
			cmd,
			args,
		)
		if err != nil {
			return fmt.Errorf("triggering web terminal: %w", err)
		}

		select {
		case <-ctx.Done(): // this blocks until the context is canceled
		case <-done: // this blocks until web terminal is exited
		}
	}

	return ctx.Err()
}
