//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
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

// phenixProc is a phenix CLI process the test does not wait for at once.
type phenixProc struct {
	mu   sync.Mutex
	out  bytes.Buffer
	done chan struct{}
	err  error
}

func (p *phenixProc) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.out.Write(b)
}

func (p *phenixProc) finished() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

// wait waits for the process and returns its output and exit error.
func (p *phenixProc) wait(t *testing.T, timeout time.Duration) (string, error) {
	t.Helper()

	select {
	case <-p.done:
	case <-time.After(timeout):
		t.Fatalf("phenix did not finish within %v", timeout)
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	return p.out.String(), p.err
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

// phenixFails runs the phenix CLI and fails the test unless it exits nonzero.
func phenixFails(t *testing.T, args ...string) string {
	t.Helper()

	out, err := phenixRun(args...)
	if err == nil {
		t.Fatalf("phenix %s succeeded, want failure\n%s", strings.Join(args, " "), out)
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

// mmText returns the simple (non-tabular) responses of a command, joined.
func mmText(t *testing.T, ns, command string) string {
	t.Helper()

	var parts []string

	for _, r := range mm(t, ns, command) {
		if r.Response != "" {
			parts = append(parts, r.Response)
		}
	}

	return strings.Join(parts, "\n")
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

// mmNamespaces lists minimega's namespaces without entering (and so creating)
// any of them.
func mmNamespaces(t *testing.T) map[string]map[string]string {
	t.Helper()

	out := make(map[string]map[string]string)

	for _, row := range mmTable(t, "", "namespace") {
		out[row["namespace"]] = row
	}

	return out
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

// rawVLANs returns the VLAN of each alias minimega holds in namespace ns.
func rawVLANs(t *testing.T, ns string) map[string]int {
	t.Helper()

	vlans := make(map[string]int)

	for _, row := range mmTable(t, ns, "vlans") {
		id, err := strconv.Atoi(row["vlan"])
		if err != nil {
			t.Fatalf("minimega vlans row %v: %v", row, err)
		}

		vlans[row["alias"]] = id
	}

	return vlans
}

// vmUUIDs returns the UUID minimega reports for each VM of namespace ns.
func vmUUIDs(t *testing.T, ns string) map[string]string {
	t.Helper()

	uuids := make(map[string]string)

	for name, row := range vmInfo(t, ns) {
		uuids[name] = row["uuid"]
	}

	return uuids
}

// splitList splits a minimega list column such as `[LAN (101), WAN (102)]`.
func splitList(column string) []string {
	s := strings.TrimSuffix(strings.TrimPrefix(column, "["), "]")
	if s == "" {
		return nil
	}

	return strings.Split(s, ", ")
}

// ovsVsctl runs ovs-vsctl and returns its trimmed output.
func ovsVsctl(t *testing.T, args ...string) (string, error) {
	t.Helper()

	out, err := exec.CommandContext(t.Context(), "ovs-vsctl", args...).CombinedOutput()

	return strings.TrimSpace(string(out)), err
}

// mustOVS runs ovs-vsctl and returns its trimmed output, failing the test when
// it exits nonzero.
func mustOVS(t *testing.T, args ...string) string {
	t.Helper()

	out, err := ovsVsctl(t, args...)
	if err != nil {
		t.Fatalf("ovs-vsctl %s: %v\n%s", strings.Join(args, " "), err, out)
	}

	return out
}

// wantTapsGone waits until no tap of taps is left on Open vSwitch or the host.
func wantTapsGone(t *testing.T, taps []string) {
	t.Helper()

	eventually(t, 30*time.Second, 500*time.Millisecond, fmt.Sprintf("taps %v to be removed", taps), func() (bool, string) {
		for _, tap := range taps {
			if br, err := ovsVsctl(t, "port-to-br", tap); err == nil {
				return false, fmt.Sprintf("%s is still on bridge %s", tap, br)
			}

			if _, err := os.Stat(filepath.Join("/sys/class/net", tap)); err == nil {
				return false, tap + " still exists on the host"
			}
		}

		return true, ""
	})
}

// headnode is the minimega host name of this runner, as `host name` reports it.
func headnode(t *testing.T) string {
	t.Helper()

	resp := mm(t, "", "host name")
	if len(resp) == 0 || resp[0].Host == "" {
		t.Fatal("minimega did not report its host name")
	}

	return resp[0].Host
}

// trimHostSuffixes trims the host name suffixes phenix strips by default
// (--hostname-suffixes), as it does when it names snapshots.
func trimHostSuffixes(host string) string {
	for _, suffix := range []string{"-minimega", "-phenix"} {
		host = strings.TrimSuffix(host, suffix)
	}

	return host
}

// minimegaQEMU reports whether argv, a process's command line, is a QEMU
// system emulator (minimega's default "kvm", or qemu-system-*, not the qemu-img
// or qemu-nbd minimega also runs there) that names one of minimega's instance
// directories under base. Every e2e count of minimega's VMs uses it.
func minimegaQEMU(argv []string) bool {
	if len(argv) == 0 {
		return false
	}

	if base := filepath.Base(argv[0]); base != "kvm" && !strings.HasPrefix(base, "qemu-system-") {
		return false
	}

	return slices.ContainsFunc(argv, func(arg string) bool { return strings.Contains(arg, cfg.mmBase+"/") })
}

// qemuPids returns the PIDs of the running processes minimegaQEMU matches.
func qemuPids() (map[string]bool, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}

	pids := make(map[string]bool)

	for _, e := range entries {
		cmdline, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if err != nil {
			continue
		}

		if minimegaQEMU(strings.Split(string(cmdline), "\x00")) {
			pids[e.Name()] = true
		}
	}

	return pids, nil
}

// qemuProcesses counts the running QEMU processes of minimega's VMs.
func qemuProcesses(t *testing.T) int {
	t.Helper()

	pids, err := qemuPids()
	if err != nil {
		t.Fatal(err)
	}

	return len(pids)
}

// readFile reads a file the product wrote and fails the test when it cannot.
func readFile(t *testing.T, path string) string {
	t.Helper()

	data, err := os.ReadFile(path) //nolint:gosec // test reads product output
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}

	return string(data)
}

// hostCmd runs a command on the host and returns its trimmed combined output.
func hostCmd(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput() //nolint:gosec // test harness

	return strings.TrimSpace(string(out)), err
}

// wantContains reports every want missing from text.
func wantContains(t *testing.T, what, text string, wants ...string) {
	t.Helper()

	for _, w := range wants {
		if !strings.Contains(text, w) {
			t.Errorf("%s does not contain %q:\n%s", what, w, text)
		}
	}
}

// wantLacks reports every unwanted string found in text.
func wantLacks(t *testing.T, what, text string, unwanted ...string) {
	t.Helper()

	for _, u := range unwanted {
		if strings.Contains(text, u) {
			t.Errorf("%s contains %q:\n%s", what, u, text)
		}
	}
}

// mustJSON returns v as JSON text and panics when v cannot be marshaled.
func mustJSON(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}

	return string(data)
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

var httpClient = &http.Client{Timeout: 10 * time.Minute} //nolint:gochecknoglobals // shared client

// api calls the phenix REST API and returns the status code and body.
func api(t *testing.T, method, path string, body any) (int, []byte) {
	t.Helper()

	var reader io.Reader

	if body != nil {
		switch b := body.(type) {
		case []byte:
			reader = bytes.NewReader(b)
		case string:
			reader = strings.NewReader(b)
		default:
			data, err := json.Marshal(b)
			if err != nil {
				t.Fatal(err)
			}

			reader = bytes.NewReader(data)
		}
	}

	req, err := http.NewRequestWithContext(context.Background(), method, "http://"+cfg.uiAddr+"/api/v1"+path, reader)
	if err != nil {
		t.Fatal(err)
	}

	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s %s: reading body: %v", method, path, err)
	}

	return resp.StatusCode, data
}

// apiOK calls the REST API, fails the test unless the status is 2xx, and
// decodes the JSON body into out when out is not nil, failing the test when
// there is no body: a zero-valued out would satisfy negative assertions.
func apiOK(t *testing.T, method, path string, body, out any) []byte {
	t.Helper()

	code, data := api(t, method, path, body)
	if code < 200 || code > 299 {
		t.Fatalf("%s %s: status %d: %s", method, path, code, data)
	}

	if out != nil {
		if len(bytes.TrimSpace(data)) == 0 {
			t.Fatalf("%s %s: status %d with an empty body, want JSON", method, path, code)
		}

		if err := json.Unmarshal(data, out); err != nil {
			t.Fatalf("%s %s: decoding %s: %v", method, path, data, err)
		}
	}

	return data
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

// requireApps fails the test unless the E2E workflow installed
// sceptre-phenix-apps for shard and the given commands are on PATH. Missing
// apps are a harness failure, never a reason to skip.
func requireApps(t *testing.T, shard string, commands ...string) {
	t.Helper()

	if os.Getenv("E2E_APPS") != "true" {
		t.Fatalf("the %s shard needs E2E_APPS=true, which the E2E workflow sets once it has installed "+
			"sceptre-phenix-apps (console scripts linked into /usr/local/bin from /opt/phenix-apps, "+
			"phenix-app-mirror built from its src/go)", shard)
	}

	for _, c := range commands {
		if _, err := exec.LookPath(c); err != nil {
			t.Fatalf("E2E_APPS=true but %s is not on PATH: %v", c, err)
		}
	}
}
