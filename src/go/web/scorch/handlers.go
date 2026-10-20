package scorch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/gofrs/uuid/v5"
	"github.com/gorilla/mux"
	"golang.org/x/net/websocket"

	"phenix/api/experiment"
	"phenix/api/scorch/scorchexe"
	"phenix/api/scorch/scorchmd"
	"phenix/app"
	"phenix/util/plog"
	"phenix/util/pubsub"
	"phenix/web/middleware"
	"phenix/web/rbac"
	"phenix/web/util"
	"phenix/web/weberror"
)

// stopWait bounds how long an experiment stop waits for its canceled SCORCH
// runs to run their stop and cleanup stages, which get TeardownTimeout in
// all. The minute left covers what a run does after them: Filebeat, its
// archive and its status.
const stopWait = scorchexe.TeardownTimeout + time.Minute

func init() { //nolint:gochecknoinits // hook registration
	// Cancel the experiment's SCORCH runs before its VMs are killed and its
	// minimega namespace is cleared: their stop and cleanup stages release
	// captures and QoS rules on those VMs, and a minimega command sent after
	// the namespace is cleared creates it again.
	experiment.RegisterHook("stopping", func(_, name string) {
		if err := scorchexe.CancelExperiment(name, stopWait); err != nil {
			plog.Error(plog.TypeSystem, "ending Scorch runs of stopping experiment", "exp", name, "err", err)
		}
	})

	// The stop refuses new runs from its stopping hook until it is done,
	// however it ends.
	experiment.RegisterHook("stop-end", func(_, name string) { scorchexe.Stopped(name) })
}

const (
	appNameScorch       = "scorch"
	TerminalBufferSize  = 32 * 1024
	TerminalInitTimeout = 5 * time.Second
)

type termClient struct {
	id   string
	ws   *websocket.Conn
	done chan struct{}

	// wmu orders the writes to the client, so output it joined after waits
	// until the output from before it joined is written.
	wmu *sync.Mutex
}

func newTermClient(ws *websocket.Conn) termClient {
	return termClient{
		id:   uuid.Must(uuid.NewV4()).String(),
		ws:   ws,
		done: make(chan struct{}),
		wmu:  new(sync.Mutex),
	}
}

// write writes b to the client after the writes to it in progress, and
// reports whether all of b was written. It is called without mu: a client
// that does not read holds up its write.
func (tc termClient) write(b []byte) bool {
	tc.wmu.Lock()
	defer tc.wmu.Unlock()

	nw, err := tc.ws.Write(b)

	return err == nil && nw == len(b)
}

// terminalInitTimeout is TerminalInitTimeout; tests shorten it.
var terminalInitTimeout = TerminalInitTimeout //nolint:gochecknoglobals // shortened by tests

var (
	rwTerm  = make(map[int]string)                //nolint:gochecknoglobals // global state
	roTerms = make(map[int]map[string]termClient) //nolint:gochecknoglobals // global state
	history = make(map[int]bytes.Buffer)          //nolint:gochecknoglobals // global state

	termClientIDs = make(map[string]chan struct{}) //nolint:gochecknoglobals // global state

	mu sync.Mutex //nolint:gochecknoglobals // global lock
)

// The terminal of a break component is a shell on the phenix host. Listing
// and watching it, and reading component output, need the experiment;
// typing into it and ending it also need the right to start the
// experiment's runs.

func mayWatch(r *http.Request, exp string) bool {
	role := middleware.RoleFromContext(r.Context())

	return role.Allowed("experiments", "get", exp)
}

func mayType(r *http.Request, exp string) bool {
	role := middleware.RoleFromContext(r.Context())

	return role.Allowed("experiments", "get", exp) && role.Allowed("experiments/trigger", "create", exp)
}

// forbid answers 403 to the request, which the role does not allow to do
// what to the experiment's SCORCH terminals or output.
func forbid(w http.ResponseWriter, r *http.Request, what, exp string) {
	user := middleware.UserFromContext(r.Context())

	plog.Warn(plog.TypeSecurity, what+" not allowed", "user", user, "exp", exp)
	http.Error(w, fmt.Sprintf("%s of experiment %s not allowed for %s", what, exp, user), http.StatusForbidden)
}

// GetTerminals - GET /experiments/{name}/scorch/terminals.
func GetTerminals(w http.ResponseWriter, r *http.Request) {
	plog.Debug(plog.TypeSystem, "HTTP handler called", "handler", "GetTerminal")

	var (
		vars = mux.Vars(r)
		exp  = vars["name"]
		role = middleware.RoleFromContext(r.Context())
	)

	terms, _ := GetExperimentTerminals(exp, -1)

	// The name can list several experiments.
	terms = slices.DeleteFunc(terms, func(t WebTerm) bool { return !role.Allowed("experiments", "get", t.Exp) })

	body, _ := json.Marshal(util.WithRoot("terminals", terms))
	_, _ = w.Write(body) //nolint:gosec // XSS via taint analysis
}

// ConnectTerminal - GET /experiments/{name}/scorch/terminals/{run}/{loop}/{stage}/{cmp}.
func ConnectTerminal(w http.ResponseWriter, r *http.Request) {
	plog.Debug(plog.TypeSystem, "HTTP handler called", "handler", "ConnectTerminal")

	var (
		vars  = mux.Vars(r)
		exp   = vars["name"]
		stage = vars["stage"]
		cmp   = vars["cmp"]
	)

	run, err := strconv.Atoi(vars["run"])
	if err != nil {
		http.Error(w, "invalid run ID provided", http.StatusBadRequest)

		return
	}

	loop, err := strconv.Atoi(vars["loop"])
	if err != nil {
		http.Error(w, "invalid loop number provided", http.StatusBadRequest)

		return
	}

	if !mayWatch(r, exp) {
		forbid(w, r, "connecting to Scorch terminals", exp)

		return
	}

	t, err := initTerminal(exp, run, loop, stage, cmp, mayType(r, exp))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)

		return
	}

	body, _ := json.Marshal(t)
	_, _ = w.Write(body) //nolint:gosec // XSS via taint analysis
}

// StreamTerminal - GET /experiments/{name}/scorch/terminals/{pid}/ws/{id}.
func StreamTerminal(w http.ResponseWriter, r *http.Request) {
	plog.Debug(plog.TypeSystem, "HTTP handler called", "handler", "StreamTerminal")

	exp := mux.Vars(r)["name"]
	pid, _ := strconv.Atoi(mux.Vars(r)["pid"])

	if !mayWatch(r, exp) {
		forbid(w, r, "streaming Scorch terminals", exp)

		return
	}

	t, err := GetTerminalByPID(pid)
	if err != nil {
		http.Error(w, "no web terminal found", http.StatusNotFound)

		return
	}

	if t.Exp != exp {
		http.Error(w, "no web terminal found", http.StatusNotFound)

		return
	}

	var (
		id       = mux.Vars(r)["id"]
		mayWrite = mayType(r, exp)
	)

	// A client ID opens one websocket, so its channel is closed once, and
	// opens the read/write end only for a role that may type. The terminal
	// maps are read under mu: a concurrent map read and write ends the
	// process.
	mu.Lock()
	done, ok := termClientIDs[id]
	owner := rwTerm[pid]
	refused := ok && owner == id && !mayWrite

	if ok && !refused {
		delete(termClientIDs, id)
	}
	mu.Unlock()

	if !ok {
		http.Error(w, "terminal client ID invalid", http.StatusNotFound)

		return
	}

	if refused {
		forbid(w, r, "typing into Scorch terminals", exp)

		return
	}

	close(done)

	t.RO = owner != id

	plog.Debug(plog.TypeSystem, "starting web terminal streamer", "pid", pid)

	websocket.Handler(terminalWsHandler(t)).ServeHTTP(w, r)
}

// ExitTerminal - POST /experiments/{name}/scorch/terminals/{pid}/exit/{id}.
func ExitTerminal(w http.ResponseWriter, r *http.Request) {
	plog.Debug(plog.TypeSystem, "HTTP handler called", "handler", "ExitTerminal")

	exp := mux.Vars(r)["name"]
	pid, _ := strconv.Atoi(mux.Vars(r)["pid"])
	id := mux.Vars(r)["id"]

	if !mayType(r, exp) {
		forbid(w, r, "exiting Scorch terminals", exp)

		return
	}

	mu.Lock()
	owner, ok := rwTerm[pid]
	mu.Unlock()

	if !ok || owner != id {
		plog.Error(
			plog.TypeSystem,
			"terminal client doesn't own R/W rights to PTY",
			"id",
			id,
			"pid",
			pid,
		)
		http.Error(w, "terminal client not allowed to exit terminal", http.StatusForbidden)

		return
	}

	t, err := GetTerminalByPID(pid)
	if err != nil {
		plog.Error(plog.TypeSystem, "web terminal for PID not found", "pid", pid)
		http.Error(w, "web terminal not found", http.StatusNotFound)

		return
	}

	if t.Exp != exp {
		http.Error(w, "no web terminal found", http.StatusNotFound)

		return
	}

	if err := KillTerminal(t); err != nil {
		plog.Error(plog.TypeSystem, "killing terminal for PID", "pid", pid, "err", err)
		http.Error(w, "error exiting terminal", http.StatusNotFound)

		return
	}

	w.WriteHeader(http.StatusNoContent)
}

//nolint:funlen // handler
func terminalWsHandler(t WebTerm) func(*websocket.Conn) {
	return func(ws *websocket.Conn) {
		_, err := os.FindProcess(t.Pid)
		if err != nil {
			plog.Error(plog.TypeSystem, "unable to find process", "pid", t.Pid)

			return
		}

		readTerm := func() {
			for {
				buf := make([]byte, TerminalBufferSize)

				nr, err := t.Pty.Read(buf)
				if err != nil {
					break
				}

				nw, err := ws.Write(buf[0:nr])
				if err != nil {
					break
				}

				if nw != nr {
					break
				}

				mu.Lock()

				// killTerminal drops the history after it closes Done.
				select {
				case <-t.Done:
				default:
					h := history[t.Pid]
					h.Write(buf[0:nr])
					history[t.Pid] = h
				}

				// A copy: observers come and go while it is written to.
				ro := slices.Collect(maps.Values(roTerms[t.Pid]))
				mu.Unlock()

				for _, client := range ro {
					if !client.write(buf[0:nr]) {
						dropObserver(t.Pid, client)
					}
				}
			}
		}

		writeTerm := func() {
			for {
				buf := make([]byte, TerminalBufferSize)

				nr, err := ws.Read(buf)
				if err != nil {
					break
				}

				nw, err := t.Pty.Write(buf[0:nr])
				if err != nil {
					break
				}

				if nw != nr {
					break
				}
			}
		}

		waitTerm := func(tc termClient) {
			select {
			case <-tc.done: // dropObserver has unlisted the client
				return
			case <-t.Done:
			}

			mu.Lock()
			delete(roTerms[t.Pid], tc.id)

			if len(roTerms[t.Pid]) == 0 {
				delete(roTerms, t.Pid)
			}
			mu.Unlock()

			// notify the read-only client that the R/W terminal has exited
			_ = tc.write([]byte("***** BREAK PROCESS EXITED *****"))
		}

		if t.RO {
			tc := newTermClient(ws)

			mu.Lock()

			terms, ok := roTerms[t.Pid]
			if !ok {
				terms = make(map[string]termClient)
			}

			terms[tc.id] = tc
			roTerms[t.Pid] = terms

			// The client is shown the output so far before the output that
			// comes after it is listed, which waits for wmu.
			tc.wmu.Lock()

			h := history[t.Pid]
			past := bytes.Clone(h.Bytes())

			mu.Unlock()

			if len(past) != 0 {
				_, _ = tc.ws.Write(past)
			}

			tc.wmu.Unlock()

			waitTerm(tc)
		} else {
			go readTerm()

			writeTerm()

			mu.Lock()
			delete(rwTerm, t.Pid)
			mu.Unlock()
		}
	}
}

// dropObserver ends the websocket of a read-only client of the terminal that
// a write failed for. It closes the client's channel once, however many
// writes fail before the client's handler has gone.
func dropObserver(pid int, tc termClient) {
	mu.Lock()
	defer mu.Unlock()

	if _, ok := roTerms[pid][tc.id]; ok {
		delete(roTerms[pid], tc.id)
		close(tc.done)
	}
}

// initTerminal hands out a client ID for the terminal of the given component,
// and its read/write end if no client holds it and rw allows it.
func initTerminal(exp string, run, loop int, stage, cmp string, rw bool) (WebTerm, error) {
	key := fmt.Sprintf("%s|%d|%d|%s|%s", exp, run, loop, stage, cmp)

	t, err := GetTerminalByExperiment(key)
	if err != nil {
		return WebTerm{}, errors.New("no web terminal found")
	}

	if t.Exp != exp {
		return WebTerm{}, errors.New("no web terminal found")
	}

	id := uuid.Must(uuid.NewV4()).String()
	t.Loc = fmt.Sprintf(
		"%sapi/v1/experiments/%s/scorch/terminals/%d/ws/%s",
		basePath,
		exp,
		t.Pid,
		id,
	)

	mu.Lock()
	defer mu.Unlock()

	if _, ok := rwTerm[t.Pid]; ok || !rw {
		t.RO = true
	} else {
		rwTerm[t.Pid] = id
		t.Exit = fmt.Sprintf(
			"%sapi/v1/experiments/%s/scorch/terminals/%d/exit/%s",
			basePath,
			exp,
			t.Pid,
			id,
		)
	}

	done := make(chan struct{})
	termClientIDs[id] = done

	expired := time.After(terminalInitTimeout)

	go func() {
		select {
		case <-expired:
		case <-done:
			return
		}

		mu.Lock()
		defer mu.Unlock()

		// A client that did not open its websocket in time loses its ID and
		// the read/write end if it took it, never another client's.
		if _, ok := termClientIDs[id]; ok {
			delete(termClientIDs, id)

			if rwTerm[t.Pid] == id {
				delete(rwTerm, t.Pid)
			}
		}
	}()

	return t, nil
}

// GetComponentOutput - GET /experiments/{name}/scorch/components/{run}/{loop}/{stage}/{cmp}.
func GetComponentOutput(w http.ResponseWriter, r *http.Request) error {
	plog.Debug(plog.TypeSystem, "HTTP handler called", "handler", "GetScorchComponentOutput")

	var (
		vars  = mux.Vars(r)
		exp   = vars["name"]
		stage = vars["stage"]
		cmp   = vars["cmp"]
	)

	run, err := strconv.Atoi(vars["run"])
	if err != nil {
		return weberror.NewWebError(err, "invalid run ID '%s' provided", vars["run"])
	}

	loop, err := strconv.Atoi(vars["loop"])
	if err != nil {
		return weberror.NewWebError(err, "invalid loop number provided")
	}

	if !mayWatch(r, exp) {
		user := middleware.UserFromContext(r.Context())

		plog.Warn(plog.TypeSecurity, "getting Scorch component output not allowed", "user", user, "exp", exp)

		err := weberror.NewWebError(nil, "getting Scorch component output of experiment %s not allowed for %s", exp, user)

		return err.SetStatus(http.StatusForbidden)
	}

	key := fmt.Sprintf("%s|%d|%d|%s|%s", exp, run, loop, stage, cmp)
	req := outputRequest{key: key, resp: make(chan outputResponse)}

	outputRequests <- req

	resp := <-req.resp

	if resp.running {
		if resp.terminal {
			t, err := initTerminal(exp, run, loop, stage, cmp, mayType(r, exp))
			if err != nil {
				return weberror.NewWebError(err, "unable to initialize terminal")
			}

			body, _ := json.Marshal(util.WithRoot("terminal", t))

			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(body) //nolint:gosec // XSS via taint analysis

			return nil
		}

		body, _ := json.Marshal(
			util.WithRoot(
				"stream",
				fmt.Sprintf(
					"%sapi/v1/experiments/%s/scorch/components/%d/%d/%s/%s/ws",
					basePath,
					exp,
					run,
					loop,
					stage,
					cmp,
				),
			),
		)

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body) //nolint:gosec // XSS via taint analysis

		return nil
	}

	body, err := json.Marshal(util.WithRoot("output", string(resp.output)))
	if err != nil {
		err := weberror.NewWebError(err, "unable to process component %s output", cmp)

		return err.SetStatus(http.StatusInternalServerError)
	}

	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)

	return nil
}

// StreamComponentOutput - GET /experiments/{name}/scorch/components/{run}/{loop}/{stage}/{cmp}/ws.
func StreamComponentOutput(w http.ResponseWriter, r *http.Request) {
	plog.Debug(plog.TypeSystem, "HTTP handler called", "handler", "StreamScorchComponentOutput")

	var (
		vars  = mux.Vars(r)
		exp   = vars["name"]
		stage = vars["stage"]
		cmp   = vars["cmp"]
	)

	run, err := strconv.Atoi(vars["run"])
	if err != nil {
		http.Error(w, "invalid run ID provided", http.StatusBadRequest)

		return
	}

	loop, err := strconv.Atoi(vars["loop"])
	if err != nil {
		http.Error(w, "invalid loop number provided", http.StatusBadRequest)

		return
	}

	if !mayWatch(r, exp) {
		forbid(w, r, "streaming Scorch component output", exp)

		return
	}

	key := fmt.Sprintf("%s|%d|%d|%s|%s", exp, run, loop, stage, cmp)

	plog.Debug(plog.TypeSystem, "starting scorch component streamer", "key", key)

	websocket.Handler(scorchComponentWsHandler(key)).ServeHTTP(w, r)
}

func scorchComponentWsHandler(key string) func(*websocket.Conn) {
	return func(ws *websocket.Conn) {
		id := uuid.Must(uuid.NewV4()).String()
		req := wsRequest{key: key, id: id, ws: ws, done: make(chan struct{})}

		// add client to list for component
		wsRequests <- req
		// wait for done channel to be closed
		<-req.done
	}
}

// GetPipelines - GET /experiments/{name}/scorch/pipelines.
func GetPipelines(w http.ResponseWriter, r *http.Request) error {
	plog.Debug(plog.TypeSystem, "HTTP handler called", "handler", "GetPipelines")

	var (
		ctx     = r.Context()
		role, _ = ctx.Value(middleware.ContextKeyRole).(rbac.Role)
		vars    = mux.Vars(r)
		name    = vars["name"]
	)

	if !role.Allowed("experiments", "get", name) {
		plog.Warn(
			plog.TypeSecurity,
			"getting experiment scorch pipelines not allowed",
			"user",

			ctx.Value(middleware.ContextKeyUser),
			"exp",
			name,
		)
		user, _ := ctx.Value(middleware.ContextKeyUser).(string)
		err := weberror.NewWebError(
			nil,
			"getting experiment %s not allowed for %s",
			name,
			user,
		)

		return err.SetStatus(http.StatusForbidden)
	}

	exp, err := experiment.Get(name)
	if err != nil {
		return weberror.NewWebError(err, "unable to get experiment %s from store", name)
	}

	md, err := scorchmd.DecodeMetadata(exp)
	if err != nil {
		err := weberror.NewWebError(err, "unable to decode scorch metadata for experiment %s", name)

		return err.SetStatus(http.StatusInternalServerError)
	}

	var pipelines []*pipeline

	for run := range md.Runs {
		pipeline, err := RequestPipeline(name, run, 0)
		if err != nil {
			return weberror.NewWebError(
				err,
				"unable to get pipeline %d for experiment %s",
				run,
				name,
			)
		}

		pipelines = append(pipelines, pipeline)
	}

	var running bool

	// first make sure Scorch app is running
	for app, status := range exp.Status.AppRunning() {
		if app == appNameScorch && status {
			running = true

			break
		}
	}

	runID := -1

	// if Scorch app is running, find out which run is currently being executed
	if running {
		// TODO: this should never be nil if Scorch is running...
		if exp.Status.AppStatus() != nil {
			if status, ok := exp.Status.AppStatus()[appNameScorch].(map[string]any); ok {
				if id, ok := status["runID"].(float64); ok {
					runID = int(id)
				}
			}
		}
	}

	body, _ := json.Marshal(map[string]any{"pipelines": pipelines, "running": runID})

	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)

	return nil
}

// GetPipeline - GET /experiments/{name}/scorch/pipelines/{run}/{loop}.
func GetPipeline(w http.ResponseWriter, r *http.Request) error {
	plog.Debug(plog.TypeSystem, "HTTP handler called", "handler", "GetPipeline")

	var (
		ctx     = r.Context()
		role, _ = ctx.Value(middleware.ContextKeyRole).(rbac.Role)
		vars    = mux.Vars(r)
		exp     = vars["name"]
	)

	run, err := strconv.Atoi(vars["run"])
	if err != nil {
		return weberror.NewWebError(err, "invalid run ID '%s' provided", vars["run"])
	}

	loop, err := strconv.Atoi(vars["loop"])
	if err != nil {
		return weberror.NewWebError(err, "invalid loop number '%s' provided", vars["loop"])
	}

	if !role.Allowed("experiments", "get", exp) {
		plog.Warn(
			plog.TypeSecurity,
			"getting experiment scorch pipeline not allowed",
			"user",

			ctx.Value(middleware.ContextKeyUser),
			"exp",
			exp,
		)
		user, _ := ctx.Value(middleware.ContextKeyUser).(string)
		err := weberror.NewWebError(
			nil,
			"getting experiment %s not allowed for %s",
			exp,
			user,
		)

		return err.SetStatus(http.StatusForbidden)
	}

	pipeline, err := RequestPipeline(exp, run, loop)
	if err != nil {
		return weberror.NewWebError(err, "unable to get pipeline %d for experiment %s", run, exp)
	}

	body, _ := json.Marshal(pipeline)

	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body) //nolint:gosec // XSS via taint analysis

	return nil
}

// TODO: change this to `scorch/runs`

// StartPipeline - POST /experiments/{name}/scorch/pipelines/{run}.
//
//nolint:funlen // handler
func StartPipeline(w http.ResponseWriter, r *http.Request) error {
	plog.Debug(plog.TypeSystem, "HTTP handler called", "handler", "StartPipeline")

	var (
		ctx     = r.Context()
		role, _ = ctx.Value(middleware.ContextKeyRole).(rbac.Role)
		vars    = mux.Vars(r)
		name    = vars["name"]
	)

	run, err := strconv.Atoi(vars["run"])
	if err != nil {
		return weberror.NewWebError(err, "invalid run ID '%s' provided", vars["run"])
	}

	if !role.Allowed("experiments/trigger", "create", name) {
		user, _ := ctx.Value(middleware.ContextKeyUser).(string)
		err := weberror.NewWebError(
			nil,
			"starting Scorch runs for experiment %s not allowed for %s",
			name,
			user,
		)

		return err.SetStatus(http.StatusForbidden)
	}

	if !role.Allowed("experiments", "get", name) {
		user, _ := ctx.Value(middleware.ContextKeyUser).(string)
		err := weberror.NewWebError(
			nil,
			"getting experiment %s not allowed for %s",
			name,
			user,
		)

		return err.SetStatus(http.StatusForbidden)
	}

	exp, err := experiment.Get(name)
	if err != nil {
		return weberror.NewWebError(err, "unable to get experiment %s from store", name)
	}

	// Refuse here what Execute would refuse, so the response says why, then
	// register the run, which refuses a run of the experiment that executes
	// in this process or a stop in progress. A run started from the CLI
	// between this check and Execute is still refused only to the log and
	// pubsub.
	err = scorchexe.Check(exp, run)

	var finished func()

	if err == nil {
		// We don't want to use the HTTP request's context here.
		ctx, finished, err = scorchexe.Register(context.Background(), name, run)
	}

	if err != nil {
		werr := weberror.NewWebError(err, "unable to start Scorch run %d for experiment %s", run, name)

		if errors.Is(err, scorchexe.ErrAlreadyRunning) || errors.Is(err, scorchexe.ErrStopping) {
			return werr.SetStatus(http.StatusConflict)
		}

		return werr
	}

	ctx = app.SetContextTriggerUI(ctx)

	go func() {
		// This also releases the run's context.
		defer finished()

		plog.Debug(plog.TypeSystem, "executing Scorch run for experiment", "exp", name, "run", run)

		key := fmt.Sprintf("%s/%d", name, run)

		pubsub.Publish("trigger-app", app.TriggerPublication{ //nolint:exhaustruct // partial initialization
			Experiment: name, App: appNameScorch, Resource: key, State: "start",
		})

		err := scorchexe.Execute(ctx, exp, run)
		if err != nil {
			// A canceled run is an error only if its stop or cleanup stage
			// then failed.
			if !scorchexe.Canceled(err) {
				plog.Error(
					plog.TypeSystem,
					"executing Scorch run for experiment",
					"exp",
					name,
					"run",
					run,
					"err",
					err,
				)

				pubsub.Publish("trigger-app", app.TriggerPublication{ //nolint:exhaustruct // partial initialization
					Experiment: name,
					App:        appNameScorch,
					Resource:   key,
					State:      "error",
					Error: fmt.Errorf(
						"failed to execute Scorch run %d for experiment %s",
						run,
						name,
					),
				})
			}
		} else {
			plog.Debug(
				plog.TypeSystem,
				"Scorch run for experiment executed successfully",
				"exp",
				name,
				"run",
				run,
			)

			pubsub.Publish("trigger-app", app.TriggerPublication{ //nolint:exhaustruct // partial initialization
				Experiment: name, App: appNameScorch, Resource: key, State: "success",
			})
		}
	}()

	w.WriteHeader(http.StatusNoContent)

	return nil
}

// TODO: change this to `scorch/runs`

// CancelPipeline - DELETE /experiments/{name}/scorch/pipelines/{run}.
func CancelPipeline(w http.ResponseWriter, r *http.Request) error {
	plog.Debug(plog.TypeSystem, "HTTP handler called", "handler", "CancelPipeline")

	var (
		ctx     = r.Context()
		role, _ = ctx.Value(middleware.ContextKeyRole).(rbac.Role)
		vars    = mux.Vars(r)
		name    = vars["name"]
	)

	run, err := strconv.Atoi(vars["run"])
	if err != nil {
		return weberror.NewWebError(err, "invalid run ID '%s' provided", vars["run"])
	}

	if !role.Allowed("experiments/trigger", "delete", name) {
		user, _ := ctx.Value(middleware.ContextKeyUser).(string)
		err := weberror.NewWebError(
			nil,
			"canceling Scorch runs for experiment %s not allowed for %s",
			name,
			user,
		)

		return err.SetStatus(http.StatusForbidden)
	}

	if scorchexe.Cancel(name, run) {
		plog.Debug(plog.TypeSystem, "canceled Scorch run for experiment", "exp", name, "run", run)

		key := fmt.Sprintf("%s/%d", name, run)

		pubsub.Publish("trigger-app", app.TriggerPublication{ //nolint:exhaustruct // partial initialization
			Experiment: name, Verb: "delete", App: appNameScorch, Resource: key, State: "success",
		})
	}

	w.WriteHeader(http.StatusNoContent)

	return nil
}
