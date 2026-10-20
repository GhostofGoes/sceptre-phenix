package shell

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/hashicorp/go-multierror"

	"phenix/util/sigterm"
)

var killDelay = 10 * time.Second //nolint:gochecknoglobals // shortened by tests

// closeDelay is how long ExecCommand still reads a canceled command's output
// once it killed the command's process group: the output pipes stay open as
// long as a process outside the group, such as one that called setsid, holds
// them.
var closeDelay = time.Second //nolint:gochecknoglobals // shortened by tests

// groups holds the process group of each command ExecCommand runs, until it
// has waited for the command, with whether the command was canceled.
//
//nolint:gochecknoglobals // the process groups phenix runs are process-wide
var (
	groupsMu  sync.Mutex
	groups    = make(map[int]*atomic.Bool)
	groupsEnd sync.Once
)

type shell struct{}

// FindCommandsWithPrefix returns the names, with prefix removed, of the
// executables in PATH whose names start with prefix. It mirrors how commands
// are resolved when they are run (see CommandExists): each PATH directory is
// searched without recursing, symbolic links are followed, directories that do
// not exist are skipped, and when a name appears in more than one directory
// only the first occurrence is returned.
func (shell) FindCommandsWithPrefix(prefix string) []string {
	var (
		commands []string
		seen     = make(map[string]struct{})
	)

	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" {
			continue
		}

		entries, err := os.ReadDir(dir)
		if err != nil {
			// A missing or unreadable PATH entry must not hide the commands in
			// the other entries.
			continue
		}

		for _, entry := range entries {
			name := entry.Name()

			if !strings.HasPrefix(name, prefix) || name == prefix {
				continue
			}

			if _, ok := seen[name]; ok {
				continue
			}

			// os.Stat follows symbolic links, so a linked command is listed when
			// its target is an executable regular file.
			info, err := os.Stat(filepath.Join(dir, name))
			if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
				continue
			}

			seen[name] = struct{}{}
			commands = append(commands, strings.TrimPrefix(name, prefix))
		}
	}

	return commands
}

func (shell) CommandExists(cmd string) bool {
	err := exec.Command("which", cmd).Run() //nolint:noctx // simple which command

	return err == nil
}

func (shell) ProcessExists(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}

	err = proc.Signal(syscall.Signal(0))
	if err == nil {
		return true
	}

	if errors.Is(err, os.ErrProcessDone) {
		return false
	}

	var errno syscall.Errno

	ok := errors.As(err, &errno)
	if !ok {
		return false
	}

	switch errno { //nolint:exhaustive // too many errors to list
	case syscall.ESRCH:
		return false
	case syscall.EPERM:
		return true
	default:
		return false
	}
}

// maxOutputLine is the longest line of a command's output ExecCommand reads,
// far above bufio's default of 64 KiB: a scheduler prints the experiment it
// returns as one line.
var maxOutputLine = 64 << 20 //nolint:gochecknoglobals // lowered by tests

//nolint:funlen // complex logic
func (shell) ExecCommand(ctx context.Context, opts ...Option) ([]byte, []byte, error) {
	o := newOptions(opts...)

	var (
		stdIn       io.Reader
		stdoutBytes []byte
		stderrBytes []byte
	)

	if o.stdin == nil {
		stdIn = os.Stdin
	} else {
		stdIn = bytes.NewBuffer(o.stdin)
	}

	cmd := exec.CommandContext(ctx, o.cmd, o.args...) //nolint:gosec // Subprocess launched with a potential tainted input

	// When the context is canceled, ask the child to terminate (so apps and
	// SCORCH components can run their own cleanup) and only kill it if it is
	// still running killDelay later. Without Cancel, CommandContext sends
	// SIGKILL immediately. The child runs in a process group of its own, and
	// both signals go to the group: a process the child started, such as a
	// shell wrapper's foreground command, would otherwise outlive it and hold
	// the output pipes open, so ExecCommand would not return.
	canceled := new(atomic.Bool)

	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} //nolint:exhaustruct // only the process group
	cmd.Cancel = func() error {
		canceled.Store(true)

		return signalGroup(cmd.Process, syscall.SIGTERM)
	}
	cmd.WaitDelay = killDelay

	// The signals of a terminal reach only phenix's own process group, so the
	// signal that ends phenix is passed on to the groups (see endGroups).
	groupsEnd.Do(func() { sigterm.OnEnd(endGroups) })

	cmd.Stdin = stdIn
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()

	cmd.Env = os.Environ()
	cmd.Env = append(cmd.Env, o.env...)

	err := cmd.Start()
	if err != nil {
		// The callers read the streams until they are closed.
		for _, stream := range []chan []byte{o.stdout, o.stderr} {
			if stream != nil {
				close(stream)
			}
		}

		return nil, nil, fmt.Errorf("starting command: %w", err)
	}

	// Before the output is read, so a command whose output was read is
	// tracked.
	defer trackGroup(cmd.Process.Pid, canceled)()

	waited := make(chan struct{})
	defer close(waited)

	go killOnCancel(ctx, cmd.Process, killDelay, closeDelay, waited, stdout, stderr)

	// Each scanner has its own error: once WaitDelay expires, os/exec closes
	// both pipes together, so both scanners can fail at once.
	var (
		errs                 error
		stdoutErr, stderrErr error
		wg                   sync.WaitGroup
	)

	wg.Add(1)

	go func() {
		defer wg.Done()

		scanner := bufio.NewScanner(stdout)
		scanner.Split(o.splitter)
		scanner.Buffer(nil, maxOutputLine)

		// Read what a failed scan leaves, so the command never blocks on a
		// full pipe and Wait returns.
		defer func() { _, _ = io.Copy(io.Discard, stdout) }()

		for scanner.Scan() {
			line := scanner.Bytes()

			stdoutBytes = append(stdoutBytes, line...)

			if o.stdout != nil {
				o.stdout <- bytes.Clone(line)
			}
		}

		if err := scanner.Err(); err != nil {
			stdoutErr = fmt.Errorf("scanning STDOUT: %w", err)
		}

		if o.stdout != nil {
			close(o.stdout)
		}
	}()

	wg.Add(1)

	go func() {
		defer wg.Done()

		scanner := bufio.NewScanner(stderr)
		scanner.Split(bufio.ScanLines)
		scanner.Buffer(nil, maxOutputLine)

		defer func() { _, _ = io.Copy(io.Discard, stderr) }()

		for scanner.Scan() {
			line := scanner.Bytes()

			stderrBytes = append(stderrBytes, line...)

			if o.stderr != nil {
				o.stderr <- bytes.Clone(line)
			}
		}

		if err := scanner.Err(); err != nil {
			stderrErr = fmt.Errorf("scanning STDERR: %w", err)
		}

		if o.stderr != nil {
			close(o.stderr)
		}
	}()

	wg.Wait()

	for _, err := range []error{stdoutErr, stderrErr} {
		if err != nil {
			errs = multierror.Append(errs, err)
		}
	}

	err = cmd.Wait()
	if err != nil {
		errs = multierror.Append(errs, fmt.Errorf("waiting for command to complete: %w", err))
	}

	return stdoutBytes, stderrBytes, errs
}

// killOnCancel kills the process group of p kill after ctx is done, unless
// waited is closed first, and closes the pipes closeAfter later still, which
// ends the reads of a process outside the group that holds them.
func killOnCancel(
	ctx context.Context,
	p *os.Process,
	kill, closeAfter time.Duration,
	waited <-chan struct{},
	pipes ...io.Closer,
) {
	// after reports whether d passed before waited was closed.
	after := func(d time.Duration) bool {
		timer := time.NewTimer(d)
		defer timer.Stop()

		select {
		case <-waited:
			return false
		case <-timer.C:
			return true
		}
	}

	select {
	case <-waited:
		return
	case <-ctx.Done():
	}

	if !after(kill) {
		return
	}

	_ = signalGroup(p, syscall.SIGKILL)

	if !after(closeAfter) {
		return
	}

	for _, pipe := range pipes {
		_ = pipe.Close()
	}
}

// trackGroup adds the process group pid leads to groups until the returned
// function is called, once the command was waited for.
func trackGroup(pid int, canceled *atomic.Bool) func() {
	groupsMu.Lock()
	defer groupsMu.Unlock()

	groups[pid] = canceled

	return func() {
		groupsMu.Lock()
		defer groupsMu.Unlock()

		delete(groups, pid)
	}
}

// endGroups passes sig, which ends phenix, on to the process group of each
// command phenix still runs, as a terminal's signal reached the command before
// it ran in a group of its own. The group of a canceled command, which the
// cancel's SIGTERM did not end, gets the SIGKILL that its kill delay would
// have sent.
func endGroups(sig os.Signal) {
	s, ok := sig.(syscall.Signal)
	if !ok {
		return
	}

	groupsMu.Lock()
	defer groupsMu.Unlock()

	for pid, canceled := range groups {
		if canceled.Load() {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
		} else {
			_ = syscall.Kill(-pid, s)
		}
	}
}

// signalGroup sends sig to the process group p leads. It reports a group that
// has no process left as [os.ErrProcessDone], which os/exec ignores from
// Cancel.
func signalGroup(p *os.Process, sig syscall.Signal) error {
	err := syscall.Kill(-p.Pid, sig)
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}

	return err //nolint:wrapcheck // os/exec compares it with os.ErrProcessDone
}
