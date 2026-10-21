package forward

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/mux"
	"golang.org/x/net/websocket"

	"phenix/api/experiment"
	"phenix/app"
	"phenix/util/mm"
	"phenix/util/plog"
	"phenix/util/pubsub"
	"phenix/web/broker"
	bt "phenix/web/broker/brokertypes"
	ft "phenix/web/forward/forwardtypes"
	"phenix/web/middleware"
	"phenix/web/rbac"
	"phenix/web/util"
)

const (
	forwardTickerInterval = 10 * time.Second
	forwardPortBase       = 50000
	forwardPortRange      = 15000

	// maxTunnelAttempts bounds the local ports a forward tries.
	maxTunnelAttempts = 10
)

var (
	forwards   = make(map[string]ft.Listener) //nolint:gochecknoglobals // global state
	forwardsMu sync.Mutex                     //nolint:gochecknoglobals // global lock

	// stopping holds the experiments this process is stopping, from the
	// stopping hook until the stop ends. Guarded by forwardsMu.
	stopping = make(map[string]struct{}) //nolint:gochecknoglobals // global state

	endAutotunnelWaits = app.EndAutotunnelWaits //nolint:gochecknoglobals // overridden by tests
)

func init() { //nolint:gochecknoinits // experiment hook registration
	experiment.RegisterHook("stopping", func(_, name string) { stopExperimentForwards(name) })
	experiment.RegisterHook("stop-end", func(_, name string) { experimentStopped(name) })
}

// stopExperimentForwards is the stopping hook of an experiment's forwards. It
// ends the startup app's autotunnel waits first: the stopping hooks run before
// the cleanup apps, and the SCORCH hook may wait minutes, so a wait that saw
// its VM's C2 client meanwhile would create a forward once they are dropped.
// It then drops the forwards before the namespace is cleared: a reaper tick
// asks minimega about their tunnels, which would create the namespace again.
//
// The service may already hold an autotunnel the startup app handed it before
// the waits ended, so the experiment is marked stopping first, and
// createPortForward refuses a forward to it until the stop ends.
func stopExperimentForwards(name string) {
	forwardsMu.Lock()
	stopping[name] = struct{}{}
	forwardsMu.Unlock()

	endAutotunnelWaits(name)
	deleteExperimentForwards(name)
}

// experimentStopped is the stop-end hook of an experiment's forwards. A stop
// ends with it also when it returns early, because another process stopped or
// restarted the experiment meanwhile, or panics; otherwise the mark would
// refuse every forward to the experiment until phenix ui restarts. Once the
// experiment is stored stopped, createPortForward refuses it as not running.
func experimentStopped(name string) {
	forwardsMu.Lock()
	defer forwardsMu.Unlock()

	delete(stopping, name)
}

// errExperimentStopping reports a forward to an experiment this process is
// stopping.
var errExperimentStopping = errors.New("experiment is stopping")

// refuseStopping is checkNotStopping for a caller that does not hold
// forwardsMu.
func refuseStopping(exp, vm string) error {
	forwardsMu.Lock()
	defer forwardsMu.Unlock()

	return checkNotStopping(exp, vm)
}

// checkNotStopping returns errExperimentStopping when this process is
// stopping exp. The caller holds forwardsMu.
func checkNotStopping(exp, vm string) error {
	if _, ok := stopping[exp]; ok {
		return fmt.Errorf("forwarding to vm %s: %w", vm, errExperimentStopping)
	}

	return nil
}

// Start runs phenix ui's port forward service, which reaps forwards whose
// minimega tunnel is gone and creates the startup app's autotunnels. It
// subscribes before returning, which is how the startup app tells that the
// process will create its autotunnels.
func Start() {
	createTunnel := pubsub.Subscribe(app.CreateTunnelTopic)

	go func() {
		ticker := time.NewTicker(forwardTickerInterval)

		for {
			select {
			case <-ticker.C:
				forwardsMu.Lock()
				reapForwards()
				forwardsMu.Unlock()
			case pub := <-createTunnel:
				tunnel, _ := pub.(app.CreateTunnel)

				err := createPortForward(
					tunnel.Experiment,
					tunnel.VM,
					tunnel.Sport,
					tunnel.Dhost,
					tunnel.Dport,
					tunnel.User,
				)
				if err != nil {
					plog.Error(
						plog.TypeSystem,
						"adding port forward",
						"exp",
						tunnel.Experiment,
						"vm",
						tunnel.VM,
						"sport",
						tunnel.Sport,
						"host",
						tunnel.Dhost,
						"dport",
						tunnel.Dport,
						"err",
						err,
					)
				}
			}
		}
	}()
}

func createPortForward(exp, vm, src, host, dst, user string) error {
	if err := refuseStopping(exp, vm); err != nil {
		return err
	}

	// minimega would create the namespace of a stopped or dry-run experiment.
	if err := experiment.RequireRunning(exp); err != nil {
		return fmt.Errorf("forwarding to vm %s: %w", vm, err)
	}

	var err error

	listener := ft.Listener{Exp: exp, VM: vm, Owner: user} //nolint:exhaustruct // partial initialization

	if host == "" {
		listener.DstHost = "127.0.0.1"
	} else {
		listener.DstHost = host
	}

	listener.SrcPort, err = strconv.Atoi(src)
	if err != nil {
		return fmt.Errorf("parsing source port %s for forward: %w", src, err)
	}

	switch strings.ToUpper(dst) {
	case "VNC":
		endpoint, err := mm.GetVNCEndpoint(mm.NS(exp), mm.VMName(vm))
		if err != nil {
			return fmt.Errorf("getting VNC endpoint for vm %s in experiment %s: %w", vm, exp, err)
		}

		tokens := strings.Split(endpoint, ":")

		listener.ClusterHost = tokens[0]
		listener.ClusterPort, _ = strconv.Atoi(tokens[1])

		listener.DstPort = 5900

		listener.QEMU = true
	default:
		info := mm.GetVMInfo(mm.NS(exp), mm.VMName(vm))
		if len(info) == 0 {
			return fmt.Errorf("vm %s not found for experiment %s", vm, exp)
		}

		listener.ClusterHost = info.Named(vm).Host

		listener.DstPort, err = strconv.Atoi(dst)
		if err != nil {
			return fmt.Errorf("parsing destination port %s for forward: %w", dst, err)
		}
	}

	forwardsMu.Lock()
	defer forwardsMu.Unlock()

	// The stopping hook may have run while minimega was queried.
	if err := checkNotStopping(exp, vm); err != nil {
		return err
	}

	reapForwards()

	if _, ok := forwards[listener.ToKey()]; ok {
		return fmt.Errorf("forward already exists for user %s", user)
	}

	if listener.ClusterPort == 0 {
		// Another local port is tried when minimega's is taken, a bounded
		// number of times: the lock every forward request needs is held.
		for attempt := 1; ; attempt++ {
			listener.ClusterPort = forwardPortBase + rand.IntN(forwardPortRange) //nolint:gosec // weak random number generator

			err = mm.CreateTunnel(
				mm.NS(exp),
				mm.VMName(vm),
				mm.TunnelSourcePort(listener.ClusterPort),
				mm.TunnelDestinationPort(listener.DstPort),
				mm.TunnelDestinationHost(listener.DstHost),
			)
			if err == nil {
				break
			}

			portTaken := strings.Contains(err.Error(), "bind: address already in use") &&
				!errors.Is(err, mm.ErrUnsafeArgument)
			if !portTaken || attempt == maxTunnelAttempts {
				return fmt.Errorf("creating tunnel: %w", err)
			}
		}
	}

	forwards[listener.ToKey()] = listener

	body, _ := json.Marshal(listener)

	broker.Broadcast(
		bt.NewRequestPolicy("vms/forwards", "create", fmt.Sprintf("%s/%s", exp, vm)),
		bt.NewResource("experiment/vm/forward", fmt.Sprintf("%s/%s", exp, vm), "create"),
		body,
	)

	return nil
}

// GetPortForwards - GET /experiments/{exp}/vms/{name}/forwards.
func GetPortForwards(w http.ResponseWriter, r *http.Request) {
	plog.Debug(plog.TypeSystem, "HTTP handler called", "handler", "GetPortForwards")

	var (
		ctx     = r.Context()
		user, _ = ctx.Value(middleware.ContextKeyUser).(string)
		role    = ctx.Value(middleware.ContextKeyRole).(rbac.Role)

		vars = mux.Vars(r)
		exp  = vars["exp"]
		vm   = vars["name"]
	)

	if !role.Allowed("vms/forwards", "list", fmt.Sprintf("%s/%s", exp, vm)) {
		plog.Warn(
			plog.TypeSecurity,
			"listing port forwards not allowed",
			"user",
			user,
			"exp",
			exp,
			"vm",
			vm,
		)

		http.Error(w, "forbidden", http.StatusForbidden)

		return
	}

	var (
		listeners []ft.Listener
		prefix    = fmt.Sprintf("%s:%s", exp, vm)
	)

	forwardsMu.Lock()
	defer forwardsMu.Unlock()

	reapForwards()

	for key := range forwards {
		if strings.HasPrefix(key, prefix) {
			listeners = append(listeners, forwards[key])
		}
	}

	body, _ := json.Marshal(util.WithRoot("listeners", listeners))
	_, _ = w.Write(body)
}

// CreatePortForward - POST /experiments/{exp}/vms/{name}/forwards?src=<int>&host=<ip>&dst=<int>.
func CreatePortForward(w http.ResponseWriter, r *http.Request) {
	plog.Debug(plog.TypeSystem, "HTTP handler called", "handler", "CreatePortForward")

	var (
		ctx     = r.Context()
		user, _ = ctx.Value(middleware.ContextKeyUser).(string)
		role    = ctx.Value(middleware.ContextKeyRole).(rbac.Role)

		vars = mux.Vars(r)
		exp  = vars["exp"]
		vm   = vars["name"]

		query = r.URL.Query()
		src   = query.Get("src")
		host  = query.Get("host")
		dst   = query.Get("dst")
	)

	if !role.Allowed("vms/forwards", "create", fmt.Sprintf("%s/%s", exp, vm)) {
		plog.Warn(
			plog.TypeSecurity,
			"creating port forwards not allowed",
			"user",
			user,
			"exp",
			exp,
			"vm",
			vm,
		)

		http.Error(w, "forbidden", http.StatusForbidden)

		return
	}

	// A VNC forward carries the VM's console.
	if strings.EqualFold(dst, "VNC") && !vncAllowed(role, user, exp, vm) {
		http.Error(w, "forbidden", http.StatusForbidden)

		return
	}

	err := createPortForward(exp, vm, src, host, dst, user)
	if err != nil {
		plog.Error(
			plog.TypeSystem,
			"creating port forward",
			"exp",
			exp,
			"vm",
			vm,
			"src",
			src,
			"host",
			host,
			"dst",
			dst,
			"user",
			user,
			"err",
			err,
		)

		http.Error(w, "unable to create tunnel to vm", http.StatusBadRequest)

		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// DeletePortForward - DELETE /experiments/{exp}/vms/{name}/forwards?host=<ip>&dst=<int>.
func DeletePortForward(w http.ResponseWriter, r *http.Request) {
	plog.Debug(plog.TypeSystem, "HTTP handler called", "handler", "DeletePortForward")

	var (
		ctx     = r.Context()
		user, _ = ctx.Value(middleware.ContextKeyUser).(string)
		role    = ctx.Value(middleware.ContextKeyRole).(rbac.Role)

		vars = mux.Vars(r)
		exp  = vars["exp"]
		vm   = vars["name"]

		query = r.URL.Query()
		host  = query.Get("host")
		dst   = query.Get("dst")
	)

	if !role.Allowed("vms/forwards", "delete", fmt.Sprintf("%s/%s", exp, vm)) {
		plog.Warn(
			plog.TypeSecurity,
			"deleting port forwards not allowed",
			"user",
			user,
			"exp",
			exp,
			"vm",
			vm,
		)
		http.Error(w, "forbidden", http.StatusForbidden)

		return
	}

	if host == "" {
		host = "127.0.0.1"
	}

	// minimega would create the namespace of a stopped or dry-run experiment,
	// whose forwards the reaper below drops.
	if experiment.RequireRunning(exp) == nil {
		info := mm.GetVMInfo(mm.NS(exp), mm.VMName(vm))
		if len(info) == 0 {
			http.Error(w, "vm not found", http.StatusNotFound)

			return
		}
	}

	remoteDst, err := strconv.Atoi(dst)
	if err != nil {
		plog.Error(plog.TypeSystem, "parsing destination port for forward", "port", dst, "err", err)

		http.Error(w, "invalid destination port", http.StatusBadRequest)

		return
	}

	key := fmt.Sprintf("%s:%s:%s:%d:%s", exp, vm, host, remoteDst, user)

	forwardsMu.Lock()
	defer forwardsMu.Unlock()

	reapForwards()

	if l, ok := forwards[key]; ok {
		// TODO: how would we go about allowing admins to close all port forwards?
		if l.Owner != user {
			http.Error(w, "forbidden", http.StatusForbidden)

			return
		}

		if !l.QEMU {
			err := mm.CloseTunnel(
				mm.NS(exp),
				mm.VMName(vm),
				mm.TunnelDestinationPort(remoteDst),
				mm.TunnelDestinationHost(host),
			)
			if err != nil {
				plog.Error(plog.TypeSystem, "closing tunnel", "err", err)

				http.Error(w, "unable to close tunnel to vm", http.StatusInternalServerError)

				return
			}
		}

		deleteForward(l)

		w.WriteHeader(http.StatusNoContent)

		return
	}

	http.Error(w, "forward not found (are you the owner?)", http.StatusBadRequest)
}

// GetPortForwardWebSocket - GET /experiments/{exp}/vms/{name}/forwards/{host}/{port}/ws.
func GetPortForwardWebSocket(w http.ResponseWriter, r *http.Request) {
	plog.Debug(plog.TypeSystem, "HTTP handler called", "handler", "GetPortForwardWebSocket")

	var (
		ctx     = r.Context()
		user, _ = ctx.Value(middleware.ContextKeyUser).(string)
		role    = ctx.Value(middleware.ContextKeyRole).(rbac.Role)

		vars = mux.Vars(r)
		exp  = vars["exp"]
		vm   = vars["name"]
		host = vars["host"]
		port = vars["port"]
	)

	if !role.Allowed("vms/forwards", "get", fmt.Sprintf("%s/%s", exp, vm)) {
		plog.Warn(
			plog.TypeSecurity,
			"accessing port forwards not allowed",
			"user",
			user,
			"exp",
			exp,
			"vm",
			vm,
		)
		http.Error(w, "forbidden", http.StatusForbidden)

		return
	}

	key := fmt.Sprintf("%s:%s:%s:%s:%s", exp, vm, host, port, user)

	forwardsMu.Lock()
	reapForwards()

	// Check to see if a forward exists that this user created.
	if listener, ok := forwards[key]; ok {
		forwardsMu.Unlock()

		connectForward(w, r, listener, role, user)

		return
	}

	var (
		prefix  = fmt.Sprintf("%s:%s:%s:%s", exp, vm, host, port)
		matches []string
	)

	// Check to see if a forward exists that anyone created.
	for key := range forwards {
		if strings.HasPrefix(key, prefix) {
			matches = append(matches, key)
		}
	}

	if len(matches) == 0 {
		forwardsMu.Unlock()

		plog.Error(plog.TypeSystem, "listener not found for forward", "key", key)
		http.Error(w, "unknown forward", http.StatusBadRequest)

		return
	}

	var (
		rsrc  = rand.NewPCG(uint64(time.Now().Unix()), 1) //nolint:gosec // integer overflow conversion int64 -> uint64
		rando = rand.New(rsrc)                            //nolint:gosec // weak random number generator
	)

	// Use a random forward so the same one doesn't get overwhelmed.
	key = matches[rando.IntN(len(matches))]
	listener := forwards[key]

	forwardsMu.Unlock()

	connectForward(w, r, listener, role, user)
}

// connectForward connects the websocket request to the forward's listener.
// A forward to the VM's VNC endpoint carries its console, so it needs the
// permission the VNC websocket needs, whoever created the forward.
func connectForward(w http.ResponseWriter, r *http.Request, listener ft.Listener, role rbac.Role, user string) {
	if listener.QEMU && !vncAllowed(role, user, listener.Exp, listener.VM) {
		http.Error(w, "forbidden", http.StatusForbidden)

		return
	}

	websocket.Handler(util.ConnectWSHandler(listener.ClusterEndpoint())).ServeHTTP(w, r)
}

// vncAllowed reports whether role may open the console of the VM, as the VNC
// websocket requires, and logs a refusal.
func vncAllowed(role rbac.Role, user, exp, vm string) bool {
	if role.Allowed("vms/vnc", "get", exp+"/"+vm) {
		return true
	}

	plog.Warn(plog.TypeSecurity, "vnc port forward not allowed", "user", user, "exp", exp, "vm", vm)

	return false
}
