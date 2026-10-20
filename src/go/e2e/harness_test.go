//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// config is read from the environment once, in TestMain.
type config struct {
	phenix    string // phenix binary
	store     string // store endpoint shared by the CLI and the UI
	mmBase    string // minimega -base directory, cleaned as minimega cleans it
	imagesDir string // minimega -filepath directory holding disk images
	workDir   string // scratch directory for configs and logs
	uiAddr    string // address phenix ui listens on
	guest     string // general-purpose guest image built from sceptre-phenix-images
	router    string // minirouter image built from sceptre-phenix-images
	tools     string // headless guest with the tools apps and SCORCH run (the e2e recipe)
}

var cfg config //nolint:gochecknoglobals // set once by TestMain

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}

	return def
}

func loadConfig() config {
	return config{
		phenix:    env("E2E_PHENIX", "/usr/local/bin/phenix"),
		store:     env("E2E_STORE", "bolt:///etc/phenix/store.bdb"),
		mmBase:    filepath.Clean(env("E2E_MM_BASE", "/tmp/minimega")),
		imagesDir: env("E2E_IMAGES_DIR", "/phenix/images"),
		workDir:   env("E2E_WORK_DIR", filepath.Join(os.TempDir(), "phenix-e2e")),
		uiAddr:    env("E2E_UI_ADDR", "127.0.0.1:3080"),
		guest:     env("E2E_GUEST_IMAGE", "ntp.qc2"),
		router:    env("E2E_ROUTER_IMAGE", "minirouter.qc2"),
		tools:     env("E2E_TOOLS_IMAGE", "e2e.qc2"),
	}
}

func TestMain(m *testing.M) {
	cfg = loadConfig()

	if os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "e2e: these tests must run as root (minimega, OVS, disk inspection)")
		os.Exit(1)
	}

	if err := os.MkdirAll(cfg.workDir, 0o750); err != nil {
		fmt.Fprintln(os.Stderr, "e2e: creating work dir:", err)
		os.Exit(1)
	}

	if _, err := mmRun("", "version"); err != nil {
		fmt.Fprintln(os.Stderr, "e2e: minimega is not reachable:", err)
		os.Exit(1)
	}

	stopUI, err := startUI()
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e: starting phenix ui:", err)
		os.Exit(1)
	}

	code := m.Run()

	stopUI()
	os.Exit(code)
}

// ---------------------------------------------------------------------------
// phenix CLI

const phenixTimeout = 10 * time.Minute

// phenixArgs gives the CLI and phenix ui the harness's store and the
// minimega base the harness talks to.
func phenixArgs(args ...string) []string {
	return append([]string{"--store.endpoint", cfg.store, "--base-dir.minimega", cfg.mmBase}, args...)
}

// phenixRun runs the phenix CLI and returns its combined output.
func phenixRun(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), phenixTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, cfg.phenix, phenixArgs(args...)...) //nolint:gosec // test harness
	out, err := cmd.CombinedOutput()

	return string(out), err
}

// phenixStdout runs the phenix CLI and returns its standard output alone, for
// commands whose output is parsed; on failure the error carries its stderr.
func phenixStdout(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), phenixTimeout)
	defer cancel()

	var stderr bytes.Buffer

	cmd := exec.CommandContext(ctx, cfg.phenix, phenixArgs(args...)...) //nolint:gosec // test harness
	cmd.Stderr = &stderr

	out, err := cmd.Output()
	if err != nil {
		return string(out), fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}

	return string(out), nil
}

// phenix runs the phenix CLI and fails the test when it exits nonzero.
func phenix(t *testing.T, args ...string) string {
	t.Helper()

	out, err := phenixRun(args...)
	if err != nil {
		t.Fatalf("phenix %s: %v\n%s", strings.Join(args, " "), err, out)
	}

	return out
}

// ---------------------------------------------------------------------------
// minimega, spoken to directly over its command socket so assertions do not
// depend on phenix's own client and parsers.

type mmResponse struct {
	Host     string          `json:"Host"`
	Response string          `json:"Response"`
	Header   []string        `json:"Header"`
	Tabular  [][]string      `json:"Tabular"`
	Error    string          `json:"Error"`
	Data     json.RawMessage `json:"Data"`
}

type mmReply struct {
	Resp   []mmResponse `json:"Resp"`
	More   bool         `json:"More"`
	Status bool         `json:"Status"`
}

// mmRun runs one minimega command, inside namespace ns when ns is not empty.
// Note that minimega creates a namespace that is named in a command, so
// checks that a namespace is gone must use mmNamespaces instead.
func mmRun(ns, command string) ([]mmResponse, error) {
	conn, err := net.DialTimeout("unix", filepath.Join(cfg.mmBase, "minimega"), 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("dialing minimega: %w", err)
	}
	defer conn.Close()

	_ = conn.SetDeadline(time.Now().Add(5 * time.Minute))

	if ns != "" {
		command = fmt.Sprintf("namespace %q %s", ns, command)
	}

	if err := json.NewEncoder(conn).Encode(map[string]any{"Command": command}); err != nil {
		return nil, fmt.Errorf("sending %q: %w", command, err)
	}

	var (
		dec  = json.NewDecoder(conn)
		resp []mmResponse
	)

	for {
		var r mmReply
		if err := dec.Decode(&r); err != nil {
			return resp, fmt.Errorf("reading reply to %q: %w", command, err)
		}

		if r.Status {
			continue
		}

		resp = append(resp, r.Resp...)

		if !r.More {
			break
		}
	}

	var errs []string

	for _, r := range resp {
		if r.Error != "" {
			errs = append(errs, r.Error)
		}
	}

	if len(errs) > 0 {
		return resp, fmt.Errorf("minimega %q: %s", command, strings.Join(errs, "; "))
	}

	return resp, nil
}

// mm runs a minimega command and fails the test on error.
func mm(t *testing.T, ns, command string) []mmResponse {
	t.Helper()

	resp, err := mmRun(ns, command)
	if err != nil {
		t.Fatal(err)
	}

	return resp
}

// mmRows flattens the tabular responses of a command into rows keyed by
// header, adding the responding host under "host" when the table lacks it.
func mmRows(resp []mmResponse) []map[string]string {
	var rows []map[string]string

	for _, r := range resp {
		for _, tr := range r.Tabular {
			row := make(map[string]string, len(r.Header)+1)

			for i, h := range r.Header {
				if i < len(tr) {
					row[h] = tr[i]
				}
			}

			if _, ok := row["host"]; !ok {
				row["host"] = r.Host
			}

			rows = append(rows, row)
		}
	}

	return rows
}

// mmTable runs a tabular minimega command and fails the test on error.
func mmTable(t *testing.T, ns, command string) []map[string]string {
	t.Helper()

	return mmRows(mm(t, ns, command))
}

// vmInfo returns minimega's vm info rows for namespace ns, keyed by VM name.
func vmInfo(t *testing.T, ns string) map[string]map[string]string {
	t.Helper()

	out := make(map[string]map[string]string)

	for _, row := range mmTable(t, ns, "vm info") {
		out[row["name"]] = row
	}

	return out
}

// ---------------------------------------------------------------------------
// waiting

// eventually polls cond every interval until it reports true or timeout
// passes, then fails the test with the last detail cond returned.
func eventually(t *testing.T, timeout, interval time.Duration, what string, cond func() (bool, string)) {
	t.Helper()

	var (
		deadline = time.Now().Add(timeout)
		detail   string
		ok       bool
	)

	for {
		ok, detail = cond()
		if ok {
			return
		}

		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s: %s", timeout, what, detail)
		}

		time.Sleep(interval)
	}
}

// ---------------------------------------------------------------------------
// long-running children

// childStart asks childStarter to start cmd.
type childStart struct {
	cmd *exec.Cmd
	err chan<- error
}

// childStarts is served by the one goroutine that starts every child
// startChild starts.
var childStarts = sync.OnceValue(func() chan<- childStart { //nolint:gochecknoglobals // one starter per binary
	starts := make(chan childStart)

	go childStarter(starts)

	return starts
})

// childStarter keeps its OS thread for the life of the test binary: Linux
// sends a child's parent-death signal when the thread that started the child
// exits, not the process, and the Go runtime ends a thread whose goroutine
// exits while it holds the thread.
func childStarter(starts <-chan childStart) {
	runtime.LockOSThread()

	for s := range starts {
		s.err <- s.cmd.Start()
	}
}

// startChild starts cmd, a child that can outlive the test that started it,
// so that it gets SIGTERM, which phenix handles as an interrupt, when the test
// binary dies. A test's panic ends the binary without TestMain's teardown,
// and -test.timeout, or a panic in a goroutine that is not a test's, ends it
// without any t.Cleanup either, which would leave the child running: a phenix
// ui that holds its address and the store, or a CLI that still changes an
// experiment the next run uses.
func startChild(cmd *exec.Cmd) error {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}

	cmd.SysProcAttr.Pdeathsig = syscall.SIGTERM

	errc := make(chan error, 1)

	childStarts() <- childStart{cmd: cmd, err: errc}

	return <-errc
}

// ---------------------------------------------------------------------------
// phenix ui and REST

func startUI() (func(), error) {
	// A listener left on the address (a phenix ui from an aborted run) would
	// answer the readiness probe while the new phenix ui fails to bind.
	if conn, err := net.DialTimeout("tcp", cfg.uiAddr, time.Second); err == nil {
		_ = conn.Close()

		return nil, fmt.Errorf("something already listens on %s; stop it or set E2E_UI_ADDR", cfg.uiAddr)
	}

	logPath := filepath.Join(cfg.workDir, "phenix-ui.log")

	logFile, err := os.Create(logPath) //nolint:gosec // test harness
	if err != nil {
		return nil, err
	}

	cmd := exec.Command(cfg.phenix, phenixArgs("ui", "--listen-endpoint", cfg.uiAddr)...) //nolint:gosec // test harness
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	if err := startChild(cmd); err != nil {
		_ = logFile.Close()

		return nil, err
	}

	// Waited for from the start, so that a phenix ui that exits (for example
	// because another process holds the address) fails the startup instead of
	// leaving the tests to run against whatever answers there.
	exited := make(chan struct{})

	go func() {
		_ = cmd.Wait()

		close(exited)
	}()

	var stopping atomic.Bool

	stop := func() {
		stopping.Store(true)

		_ = cmd.Process.Signal(os.Interrupt)

		select {
		case <-exited:
		case <-time.After(15 * time.Second):
			_ = cmd.Process.Kill()

			<-exited
		}

		_ = logFile.Close()
	}

	// TestMain runs this before -test.timeout is armed, so every probe has
	// its own timeout.
	probe := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(60 * time.Second)

	for time.Now().Before(deadline) {
		select {
		case <-exited:
			_ = logFile.Close()

			return nil, fmt.Errorf("phenix ui exited while starting (%s); see %s", cmd.ProcessState, logPath)
		default:
		}

		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://"+cfg.uiAddr+"/version", nil)
		if err != nil {
			stop()

			return nil, err
		}

		resp, err := probe.Do(req)
		if err == nil {
			_ = resp.Body.Close()

			if resp.StatusCode == http.StatusOK {
				// Every REST and websocket call after phenix ui died fails
				// with "connection refused"; say once why.
				go func() {
					<-exited

					if !stopping.Load() {
						fmt.Fprintf(
							os.Stderr,
							"e2e: phenix ui exited during the tests (%s), so every later REST and websocket call fails; see %s\n",
							cmd.ProcessState,
							logPath,
						)
					}
				}()

				return stop, nil
			}
		}

		time.Sleep(500 * time.Millisecond)
	}

	stop()

	return nil, fmt.Errorf("phenix ui did not answer on %s; see %s", cfg.uiAddr, logPath)
}

// ---------------------------------------------------------------------------
// files

// writeFile writes content under the work directory and returns its path.
func writeFile(t *testing.T, name, content string) string {
	t.Helper()

	path := filepath.Join(cfg.workDir, t.Name(), name)

	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	return path
}

// requireImage fails the test when a disk image the test needs is missing.
// Images are built from sceptre-phenix-images by the E2E workflow; a missing
// image is a harness failure, never a reason to skip.
func requireImage(t *testing.T, name string) {
	t.Helper()

	if _, err := os.Stat(filepath.Join(cfg.imagesDir, name)); err != nil {
		t.Fatalf("disk image %s is required: %v", name, err)
	}
}
