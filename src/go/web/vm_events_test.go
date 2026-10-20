package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang/mock/gomock"
	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"

	"phenix/store"
	v1 "phenix/types/version/v1"
	"phenix/util/mm"
	"phenix/web/broker"
	bt "phenix/web/broker/brokertypes"
	"phenix/web/middleware"
	"phenix/web/rbac"
)

const brokerProbe = "test/probe"

var startBrokerOnce sync.Once //nolint:gochecknoglobals // the broker runs once per test binary

// vmEventsMM fails the minimega calls behind the VM start, stop and restart
// handlers with the configured errors.
type vmEventsMM struct {
	mm.MM

	startErr, stopErr, stateErr error
}

func (m vmEventsMM) StartVM(...mm.Option) error { return m.startErr }

func (m vmEventsMM) StopVM(...mm.Option) error { return m.stopErr }

func (m vmEventsMM) GetVMState(...mm.Option) (string, error) { return "", m.stateErr }

// vmEventsRole allows starting, stopping and restarting every VM.
func vmEventsRole() rbac.Role {
	return rbac.Role{
		Spec: &v1.RoleSpec{
			Policies: []*v1.PolicySpec{
				{
					Resources:     []string{"vms/start", "vms/stop", "vms/restart"},
					ResourceNames: []string{"*/*"},
					Verbs:         []string{"update"},
				},
			},
		},
	}
}

// recordBroadcasts connects a websocket client with role to the broker and
// returns the resources of the messages the broker sends it.
func recordBroadcasts(t *testing.T, role rbac.Role) <-chan bt.Resource {
	t.Helper()

	startBrokerOnce.Do(func() { go broker.Start() })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		broker.ServeWS(w, r.WithContext(context.WithValue(r.Context(), middleware.ContextKeyRole, role)))
	}))
	t.Cleanup(srv.Close)

	conn, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("connecting to the broker: %v", err)
	}

	_ = resp.Body.Close()

	var (
		events = make(chan bt.Resource, 64)
		done   = make(chan struct{})
	)

	t.Cleanup(func() {
		close(done)

		_ = conn.Close()
	})

	go func() {
		for {
			_, frame, err := conn.ReadMessage()
			if err != nil {
				return
			}

			for line := range bytes.SplitSeq(frame, []byte("\n")) {
				var pub bt.Publish
				if json.Unmarshal(line, &pub) != nil || pub.Resource == nil {
					continue
				}

				select {
				case events <- *pub.Resource:
				case <-done:
					return
				}
			}
		}
	}()

	// The broker registers the client in its own goroutine. Once the client
	// gets a probe, it gets every broadcast sent after it.
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	timeout := time.After(5 * time.Second)

	for {
		broker.Broadcast(nil, bt.NewResource(brokerProbe, "", ""), nil)

		select {
		case ev := <-events:
			if ev.Type == brokerProbe {
				return events
			}
		case <-ticker.C:
		case <-timeout:
			t.Fatal("the broker never delivered a probe to the websocket client")
		}
	}
}

// vmEvents returns the experiment/vm resources the handler broadcast to
// events. It then broadcasts a probe and reads up to it, so an event beyond
// count, or one where none is wanted, is returned too: the broker delivers
// each client's messages in order.
func vmEvents(t *testing.T, events <-chan bt.Resource, count int) []bt.Resource {
	t.Helper()

	var (
		got     []bt.Resource
		timeout = time.After(5 * time.Second)
	)

	for len(got) < count {
		select {
		case ev := <-events:
			if ev.Type == "experiment/vm" {
				got = append(got, ev)
			}
		case <-timeout:
			t.Fatalf("got experiment/vm events %v, want %d", got, count)
		}
	}

	broker.Broadcast(nil, bt.NewResource(brokerProbe, "", ""), nil)

	for {
		select {
		case ev := <-events:
			switch ev.Type {
			case brokerProbe:
				return got
			case "experiment/vm":
				got = append(got, ev)
			}
		case <-timeout:
			t.Fatalf("the broker never delivered the probe after experiment/vm events %v", got)
		}
	}
}

// TestVMOperationEventsNameTheExperiment is a regression test: the starting,
// stopping and restarting events, and the errors of a start or stop, named the
// VM without its experiment, unlike every other experiment/vm event.
func TestVMOperationEventsNameTheExperiment(t *testing.T) {
	const (
		expName = "test-experiment"
		full    = expName + "/test-vm"
	)

	errMM := errors.New("minimega failed")

	tests := []struct {
		name    string
		handler http.HandlerFunc
		mm      vmEventsMM
		missing bool // the experiment is not in the store
		vm      string
		code    int
		actions []string
	}{
		{
			name:    "start fails in minimega",
			handler: StartVM,
			mm:      vmEventsMM{startErr: errMM},
			code:    http.StatusInternalServerError,
			actions: []string{"starting", "errorStarting"},
		},
		{
			// Refused before the starting event: the experiment is gone.
			name:    "start of a VM whose experiment is gone",
			handler: StartVM,
			missing: true,
			code:    http.StatusNotFound,
		},
		{
			name:    "start of a VM missing from the topology",
			handler: StartVM,
			vm:      "missing-vm",
			code:    http.StatusInternalServerError,
			actions: []string{"starting", "errorStarting"},
		},
		{
			name:    "stop fails in minimega",
			handler: StopVM,
			mm:      vmEventsMM{stopErr: errMM},
			code:    http.StatusInternalServerError,
			actions: []string{"stopping", "errorStopping"},
		},
		{
			// Refused before the stopping event: the experiment is gone.
			name:    "stop of a VM whose experiment is gone",
			handler: StopVM,
			missing: true,
			code:    http.StatusNotFound,
		},
		{
			name:    "stop of a VM missing from the topology",
			handler: StopVM,
			vm:      "missing-vm",
			code:    http.StatusInternalServerError,
			actions: []string{"stopping", "errorStopping"},
		},
		{
			name:    "restart fails in minimega",
			handler: RestartVM,
			mm:      vmEventsMM{stateErr: errMM},
			code:    http.StatusInternalServerError,
			actions: []string{"restarting"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			originalStore, originalMM := store.DefaultStore, mm.DefaultMM
			t.Cleanup(func() {
				store.DefaultStore = originalStore //nolint:reassign // restore test double
				mm.DefaultMM = originalMM          //nolint:reassign // restore test double
			})

			if test.missing {
				m := store.NewMockStore(gomock.NewController(t))
				m.EXPECT().Get(gomock.Any()).Return(store.ErrNotExist).AnyTimes()

				store.DefaultStore = m //nolint:reassign // install test double
			} else {
				installCaptureHandlerTestExperiment(t)
			}

			mm.DefaultMM = test.mm //nolint:reassign // install test double

			vmName, want := "test-vm", full
			if test.vm != "" {
				vmName, want = test.vm, expName+"/"+test.vm
			}

			events := recordBroadcasts(t, vmEventsRole())

			req := httptest.NewRequest(http.MethodPost, "/api/v1/experiments/"+expName+"/vms/"+vmName, nil)
			req = mux.SetURLVars(req, map[string]string{"exp": expName, "name": vmName})
			req = req.WithContext(context.WithValue(req.Context(), middleware.ContextKeyRole, vmEventsRole()))

			rec := httptest.NewRecorder()
			test.handler(rec, req)

			if rec.Code != test.code {
				t.Errorf("handler answered %d %q, want %d", rec.Code, rec.Body.String(), test.code)
			}

			var actions []string

			for _, ev := range vmEvents(t, events, len(test.actions)) {
				actions = append(actions, ev.Action)

				if ev.Name != want {
					t.Errorf("experiment/vm %s event names the VM %q, want %q", ev.Action, ev.Name, want)
				}
			}

			if !slices.Equal(actions, test.actions) {
				t.Errorf("experiment/vm events = %v, want %v", actions, test.actions)
			}
		})
	}
}
