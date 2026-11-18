package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/mux"

	"phenix/app"
	v1 "phenix/types/version/v1"
	"phenix/util/pubsub"
	"phenix/web/cache"
	"phenix/web/middleware"
	"phenix/web/rbac"
)

// triggerRole may trigger and cancel the apps of every experiment.
func triggerRole() rbac.Role {
	return rbac.Role{Spec: &v1.RoleSpec{Policies: []*v1.PolicySpec{{
		Resources:     []string{"experiments/trigger"},
		ResourceNames: []string{"*"},
		Verbs:         []string{"create", "delete"},
	}}}}
}

// postTrigger sends POST /experiments/{name}/trigger?apps=<apps>.
func postTrigger(t *testing.T, name, apps string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/experiments/"+name+"/trigger?apps="+apps, nil)
	req = mux.SetURLVars(req, map[string]string{"name": name})
	req = req.WithContext(context.WithValue(req.Context(), middleware.ContextKeyRole, triggerRole()))

	rec := httptest.NewRecorder()
	TriggerExperimentApps(rec, req)

	return rec
}

// stubTriggerRunning makes triggered runs call run for the rest of the test.
func stubTriggerRunning(t *testing.T, run func(ctx context.Context, exp string, apps ...string) error) {
	t.Helper()

	original := triggerRunning

	t.Cleanup(func() { triggerRunning = original })

	triggerRunning = run
}

// TestTriggerDropsItsCancelerWhenDone is a regression test: a triggered app
// run removed its canceler only when it failed, so every successful trigger
// left one in cancelers["<exp>/<app>"] for the life of phenix ui.
func TestTriggerDropsItsCancelerWhenDone(t *testing.T) {
	t.Cleanup(func() {
		commonMu.Lock()
		delete(cancelers, "trig-done/app1")
		commonMu.Unlock()
	})

	for _, result := range []error{nil, errors.New("app failed")} {
		stubTriggerRunning(t, func(context.Context, string, ...string) error { return result })

		if rec := postTrigger(t, "trig-done", "app1"); rec.Code != http.StatusNoContent {
			t.Fatalf("POST trigger = %d %q", rec.Code, rec.Body.String())
		}

		left := true

		for deadline := time.Now().Add(2 * time.Second); left && time.Now().Before(deadline); {
			time.Sleep(10 * time.Millisecond)

			commonMu.Lock()
			_, left = cancelers["trig-done/app1"]
			commonMu.Unlock()
		}

		if left {
			t.Errorf("a trigger that returned %v left its canceler registered", result)
		}
	}
}

// TestStopEndsTriggeredRuns is a regression test: an experiment stop canceled
// and waited for only the start and the periodic app runs, so a run started
// through POST /experiments/{name}/trigger, such as a SCORCH run, kept going
// and sent minimega commands that created the cleared namespace again, and a
// trigger sent during the stop was accepted.
func TestStopEndsTriggeredRuns(t *testing.T) {
	const name = "trig-stop"

	var (
		started = make(chan struct{})
		ended   = make(chan struct{})
	)

	stubTriggerRunning(t, func(ctx context.Context, _ string, _ ...string) error {
		close(started)

		<-ctx.Done() // runs until canceled
		time.Sleep(50 * time.Millisecond)
		close(ended)

		return ctx.Err()
	})

	if rec := postTrigger(t, name, "scorch"); rec.Code != http.StatusNoContent {
		t.Fatalf("POST trigger = %d %q", rec.Code, rec.Body.String())
	}

	<-started

	if err := cache.LockExperimentForStopping(name); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { cache.UnlockExperiment(name) })

	if rec := postTrigger(t, name, "scorch"); rec.Code != http.StatusConflict {
		t.Errorf("POST trigger during the stop = %d %q, want 409", rec.Code, rec.Body.String())
	}

	endExperimentRuns(name)

	select {
	case <-ended:
	default:
		t.Error("the stop returned before the triggered run ended")
	}
}

// TestStopDuringTriggerPublishCancelsTheRun is a regression test: a triggered
// run registered its canceler only after it published its start, on the
// unbuffered trigger-app topic, so a stop that took the experiment's cancelers
// while that publish blocked missed the run, which then started uncanceled and
// held the stop until it ended on its own, hours later for a SCORCH run.
func TestStopDuringTriggerPublishCancelsTheRun(t *testing.T) {
	const name = "trig-publish"

	canceled := make(chan bool, 1)

	stubTriggerRunning(t, func(ctx context.Context, _ string, _ ...string) error {
		select {
		case <-ctx.Done():
			canceled <- true
		case <-time.After(2 * time.Second):
			canceled <- false
		}

		return ctx.Err()
	})

	// Publish sends to the subscribers in turn, so holding second holds the
	// start's publish once first received it.
	var (
		first, second = pubsub.Subscribe("trigger-app"), pubsub.Subscribe("trigger-app")
		done          = make(chan struct{})
	)

	drain := func(ch chan any) {
		for {
			select {
			case <-ch:
			case <-done:
				return
			}
		}
	}

	t.Cleanup(func() {
		go drain(first)
		go drain(second)

		pubsub.Unsubscribe("trigger-app", first)
		pubsub.Unsubscribe("trigger-app", second)
		close(done)
	})

	if rec := postTrigger(t, name, "app1"); rec.Code != http.StatusNoContent {
		t.Fatalf("POST trigger = %d %q", rec.Code, rec.Body.String())
	}

	for started := false; !started; {
		select {
		case pub := <-first:
			trigger, _ := pub.(app.TriggerPublication)
			started = trigger.Experiment == name && trigger.State == "start"
		case <-time.After(5 * time.Second):
			t.Fatal("the triggered run published no start")
		}
	}

	if err := cache.LockExperimentForStopping(name); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { cache.UnlockExperiment(name) })

	stopped := make(chan struct{})

	go func() {
		endExperimentRuns(name)
		close(stopped)
	}()

	// The stop drops the experiment's triggered runs once it took their
	// cancelers.
	for taken := false; !taken; time.Sleep(time.Millisecond) {
		commonMu.Lock()
		_, held := triggers[name]
		commonMu.Unlock()

		taken = !held
	}

	go drain(first)
	go drain(second)

	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("the stop did not end the triggered run")
	}

	select {
	case wasCanceled := <-canceled:
		if !wasCanceled {
			t.Error("the run whose start was publishing during the stop ran uncanceled")
		}
	default:
	}
}
