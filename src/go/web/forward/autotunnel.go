package forward

import (
	"errors"
	"maps"
	"slices"
	"sync"
	"time"

	"phenix/api/experiment"
	"phenix/app"
	"phenix/util/mm"
	"phenix/util/plog"
	ft "phenix/web/forward/forwardtypes"
)

// phenix ui keeps the startup autotunnels of every running experiment,
// whichever process started it: each pass of the forward service reads the
// running experiments from the store, and creates the forward of each
// phenix/startup-autotunnel entry phenix ui does not hold once the VM's C2
// client is active. A forward goes when its minimega tunnel does, which
// happens whenever the VM's miniccc client reconnects (a guest reboot, a
// restart, a redeploy, a restored snapshot) or the VM goes away, and the reaper
// drops it; the next pass creates it again. After a phenix ui restart, a pass
// adopts the minimega tunnel the earlier process left open.

var (
	// An autotunnel whose VM's C2 client is not active is checked again every
	// pass until it has waited autotunnelProgressInterval, then every
	// autotunnelSlowInterval. It logs its wait when it begins, then every
	// autotunnelProgressInterval, as it does a creation that keeps failing.
	autotunnelSlowInterval     = 15 * time.Second    //nolint:gochecknoglobals // overridden by tests
	autotunnelProgressInterval = mm.DefaultC2Timeout //nolint:gochecknoglobals // overridden by tests

	// autotunnelC2Timeout bounds one check of a VM's C2 client, and at most
	// autotunnelC2Checks run at once.
	autotunnelC2Timeout = time.Second //nolint:gochecknoglobals // overridden by tests
	autotunnelC2Checks  = 8           //nolint:gochecknoglobals // overridden by tests

	// autotunnels holds the startup autotunnels of the running experiments by
	// their forward's key. autotunnelsMu guards it and is held by a whole pass,
	// so the stopping hook can wait for one that may still ask minimega about
	// the experiment's VMs.
	autotunnels   = make(map[string]*autotunnelState) //nolint:gochecknoglobals // per-process state
	autotunnelsMu sync.Mutex                          //nolint:gochecknoglobals // guards autotunnels

	// autotunnelListErr is the last error listing the experiments, logged
	// once. Guarded by autotunnelsMu.
	autotunnelListErr string //nolint:gochecknoglobals // per-process state
)

// autotunnelState is what phenix ui knows about one startup autotunnel of an
// experiment's run.
type autotunnelState struct {
	app.Autotunnel

	start      string    // the experiment's start time, which names the run
	held       bool      // phenix ui held its forward at the last pass
	created    bool      // its forward was created during this run
	recreating bool      // it went down and its re-creation was not logged yet
	since      time.Time // when it began waiting for its forward
	reported   time.Time // when its wait or failure was last logged
	next       time.Time // when its VM's C2 client is checked next
	lastErr    string    // the last error creating its forward
}

// wantedAutotunnel is a startup autotunnel of a running experiment.
type wantedAutotunnel struct {
	tunnel app.Autotunnel
	start  string
}

func autotunnelKey(t app.Autotunnel) string {
	return ft.Listener{ //nolint:exhaustruct // the key fields only
		Exp: t.Experiment, VM: t.VM, DstHost: t.Dhost, DstPort: t.DstPort(), Owner: app.AutotunnelOwner,
	}.ToKey()
}

// wantedAutotunnels returns the startup autotunnels of the running
// experiments by their forward's key. It reports whether it read every
// experiment; when it did not, what is missing may still run. The caller holds
// autotunnelsMu.
func wantedAutotunnels() (map[string]wantedAutotunnel, bool) {
	exps, err := experiment.List()

	switch {
	case err == nil:
		autotunnelListErr = ""
	case err.Error() != autotunnelListErr:
		autotunnelListErr = err.Error()

		plog.Warn(plog.TypeSystem, "listing experiments for their startup autotunnels", "err", err)
	}

	wanted := make(map[string]wantedAutotunnel)

	for _, exp := range exps {
		if !exp.Running() || exp.DryRun() {
			continue
		}

		// The startup app logged the entries it ignores when the experiment
		// started.
		tunnels, _ := app.Autotunnels(&exp)

		for _, tunnel := range tunnels {
			wanted[autotunnelKey(tunnel)] = wantedAutotunnel{tunnel: tunnel, start: exp.Status.StartTime()}
		}
	}

	return wanted, err == nil
}

// reconcileAutotunnels is one pass over the startup autotunnels: it creates
// those whose forward phenix ui does not hold once their VM's C2 client is
// active, and forgets those of experiments that stopped.
func reconcileAutotunnels(now time.Time) {
	autotunnelsMu.Lock()
	defer autotunnelsMu.Unlock()

	wanted, complete := wantedAutotunnels()

	forwardsMu.Lock()
	reapForwards()

	held := make(map[string]bool, len(wanted))
	for key := range wanted {
		_, held[key] = forwards[key]
	}

	stoppingNow := maps.Clone(stopping)

	forwardsMu.Unlock()

	for key, st := range autotunnels {
		w, ok := wanted[key]
		if ok && w.start != st.start || !ok && complete {
			forgetAutotunnel(key, st)
		}
	}

	// The autotunnels to create, or whose VM's C2 client to check, by VM.
	due := make(map[string][]*autotunnelState)

	for key, w := range wanted {
		if _, ok := stoppingNow[w.tunnel.Experiment]; ok {
			continue
		}

		st := autotunnels[key]
		if st == nil {
			st = &autotunnelState{Autotunnel: w.tunnel, start: w.start, since: now, next: now} //nolint:exhaustruct // zero values
			autotunnels[key] = st

			plog.Info(plog.TypeSystem, "startup autotunnel requested", st.LogArgs()...)
		}

		if held[key] {
			st.held = true

			continue
		}

		if st.held {
			st.held, st.recreating = false, true
			st.since, st.next, st.reported = now, now, time.Time{}

			plog.Warn(
				plog.TypeSystem,
				"startup autotunnel down: re-creating it once the VM's C2 client is active",
				st.LogArgs()...,
			)
		}

		if now.Before(st.next) {
			continue
		}

		vm := st.Experiment + "/" + st.VM
		due[vm] = append(due[vm], st)
	}

	active := checkAutotunnelC2(due)

	for vm, sts := range due {
		err, checked := active[vm]
		if !checked {
			continue // the experiment stopped meanwhile
		}

		for _, st := range sts {
			if err == nil || st.VNC() {
				st.create(now)
			} else {
				st.wait(now, err)
			}
		}
	}
}

// checkAutotunnelC2 checks, at once, the C2 client of each VM with due
// autotunnels that need one, and returns nil for an active one or the error,
// by VM. A VM whose experiment stopped meanwhile is left out; one whose
// autotunnels all forward to VNC gets nil without a check.
func checkAutotunnelC2(due map[string][]*autotunnelState) map[string]error {
	var (
		results = make(map[string]error, len(due))
		mu      sync.Mutex
		wg      sync.WaitGroup
		slots   = make(chan struct{}, autotunnelC2Checks)
	)

	for vm, sts := range due {
		if !slices.ContainsFunc(sts, func(st *autotunnelState) bool { return !st.VNC() }) {
			results[vm] = nil

			continue
		}

		st := sts[0]

		wg.Add(1)

		go func() {
			defer wg.Done()

			slots <- struct{}{}
			defer func() { <-slots }()

			// A stop marks the experiment stopping, or clears its start time,
			// and minimega creates a namespace again for any command that
			// names it once the stop cleared it.
			if refuseStopping(st.Experiment, st.VM) != nil || !app.ExperimentStillStarted(st.Experiment, st.start) {
				return
			}

			err := mm.IsC2ClientActive(mm.C2NS(st.Experiment), mm.C2VM(st.VM), mm.C2Timeout(autotunnelC2Timeout))

			mu.Lock()
			defer mu.Unlock()

			results[vm] = err
		}()
	}

	wg.Wait()

	return results
}

// create creates the autotunnel's forward, adopting a minimega tunnel already
// open to its destination.
func (st *autotunnelState) create(now time.Time) {
	if st.recreating {
		st.recreating = false

		plog.Info(plog.TypeSystem, "startup autotunnel re-creating: the VM's C2 client is active", st.LogArgs()...)
	}

	l, adopted, err := addPortForward(st.Experiment, st.VM, st.Sport, st.Dhost, st.Dport, app.AutotunnelOwner, true)
	if err != nil {
		st.next = now.Add(autotunnelSlowInterval)

		if errors.Is(err, errExperimentStopping) {
			return
		}

		if err.Error() != st.lastErr || now.Sub(st.reported) >= autotunnelProgressInterval {
			st.lastErr, st.reported = err.Error(), now

			plog.Error(plog.TypeSystem, "startup autotunnel not created: creating the port forward", st.LogArgs("err", err)...)
		}

		return
	}

	plog.Info(plog.TypeSystem, "startup autotunnel created", st.LogArgs(
		"recreated", st.created, "adopted", adopted,
		"clusterHost", l.ClusterHost, "clusterPort", l.ClusterPort,
		"waited", now.Sub(st.since).Round(time.Second),
	)...)

	st.created, st.held = true, true
	st.lastErr, st.reported = "", time.Time{}
}

// wait records that the autotunnel's VM has no active C2 client, err telling
// why, and logs the wait when it begins and every autotunnelProgressInterval.
func (st *autotunnelState) wait(now time.Time, err error) {
	waited := now.Sub(st.since)

	st.next = now
	if waited >= autotunnelProgressInterval {
		st.next = now.Add(autotunnelSlowInterval)
	}

	args := st.LogArgs("waited", waited.Round(time.Second))
	if !errors.Is(err, mm.ErrC2ClientNotActive) {
		args = append(args, "err", err)
	}

	switch {
	case st.reported.IsZero():
		plog.Info(plog.TypeSystem, "startup autotunnel waiting for the C2 client", args...)
	case now.Sub(st.reported) >= autotunnelProgressInterval:
		plog.Warn(
			plog.TypeSystem,
			"startup autotunnel still waiting for the C2 client: the VM may not be started yet or may boot slowly",
			args...,
		)
	default:
		return
	}

	st.reported = now
}

// endAutotunnels forgets the startup autotunnels of the experiment. It waits
// for a pass in progress, which may still ask minimega about its VMs.
func endAutotunnels(exp string) {
	autotunnelsMu.Lock()
	defer autotunnelsMu.Unlock()

	for key, st := range autotunnels {
		if st.Experiment == exp {
			forgetAutotunnel(key, st)
		}
	}
}

// forgetAutotunnel forgets the autotunnel of a run that ended. The caller
// holds autotunnelsMu.
func forgetAutotunnel(key string, st *autotunnelState) {
	delete(autotunnels, key)

	if st.created {
		plog.Info(plog.TypeSystem, "startup autotunnel removed: the experiment stopped", st.LogArgs()...)
	} else {
		plog.Info(
			plog.TypeSystem,
			"startup autotunnel not created: the experiment stopped before the C2 client registered",
			st.LogArgs()...,
		)
	}
}
