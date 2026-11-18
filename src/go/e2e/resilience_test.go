//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unicode"

	"github.com/gorilla/websocket"
)

// Resilience shard: phenix against a minimega daemon that is shut down and
// started again (F3-06), killed and recovered (F3-07), restarted on another
// -filepath (F3-02) or without -abssnapshot (F2-01, F2-08), or run under a
// suffixed host name (F3-08), while phenix ui keeps running. The tests stop and
// start the one daemon the E2E action started, with its binary, flags,
// environment, working directory and output files, and every test restores that
// daemon in t.Cleanup so the tests after it find it unchanged. Every VM is
// hollow.

const (
	// resilienceSuffixedHost is the host name F3-08 gives minimega. phenix
	// trims its default -minimega suffix (--hostname-suffixes) to
	// resilienceTrimmedHost.
	resilienceSuffixedHost = "e2e-head-minimega"
	resilienceTrimmedHost  = "e2e-head"

	// resilienceHostsMarker tags the /etc/hosts line F3-08 adds so that phenix
	// can dial the suffixed host name for VNC.
	resilienceHostsMarker = "# phenix-e2e-resilience"
	resilienceHostsFile   = "/etc/hosts"

	// resilienceDefaultBase is minimega's -base when the flag is not given.
	resilienceDefaultBase = "/tmp/minimega"

	// resiliencePhenixBase is phenix's base directory in the CLI and the UI.
	resiliencePhenixBase = "/phenix"

	// resilienceUpWait is how long the E2E action waits for minimega's socket.
	resilienceUpWait = 30 * time.Second

	// resilienceRecoverWait also covers -recover reattaching to every VM.
	resilienceRecoverWait = 60 * time.Second

	// resilienceExitWait bounds minimega's teardown after SIGTERM.
	resilienceExitWait = 60 * time.Second

	// resilienceVMWait bounds hollow VMs reaching RUNNING.
	resilienceVMWait = 2 * time.Minute

	// resilienceQEMUWait bounds QEMU processes exiting once they are killed.
	resilienceQEMUWait = 15 * time.Second

	// resilienceEarlyExit is how long a starting daemon may be missing from
	// /proc before the start counts as failed.
	resilienceEarlyExit = 3 * time.Second

	resiliencePoll = 250 * time.Millisecond

	// resilienceUIAttempts is how many GET /hosts, one second apart, phenix ui
	// may need to reach a restarted minimega.
	resilienceUIAttempts = 5

	// resilienceTailLines is how much of minimega's output a failure shows.
	resilienceTailLines = 30

	// resilienceRFB starts every VNC server greeting (RFB 003.00x).
	resilienceRFB = "RFB 00"

	// resilienceQEMUComm is the process name minimega's -recover looks for
	// (internal/recovery/recovery.go, processes).
	resilienceQEMUComm = "qemu-system-x86"

	resilienceStateRunning = "RUNNING"
)

// ---------------------------------------------------------------------------
// the minimega daemon

// resilienceDaemon records how a minimega daemon was started, so that it can
// be started again exactly the same way.
type resilienceDaemon struct {
	argv    []string // binary and flags
	exe     string   // the binary, as /proc/<pid>/exe reads
	env     []string // environment
	dir     string   // working directory
	stdout  string   // file the daemon's stdout and stderr are appended to
	logfile string   // minimega -logfile
	uts     string   // UTS namespace, as /proc/<pid>/ns/uts reads
	host    string   // host name minimega reports
}

// resilienceOrigin is the daemon the E2E action started, captured once per
// test binary before any test of the shard restarts it.
var resilienceOrigin struct { //nolint:gochecknoglobals // shared by the shard's tests
	once   sync.Once
	daemon resilienceDaemon
	err    error
}

// resilienceOriginal returns how the daemon the E2E action started runs. It
// is read from /proc once, before any test of this shard restarts the daemon.
func resilienceOriginal(t *testing.T) resilienceDaemon {
	t.Helper()

	o := &resilienceOrigin

	o.once.Do(func() { o.daemon, o.err = resilienceCapture() })

	if o.err != nil {
		t.Fatalf("reading how the minimega daemon was started: %v", o.err)
	}

	return o.daemon
}

func resilienceCapture() (resilienceDaemon, error) {
	pid, err := resilienceDaemonPID()
	if err != nil {
		return resilienceDaemon{}, err
	}

	proc := filepath.Join("/proc", strconv.Itoa(pid))

	argv, err := resilienceCmdline(pid)
	if err != nil {
		return resilienceDaemon{}, err
	}

	exe, err := os.Readlink(filepath.Join(proc, "exe"))
	if err != nil {
		return resilienceDaemon{}, fmt.Errorf("finding the minimega binary: %w", err)
	}

	if _, err := os.Stat(argv[0]); err != nil || !filepath.IsAbs(argv[0]) {
		argv[0] = exe
	}

	environ, err := os.ReadFile(filepath.Join(proc, "environ"))
	if err != nil {
		return resilienceDaemon{}, fmt.Errorf("reading minimega's environment: %w", err)
	}

	uts, err := os.Readlink(filepath.Join(proc, "ns", "uts"))
	if err != nil {
		return resilienceDaemon{}, fmt.Errorf("reading minimega's UTS namespace: %w", err)
	}

	host, err := resilienceMMHost()
	if err != nil {
		return resilienceDaemon{}, err
	}

	d := resilienceDaemon{
		argv:    argv,
		exe:     exe,
		env:     resilienceSplitNUL(environ),
		dir:     "/",
		stdout:  filepath.Join(cfg.mmBase, "minimega.stdout"),
		logfile: filepath.Join(cfg.mmBase, "minimega.log"),
		uts:     uts,
		host:    host,
	}

	if dir, err := os.Readlink(filepath.Join(proc, "cwd")); err == nil {
		if _, err := os.Stat(dir); err == nil {
			d.dir = dir
		}
	}

	// The action redirects the daemon's stdout and stderr into a file under
	// its base directory; keep appending to the same file.
	if out, err := os.Readlink(filepath.Join(proc, "fd", "1")); err == nil &&
		filepath.IsAbs(out) && !strings.HasPrefix(out, "/dev/") && !strings.HasSuffix(out, " (deleted)") {
		d.stdout = out
	}

	if v, ok := resilienceFlag(argv, "logfile"); ok {
		d.logfile = v
	}

	if !slices.ContainsFunc(d.env, func(kv string) bool { return strings.HasPrefix(kv, "PATH=") }) {
		d.env = append(d.env, "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	}

	return d, nil
}

// resilienceDaemonPID finds the minimega daemon serving cfg.mmBase.
func resilienceDaemonPID() (int, error) {
	pids, err := resilienceDaemonPIDs()
	if err != nil {
		return 0, err
	}

	switch len(pids) {
	case 0:
		return 0, fmt.Errorf("no minimega daemon runs with -base %s", cfg.mmBase)
	case 1:
		return pids[0], nil
	default:
		return 0, fmt.Errorf("several minimega daemons run with -base %s: %v", cfg.mmBase, pids)
	}
}

// resilienceDaemonPIDs lists the live minimega daemons serving cfg.mmBase.
func resilienceDaemonPIDs() ([]int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}

	var pids []int

	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}

		argv, err := resilienceCmdline(pid)
		if err != nil || filepath.Base(argv[0]) != "minimega" || !resilienceAlive(pid) {
			continue
		}

		// minimega -e, -attach and -pipe are clients of a daemon.
		if resilienceHasFlag(argv, "e") || resilienceHasFlag(argv, "attach") || resilienceHasFlag(argv, "pipe") {
			continue
		}

		base, ok := resilienceFlag(argv, "base")
		if !ok {
			base = resilienceDefaultBase
		}

		if filepath.Clean(base) == filepath.Clean(cfg.mmBase) {
			pids = append(pids, pid)
		}
	}

	return pids, nil
}

// resilienceCmdline returns the command line of a process.
func resilienceCmdline(pid int) ([]string, error) {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return nil, err
	}

	argv := resilienceSplitNUL(data)
	if len(argv) == 0 {
		return nil, fmt.Errorf("process %d has no command line", pid)
	}

	return argv, nil
}

func resilienceSplitNUL(data []byte) []string {
	var out []string

	for s := range strings.SplitSeq(string(data), "\x00") {
		if s != "" {
			out = append(out, s)
		}
	}

	return out
}

// resilienceAlive reports whether a process exists and has not exited; a
// zombie counts as exited.
func resilienceAlive(pid int) bool {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return false
	}

	i := bytes.LastIndexByte(data, ')')
	if i < 0 {
		return false
	}

	fields := strings.Fields(string(data[i+1:]))

	return len(fields) > 0 && fields[0] != "Z" && fields[0] != "X"
}

// resilienceFlag returns the value of a Go-style flag (-name value, -name=value
// or the same with two dashes) on a command line.
func resilienceFlag(argv []string, name string) (string, bool) {
	for i := 1; i < len(argv); i++ {
		if !strings.HasPrefix(argv[i], "-") {
			continue
		}

		key, value, hasValue := strings.Cut(strings.TrimLeft(argv[i], "-"), "=")
		if key != name {
			continue
		}

		if hasValue {
			return value, true
		}

		if i+1 < len(argv) {
			return argv[i+1], true
		}
	}

	return "", false
}

// resilienceHasFlag reports whether a command line sets a flag.
func resilienceHasFlag(argv []string, name string) bool {
	for _, a := range argv[1:] {
		if !strings.HasPrefix(a, "-") {
			continue
		}

		if key, _, _ := strings.Cut(strings.TrimLeft(a, "-"), "="); key == name {
			return true
		}
	}

	return false
}

// resilienceRecoverArgv is argv with -recover instead of -force: minimega
// removes the stale socket a killed daemon leaves and rebuilds its namespaces
// from the QEMU processes that still run (cmd/minimega/main.go, recovery.go).
func resilienceRecoverArgv(argv []string) []string {
	out := []string{argv[0], "-recover"}

	for _, a := range argv[1:] {
		if key, _, _ := strings.Cut(strings.TrimLeft(a, "-"), "="); strings.HasPrefix(a, "-") && key == "force" {
			continue
		}

		out = append(out, a)
	}

	return out
}

// resilienceWithFlag is argv with the value flag name set to value, in place
// of any value it had.
func resilienceWithFlag(argv []string, name, value string) []string {
	out := []string{argv[0]}

	for i := 1; i < len(argv); i++ {
		key, _, hasValue := strings.Cut(strings.TrimLeft(argv[i], "-"), "=")
		if !strings.HasPrefix(argv[i], "-") || key != name {
			out = append(out, argv[i])

			continue
		}

		if !hasValue {
			i++ // the value is the next argument
		}
	}

	return append(out, "-"+name+"="+value)
}

// resilienceWithoutBoolFlag is argv without the boolean flag name.
func resilienceWithoutBoolFlag(argv []string, name string) []string {
	out := []string{argv[0]}

	for _, a := range argv[1:] {
		if key, _, _ := strings.Cut(strings.TrimLeft(a, "-"), "="); strings.HasPrefix(a, "-") && key == name {
			continue
		}

		out = append(out, a)
	}

	return out
}

// resilienceMMHost returns the host name the running minimega reports.
func resilienceMMHost() (string, error) {
	resp, err := mmRun("", "host name")
	if err != nil {
		return "", err
	}

	if len(resp) == 0 || resp[0].Host == "" {
		return "", errors.New("minimega did not report its host name")
	}

	return resp[0].Host, nil
}

// resilienceLaunch starts minimega with argv the way the E2E action does: as
// root, detached with setsid and nohup, with the original daemon's environment
// and working directory, appending its stdout and stderr to the original
// file. With hostname set the daemon runs in a new UTS namespace under that
// host name. It returns once the command socket answers. It refuses to start
// a second daemon for cfg.mmBase: the version check could then be answered by
// the old one.
func resilienceLaunch(d resilienceDaemon, argv []string, hostname string, timeout time.Duration) error {
	running, err := resilienceDaemonPIDs()
	if err != nil {
		return err
	}

	if len(running) > 0 {
		return fmt.Errorf("minimega (pid %v) still serves %s; stop it before starting another", running, cfg.mmBase)
	}

	launcher := argv
	if hostname != "" {
		launcher = slices.Concat([]string{"unshare", "--uts", "--", "sh", "-c", `hostname "$0" && exec "$@"`, hostname}, argv)
	}

	script := `out=$1; shift; setsid nohup "$@" >>"$out" 2>&1 </dev/null &`

	cmd := exec.Command("bash", slices.Concat([]string{"-c", script, "bash", d.stdout}, launcher)...) //nolint:gosec // test harness
	cmd.Env = d.env
	cmd.Dir = d.dir

	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("starting minimega: %w\n%s", err, out)
	}

	started := time.Now()

	for {
		_, err := mmRun("", "version")
		if err == nil {
			return nil
		}

		if time.Since(started) > timeout {
			return fmt.Errorf("minimega did not answer within %v: %w\n%s", timeout, err, resilienceTails(d))
		}

		if time.Since(started) > resilienceEarlyExit {
			if _, perr := resilienceDaemonPID(); perr != nil {
				return fmt.Errorf("minimega exited while starting (%w): %w\n%s", perr, err, resilienceTails(d))
			}
		}

		time.Sleep(resiliencePoll)
	}
}

// resilienceTails returns the end of minimega's output and log files.
func resilienceTails(d resilienceDaemon) string {
	return fmt.Sprintf("--- %s\n%s\n--- %s\n%s", d.stdout, resilienceTail(d.stdout), d.logfile, resilienceTail(d.logfile))
}

func resilienceTail(path string) string {
	data, err := os.ReadFile(path) //nolint:gosec // test harness
	if err != nil {
		return fmt.Sprintf("(%v)", err)
	}

	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) > resilienceTailLines {
		lines = lines[len(lines)-resilienceTailLines:]
	}

	return strings.Join(lines, "\n")
}

// resilienceStart starts minimega with argv (see resilienceLaunch) and checks
// the host name it reports.
func resilienceStart(t *testing.T, argv []string, hostname string) {
	t.Helper()

	d := resilienceOriginal(t)

	if err := resilienceLaunch(d, argv, hostname, resilienceUpWait); err != nil {
		t.Fatal(err)
	}

	want := d.host
	if hostname != "" {
		want = hostname
	}

	if got, err := resilienceMMHost(); err != nil || got != want {
		t.Fatalf("the restarted minimega reports host name %q (%v), want %q", got, err, want)
	}
}

// resilienceStop sends sig to the minimega daemon, waits for it to exit and
// returns how long that took.
func resilienceStop(t *testing.T, sig syscall.Signal) time.Duration {
	t.Helper()

	pid, err := resilienceDaemonPID()
	if err != nil {
		t.Fatal(err)
	}

	start := time.Now()

	if err := syscall.Kill(pid, sig); err != nil {
		t.Fatalf("signalling minimega (pid %d): %v", pid, err)
	}

	eventually(t, resilienceExitWait, resiliencePoll, fmt.Sprintf("minimega (pid %d) to exit after %v", pid, sig), func() (bool, string) {
		return !resilienceAlive(pid), "still running"
	})

	return time.Since(start)
}

func resilienceWaitExit(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)

	for resilienceAlive(pid) {
		if time.Now().After(deadline) {
			return false
		}

		time.Sleep(resiliencePoll)
	}

	return true
}

// resilienceKill stops any minimega daemon for cfg.mmBase, gracefully first,
// and removes the socket a killed daemon leaves behind, as -force would.
func resilienceKill(t *testing.T) {
	t.Helper()

	pids, err := resilienceDaemonPIDs()
	if err != nil {
		t.Fatal(err)
	}

	for _, pid := range pids {
		_ = syscall.Kill(pid, syscall.SIGTERM)

		if !resilienceWaitExit(pid, resilienceExitWait) {
			_ = syscall.Kill(pid, syscall.SIGKILL)

			if !resilienceWaitExit(pid, resilienceQEMUWait) {
				t.Fatalf("minimega (pid %d) does not exit", pid)
			}
		}
	}

	_ = os.Remove(filepath.Join(cfg.mmBase, "minimega"))
}

// resilienceIsOriginal reports whether the running daemon is the original
// one: same binary and flags, same UTS namespace, and answering.
func resilienceIsOriginal(d resilienceDaemon) bool {
	pid, err := resilienceDaemonPID()
	if err != nil {
		return false
	}

	argv, err := resilienceCmdline(pid)
	if err != nil || !slices.Equal(argv[1:], d.argv[1:]) {
		return false
	}

	proc := filepath.Join("/proc", strconv.Itoa(pid))

	if exe, err := os.Readlink(filepath.Join(proc, "exe")); err != nil || exe != d.exe {
		return false
	}

	if uts, err := os.Readlink(filepath.Join(proc, "ns", "uts")); err != nil || uts != d.uts {
		return false
	}

	_, err = mmRun("", "version")

	return err == nil
}

// resilienceRestore makes sure the original daemon runs again and that phenix
// ui talks to it, so the tests after this one are not affected.
func resilienceRestore(t *testing.T) {
	t.Helper()

	d := resilienceOriginal(t)

	if !resilienceIsOriginal(d) {
		resilienceKill(t)
		resilienceStart(t, d.argv, "")
	}

	resilienceWarmUI(t)
}

// resilienceEnsureUp starts the original daemon when no daemon answers, so an
// experiment's teardown can reach minimega after a test failed mid-restart.
func resilienceEnsureUp(t *testing.T) {
	t.Helper()

	if _, err := mmRun("", "version"); err == nil {
		return
	}

	resilienceKill(t)
	resilienceStart(t, resilienceOriginal(t).argv, "")
	resilienceWarmUI(t)
}

// resilienceWarmUI waits until phenix ui answers a request that needs
// minimega, which replaces a connection the UI holds to an old daemon.
func resilienceWarmUI(t *testing.T) {
	t.Helper()

	eventually(t, resilienceUpWait, time.Second, "phenix ui to reach minimega (GET /hosts)", func() (bool, string) {
		code, body := api(t, http.MethodGet, "/hosts", nil)

		return code == http.StatusOK, fmt.Sprintf("%d %s", code, bytes.TrimSpace(body))
	})
}

// resilienceDebugLog raises minimega's log level to debug while fn runs and
// returns the requests minimega logged as received over its socket.
func resilienceDebugLog(t *testing.T, fn func()) []string {
	t.Helper()

	d := resilienceOriginal(t)

	st, err := os.Stat(d.logfile)
	if err != nil {
		t.Fatalf("minimega log %s is required: %v", d.logfile, err)
	}

	level := strings.TrimSpace(mmText(t, "", "log level"))
	if level == "" {
		level = "info"
	}

	restore := func() { _, _ = mmRun("", "log level "+level) }

	t.Cleanup(restore)
	mm(t, "", "log level debug")

	fn()

	restore()

	f, err := os.Open(d.logfile)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	if _, err := f.Seek(st.Size(), io.SeekStart); err != nil {
		t.Fatal(err)
	}

	data, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}

	var requests []string

	for line := range strings.SplitSeq(string(data), "\n") {
		if _, req, ok := strings.Cut(line, "got request over socket: "); ok {
			requests = append(requests, req)
		}
	}

	return requests
}

// ---------------------------------------------------------------------------
// VMs, QEMU processes and orphans

// resilienceVM is a VM row of minimega's vm info.
type resilienceVM struct {
	uuid, state, host string
	taps              []string
}

func resilienceVMs(t *testing.T, ns string) map[string]resilienceVM {
	t.Helper()

	out := make(map[string]resilienceVM)

	for name, row := range vmInfo(t, ns) {
		out[name] = resilienceVM{
			uuid:  row["uuid"],
			state: row["state"],
			host:  row["host"],
			taps:  splitList(row["tap"]),
		}
	}

	return out
}

// resilienceNamespace reports minimega's row for namespace ns without entering
// (and so creating) it.
func resilienceNamespace(t *testing.T, ns string) (map[string]string, bool) {
	t.Helper()

	row, ok := mmNamespaces(t)[ns]

	return row, ok
}

// resilienceWantNoNamespace checks that minimega holds no namespace ns.
func resilienceWantNoNamespace(t *testing.T, ns, when string) {
	t.Helper()

	if row, ok := resilienceNamespace(t, ns); ok {
		t.Errorf("minimega still holds namespace %s %s: %v", ns, when, row)
	}
}

// resilienceProc is a QEMU process minimega launched under cfg.mmBase.
type resilienceProc struct {
	pid     int
	comm    string // kernel process name
	uuid    string // -uuid
	id      string // -name, minimega's VM id
	cmdline []string
}

// resilienceQEMU returns the live QEMU processes running under cfg.mmBase,
// keyed by VM UUID.
func resilienceQEMU(t *testing.T) map[string]resilienceProc {
	t.Helper()

	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatal(err)
	}

	out := make(map[string]resilienceProc)

	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}

		if p, ok := resilienceQEMUProc(pid); ok {
			out[p.uuid] = p
		}
	}

	return out
}

func resilienceQEMUProc(pid int) (resilienceProc, bool) {
	argv, err := resilienceCmdline(pid)
	if err != nil || !resilienceAlive(pid) {
		return resilienceProc{}, false
	}

	if !minimegaQEMU(argv) {
		return resilienceProc{}, false
	}

	p := resilienceProc{pid: pid, cmdline: argv}

	for i := 0; i+1 < len(argv); i++ {
		switch argv[i] {
		case "-uuid":
			p.uuid = argv[i+1]
		case "-name":
			p.id = argv[i+1]
		}
	}

	if p.uuid == "" {
		return resilienceProc{}, false
	}

	if comm, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "comm")); err == nil {
		p.comm = strings.TrimSpace(string(comm))
	}

	return p, true
}

// resilienceUses reports whether a QEMU process uses tap or a path under dir.
func resilienceUses(p resilienceProc, tap, dir string) bool {
	for _, arg := range p.cmdline {
		if dir != "" && strings.Contains(arg, dir+"/") {
			return true
		}

		if tap == "" {
			continue
		}

		for field := range strings.SplitSeq(arg, ",") {
			if field == "ifname="+tap {
				return true
			}
		}
	}

	return false
}

// resilienceUsedBy reports whether any of the QEMU processes uses tap or a
// path under dir.
func resilienceUsedBy(procs map[string]resilienceProc, tap, dir string) bool {
	for _, p := range procs {
		if resilienceUses(p, tap, dir) {
			return true
		}
	}

	return false
}

// resilienceOrphans tracks the QEMU processes and taps of an experiment's VMs,
// so a test that kills minimega can remove what no daemon knows about anymore.
type resilienceOrphans struct {
	ns    string
	procs map[string]resilienceProc // by VM UUID
	names map[string]string         // VM name by VM UUID
	taps  map[string]bool
}

func newResilienceOrphans(ns string) *resilienceOrphans {
	return &resilienceOrphans{
		ns:    ns,
		procs: make(map[string]resilienceProc),
		names: make(map[string]string),
		taps:  make(map[string]bool),
	}
}

// track records the QEMU process and taps of every VM in vms and fails the
// test when a VM has no QEMU process.
func (o *resilienceOrphans) track(t *testing.T, vms map[string]resilienceVM) {
	t.Helper()

	procs := resilienceQEMU(t)

	for name, vm := range vms {
		p, ok := procs[vm.uuid]
		if !ok {
			t.Fatalf("no QEMU process runs VM %s (uuid %s)", name, vm.uuid)
		}

		o.procs[vm.uuid] = p
		o.names[vm.uuid] = name

		for _, tap := range vm.taps {
			o.taps[tap] = true
		}
	}
}

// alive returns the tracked QEMU processes that still run.
func (o *resilienceOrphans) alive() []resilienceProc {
	var out []resilienceProc

	for _, p := range o.procs {
		if q, ok := resilienceQEMUProc(p.pid); ok && q.uuid == p.uuid {
			out = append(out, p)
		}
	}

	return out
}

// waitGone waits up to resilienceQEMUWait for the tracked QEMU processes to
// exit and returns those still running.
func (o *resilienceOrphans) waitGone() []resilienceProc {
	deadline := time.Now().Add(resilienceQEMUWait)

	for {
		left := o.alive()
		if len(left) == 0 || time.Now().After(deadline) {
			return left
		}

		time.Sleep(resiliencePoll)
	}
}

// reap kills the tracked QEMU processes that still run, then removes their
// taps, instance directories and namespace links unless a live VM uses them.
func (o *resilienceOrphans) reap(t *testing.T) {
	t.Helper()

	for _, p := range o.alive() {
		t.Logf("killing orphaned QEMU pid %d (VM %s, uuid %s)", p.pid, o.names[p.uuid], p.uuid)

		_ = syscall.Kill(p.pid, syscall.SIGKILL)
	}

	if left := o.waitGone(); len(left) > 0 {
		t.Errorf("%d orphaned QEMU processes survive SIGKILL", len(left))
	}

	live := resilienceQEMU(t)

	for tap := range o.taps {
		if _, err := os.Stat(filepath.Join("/sys/class/net", tap)); err != nil {
			continue
		}

		if resilienceUsedBy(live, tap, "") {
			continue
		}

		_ = run("ovs-vsctl", "--if-exists", "del-port", tap)
		_ = run("ip", "link", "del", tap)
	}

	for uuid, p := range o.procs {
		if _, ok := live[uuid]; ok {
			continue
		}

		_ = os.Remove(filepath.Join(cfg.mmBase, "namespaces", o.ns, uuid))

		if _, err := strconv.Atoi(p.id); err != nil {
			continue
		}

		dir := filepath.Join(cfg.mmBase, p.id)

		name, err := os.ReadFile(filepath.Join(dir, "name")) //nolint:gosec // test harness
		if err != nil || strings.TrimSpace(string(name)) != o.names[uuid] {
			continue
		}

		if !resilienceUsedBy(live, "", dir) {
			_ = os.RemoveAll(dir)
		}
	}

	clear(o.procs)
	clear(o.names)
	clear(o.taps)
}

// ---------------------------------------------------------------------------
// REST

// resilienceAPIHost is a cluster host as GET /hosts returns it.
type resilienceAPIHost struct {
	Name        string `json:"name"`
	Headnode    bool   `json:"headnode"`
	Schedulable bool   `json:"schedulable"`
	DiskUsage   struct {
		Phenix   float64 `json:"diskphenix"`
		Minimega float64 `json:"diskminimega"`
	} `json:"diskusage"`
}

// resilienceAPIVM is a VM as GET /experiments/{name}/vms returns it.
type resilienceAPIVM struct {
	Name    string `json:"name"`
	Host    string `json:"host"`
	State   string `json:"state"`
	Running bool   `json:"running"`
	Disk    string `json:"disk"`
}

type resilienceAPIVMList struct {
	VMs   []resilienceAPIVM `json:"vms"`
	Total int               `json:"total"`
}

func resilienceGetHosts(t *testing.T) (int, []resilienceAPIHost, []byte) {
	t.Helper()

	code, body := api(t, http.MethodGet, "/hosts", nil)

	var cluster struct {
		Hosts []resilienceAPIHost `json:"hosts"`
	}

	if code == http.StatusOK {
		if err := json.Unmarshal(body, &cluster); err != nil {
			t.Fatalf("decoding GET /hosts %s: %v", body, err)
		}
	}

	return code, cluster.Hosts, bytes.TrimSpace(body)
}

// resilienceUIHost returns the one host phenix ui reports.
func resilienceUIHost(t *testing.T) string {
	t.Helper()

	code, hosts, body := resilienceGetHosts(t)
	if code != http.StatusOK || len(hosts) != 1 {
		t.Fatalf("GET /hosts = %d %s, want one host", code, body)
	}

	return hosts[0].Name
}

// resilienceWantUIHost checks that phenix ui reaches a restarted minimega
// within resilienceUIAttempts requests one second apart, with no failure (the
// command that finds the connection to the old daemon dead is sent again on a
// new one), and reports the host it reported before.
func resilienceWantUIHost(t *testing.T, want string) {
	t.Helper()

	failures := 0

	for range resilienceUIAttempts {
		code, hosts, body := resilienceGetHosts(t)
		if code == http.StatusOK {
			if len(hosts) != 1 || hosts[0].Name != want {
				t.Errorf("GET /hosts after the restart lists %+v, want only %q", hosts, want)
			}

			if failures > 0 {
				t.Errorf("GET /hosts failed %d times before it succeeded, want none", failures)
			}

			return
		}

		failures++

		t.Logf("GET /hosts after the restart: %d %s", code, body)
		time.Sleep(time.Second)
	}

	t.Errorf("GET /hosts still fails after %d attempts one second apart", resilienceUIAttempts)
}

func resilienceRESTVMs(t *testing.T, exp string) (int, resilienceAPIVMList, []byte) {
	t.Helper()

	code, body := api(t, http.MethodGet, "/experiments/"+exp+"/vms", nil)

	var list resilienceAPIVMList

	if code == http.StatusOK {
		if err := json.Unmarshal(body, &list); err != nil {
			t.Fatalf("decoding GET /experiments/%s/vms %s: %v", exp, body, err)
		}
	}

	return code, list, bytes.TrimSpace(body)
}

// ---------------------------------------------------------------------------
// topology and files

// resilienceTopology is a topology of hollow Linux nodes on VLAN LAN.
func resilienceTopology(vms ...string) string {
	nodes := make([]string, 0, len(vms))

	for i, vm := range vms {
		nodes = append(nodes, hollowNode(vm, "LAN", fmt.Sprintf("10.66.0.%d", i+1), ""))
	}

	return topology(nodes...)
}

// resilienceSnapshot is the injection snapshot phenix names after the
// headnode it found.
func resilienceSnapshot(head, exp, vm string) string {
	return filepath.Join(cfg.imagesDir, fmt.Sprintf("%s_%s_%s_snapshot", head, exp, vm))
}

// resilienceListsAll reports whether phenix CLI output names every VM.
func resilienceListsAll(out string, vms []string) bool {
	words := strings.FieldsFunc(out, func(r rune) bool { return unicode.IsSpace(r) || r == '|' })

	for _, vm := range vms {
		if !slices.Contains(words, vm) {
			return false
		}
	}

	return true
}

// resilienceHostsEntry maps host to 127.0.0.1 in /etc/hosts until the test
// ends.
func resilienceHostsEntry(t *testing.T, host string) {
	t.Helper()

	st, err := os.Stat(resilienceHostsFile)
	if err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(resilienceHostsFile)
	if err != nil {
		t.Fatal(err)
	}

	content := string(data)
	if content != "" && !strings.HasSuffix(content, "\n") {
		content += "\n"
	}

	t.Cleanup(func() {
		data, err := os.ReadFile(resilienceHostsFile)
		if err != nil {
			t.Errorf("cleanup: reading %s: %v", resilienceHostsFile, err)

			return
		}

		var keep []string

		for line := range strings.SplitAfterSeq(string(data), "\n") {
			if !strings.Contains(line, resilienceHostsMarker) {
				keep = append(keep, line)
			}
		}

		if err := os.WriteFile(resilienceHostsFile, []byte(strings.Join(keep, "")), st.Mode().Perm()); err != nil {
			t.Errorf("cleanup: restoring %s: %v", resilienceHostsFile, err)
		}
	})

	content += "127.0.0.1 " + host + " " + resilienceHostsMarker + "\n"

	if err := os.WriteFile(resilienceHostsFile, []byte(content), st.Mode().Perm()); err != nil {
		t.Fatal(err)
	}

	eventually(t, resilienceUpWait, time.Second, host+" to resolve", func() (bool, string) {
		addrs, err := net.DefaultResolver.LookupHost(t.Context(), host)

		return err == nil && slices.Contains(addrs, "127.0.0.1"), fmt.Sprintf("%v %v", addrs, err)
	})
}

// resilienceRemoveSnapshots removes the injection snapshots named after the
// suffixed headnode, which phenix cannot delete once minimega runs under its
// usual host name again, and any C2 files staged for the experiment.
func resilienceRemoveSnapshots(t *testing.T, exp string) {
	t.Helper()

	matches, _ := filepath.Glob(filepath.Join(cfg.imagesDir, resilienceTrimmedHost+"*_"+exp+"_*_snapshot"))

	for _, m := range matches {
		if err := os.Remove(m); err == nil {
			t.Logf("removed leftover snapshot %s", m)
		}
	}

	_ = os.RemoveAll(filepath.Join(cfg.imagesDir, exp))
}

// ---------------------------------------------------------------------------
// F3-06: graceful restart

// TestResilienceGracefulRestart covers F3-06: minimega shut down with SIGTERM
// and started again with the same flags while an experiment runs and phenix
// ui stays up.
func TestResilienceGracefulRestart(t *testing.T) {
	hollowImages(t)

	const name = "e2e-rs-term"

	var (
		daemon  = resilienceOriginal(t)
		vms     = []string{"term1", "term2", "term3"}
		orphans = newResilienceOrphans(name)
	)

	// Cleanups run last in, first out: a daemon answers again before the
	// experiment is torn down, it is the original one afterwards, and QEMU
	// processes that outlived their daemon go last.
	t.Cleanup(func() { orphans.reap(t) })
	t.Cleanup(func() { resilienceRestore(t) })

	exp := newExperiment(t, name, resilienceTopology(vms...), "")

	t.Cleanup(func() { resilienceEnsureUp(t) })

	head := resilienceUIHost(t)

	t.Run("F3-06 the UI redials its stale minimega connection", func(t *testing.T) {
		resilienceStop(t, syscall.SIGTERM)
		resilienceStart(t, daemon.argv, "")

		// The UI has not talked to minimega since the restart, so this is the
		// first command on the connection it opened to the old daemon.
		code, body := api(t, http.MethodGet, "/hosts", nil)

		if code != http.StatusOK {
			t.Errorf("first GET /hosts after a minimega restart = %d %s, want 200", code, bytes.TrimSpace(body))
		}

		resilienceWantUIHost(t, head)
	})

	exp.start()
	exp.waitRunning(resilienceVMWait, vms...)

	before := resilienceVMs(t, name)
	orphans.track(t, before)
	resilienceUIHost(t) // The UI holds a live connection again.

	t.Run("F3-06 SIGTERM tears down every VM", func(t *testing.T) {
		took := resilienceStop(t, syscall.SIGTERM)
		t.Logf("minimega exited %v after SIGTERM", took.Round(time.Millisecond))

		if left := orphans.waitGone(); len(left) > 0 {
			t.Errorf("%d QEMU processes of %s survive minimega's teardown", len(left), name)
		}

		_, err := os.Stat(filepath.Join(cfg.mmBase, "minimega"))
		t.Logf("command socket after the graceful shutdown: %v", err)
	})

	t.Run("F3-06 phenix while minimega is down", func(t *testing.T) { resilienceCheckDown(t, exp) })

	resilienceStart(t, daemon.argv, "")

	t.Run("F3-06 the UI reconnects after the restart", func(t *testing.T) { resilienceWantUIHost(t, head) })

	t.Run("F3-06 the experiment starts again on the new daemon", func(t *testing.T) {
		resilienceCheckRestarted(t, exp, daemon, vms, before)
	})
}

// resilienceCheckDown checks what phenix reports for a running experiment
// while no minimega answers.
func resilienceCheckDown(t *testing.T, exp *experiment) {
	t.Helper()

	if pids, err := resilienceDaemonPIDs(); err != nil || len(pids) > 0 {
		t.Fatalf("minimega must be down for these checks: daemons %v (%v)", pids, err)
	}

	// GetClusterHosts reports why it found no hosts.
	if code, _, body := resilienceGetHosts(t); code != http.StatusInternalServerError || !bytes.Contains(body, []byte("unable to dial")) {
		t.Errorf("GET /hosts with minimega down = %d %s, want 500 reporting that minimega cannot be dialed", code, body)
	}

	if !exp.running() {
		t.Error("phenix marked the experiment stopped while minimega was down")
	}

	// vm.List names the experiment it could not ask minimega about.
	want := "getting VMs of " + exp.name + " from minimega"

	code, _, body := resilienceRESTVMs(t, exp.name)

	if code != http.StatusInternalServerError || !bytes.Contains(body, []byte(want)) {
		t.Errorf("GET /experiments/%s/vms with minimega down = %d %s, want 500 %q", exp.name, code, body, want)
	}

	out, err := phenixRun("vm", "info", exp.name)

	if err == nil || !strings.Contains(out, want) {
		t.Errorf("phenix vm info %s with minimega down = %v, want a failure reporting %q:\n%s", exp.name, err, want, out)
	}
}

// resilienceCheckRestarted stops the experiment the restart left without VMs
// and starts it again through the UI, whose minimega connection lived through
// the restart.
func resilienceCheckRestarted(t *testing.T, exp *experiment, daemon resilienceDaemon, vms []string, before map[string]resilienceVM) {
	t.Helper()

	// Fail this subtest, not the test that created the experiment: that would
	// skip every later subtest.
	exp = &experiment{t: t, name: exp.name}

	row, ok := resilienceNamespace(t, exp.name)
	t.Logf("after the restart minimega holds namespace %s: %v %v", exp.name, ok, row)

	exp.stop()

	if st := exp.doc().Status.StartTime; st != "" {
		t.Errorf("status.startTime = %q after stop, want empty", st)
	}

	apiOK(t, http.MethodPost, "/experiments/"+exp.name+"/start", nil, nil)
	exp.waitRunning(resilienceVMWait, vms...)

	var (
		after = resilienceVMs(t, exp.name)
		doc   = exp.doc()
	)

	for _, vm := range vms {
		if after[vm].uuid == before[vm].uuid {
			t.Errorf("%s kept uuid %s across the restart, want a new VM", vm, before[vm].uuid)
		}

		if after[vm].host != daemon.host || doc.Status.Schedules[vm] != daemon.host {
			t.Errorf("%s runs on %q, status.schedules says %q, want %q", vm, after[vm].host, doc.Status.Schedules[vm], daemon.host)
		}

		// The UI named the snapshot after the headnode it asked the new
		// daemon for; an empty Headnode() would give `_<exp>_<vm>_snapshot`.
		if _, err := os.Stat(resilienceSnapshot(trimHostSuffixes(daemon.host), exp.name, vm)); err != nil {
			t.Errorf("injection snapshot of %s: %v", vm, err)
		}
	}
}

// ---------------------------------------------------------------------------
// F3-07: SIGKILL and -recover

// TestResilienceKillRecover covers F3-07: minimega killed with SIGKILL while an
// experiment runs, started with -recover, and later killed and started with
// its original -force. It characterizes what minimega rebuilds and checks that
// phenix reports minimega's view and can still stop and start the experiment.
func TestResilienceKillRecover(t *testing.T) {
	hollowImages(t)

	const name = "e2e-rs-kill"

	var (
		daemon  = resilienceOriginal(t)
		vms     = []string{"kill1", "kill2"}
		orphans = newResilienceOrphans(name)
	)

	t.Cleanup(func() { orphans.reap(t) })
	t.Cleanup(func() { resilienceRestore(t) })

	exp := newExperiment(t, name, resilienceTopology(vms...), "")

	t.Cleanup(func() { resilienceEnsureUp(t) })

	exp.start()
	exp.waitRunning(resilienceVMWait, vms...)

	before := resilienceVMs(t, name)
	orphans.track(t, before)

	for uuid, p := range orphans.procs {
		t.Logf("QEMU pid %d runs %s (id %s, uuid %s) as process %q", p.pid, orphans.names[uuid], p.id, uuid, p.comm)
	}

	head := resilienceUIHost(t)

	t.Run("F3-07 SIGKILL leaves the VMs running without minimega", func(t *testing.T) {
		resilienceStop(t, syscall.SIGKILL)

		_, err := os.Stat(filepath.Join(cfg.mmBase, "minimega"))
		t.Logf("command socket after SIGKILL: %v", err)

		// minimega: QEMU is not tied to the daemon (no parent-death signal), so it outlives a killed minimega.
		t.Logf("%d of %d QEMU processes outlived minimega", len(orphans.alive()), len(orphans.procs))
	})

	// Whether minimega came up with -recover: when it did not, nothing
	// reattached to the VMs, which then outlive the stop whatever their name.
	recovered := false

	t.Run("F3-07 minimega -recover", func(t *testing.T) {
		resilienceCheckRecover(t, exp, daemon, before, head, &recovered)
	})

	t.Run("F3-07 phenix stops the recovered experiment", func(t *testing.T) {
		exp := &experiment{t: t, name: name}

		exp.stop()

		resilienceWantNoNamespace(t, name, "after phenix stop")

		if st := exp.doc().Status.StartTime; st != "" {
			t.Errorf("status.startTime = %q after stop, want empty", st)
		}

		for _, p := range orphans.waitGone() {
			if !recovered {
				t.Logf("minimega: QEMU pid %d of %s outlived clear namespace; -recover did not come up, so nothing reattached to it",
					p.pid, orphans.names[p.uuid])

				continue
			}

			if p.comm == resilienceQEMUComm {
				t.Errorf("QEMU pid %d of %s, a process -recover reattaches to, outlived clear namespace", p.pid, orphans.names[p.uuid])

				continue
			}

			// minimega: -recover reattaches only to processes named qemu-system-x86, so a VM whose QEMU runs under another name (kvm) comes back QUIT and clear namespace leaves its QEMU running.
			t.Logf("minimega: QEMU pid %d of %s outlived clear namespace after the restart (process %q; -recover looks for %q)",
				p.pid, orphans.names[p.uuid], p.comm, resilienceQEMUComm)
		}

		orphans.reap(t)
	})

	t.Run("F3-07 a restart without -recover orphans the VMs", func(t *testing.T) {
		resilienceCheckForceRestart(t, exp, daemon, vms, orphans)
	})
}

// resilienceCheckRecover starts minimega with -recover after a SIGKILL, logs
// what it rebuilt, and checks that phenix reports exactly that. It sets *up
// once the daemon runs with -recover.
func resilienceCheckRecover(
	t *testing.T,
	exp *experiment,
	daemon resilienceDaemon,
	before map[string]resilienceVM,
	head string,
	up *bool,
) {
	t.Helper()

	name := exp.name
	argv := resilienceRecoverArgv(daemon.argv)

	// A daemon that survived the SIGKILL step would make the launch below
	// fail for a reason that says nothing about -recover.
	if pids, err := resilienceDaemonPIDs(); err != nil || len(pids) > 0 {
		t.Fatalf("minimega must be down before -recover: daemons %v (%v)", pids, err)
	}

	if err := resilienceLaunch(daemon, argv, "", resilienceRecoverWait); err != nil {
		resilienceKill(t)
		resilienceStart(t, daemon.argv, "")
		resilienceWarmUI(t)

		// minimega: a recovery error is fatal to the daemon (cmd/minimega/main.go "recovery failed"), so -recover can leave no daemon at all.
		t.Skipf("minimega: %s did not come up, so there is no recovered state to check: %v", strings.Join(argv[1:], " "), err)
	}

	*up = true

	recovered := resilienceVMs(t, name)

	for vm, b := range before {
		r, ok := recovered[vm]
		t.Logf("after -recover %s: listed %v, state %q, same uuid %v, taps %v (before: %s, taps %v)",
			vm, ok, r.state, r.uuid == b.uuid, r.taps, b.state, b.taps)
	}

	resilienceWantUIHost(t, head)

	code, list, body := resilienceRESTVMs(t, name)
	if code != http.StatusOK {
		t.Fatalf("GET /experiments/%s/vms after -recover = %d %s", name, code, body)
	}

	listed := make(map[string]resilienceAPIVM, len(list.VMs))
	for _, vm := range list.VMs {
		listed[vm.Name] = vm
	}

	for vm, r := range recovered {
		got, ok := listed[vm]
		if !ok {
			t.Errorf("GET /experiments/%s/vms omits %s, which minimega recovered as %s", name, vm, r.state)

			continue
		}

		if got.State != r.state || got.Running != (r.state == resilienceStateRunning) {
			t.Errorf("GET /experiments/%s/vms reports %s as %s (running %v), minimega as %s", name, vm, got.State, got.Running, r.state)
		}
	}

	if len(listed) != len(recovered) {
		t.Errorf("GET /experiments/%s/vms lists %d VMs, minimega recovered %d: %s", name, len(listed), len(recovered), body)
	}

	if out := phenix(t, "vm", "info", name); len(recovered) > 0 && !resilienceListsAll(out, slices.Collect(maps.Keys(recovered))) {
		t.Errorf("phenix vm info %s does not list the recovered VMs:\n%s", name, out)
	}
}

// resilienceCheckForceRestart starts the experiment, kills minimega and starts
// it with its original flags (-force, no -recover). minimega then knows none
// of the VMs, whose QEMU processes and taps live on; phenix must still be able
// to stop the experiment and start it again or fail cleanly.
func resilienceCheckForceRestart(t *testing.T, exp *experiment, daemon resilienceDaemon, vms []string, orphans *resilienceOrphans) {
	t.Helper()

	// Fail this subtest, not the test that created the experiment: that would
	// skip every later subtest.
	exp = &experiment{t: t, name: exp.name}

	exp.start()
	exp.waitRunning(resilienceVMWait, vms...)

	before := resilienceVMs(t, exp.name)
	orphans.track(t, before)

	resilienceStop(t, syscall.SIGKILL)
	resilienceStart(t, daemon.argv, "")
	resilienceWarmUI(t)

	row, ok := resilienceNamespace(t, exp.name)
	t.Logf("after a -force restart minimega holds namespace %s: %v %v; %d of %d QEMU processes still run",
		exp.name, ok, row, len(orphans.alive()), len(before))

	for tap := range orphans.taps {
		br, err := ovsVsctl(t, "port-to-br", tap)
		t.Logf("orphaned tap %s: bridge %q (%v)", tap, br, err)
	}

	// The VMs are gone from minimega, but the stop must still succeed.
	exp.stop()

	if st := exp.doc().Status.StartTime; st != "" {
		t.Errorf("status.startTime = %q after stop, want empty", st)
	}

	// Next to the orphans a start either runs every VM or fails and leaves
	// nothing behind.
	out, err := phenixRun("experiment", "start", exp.name)
	if err != nil {
		t.Logf("experiment start next to the orphaned VMs failed: %v\n%s", err, out)

		if exp.running() {
			t.Errorf("the failed start left %s marked running", exp.name)
		}

		resilienceWantNoNamespace(t, exp.name, "after the failed start")

		return
	}

	exp.waitRunning(resilienceVMWait, vms...)

	live := resilienceQEMU(t)

	for vm, row := range resilienceVMs(t, exp.name) {
		for _, tap := range row.taps {
			// minimega: new tap names skip existing devices (internal/bridge/bridges.go NewBridges), so a VM never gets the tap of a live QEMU.
			for uuid, p := range live {
				if !strings.EqualFold(uuid, row.uuid) && resilienceUses(p, tap, "") {
					t.Errorf("%s took tap %s, which QEMU pid %d (uuid %s, VM %q) still uses", vm, tap, p.pid, uuid, orphans.names[uuid])
				}
			}
		}

		t.Logf("%s runs again as uuid %s with taps %v next to the orphans", vm, row.uuid, row.taps)
	}

	exp.stop()
}

// ---------------------------------------------------------------------------
// F3-02 with another -filepath

// TestResilienceFilesDir covers what F3-02 cannot on the CI runners, where
// minimega's -filepath is phenix's own fallback, <phenix base>/images: with
// minimega restarted on another -filepath, a phenix command must place the
// disks of a stopped experiment in the files directory minimega reports in
// `args`. A phenix command reads `args` once its bases are set and keeps
// minimega's answer for the life of the process (util/mm/file.go), so a CLI
// command started after the restart is asked, not the phenix ui started
// before it.
func TestResilienceFilesDir(t *testing.T) {
	hollowImages(t)

	const name = "e2e-rs-files"

	var (
		daemon = resilienceOriginal(t)
		// Created first, so it is removed after the original daemon is back.
		dir = t.TempDir()
	)

	t.Cleanup(func() { resilienceRestore(t) })

	exp := newExperiment(t, name, resilienceTopology("files1"), "")

	t.Cleanup(func() { resilienceEnsureUp(t) })

	resilienceStop(t, syscall.SIGTERM)
	resilienceStart(t, resilienceWithFlag(daemon.argv, "filepath", dir), "")

	if rows := mmTable(t, "", "args"); len(rows) != 1 || rows[0]["filepath"] != dir {
		t.Fatalf("minimega args = %v after the restart, want filepath %s", rows, dir)
	}

	want := filepath.Join(dir, ext4Image)

	if out := phenix(t, "vm", "info", exp.name); !strings.Contains(out, want) {
		t.Errorf("phenix vm info %s does not place files1's disk at %s, in minimega's -filepath:\n%s", exp.name, want, out)
	}

	resilienceRestore(t)
}

// ---------------------------------------------------------------------------
// F2-01 and F2-08 without -abssnapshot

// TestResilienceRelativeSnapshots covers F2-01 and F2-08 with minimega
// started without -abssnapshot, minimega's default, which the disk shard's
// daemon never runs without: an injection snapshot then records its image by
// a path relative to the files directory, and reset-disk must still give the
// VM an instance disk whose backing chain resolves, so that it starts again.
func TestResilienceRelativeSnapshots(t *testing.T) {
	hollowImages(t)

	const (
		name = "e2e-rs-rel"
		vm   = "rel1"
	)

	daemon := resilienceOriginal(t)

	t.Cleanup(func() {
		left, _ := filepath.Glob(filepath.Join(cfg.imagesDir, "*_"+name+"_*_snapshot"))
		for _, path := range left {
			_ = os.Remove(path)
		}

		_ = os.RemoveAll(filepath.Join(cfg.imagesDir, name))
	})
	t.Cleanup(func() { resilienceRestore(t) })

	resilienceStop(t, syscall.SIGTERM)
	resilienceStart(t, resilienceWithoutBoolFlag(daemon.argv, "abssnapshot"), "")
	resilienceWarmUI(t)

	if diskAbsSnapshot(t) {
		t.Fatal("minimega runs with -abssnapshot after a restart without it")
	}

	exp := newExperiment(t, name, resilienceTopology(vm), "")

	t.Cleanup(func() { resilienceEnsureUp(t) })

	exp.start()
	exp.waitRunning(resilienceVMWait, vm)

	var (
		snap = resilienceSnapshot(trimHostSuffixes(daemon.host), name, vm)
		base = filepath.Join(cfg.imagesDir, ext4Image)
	)

	t.Run("F2-01 the injection snapshot names its image relative to the files directory", func(t *testing.T) {
		if info := diskImgInfo(t, snap); info.BackingFilename != ext4Image || filepath.Clean(info.FullBackingFilename) != base {
			t.Errorf("%s is backed by %q (%q), want %q resolving to %s",
				snap, info.BackingFilename, info.FullBackingFilename, ext4Image, base)
		}
	})

	t.Run("F2-08 reset-disk", func(t *testing.T) {
		before := vmInfo(t, name)[vm]
		overlay := filepath.Join(cfg.mmBase, before["id"], "disk-0.qcow2")

		phenix(t, "vm", "reset-disk", name, vm)

		if row := diskWaitState(t, name, vm, resilienceStateRunning); row["id"] != before["id"] || row["pid"] == before["pid"] {
			t.Errorf("%s runs with id %s and QEMU pid %s after reset-disk, want the same VM (%s) relaunched (not pid %s)",
				vm, row["id"], row["pid"], before["id"], before["pid"])
		}

		chain := diskBackingChain(t, overlay)
		if len(chain) != 3 || filepath.Clean(chain[0].FullBackingFilename) != snap || filepath.Clean(chain[1].FullBackingFilename) != base {
			t.Errorf("the reset instance disk of %s has the backing chain %+v, want %s on %s on %s", vm, chain, overlay, snap, base)
		}
	})
}

// ---------------------------------------------------------------------------
// F3-08: suffixed minimega host name

// TestResilienceSuffixedHostname covers F3-08: minimega runs under a host name
// carrying phenix's default -minimega suffix, as in the compose deployment.
// phenix must trim the suffix where it names the headnode (GET /hosts,
// snapshot names, headnode checks) and keep minimega's own name where
// minimega or the network needs it (status.schedules, VNC, scheduling).
func TestResilienceSuffixedHostname(t *testing.T) {
	hollowImages(t)

	const name = "e2e-rs-uts"

	var (
		daemon = resilienceOriginal(t)
		vms    = []string{"uts1", "uts2"}
	)

	resilienceHostsEntry(t, resilienceSuffixedHost)
	t.Cleanup(func() { resilienceRemoveSnapshots(t, name) })
	t.Cleanup(func() { resilienceRestore(t) })

	resilienceStop(t, syscall.SIGTERM)
	resilienceStart(t, daemon.argv, resilienceSuffixedHost)
	resilienceWarmUI(t)

	t.Run("F3-08 minimega runs under its own host name", func(t *testing.T) { resilienceCheckUTS(t, daemon) })

	exp := newExperiment(t, name, resilienceTopology(vms...), "")

	t.Cleanup(func() { resilienceEnsureUp(t) })

	var (
		hostsCode int
		hosts     []resilienceAPIHost
		hostsBody []byte
		listCode  int
		list      resilienceAPIVMList
		listBody  []byte
	)

	requests := resilienceDebugLog(t, func() {
		exp.start()
		exp.waitRunning(resilienceVMWait, vms...)

		hostsCode, hosts, hostsBody = resilienceGetHosts(t)
		listCode, list, listBody = resilienceRESTVMs(t, name)

		phenix(t, "vm", "info", name)
	})

	t.Run("F3-08 GET /hosts trims the suffix", func(t *testing.T) {
		if hostsCode != http.StatusOK || len(hosts) != 1 {
			t.Fatalf("GET /hosts = %d %s, want one host", hostsCode, hostsBody)
		}

		resilienceCheckSuffixHost(t, hosts[0])
	})

	t.Run("F3-08 minimega's host name reaches status.schedules", func(t *testing.T) { resilienceCheckSuffixVMs(t, exp, vms) })

	t.Run("F3-08 snapshots are named after the trimmed headnode", func(t *testing.T) {
		for _, vm := range vms {
			if _, err := os.Stat(resilienceSnapshot(resilienceTrimmedHost, name, vm)); err != nil {
				t.Errorf("injection snapshot of %s: %v", vm, err)
			}

			if _, err := os.Stat(resilienceSnapshot(resilienceSuffixedHost, name, vm)); !os.IsNotExist(err) {
				t.Errorf("phenix named the snapshot of %s after the untrimmed host name (stat: %v)", vm, err)
			}
		}
	})

	t.Run("F3-08 phenix treats the suffixed host as the headnode", func(t *testing.T) {
		if listCode != http.StatusOK {
			t.Fatalf("GET /experiments/%s/vms = %d %s", name, listCode, listBody)
		}

		resilienceCheckSuffixHeadnode(t, requests, list, len(vms))
	})

	t.Run("F3-08 VNC reaches the console through the suffixed host", func(t *testing.T) { resilienceCheckVNC(t, name, vms[0]) })

	t.Run("F3-08 stop deletes the snapshots it named", func(t *testing.T) {
		(&experiment{t: t, name: name}).stop()

		for _, vm := range vms {
			if _, err := os.Stat(resilienceSnapshot(resilienceTrimmedHost, name, vm)); !os.IsNotExist(err) {
				t.Errorf("injection snapshot of %s was not deleted on stop (stat: %v)", vm, err)
			}
		}
	})

	t.Run("F3-08 round-robin schedules on minimega's host name", func(t *testing.T) { resilienceCheckRoundRobin(t, exp, vms) })

	t.Run("F3-08 the original minimega comes back", func(t *testing.T) {
		resilienceRestore(t)

		if !resilienceIsOriginal(daemon) {
			t.Fatal("the restored minimega does not run like the original")
		}

		if host, err := resilienceMMHost(); err != nil || host != daemon.host {
			t.Errorf("the restored minimega reports host %q (%v), want %q", host, err, daemon.host)
		}
	})
}

// resilienceCheckUTS checks that the daemon runs with the original flags in a
// UTS namespace of its own.
func resilienceCheckUTS(t *testing.T, daemon resilienceDaemon) {
	t.Helper()

	pid, err := resilienceDaemonPID()
	if err != nil {
		t.Fatal(err)
	}

	if argv, err := resilienceCmdline(pid); err != nil || !slices.Equal(argv[1:], daemon.argv[1:]) {
		t.Errorf("minimega runs as %q (%v), want the flags %q", argv, err, daemon.argv[1:])
	}

	if uts, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "ns", "uts")); err != nil || uts == daemon.uts {
		t.Errorf("minimega's UTS namespace is %q (%v), want one other than the runner's %q", uts, err, daemon.uts)
	}

	if host, err := resilienceMMHost(); err != nil || host != resilienceSuffixedHost {
		t.Errorf("minimega reports host %q (%v), want %q", host, err, resilienceSuffixedHost)
	}
}

// resilienceCheckSuffixHost checks the host phenix reports for a minimega
// running under the suffixed host name.
func resilienceCheckSuffixHost(t *testing.T, h resilienceAPIHost) {
	t.Helper()

	if h.Name != resilienceTrimmedHost || !h.Headnode || !h.Schedulable {
		t.Errorf("host %q headnode %v schedulable %v, want %q, true, true", h.Name, h.Headnode, h.Schedulable, resilienceTrimmedHost)
	}

	// Disk usage comes from `shell df` on the host phenix names; it is 0 when
	// phenix sent it through the mesh to a host minimega does not know.
	for path, usage := range map[string]float64{resiliencePhenixBase: h.DiskUsage.Phenix, cfg.mmBase: h.DiskUsage.Minimega} {
		if usage <= 0 || usage > 100 {
			t.Errorf("disk usage of %s = %v%%, want a percentage from df on the headnode", path, usage)
		}
	}
}

// resilienceCheckSuffixVMs checks that minimega places the VMs on its own
// host name and that phenix records that name as their placement.
func resilienceCheckSuffixVMs(t *testing.T, exp *experiment, vms []string) {
	t.Helper()

	// Fail this subtest, not the test that created the experiment: that would
	// skip every later subtest.
	exp = &experiment{t: t, name: exp.name}

	var (
		info = resilienceVMs(t, exp.name)
		doc  = exp.doc()
	)

	for _, vm := range vms {
		if info[vm].host != resilienceSuffixedHost {
			t.Errorf("minimega runs %s on %q, want %q", vm, info[vm].host, resilienceSuffixedHost)
		}

		// status.schedules is what minimega reports, so it stays untrimmed.
		if got := doc.Status.Schedules[vm]; got != resilienceSuffixedHost {
			t.Errorf("status.schedules[%s] = %q, want minimega's host name %q", vm, got, resilienceSuffixedHost)
		}
	}
}

// resilienceCheckSuffixHeadnode checks that phenix sent nothing to the headnode
// through the mesh, which it does when it fails to recognize the headnode, and
// that `disk info` answered locally (the REST disk is the base image).
func resilienceCheckSuffixHeadnode(t *testing.T, requests []string, list resilienceAPIVMList, want int) {
	t.Helper()

	if len(requests) == 0 {
		t.Fatal("minimega logged no requests at debug level")
	}

	meshAll := 0

	for _, r := range requests {
		if strings.Contains(r, "mesh send "+resilienceTrimmedHost) {
			t.Errorf("phenix sent a command to the headnode through the mesh: %s", r)
		}

		if strings.Contains(r, "mesh send all") {
			meshAll++
		}
	}

	t.Logf("minimega received %d requests while phenix started and listed the experiment, %d of them mesh send all", len(requests), meshAll)

	if list.Total != want || len(list.VMs) != want {
		t.Errorf("GET .../vms lists %d VMs (total %d), want %d", len(list.VMs), list.Total, want)
	}

	for _, vm := range list.VMs {
		if filepath.Base(vm.Disk) != ext4Image {
			t.Errorf("GET .../vms disk of %s = %q, want the base image %s from disk info on the headnode", vm.Name, vm.Disk, ext4Image)
		}

		t.Logf("GET .../vms reports %s on host %q", vm.Name, vm.Host)
	}
}

// resilienceCheckVNC checks the VNC page of a VM and that its websocket relays
// the VM's VNC server, which phenix dials at minimega's host name.
func resilienceCheckVNC(t *testing.T, exp, vm string) {
	t.Helper()

	path := "/experiments/" + exp + "/vms/" + vm + "/vnc"

	if code, body := api(t, http.MethodGet, path, nil); code != http.StatusOK {
		t.Fatalf("GET %s = %d %s", path, code, bytes.TrimSpace(body))
	}

	ws, _, err := websocket.DefaultDialer.DialContext(t.Context(), "ws://"+cfg.uiAddr+"/api/v1"+path+"/ws",
		http.Header{"Origin": []string{"http://" + cfg.uiAddr}})
	if err != nil {
		t.Fatalf("connecting to the VNC websocket of %s: %v", vm, err)
	}
	defer ws.Close()

	_ = ws.SetReadDeadline(time.Now().Add(resilienceUpWait))

	var got []byte

	for len(got) < len(resilienceRFB) {
		_, data, err := ws.ReadMessage()
		if err != nil {
			t.Fatalf("reading the VNC websocket of %s after %q (phenix dials %s:<vnc_port>): %v", vm, got, resilienceSuffixedHost, err)
		}

		got = append(got, data...)
	}

	if !bytes.HasPrefix(got, []byte(resilienceRFB)) {
		t.Errorf("the VNC websocket of %s starts with %q, want a VNC greeting %q", vm, got, resilienceRFB)
	}
}

// resilienceCheckRoundRobin schedules the stopped experiment with round-robin
// and starts it: minimega accepts only host names of the namespace in `vm
// config schedule` (cmd/minimega/vmconfig.go validSchedule), so phenix must
// schedule on minimega's own name, not the trimmed one.
func resilienceCheckRoundRobin(t *testing.T, exp *experiment, vms []string) {
	t.Helper()

	// Fail this subtest, not the test that created the experiment: that would
	// skip every later subtest.
	exp = &experiment{t: t, name: exp.name}

	phenix(t, "experiment", "schedule", exp.name, "round-robin")

	sched, _ := exp.doc().Spec["schedules"].(map[string]any)
	t.Logf("round-robin wrote spec.schedules %v", sched)

	out, err := phenixRun("experiment", "start", exp.name)

	if err != nil {
		t.Fatalf("experiment start after round-robin scheduling: %v\n%s", err, out)
	}

	exp.waitRunning(resilienceVMWait, vms...)

	for vm, row := range resilienceVMs(t, exp.name) {
		if row.host != resilienceSuffixedHost {
			t.Errorf("round-robin placed %s on %q, want %q", vm, row.host, resilienceSuffixedHost)
		}
	}

	exp.stop()
}
