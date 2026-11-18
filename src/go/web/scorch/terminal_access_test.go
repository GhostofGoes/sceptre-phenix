package scorch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"golang.org/x/net/websocket"

	v1 "phenix/types/version/v1"
	"phenix/web/middleware"
	"phenix/web/rbac"
	"phenix/web/weberror"
)

// testRole returns a role with the given policies, each a resource and a
// verb on every name.
func testRole(policies ...[2]string) rbac.Role {
	spec := &v1.RoleSpec{}

	for _, p := range policies {
		spec.Policies = append(spec.Policies, &v1.PolicySpec{
			Resources:     []string{p[0]},
			ResourceNames: []string{"*"},
			Verbs:         []string{p[1]},
		})
	}

	return rbac.Role{Spec: spec}
}

// terminalTestServer serves the SCORCH terminal and component output routes
// as web/server.go does. A request has the role its "role" query parameter
// names in roles.
func terminalTestServer(t *testing.T, roles map[string]rbac.Role) *httptest.Server {
	t.Helper()

	router := mux.NewRouter()
	api := router.PathPrefix("/api/v1").Subrouter()

	api.Use(func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			name := r.URL.Query().Get("role")

			role, ok := roles[name]
			if !ok {
				http.Error(w, "unknown test role "+name, http.StatusUnauthorized)

				return
			}

			ctx := context.WithValue(r.Context(), middleware.ContextKeyRole, role)
			ctx = context.WithValue(ctx, middleware.ContextKeyUser, name)

			h.ServeHTTP(w, r.WithContext(ctx))
		})
	})

	api.Handle(
		"/experiments/{name}/scorch/components/{run}/{loop}/{stage}/{cmp}",
		weberror.ErrorHandler(GetComponentOutput),
	).Methods("GET")
	api.HandleFunc("/experiments/{name}/scorch/components/{run}/{loop}/{stage}/{cmp}/ws", StreamComponentOutput).
		Methods("GET")
	api.HandleFunc("/experiments/{name}/scorch/terminals", GetTerminals).Methods("GET")
	api.HandleFunc("/experiments/{name}/scorch/terminals/{pid}/exit/{id}", ExitTerminal).Methods("POST")
	api.HandleFunc("/experiments/{name}/scorch/terminals/{pid}/ws/{id}", StreamTerminal).Methods("GET")
	api.HandleFunc("/experiments/{name}/scorch/terminals/{run}/{loop}/{stage}/{cmp}", ConnectTerminal).
		Methods("GET")

	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)

	return srv
}

// testTerminal creates the web terminal of the break component brk in the
// start stage of run 0 of exp, a process that runs until the test ends.
func testTerminal(t *testing.T, exp string) WebTerm {
	t.Helper()

	return testTerminalCmd(t, exp, "/bin/sleep", "60")
}

// testTerminalCmd is testTerminal with the given command for the process.
func testTerminalCmd(t *testing.T, exp, cmd string, args ...string) WebTerm {
	t.Helper()

	if _, err := CreateWebTerminal(
		context.Background(), exp, 0, 0, "start", "brk", t.TempDir(), cmd, args,
	); err != nil {
		t.Fatal(err)
	}

	terms, _ := GetExperimentTerminals(exp, -1)
	if len(terms) != 1 {
		t.Fatalf("terminals of %s = %v, want one", exp, terms)
	}

	t.Cleanup(func() { _ = KillTerminal(terms[0]) })

	return terms[0]
}

// requestTerminal asks for the terminal of term's component as role, as the
// web UI does, and returns the status and the terminal answered.
func requestTerminal(t *testing.T, srv *httptest.Server, role string, term WebTerm) (int, WebTerm) {
	t.Helper()

	url := fmt.Sprintf(
		"%s/api/v1/experiments/%s/scorch/terminals/%d/%d/%s/%s?role=%s",
		srv.URL, term.Exp, term.Run, term.Loop, term.Stage, term.Name, role,
	)

	resp, err := http.Get(url) //nolint:gosec // test server URL
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = resp.Body.Close() }()

	var got WebTerm

	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
			t.Fatalf("decoding the terminal: %v", err)
		}
	}

	return resp.StatusCode, got
}

// openTerminal opens the websocket at loc, a terminal's location, as role.
func openTerminal(srv *httptest.Server, role, loc string) (*websocket.Conn, error) {
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/" + strings.TrimPrefix(loc, "/") + "?role=" + role

	return websocket.Dial(url, "", srv.URL) //nolint:wrapcheck // test helper
}

// waitNoOwner waits until no client owns the read/write end of the terminal.
func waitNoOwner(t *testing.T, pid int) {
	t.Helper()

	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		mu.Lock()
		_, owned := rwTerm[pid]
		mu.Unlock()

		if !owned {
			return
		}
	}

	t.Errorf("the read/write end of terminal %d is still owned", pid)
}

// TestTerminalOwnershipReadUnderLock is a regression test: StreamTerminal and
// ExitTerminal read which client owns a terminal's read/write end without the
// lock under which the other handlers change it, and a concurrent map read
// and write ends phenix ui. Run it with -race.
func TestTerminalOwnershipReadUnderLock(t *testing.T) {
	admin := testRole([2]string{"*", "*"}, [2]string{"*/*", "*"})
	srv := terminalTestServer(t, map[string]rbac.Role{"admin": admin})
	terms := []WebTerm{testTerminal(t, "own-exp-a"), testTerminal(t, "own-exp-b")}

	var (
		wg   sync.WaitGroup
		stop = make(chan struct{})
	)

	// Exit requests of a client that owns nothing.
	wg.Add(1)

	go func() {
		defer wg.Done()

		url := fmt.Sprintf(
			"%s/api/v1/experiments/%s/scorch/terminals/%d/exit/nobody?role=admin",
			srv.URL, terms[0].Exp, terms[0].Pid,
		)

		for {
			select {
			case <-stop:
				return
			default:
			}

			resp, err := http.Post(url, "", nil) //nolint:gosec // test server URL
			if err != nil {
				t.Error(err)

				return
			}

			_ = resp.Body.Close()

			if resp.StatusCode != http.StatusForbidden {
				t.Errorf("exit by a client that owns nothing: status %d, want 403", resp.StatusCode)

				return
			}
		}
	}()

	// Clients that take the read/write end, open its websocket and leave.
	var owners sync.WaitGroup

	for _, term := range terms {
		owners.Add(1)

		go func() {
			defer owners.Done()

			for range 20 {
				code, got := requestTerminal(t, srv, "admin", term)
				if code != http.StatusOK || got.RO {
					t.Errorf("terminal request: status %d, read-only %t, want 200 and read/write", code, got.RO)

					return
				}

				ws, err := openTerminal(srv, "admin", got.Loc)
				if err != nil {
					t.Errorf("opening the terminal websocket: %v", err)

					return
				}

				_ = ws.Close()

				waitNoOwner(t, term.Pid)
			}
		}()
	}

	owners.Wait()
	close(stop)
	wg.Wait()
}

// TestTerminalInitTimeoutKeepsOwner is a regression test: a client that asked
// for a terminal and did not open its websocket in time gave up the
// read/write end whoever owned it, so the owner's exit was refused and the
// next client got a second read/write end.
func TestTerminalInitTimeoutKeepsOwner(t *testing.T) {
	original := terminalInitTimeout
	terminalInitTimeout = 100 * time.Millisecond

	t.Cleanup(func() { terminalInitTimeout = original })

	srv := terminalTestServer(t, map[string]rbac.Role{
		"admin":   testRole([2]string{"*", "*"}, [2]string{"*/*", "*"}),
		"watcher": testRole([2]string{"experiments", "get"}),
	})
	term := testTerminal(t, "own-exp-idle")

	code, owner := requestTerminal(t, srv, "admin", term)
	if code != http.StatusOK || owner.RO {
		t.Fatalf("first terminal request: status %d, read-only %t, want 200 and read/write", code, owner.RO)
	}

	ws, err := openTerminal(srv, "admin", owner.Loc)
	if err != nil {
		t.Fatalf("opening the owner's websocket: %v", err)
	}

	defer func() { _ = ws.Close() }()

	// A client that never opens its websocket.
	if code, idle := requestTerminal(t, srv, "watcher", term); code != http.StatusOK || !idle.RO {
		t.Fatalf("second terminal request: status %d, read-only %t, want 200 and read-only", code, idle.RO)
	}

	time.Sleep(4 * terminalInitTimeout)

	if code, next := requestTerminal(t, srv, "admin", term); code != http.StatusOK || !next.RO {
		t.Errorf("terminal request after the idle client's timeout: status %d, read-only %t, want 200 and read-only",
			code, next.RO)
	}

	resp, err := http.Post(srv.URL+"/"+owner.Exit+"?role=admin", "", nil) //nolint:gosec // test server URL
	if err != nil {
		t.Fatal(err)
	}

	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("the owner's exit after the idle client's timeout: status %d, want 204", resp.StatusCode)
	}
}

// outputStatus asks GetComponentOutput for the output of the component brk
// in the start stage of run 0 of exp as role, and returns the status
// answered, or 0 if it did not answer within five seconds. It calls the
// handler itself: in this test the component processor, which it would wait
// for, does not run.
func outputStatus(t *testing.T, role rbac.Role, exp string) int {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/experiments/"+exp+"/scorch/components/0/0/start/brk", nil)
	req = mux.SetURLVars(req, map[string]string{"name": exp, "run": "0", "loop": "0", "stage": "start", "cmp": "brk"})

	ctx := context.WithValue(req.Context(), middleware.ContextKeyRole, role)
	ctx = context.WithValue(ctx, middleware.ContextKeyUser, "test-user")

	rec := httptest.NewRecorder()
	done := make(chan struct{})

	go func() {
		defer close(done)

		weberror.ErrorHandler(GetComponentOutput).ServeHTTP(rec, req.WithContext(ctx))
	}()

	select {
	case <-done:
		return rec.Code
	case <-time.After(5 * time.Second):
		return 0
	}
}

// TestTerminalAccessNeedsRoles is a regression test: the SCORCH terminal and
// component output handlers checked no role, so any signed-in user could
// list a break component's terminal, take its read/write end, a shell on the
// phenix host, end the operator's terminal, and read any component's output.
func TestTerminalAccessNeedsRoles(t *testing.T) {
	srv := terminalTestServer(t, map[string]rbac.Role{
		"viewer":   testRole([2]string{"vms", "list"}),
		"watcher":  testRole([2]string{"experiments", "get"}),
		"operator": testRole([2]string{"experiments", "get"}, [2]string{"experiments/trigger", "create"}),
	})
	term := testTerminal(t, "rbac-exp")
	client := &http.Client{Timeout: 5 * time.Second}

	status := func(method, path string) int {
		t.Helper()

		req, err := http.NewRequestWithContext(context.Background(), method, srv.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}

		resp, err := client.Do(req)
		if err != nil {
			t.Errorf("%s %s: %v", method, path, err)

			return 0
		}

		_ = resp.Body.Close()

		return resp.StatusCode
	}

	listed := func(role string) bool {
		t.Helper()

		resp, err := client.Get(srv.URL + "/api/v1/experiments/rbac-exp/scorch/terminals?role=" + role)
		if err != nil {
			t.Fatal(err)
		}

		defer func() { _ = resp.Body.Close() }()

		var body struct {
			Terminals []WebTerm `json:"terminals"`
		}

		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("decoding the terminals: %v", err)
		}

		return len(body.Terminals) != 0
	}

	output := "/api/v1/experiments/rbac-exp/scorch/components/0/0/start/brk"

	// A role without the experiment sees and reaches nothing.
	if listed("viewer") {
		t.Error("the terminal list shows the terminal to a role without the experiment")
	}

	if code, _ := requestTerminal(t, srv, "viewer", term); code != http.StatusForbidden {
		t.Errorf("terminal request by a role without the experiment: status %d, want 403", code)
	}

	if code := outputStatus(t, testRole([2]string{"vms", "list"}), "rbac-exp"); code != http.StatusForbidden {
		t.Errorf("component output request by a role without the experiment: status %d, want 403", code)
	}

	if ws, err := openTerminal(srv, "viewer", strings.TrimPrefix(output, "/")+"/ws"); err == nil {
		_ = ws.Close()

		t.Error("a role without the experiment opened a component output stream")
	}

	// A role that may not start runs watches, even when it asks first.
	if !listed("watcher") {
		t.Error("the terminal list hides the terminal from a role with the experiment")
	}

	code, watched := requestTerminal(t, srv, "watcher", term)
	if code != http.StatusOK || !watched.RO || watched.Exit != "" {
		t.Fatalf("first terminal request by a role that may not start runs: status %d, read-only %t, exit %q, "+
			"want 200, read-only and no exit", code, watched.RO, watched.Exit)
	}

	ws, err := openTerminal(srv, "watcher", watched.Loc)
	if err != nil {
		t.Fatalf("a role with the experiment could not watch the terminal: %v", err)
	}

	_ = ws.Close()

	// The operator gets the read/write end, which only roles that may type
	// open or end.
	code, owned := requestTerminal(t, srv, "operator", term)
	if code != http.StatusOK || owned.RO || owned.Exit == "" {
		t.Fatalf("terminal request by a role that may start runs: status %d, read-only %t, exit %q, "+
			"want 200, read/write and an exit", code, owned.RO, owned.Exit)
	}

	for _, role := range []string{"viewer", "watcher"} {
		if ws, err := openTerminal(srv, role, owned.Loc); err == nil {
			_ = ws.Close()

			t.Errorf("role %s opened the read/write end", role)
		}

		if code := status(http.MethodPost, "/"+owned.Exit+"?role="+role); code != http.StatusForbidden {
			t.Errorf("exit by role %s: status %d, want 403", role, code)
		}
	}

	ws, err = openTerminal(srv, "operator", owned.Loc)
	if err != nil {
		t.Fatalf("the operator could not open the read/write end after the refusals: %v", err)
	}

	defer func() { _ = ws.Close() }()

	if code := status(http.MethodPost, "/"+owned.Exit+"?role=operator"); code != http.StatusNoContent {
		t.Errorf("exit by the operator: status %d, want 204", code)
	}
}

// TestTerminalObserversLeaving is a regression test: when a write to a
// read-only client of a terminal failed, the terminal's reader closed the
// client's channel but left the client listed until its handler removed it,
// so the next output closed the channel again, a panic that ended phenix ui.
// The reader also ranged over the listed clients outside the lock under
// which they come and go. Run it with -race.
func TestTerminalObserversLeaving(t *testing.T) {
	srv := terminalTestServer(t, map[string]rbac.Role{"admin": testRole([2]string{"*", "*"}, [2]string{"*/*", "*"})})
	term := testTerminalCmd(t, "obs-exp", "/usr/bin/yes")

	code, owned := requestTerminal(t, srv, "admin", term)
	if code != http.StatusOK || owned.RO {
		t.Fatalf("terminal request: status %d, read-only %t, want 200 and read/write", code, owned.RO)
	}

	owner, err := openTerminal(srv, "admin", owned.Loc)
	if err != nil {
		t.Fatalf("opening the read/write end: %v", err)
	}

	defer func() { _ = owner.Close() }()

	// The owner reads the endless output, so the terminal keeps writing.
	go func() { _, _ = io.Copy(io.Discard, owner) }()

	for range 30 {
		var observers sync.WaitGroup

		for range 4 {
			observers.Add(1)

			go func() {
				defer observers.Done()

				code, watched := requestTerminal(t, srv, "admin", term)
				if code != http.StatusOK || !watched.RO {
					t.Errorf("observer request: status %d, read-only %t, want 200 and read-only", code, watched.RO)

					return
				}

				ws, err := openTerminal(srv, "admin", watched.Loc)
				if err != nil {
					t.Errorf("opening an observer: %v", err)

					return
				}

				_ = ws.Close()
			}()
		}

		observers.Wait()
		time.Sleep(10 * time.Millisecond)
	}
}

// TestKilledTerminalDropsHistory is a regression test: a terminal's output,
// which read-only clients are shown when they join, was dropped only when a
// client exited the terminal, so the output of a terminal its run's cancel
// ended stayed in memory until phenix ui stopped and was shown to the
// clients of a later terminal with the same PID.
func TestKilledTerminalDropsHistory(t *testing.T) {
	srv := terminalTestServer(t, map[string]rbac.Role{"admin": testRole([2]string{"*", "*"}, [2]string{"*/*", "*"})})
	term := testTerminalCmd(t, "hist-exp", "/bin/sh", "-c", "echo history-marker; exec sleep 60")

	code, owned := requestTerminal(t, srv, "admin", term)
	if code != http.StatusOK || owned.RO {
		t.Fatalf("terminal request: status %d, read-only %t, want 200 and read/write", code, owned.RO)
	}

	ws, err := openTerminal(srv, "admin", owned.Loc)
	if err != nil {
		t.Fatalf("opening the read/write end: %v", err)
	}

	defer func() { _ = ws.Close() }()

	go func() { _, _ = io.Copy(io.Discard, ws) }()

	recorded := func() bool {
		mu.Lock()
		defer mu.Unlock()

		h, ok := history[term.Pid]

		return ok && strings.Contains(h.String(), "history-marker")
	}

	for deadline := time.Now().Add(5 * time.Second); !recorded(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the terminal's output was never recorded")
		}
	}

	// What the watcher of the run's context does when the run is canceled.
	if err := KillTerminal(term); err != nil {
		t.Fatal(err)
	}

	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	_, kept := history[term.Pid]
	mu.Unlock()

	if kept {
		t.Error("the killed terminal's output is still kept")
	}
}

// processGone reports whether the process with the given PID ended and was
// reaped within five seconds.
func processGone(pid int) bool {
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return true
		}
	}

	return false
}

// TestStalledObserverKeepsNoTerminalRunning checks that ending a terminal
// kills its process and returns although a read-only client of the terminal
// does not read its output.
func TestStalledObserverKeepsNoTerminalRunning(t *testing.T) {
	srv := terminalTestServer(t, map[string]rbac.Role{"watcher": testRole([2]string{"experiments", "get"})})
	term := testTerminal(t, "stall-exp")

	// More output than the socket buffers between the server and a client
	// hold.
	mu.Lock()
	history[term.Pid] = *bytes.NewBuffer(bytes.Repeat([]byte("x"), 16<<20))
	mu.Unlock()

	code, watched := requestTerminal(t, srv, "watcher", term)
	if code != http.StatusOK || !watched.RO {
		t.Fatalf("terminal request: status %d, read-only %t, want 200 and read-only", code, watched.RO)
	}

	// An observer that never reads.
	ws, err := openTerminal(srv, "watcher", watched.Loc)
	if err != nil {
		t.Fatalf("opening the observer: %v", err)
	}

	defer func() { _ = ws.Close() }()

	// Wait until the observer is listed, which it is under the lock before
	// it is shown the output so far.
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if !mu.TryLock() {
			continue
		}

		_, listed := roTerms[term.Pid]
		mu.Unlock()

		if listed {
			break
		}
	}

	ended := make(chan error, 1)

	go func() { ended <- KillTerminal(term) }()

	select {
	case err := <-ended:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ending the terminal waits for an observer that does not read")
	}

	if !processGone(term.Pid) {
		t.Errorf("the process of the ended terminal, PID %d, still runs", term.Pid)
	}
}

// TestKillTerminalKillsBeforeLock checks that KillTerminal kills the
// terminal's process before it takes the lock of the terminals to drop the
// terminal's output, so whatever holds that lock cannot keep the shell
// running.
func TestKillTerminalKillsBeforeLock(t *testing.T) {
	term := testTerminal(t, "lock-exp")
	ended := make(chan error, 1)

	mu.Lock()

	go func() { ended <- KillTerminal(term) }()

	gone := processGone(term.Pid)

	mu.Unlock()

	if !gone {
		t.Errorf("the process of the ended terminal, PID %d, still runs while the lock is held", term.Pid)
	}

	if err := <-ended; err != nil {
		t.Fatal(err)
	}
}

// TestTerminalRoutesWithoutRole checks that the SCORCH terminal and component
// output handlers answer 403 to a request that the authentication middleware
// passes with no role, one whose path ends in /login or /signup.
func TestTerminalRoutesWithoutRole(t *testing.T) {
	router := mux.NewRouter()
	api := router.PathPrefix("/api/v1").Subrouter()

	api.Use(middleware.Auth("secret-key", ""))
	api.Handle(
		"/experiments/{name}/scorch/components/{run}/{loop}/{stage}/{cmp}",
		weberror.ErrorHandler(GetComponentOutput),
	).Methods("GET")
	api.HandleFunc("/experiments/{name}/scorch/terminals/{pid}/exit/{id}", ExitTerminal).Methods("POST")
	api.HandleFunc("/experiments/{name}/scorch/terminals/{pid}/ws/{id}", StreamTerminal).Methods("GET")
	api.HandleFunc("/experiments/{name}/scorch/terminals/{run}/{loop}/{stage}/{cmp}", ConnectTerminal).
		Methods("GET")

	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)

	term := testTerminal(t, "norole-exp")
	client := &http.Client{Timeout: 5 * time.Second}

	for _, route := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/experiments/norole-exp/scorch/components/0/0/start/login"},
		{http.MethodGet, "/api/v1/experiments/norole-exp/scorch/terminals/0/0/start/signup"},
		{http.MethodGet, fmt.Sprintf("/api/v1/experiments/norole-exp/scorch/terminals/%d/ws/login", term.Pid)},
		{http.MethodPost, fmt.Sprintf("/api/v1/experiments/norole-exp/scorch/terminals/%d/exit/login", term.Pid)},
	} {
		req, err := http.NewRequestWithContext(context.Background(), route.method, srv.URL+route.path, nil)
		if err != nil {
			t.Fatal(err)
		}

		resp, err := client.Do(req)
		if err != nil {
			t.Errorf("%s %s without a role: %v, want 403", route.method, route.path, err)

			continue
		}

		_ = resp.Body.Close()

		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s %s without a role: status %d, want 403", route.method, route.path, resp.StatusCode)
		}
	}
}

// upgradeStatus asks to open the websocket at loc, a terminal's location, as
// role, and returns the status answered.
func upgradeStatus(srv *httptest.Server, role, loc string) (int, error) {
	url := srv.URL + "/" + strings.TrimPrefix(loc, "/") + "?role=" + role

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		return 0, err //nolint:wrapcheck // test helper
	}

	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	req.Header.Set("Origin", srv.URL)

	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return 0, err //nolint:wrapcheck // test helper
	}

	_ = resp.Body.Close()

	return resp.StatusCode, nil
}

// TestTerminalClientIDOpensOnce is a regression test: a client ID opens one
// websocket, so the channel that tells the ID's timeout the ID was taken is
// closed once. Two websockets opened with one ID, one after the other or at
// once, closed it twice, a panic.
func TestTerminalClientIDOpensOnce(t *testing.T) {
	srv := terminalTestServer(t, map[string]rbac.Role{"watcher": testRole([2]string{"experiments", "get"})})
	term := testTerminal(t, "once-exp")

	// One after the other.
	code, watched := requestTerminal(t, srv, "watcher", term)
	if code != http.StatusOK {
		t.Fatalf("terminal request: status %d, want 200", code)
	}

	ws, err := openTerminal(srv, "watcher", watched.Loc)
	if err != nil {
		t.Fatalf("opening the terminal websocket: %v", err)
	}

	defer func() { _ = ws.Close() }()

	if code, err := upgradeStatus(srv, "watcher", watched.Loc); err != nil || code != http.StatusNotFound {
		t.Errorf("a second websocket with a client ID: status %d (%v), want 404", code, err)
	}

	// At once.
	code, watched = requestTerminal(t, srv, "watcher", term)
	if code != http.StatusOK {
		t.Fatalf("terminal request: status %d, want 200", code)
	}

	const tries = 8

	var (
		wg    sync.WaitGroup
		codes = make(chan int, tries)
	)

	for range tries {
		wg.Add(1)

		go func() {
			defer wg.Done()

			code, err := upgradeStatus(srv, "watcher", watched.Loc)
			if err != nil {
				t.Errorf("a websocket with a shared client ID: %v, want 101 or 404", err)

				return
			}

			codes <- code
		}()
	}

	wg.Wait()
	close(codes)

	opened := 0

	for code := range codes {
		switch code {
		case http.StatusSwitchingProtocols:
			opened++
		case http.StatusNotFound:
		default:
			t.Errorf("a websocket with a shared client ID: status %d, want 101 or 404", code)
		}
	}

	if opened != 1 {
		t.Errorf("%d websockets opened with one client ID, want 1", opened)
	}
}
