package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"golang.org/x/net/websocket"
	"pgregory.net/rapid"

	"phenix/util/polltest"
	"phenix/util/testenv"
	"phenix/web/lifecycle"
	"phenix/web/rbac"
)

const consoleTestTimeout = 5 * time.Second

// useTestConsoles runs `cat` as the console, which echoes input and exits on
// CTRL+D, and ends every console the test leaves running.
func useTestConsoles(t *testing.T, grace time.Duration) *consoleRegistry {
	t.Helper()

	testenv.RequireCommands(t, "cat")

	original, originalOpts := consoles, o
	registry := newConsoleRegistry(grace, func(ctx context.Context) (*exec.Cmd, error) {
		return exec.CommandContext(ctx, "cat"), nil
	})

	consoles = registry
	o.minimegaConsole = true

	t.Cleanup(func() {
		registry.mu.Lock()
		running := make([]*consoleSession, 0, len(registry.sessions))
		for _, console := range registry.sessions {
			running = append(running, console)
		}
		registry.mu.Unlock()

		for _, console := range running {
			console.end(consoleEndedByUser)
		}

		consoles, o = original, originalOpts
	})

	return registry
}

// consoleAPI serves the API to user with role, and returns its URL.
func consoleAPI(t *testing.T, user string, role rbac.Role) string {
	t.Helper()

	return serveAs(t, user, role).URL + "/api/v1"
}

// consoleRole may start and use consoles.
func consoleRole() rbac.Role {
	return combinedRole(testRole("miniconsole", "get"), testRole("miniconsole", "post"))
}

// createTestConsole starts a console as the API's user.
func createTestConsole(t *testing.T, api string) int {
	t.Helper()

	resp := do(t, http.MethodPost, api+"/console", "")
	if ct := resp.Header.Get("Content-Type"); resp.StatusCode != http.StatusOK || ct != "application/json" {
		t.Fatalf("POST /console = %d %s, want %d application/json", resp.StatusCode, ct, http.StatusOK)
	}

	var body struct {
		PID int `json:"pid"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decoding POST /console: %v", err)
	}

	return body.PID
}

func dialTestConsole(t *testing.T, api string, pid int) (*websocket.Conn, error) {
	t.Helper()

	url := "ws" + strings.TrimPrefix(api, "http") + "/console/" + strconv.Itoa(pid) + "/ws"

	ws, err := websocket.Dial(url, "", api)
	if err == nil {
		t.Cleanup(func() { ws.Close() })
	}

	return ws, err
}

// readConsoleUntil reads from ws until its output contains want.
func readConsoleUntil(t *testing.T, ws *websocket.Conn, want string) {
	t.Helper()

	_ = ws.SetReadDeadline(time.Now().Add(consoleTestTimeout))

	var (
		output bytes.Buffer
		buf    = make([]byte, 4096)
	)

	for !strings.Contains(output.String(), want) {
		n, err := ws.Read(buf)
		output.Write(buf[:n])

		if err != nil {
			t.Fatalf("reading the console for %q = %v, having read %q", want, err, output.String())
		}
	}
}

func waitConsoleEnded(t *testing.T, console *consoleSession) {
	t.Helper()

	select {
	case <-console.done:
	case <-time.After(consoleTestTimeout):
		t.Fatalf("console %d did not end within %v", console.pid, consoleTestTimeout)
	}
}

// waitConsoleClients waits until n websockets are attached to the console.
func waitConsoleClients(t *testing.T, console *consoleSession, n int) {
	t.Helper()

	polltest.Until(t, fmt.Sprintf("%d websockets attached to console %d", n, console.pid), func() bool {
		console.mu.Lock()
		defer console.mu.Unlock()

		return len(console.clients) == n
	})
}

// idleTimer returns what the console's running idle timer passes expire once
// the grace period has passed.
func idleTimer(t *testing.T, console *consoleSession) int {
	t.Helper()

	console.mu.Lock()
	defer console.mu.Unlock()

	if console.idle == nil {
		t.Fatalf("console %d has no idle timer running", console.pid)
	}

	return console.idleGen
}

// echoTestConsole checks the console is still running: cat echoes a line.
func echoTestConsole(t *testing.T, ws *websocket.Conn, line string) {
	t.Helper()

	if _, err := ws.Write([]byte(line + "\n")); err != nil {
		t.Fatalf("writing %q to the console: %v", line, err)
	}

	readConsoleUntil(t, ws, line+"\r\n"+line+"\r\n")
}

// A console history keeps the newest output that fits, and replays it from the
// start of a line.
func TestConsoleHistory(t *testing.T) {
	t.Parallel()

	history := newConsoleHistory(16)

	// each write in turn, and what the 16-byte history then replays
	for _, step := range []struct{ write, want string }{
		{"one\n", "one\n"},
		{"two\n", "one\ntwo\n"},
		// fills it exactly: nothing dropped yet
		{"abcd\n\x1b[1", "one\ntwo\nabcd\n\x1b[1"},
		// wraps: the oldest output goes, and the replay starts at a line
		{"mfour\n", "abcd\n\x1b[1mfour\n"},
		// more than it holds at once: only the end is kept
		{"a very long line\nof output!\n", "of output!\n"},
	} {
		history.Write([]byte(step.write))

		if got := string(history.Bytes()); got != step.want {
			t.Errorf("after Write(%q), Bytes() = %q, want %q", step.write, got, step.want)
		}
	}
}

// consoleHistoryModel is what a size-byte history replays of all the output written to it.
func consoleHistoryModel(all []byte, size int) []byte {
	if len(all) <= size {
		return all
	}

	kept := all[len(all)-size:]
	if i := bytes.IndexByte(kept, '\n'); i >= 0 {
		kept = kept[i+1:]
	}

	return kept
}

// A console history of any size replays what a model of it keeps, after each
// write.
func TestConsoleHistoryProperties(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(t *rapid.T) {
		size := rapid.IntRange(1, 24).Draw(t, "size")
		writes := rapid.SliceOfN(rapid.SliceOfN(rapid.SampledFrom([]byte("ab\n")), 0, 3*size), 1, 12).Draw(t, "writes")

		history := newConsoleHistory(size)

		all := make([]byte, 0, len(writes)*3*size)

		for _, output := range writes {
			history.Write(output)
			all = append(all, output...)

			if got, want := history.Bytes(), consoleHistoryModel(all, size); !bytes.Equal(got, want) {
				t.Fatalf("newConsoleHistory(%d) after writes %q: Bytes() = %q, want %q", size, writes, got, want)
			}
		}
	})
}

// A websocket that attaches to a console gets the earlier output, and each
// websocket gets the new output.
func TestConsoleReplaysOutputToAnotherWebSocket(t *testing.T) {
	registry := useTestConsoles(t, time.Minute)
	api := consoleAPI(t, "alice", consoleRole())
	pid := createTestConsole(t, api)
	console := registry.lookup(pid)

	first, err := dialTestConsole(t, api, pid)
	if err != nil {
		t.Fatalf("dialing console %d: %v", pid, err)
	}

	if _, err := first.Write([]byte("hello console\n")); err != nil {
		t.Fatalf("writing to console %d: %v", pid, err)
	}

	readConsoleUntil(t, first, "hello console\r\nhello console\r\n")

	// a second tab gets what the console printed before it attached
	second, err := dialTestConsole(t, api, pid)
	if err != nil {
		t.Fatalf("dialing console %d a second time: %v", pid, err)
	}

	readConsoleUntil(t, second, "hello console\r\nhello console\r\n")

	// and both get new output, whichever sent the input
	if _, err := second.Write([]byte("again\n")); err != nil {
		t.Fatalf("writing to console %d from the second websocket: %v", pid, err)
	}

	readConsoleUntil(t, first, "again\r\nagain\r\n")
	readConsoleUntil(t, second, "again\r\nagain\r\n")

	// closing them leaves the console, and its output, for the next one
	first.Close()
	second.Close()
	waitConsoleClients(t, console, 0)

	third, err := dialTestConsole(t, api, pid)
	if err != nil {
		t.Fatalf("dialing console %d a third time: %v", pid, err)
	}

	readConsoleUntil(t, third, "again\r\nagain\r\n")
}

// The grace period is far longer than the test, which fires the idle timers
// itself, as they fire once it has passed.
func TestConsoleEndsOnceUnattachedForTheGracePeriod(t *testing.T) {
	registry := useTestConsoles(t, time.Minute)
	api := consoleAPI(t, "alice", consoleRole())
	pid := createTestConsole(t, api)
	console := registry.lookup(pid)
	events := captureAudit(t)

	unattached := idleTimer(t, console)

	ws, err := dialTestConsole(t, api, pid)
	if err != nil {
		t.Fatalf("dialing console %d: %v", pid, err)
	}

	waitConsoleClients(t, console, 1)

	// attached for longer than the grace period: still running
	console.expire(unattached)
	echoTestConsole(t, ws, "attached")

	// a reload: detached, then attached again within the grace period
	ws.Close()
	waitConsoleClients(t, console, 0)

	detached := idleTimer(t, console)

	ws, err = dialTestConsole(t, api, pid)
	if err != nil {
		t.Fatalf("dialing console %d again: %v", pid, err)
	}

	waitConsoleClients(t, console, 1)

	console.expire(detached)
	echoTestConsole(t, ws, "reloaded")

	// the last tab closed
	ws.Close()
	waitConsoleClients(t, console, 0)

	console.expire(idleTimer(t, console))
	waitConsoleEnded(t, console)

	wantAudit(t, events(), "console-end", "success", map[string]any{
		"user.name": "alice", "process.pid": pid, "phenix.console.end_reason": "idle",
	})

	if n := countAudit(events(), "console-attach", "success", map[string]any{"process.pid": pid}); n != 2 {
		t.Errorf("console-attach success audit events = %d, want 2", n)
	}

	if resp := do(t, http.MethodGet, api+"/console/"+strconv.Itoa(pid), ""); resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET /console/%d after it ended = %d, want %d", pid, resp.StatusCode, http.StatusNotFound)
	}
}

// A websocket that stops reading is dropped once its queue of output is full,
// and the others keep getting the console's output.
func TestConsoleDropsASlowWebSocket(t *testing.T) {
	registry := useTestConsoles(t, time.Minute)
	api := consoleAPI(t, "alice", consoleRole())
	pid := createTestConsole(t, api)
	console := registry.lookup(pid)

	// nothing writes this client's queue to its websocket, as when the
	// browser behind it stops reading
	slow, err := console.attach()
	if err != nil {
		t.Fatalf("attaching to console %d: %v", pid, err)
	}

	for i := range consoleClientQueue {
		console.broadcast(fmt.Appendf(nil, "line %d\n", i))
	}

	ws, err := dialTestConsole(t, api, pid)
	if err != nil {
		t.Fatalf("dialing console %d: %v", pid, err)
	}

	waitConsoleClients(t, console, 2)
	readConsoleUntil(t, ws, fmt.Sprintf("line %d\n", consoleClientQueue-1))

	echoTestConsole(t, ws, "more output")
	waitConsoleClients(t, console, 1)

	// the dropped websocket is written what was already queued for it, then
	// closed
	queued := 0
	for range slow.out {
		queued++
	}

	if queued != consoleClientQueue {
		t.Errorf("dropped client's queued chunks = %d, want %d", queued, consoleClientQueue)
	}
}

// A console ends when no websocket attaches to it within its grace period.
func TestConsoleEndsIfNoWebSocketEverAttaches(t *testing.T) {
	registry := useTestConsoles(t, 100*time.Millisecond)
	pid := createTestConsole(t, consoleAPI(t, "alice", consoleRole()))

	waitConsoleEnded(t, registry.lookup(pid))
}

// A console ends when its process exits: its websocket closes, the end is
// audited, and the console refuses a new websocket.
func TestConsoleEndsWhenItsProcessExits(t *testing.T) {
	registry := useTestConsoles(t, time.Minute)
	api := consoleAPI(t, "alice", consoleRole())
	pid := createTestConsole(t, api)
	console := registry.lookup(pid)
	events := captureAudit(t)

	ws, err := dialTestConsole(t, api, pid)
	if err != nil {
		t.Fatalf("dialing console %d: %v", pid, err)
	}

	// CTRL+D: cat exits
	if _, err := ws.Write([]byte{4}); err != nil {
		t.Fatalf("writing CTRL+D to console %d: %v", pid, err)
	}

	waitConsoleEnded(t, console)

	// the websocket closes once the console has ended
	_ = ws.SetReadDeadline(time.Now().Add(consoleTestTimeout))

	buf := make([]byte, 64)
	for {
		if _, err := ws.Read(buf); err != nil {
			if !errors.Is(err, io.EOF) {
				t.Errorf("reading the websocket of ended console %d = %v, want %v", pid, err, io.EOF)
			}

			break
		}
	}

	if registry.lookup(pid) != nil {
		t.Errorf("lookup(%d) of the ended console = registered, want nil", pid)
	}

	got := events()
	wantAudit(t, got, "console-attach", "success", map[string]any{"user.name": "alice", "process.pid": pid})
	wantAudit(t, got, "console-end", "success", map[string]any{
		"user.name": "alice", "process.pid": pid, "phenix.console.end_reason": "exited",
	})

	if _, err := dialTestConsole(t, api, pid); err == nil {
		t.Errorf("dialing ended console %d = nil error, want it refused", pid)
	}
}

// A DELETE ends the console before it answers, audits the end, and answers
// 404 for that console after.
func TestDeleteConsoleEndsIt(t *testing.T) {
	registry := useTestConsoles(t, time.Minute)
	api := consoleAPI(t, "alice", consoleRole())
	events := captureAudit(t)
	pid := createTestConsole(t, api)
	console := registry.lookup(pid)

	resp := do(t, http.MethodDelete, api+"/console/"+strconv.Itoa(pid), "")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE /console/%d = %d, want %d", pid, resp.StatusCode, http.StatusNoContent)
	}

	// ended by the time the request answers
	select {
	case <-console.done:
	default:
		t.Errorf("console %d still running after DELETE answered", pid)
	}

	got := events()
	wantAudit(t, got, "console-create", "success", map[string]any{"user.name": "alice", "process.pid": pid})
	wantAudit(t, got, "console-terminate", "success", map[string]any{"user.name": "alice", "process.pid": pid})
	wantAudit(t, got, "console-end", "success", map[string]any{
		"user.name": "alice", "process.pid": pid, "phenix.console.end_reason": "user",
	})

	resp = do(t, http.MethodDelete, api+"/console/"+strconv.Itoa(pid), "")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("DELETE /console/%d again = %d, want %d", pid, resp.StatusCode, http.StatusNotFound)
	}
}

// Only the user who started a console may use it: the requests of other users
// answer 404 and are audited.
func TestConsoleIsOnlyItsOwners(t *testing.T) {
	registry := useTestConsoles(t, time.Minute)

	var (
		alice  = consoleAPI(t, "alice", consoleRole())
		bob    = consoleAPI(t, "bob", consoleRole())
		pid    = createTestConsole(t, alice)
		path   = "/console/" + strconv.Itoa(pid)
		events = captureAudit(t)
	)

	for _, tt := range []struct{ method, path string }{
		{http.MethodGet, path},
		{http.MethodPost, path + "/size?cols=100&rows=40"},
		{http.MethodDelete, path},
	} {
		if resp := do(t, tt.method, bob+tt.path, ""); resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s %s as bob = %d, want %d", tt.method, tt.path, resp.StatusCode, http.StatusNotFound)
		}
	}

	if _, err := dialTestConsole(t, bob, pid); err == nil {
		t.Errorf("dialing alice's console %d as bob = nil error, want it refused", pid)
	}

	if registry.lookup(pid) == nil {
		t.Fatalf("lookup(%d) after bob's requests = nil, want alice's console running", pid)
	}

	for _, action := range []string{"console-terminate", "console-attach"} {
		wantAudit(t, events(), action, "failure", map[string]any{
			"user.name": "bob", "process.pid": pid, "event.reason": "not owner",
		})
	}

	resp := do(t, http.MethodGet, alice+path, "")
	if ct := resp.Header.Get("Content-Type"); resp.StatusCode != http.StatusOK || ct != "application/json" {
		t.Errorf("GET %s as alice = %d %s, want %d application/json", path, resp.StatusCode, ct, http.StatusOK)
	}

	if resp := do(t, http.MethodPost, alice+path+"/size?cols=100&rows=40", ""); resp.StatusCode != http.StatusOK {
		t.Errorf("POST %s/size as alice = %d, want %d", path, resp.StatusCode, http.StatusOK)
	}
}

// The console routes refuse, and audit, a role that does not have the
// permission each route needs.
func TestConsoleRoutesNeedPermission(t *testing.T) {
	useTestConsoles(t, time.Minute)

	pid := createTestConsole(t, consoleAPI(t, "alice", consoleRole()))
	path := "/console/" + strconv.Itoa(pid)

	// may use the console, but not start, resize or end one
	viewer := consoleAPI(t, "alice", testRole("miniconsole", "get"))
	events := captureAudit(t)

	for _, tt := range []struct{ method, path string }{
		{http.MethodPost, "/console"},
		{http.MethodPost, path + "/size?cols=100&rows=40"},
		{http.MethodDelete, path},
	} {
		if resp := do(t, tt.method, viewer+tt.path, ""); resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s %s as a console viewer = %d, want %d", tt.method, tt.path, resp.StatusCode, http.StatusForbidden)
		}
	}

	none := consoleAPI(t, "alice", testRole("experiments", "list"))

	if resp := do(t, http.MethodGet, none+path, ""); resp.StatusCode != http.StatusForbidden {
		t.Errorf("GET %s without console permission = %d, want %d", path, resp.StatusCode, http.StatusForbidden)
	}

	if _, err := dialTestConsole(t, none, pid); err == nil {
		t.Errorf("dialing console %d without console permission = nil error, want it refused", pid)
	}

	for _, action := range []string{"console-create", "console-terminate"} {
		wantDenied(t, events(), action)
	}

	// the status check, resize and websocket refusals
	for _, route := range []string{path, path + "/size", path + "/ws"} {
		wantAudit(t, events(), "console-attach", "failure", map[string]any{
			"event.reason": "forbidden", "url.path": "/api/v1" + route,
		})
	}
}

// A POST that starts a console answers 405 when the console is disabled.
func TestCreateConsoleNeedsTheConsoleEnabled(t *testing.T) {
	useTestConsoles(t, time.Minute)

	o.minimegaConsole = false

	resp := do(t, http.MethodPost, consoleAPI(t, "alice", consoleRole())+"/console", "")
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST /console with the console disabled = %d, want %d", resp.StatusCode, http.StatusMethodNotAllowed)
	}
}

// consoleStuckSession registers, as owner's, a console whose process (sleep)
// can be killed but which never ends, and whose tty is tty.
func consoleStuckSession(t *testing.T, registry *consoleRegistry, owner string, tty *os.File) *consoleSession {
	t.Helper()

	testenv.RequireCommands(t, "sleep")

	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting sleep: %v", err)
	}

	console := &consoleSession{
		pid: cmd.Process.Pid, owner: owner, tty: tty, proc: cmd.Process, grace: time.Minute, registry: registry,
		done: make(chan struct{}), mu: sync.Mutex{}, clients: make(map[*consoleClient]struct{}),
		history: newConsoleHistory(16),
	}

	registry.mu.Lock()
	registry.sessions[console.pid] = console
	registry.mu.Unlock()

	// before useTestConsoles ends what is left, which would wait for this
	t.Cleanup(func() {
		registry.remove(console)

		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	return console
}

// consoleTempFile is a new regular file, open for the rest of the test.
func consoleTempFile(t *testing.T) *os.File {
	t.Helper()

	f, err := os.Create(filepath.Join(t.TempDir(), "tty"))
	if err != nil {
		t.Fatalf("os.Create() error = %v", err)
	}

	t.Cleanup(func() { _ = f.Close() })

	return f
}

// consoleClosedFile is a regular file that is already closed.
func consoleClosedFile(t *testing.T) *os.File {
	t.Helper()

	f := consoleTempFile(t)
	_ = f.Close()

	return f
}

// The minimega console is `phenix mm --attach` of this very executable,
// killed when its context ends.
func TestMinimegaConsoleCommand(t *testing.T) {
	t.Parallel()

	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable() error = %v", err)
	}

	cmd, err := minimegaConsoleCommand(t.Context())
	if err != nil {
		t.Fatalf("minimegaConsoleCommand() error = %v", err)
	}

	if diff := cmp.Diff([]string{exe, "mm", "--attach"}, cmd.Args); diff != "" {
		t.Errorf("minimegaConsoleCommand() args mismatch (-want +got):\n%s", diff)
	}

	if cmd.Path != exe || cmd.Cancel == nil {
		t.Errorf("minimegaConsoleCommand() = path %q, cancel set %t; want %q, true", cmd.Path, cmd.Cancel != nil, exe)
	}
}

// A console that cannot start is a 500 naming why, audited as a failure.
func TestCreateConsoleStartFails(t *testing.T) {
	for name, tc := range map[string]struct {
		err     error
		program string
	}{
		"no command": {err: errors.New("no phenix executable")},
		"no program": {program: filepath.Join(t.TempDir(), "missing")},
	} {
		t.Run(name, func(t *testing.T) {
			registry := useTestConsoles(t, time.Minute)
			registry.command = func(ctx context.Context) (*exec.Cmd, error) {
				if tc.err != nil {
					return nil, tc.err
				}

				return exec.CommandContext(ctx, tc.program), nil
			}

			events := captureAudit(t)

			resp := do(t, http.MethodPost, consoleAPI(t, "alice", consoleRole())+"/console", "")
			if body := readBody(t, resp); resp.StatusCode != http.StatusInternalServerError ||
				!strings.Contains(body, "could not start terminal") {
				t.Errorf("POST /console = %d %s, want %d could not start terminal", resp.StatusCode, body, http.StatusInternalServerError)
			}

			wantAudit(t, events(), "console-create", "failure", map[string]any{"user.name": "alice"})

			if n := len(registry.sessions); n != 0 {
				t.Errorf("consoles after a failed start = %d, want 0", n)
			}
		})
	}
}

// start returns the command's error, and wraps one starting its process.
func TestConsoleRegistryStartFails(t *testing.T) {
	t.Parallel()

	refused := errors.New("no phenix executable")

	registry := newConsoleRegistry(time.Minute, func(context.Context) (*exec.Cmd, error) { return nil, refused })
	if _, err := registry.start(t.Context(), "alice"); !errors.Is(err, refused) {
		t.Errorf("start() with a failing command error = %v, want %v", err, refused)
	}

	missing := filepath.Join(t.TempDir(), "missing")

	registry = newConsoleRegistry(time.Minute, func(ctx context.Context) (*exec.Cmd, error) {
		return exec.CommandContext(ctx, missing), nil
	})
	if _, err := registry.start(t.Context(), "alice"); err == nil || !strings.Contains(err.Error(), "starting console process") {
		t.Errorf("start() of a missing program error = %v, want it to say starting console process", err)
	}
}

// DELETE /console/{pid} of a console that does not end in time is a 500,
// audited as a failure.
func TestDeleteConsoleThatDoesNotEnd(t *testing.T) {
	registry := useTestConsoles(t, time.Minute)
	console := consoleStuckSession(t, registry, "alice", consoleTempFile(t))
	api := consoleAPI(t, "alice", consoleRole())
	events := captureAudit(t)

	path := "/console/" + strconv.Itoa(console.pid)

	resp := do(t, http.MethodDelete, api+path, "")
	if got := errorMessage(t, resp.StatusCode, readBody(t, resp)); resp.StatusCode != http.StatusInternalServerError ||
		got != "console did not end" {
		t.Errorf("DELETE %s = %d %q, want %d %q", path, resp.StatusCode, got, http.StatusInternalServerError, "console did not end")
	}

	if failed := wantAudit(t, events(), "console-terminate", "failure", map[string]any{"process.pid": console.pid}); failed != nil {
		wantErrorMessage(t, failed, errConsoleNotEnded.Error())
	}

	console.mu.Lock()
	defer console.mu.Unlock()

	if console.endReason != consoleEndedByUser {
		t.Errorf("end reason of a console the user ended = %q, want %q", console.endReason, consoleEndedByUser)
	}
}

// A resize needs a pid and both sizes as 16-bit numbers; one the pty refuses
// is a 500.
func TestResizeConsoleRefuses(t *testing.T) {
	registry := useTestConsoles(t, time.Minute)
	api := consoleAPI(t, "alice", consoleRole())
	pid := strconv.Itoa(createTestConsole(t, api))
	notTTY := strconv.Itoa(consoleStuckSession(t, registry, "alice", consoleTempFile(t)).pid)
	closed := strconv.Itoa(consoleStuckSession(t, registry, "alice", consoleClosedFile(t)).pid)

	for name, tc := range map[string]struct {
		path, msg string
		want      int
	}{
		"pid not a number": {"/console/abc/size?cols=80&rows=24", "invalid pid", http.StatusBadRequest},
		"unknown pid":      {"/console/0/size?cols=80&rows=24", "console not found", http.StatusNotFound},
		"bad query":        {"/console/" + pid + "/size?cols=80;rows=24", "invalid form", http.StatusBadRequest},
		"rows too many":    {"/console/" + pid + "/size?cols=80&rows=65536", "invalid rows", http.StatusBadRequest},
		"cols too many":    {"/console/" + pid + "/size?cols=99999999999&rows=24", "invalid cols", http.StatusBadRequest},
		"not a terminal":   {"/console/" + notTTY + "/size?cols=80&rows=24", "set winsize failed", http.StatusInternalServerError},
		"closed pty":       {"/console/" + closed + "/size?cols=80&rows=24", "set winsize failed", http.StatusInternalServerError},
	} {
		t.Run(name, func(t *testing.T) {
			resp := do(t, http.MethodPost, api+tc.path, "")
			if got := errorMessage(t, resp.StatusCode, readBody(t, resp)); resp.StatusCode != tc.want || got != tc.msg {
				t.Errorf("POST %s = %d %q, want %d %q", tc.path, resp.StatusCode, got, tc.want, tc.msg)
			}
		})
	}

	// sizes the router's patterns would not route here
	console := consoleStuckSession(t, registry, "test-user", consoleTempFile(t))

	for query, msg := range map[string]string{
		"cols=80":         "invalid rows",
		"cols=80&rows=":   "invalid rows",
		"cols=80&rows=ab": "invalid rows",
		"rows=24":         "invalid cols",
		"rows=24&cols=-1": "invalid cols",
	} {
		t.Run(query, func(t *testing.T) {
			req := roleRequest(http.MethodPost, "/console/size?"+query, "", map[string]string{"pid": strconv.Itoa(console.pid)},
				consoleRole())

			status, werr := webErrorStatus(t, ResizeConsole(httptest.NewRecorder(), req))
			if status != http.StatusBadRequest || werr.Message != msg {
				t.Errorf("ResizeConsole(%s) = %d %+v, want %d %q", query, status, werr, http.StatusBadRequest, msg)
			}
		})
	}
}

// resize sets the size of a pty, and fails on a file that is not one, is
// closed or is missing.
func TestConsoleSessionResize(t *testing.T) {
	registry := useTestConsoles(t, time.Minute)
	console := registry.lookup(createTestConsole(t, consoleAPI(t, "alice", consoleRole())))

	if err := console.resize(40, 100); err != nil {
		t.Errorf("resize(40, 100) of a pty error = %v", err)
	}

	tests := map[string]struct {
		tty  *os.File
		want string
	}{
		"not a terminal": {consoleTempFile(t), syscall.ENOTTY.Error()},
		"closed":         {consoleClosedFile(t), "closed file"},
		"missing":        {nil, os.ErrInvalid.Error()},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			s := &consoleSession{tty: tc.tty}
			if err := s.resize(24, 80); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("resize() of a %s file error = %v, want it to hold %q", name, err, tc.want)
			}
		})
	}
}

// Attaching to a console that ended after it was looked up fails, audited,
// and the websocket closes.
func TestWsConsoleAttachToEndedConsole(t *testing.T) {
	registry := useTestConsoles(t, time.Minute)
	console := consoleStuckSession(t, registry, "alice", consoleTempFile(t))
	console.ended = true

	api := consoleAPI(t, "alice", consoleRole())
	events := captureAudit(t)

	ws, err := dialTestConsole(t, api, console.pid)
	if err != nil {
		t.Fatalf("dialing console %d: %v", console.pid, err)
	}

	_ = ws.SetReadDeadline(time.Now().Add(consoleTestTimeout))

	if _, err := ws.Read(make([]byte, 64)); !errors.Is(err, io.EOF) {
		t.Errorf("reading the websocket of an ended console = %v, want %v", err, io.EOF)
	}

	if failed := wantAudit(t, events(), "console-attach", "failure", map[string]any{"process.pid": console.pid}); failed != nil {
		wantErrorMessage(t, failed, errConsoleEnded.Error())
	}
}

// Once the server starts shutting down, a console websocket is refused with
// 503.
func TestWsConsoleRefusedAtShutdown(t *testing.T) {
	useTestConsoles(t, time.Minute)

	lifecycle.Start()
	t.Cleanup(lifecycle.Start)

	api := consoleAPI(t, "alice", consoleRole())
	pid := createTestConsole(t, api)

	lifecycle.Stop()

	resp := do(t, http.MethodGet, api+"/console/"+strconv.Itoa(pid)+"/ws", "")
	if got := errorMessage(t, resp.StatusCode, readBody(t, resp)); resp.StatusCode != http.StatusServiceUnavailable ||
		got != lifecycle.Unavailable || resp.Header.Get("Retry-After") != lifecycle.RetryAfter {
		t.Errorf("GET console websocket at shutdown = %d %q Retry-After %q, want %d %q %q", resp.StatusCode, got,
			resp.Header.Get("Retry-After"), http.StatusServiceUnavailable, lifecycle.Unavailable, lifecycle.RetryAfter)
	}
}

// endAll ends every console for the reason, counts those not ended once ctx
// is done, and lets no console start afterwards.
func TestConsoleRegistryEndAllLeavesStuckConsoles(t *testing.T) {
	registry := useTestConsoles(t, time.Minute)
	stuck := consoleStuckSession(t, registry, "alice", consoleTempFile(t))
	running := registry.lookup(createTestConsole(t, consoleAPI(t, "alice", consoleRole())))

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	if left := registry.endAll(ctx, consoleEndedShutdown); left != 1 {
		t.Errorf("endAll() with one console that never ends = %d left, want 1", left)
	}

	waitConsoleEnded(t, running)

	for _, console := range []*consoleSession{stuck, running} {
		console.mu.Lock()
		reason := console.endReason
		console.mu.Unlock()

		if reason != consoleEndedShutdown {
			t.Errorf("console %d end reason = %q, want %q", console.pid, reason, consoleEndedShutdown)
		}
	}

	if _, err := registry.start(t.Context(), "alice"); !errors.Is(err, errServerStopping) {
		t.Errorf("start() after endAll() error = %v, want %v", err, errServerStopping)
	}
}

// isDone reports whether a channel is closed, without waiting.
func TestIsDone(t *testing.T) {
	t.Parallel()

	open, closed := make(chan struct{}), make(chan struct{})
	close(closed)

	if isDone(open) || !isDone(closed) {
		t.Errorf("isDone(open), isDone(closed) = %t, %t; want false, true", isDone(open), isDone(closed))
	}
}

// An ended console keeps no more output and sends none, and takes no new
// websocket.
func TestEndedConsoleSessionIgnoresOutput(t *testing.T) {
	t.Parallel()

	s := &consoleSession{clients: make(map[*consoleClient]struct{}), history: newConsoleHistory(64)}

	client, err := s.attach()
	if err != nil {
		t.Fatalf("attach() error = %v", err)
	}

	s.broadcast([]byte("before\n"))

	s.mu.Lock()
	s.ended = true
	s.mu.Unlock()

	s.broadcast([]byte("after\n"))

	if got := string(s.history.Bytes()); got != "before\n" {
		t.Errorf("history after output once ended = %q, want %q", got, "before\n")
	}

	if got := len(client.out); got != 1 {
		t.Errorf("chunks sent to the client = %d, want 1, from before the console ended", got)
	}

	if _, err := s.attach(); !errors.Is(err, errConsoleEnded) {
		t.Errorf("attach() to an ended console error = %v, want %v", err, errConsoleEnded)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.idle != nil {
		t.Error("an attached console has an idle timer running")
	}
}

// A client writes its queued output to its websocket in order, and stops at
// the first write that fails, closing the websocket either way.
func TestConsoleClientServe(t *testing.T) {
	t.Parallel()

	for name, closeFirst := range map[string]bool{"written": false, "write fails": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			client := &consoleClient{out: make(chan []byte, 3)}
			client.out <- []byte("one ")
			client.out <- []byte("two")

			served := make(chan struct{})

			server := httptest.NewServer(websocket.Handler(func(ws *websocket.Conn) {
				if closeFirst {
					_ = ws.Close()
				} else {
					close(client.out)
				}

				written := make(chan struct{})
				client.serve(ws, written)
				<-written

				close(served)
			}))
			t.Cleanup(server.Close)

			ws, err := websocket.Dial("ws"+strings.TrimPrefix(server.URL, "http"), "", server.URL)
			if err != nil {
				t.Fatalf("websocket.Dial() error = %v", err)
			}
			defer ws.Close()

			_ = ws.SetReadDeadline(time.Now().Add(consoleTestTimeout))
			got, _ := io.ReadAll(ws)

			select {
			case <-served:
			case <-time.After(consoleTestTimeout):
				t.Fatal("serve() did not return")
			}

			want, left := "one two", 0
			if closeFirst {
				want, left = "", 1
			}

			if string(got) != want || len(client.out) != left {
				t.Errorf("serve() wrote %q, left %d chunks; want %q, %d", got, len(client.out), want, left)
			}
		})
	}
}
