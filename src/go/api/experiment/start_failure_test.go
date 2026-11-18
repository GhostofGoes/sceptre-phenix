package experiment_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/golang/mock/gomock"

	"phenix/api/experiment"
	"phenix/store"
	"phenix/util/file"
	"phenix/util/mm"
	"phenix/util/notes"
)

// failedStartEvents records, in order, the namespace clears and file
// deletions a start sends to minimega.
type failedStartEvents struct {
	mu     sync.Mutex
	events []string
}

func (e *failedStartEvents) add(event string) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.events = append(e.events, event)
}

// failedStartMM is a minimega double whose script, launch, VM list or VLAN
// list fails, as a minimega that refuses the start does.
type failedStartMM struct {
	*startMM

	events    *failedStartEvents
	scriptErr error
	launchErr error
	vlansErr  error
	clearErr  error
	vmError   bool // minimega leaves the VM idle in ERROR
}

func (m failedStartMM) ReadScriptFromFile(ns, filename string) error {
	if !strings.HasSuffix(filename, "-cc.mm") && m.scriptErr != nil {
		return m.scriptErr
	}

	return m.startMM.ReadScriptFromFile(ns, filename)
}

func (m failedStartMM) ClearNamespace(ns string) error {
	m.events.add("clear namespace " + ns)

	if m.clearErr != nil {
		return m.clearErr
	}

	return m.startMM.ClearNamespace(ns)
}

func (m failedStartMM) LaunchVMs(ns string, start ...string) error {
	if m.launchErr != nil {
		return m.launchErr
	}

	return m.startMM.LaunchVMs(ns, start...)
}

func (m failedStartMM) GetVMInfo(opts ...mm.Option) mm.VMs {
	vms := m.startMM.GetVMInfo(opts...)

	if m.vmError {
		vms[0].State = "ERROR"
	}

	return vms
}

func (m failedStartMM) GetVLANs(opts ...mm.Option) (map[string]int, error) {
	if m.vlansErr != nil {
		return nil, m.vlansErr
	}

	return m.startMM.GetVLANs(opts...)
}

type failedStartFiles struct {
	file.ClusterFiles

	events *failedStartEvents
}

func (f failedStartFiles) DeleteFile(path string) error {
	f.events.add("file delete " + path)

	return nil
}

// TestStartFailureDeletesSnapshots is a regression test: when minimega
// refused a start, for example a script line with `host is not in
// namespace`, the start cleared the namespace but left behind the disk
// snapshots the minimega script had already created.
func TestStartFailureDeletesSnapshots(t *testing.T) {
	tests := []struct {
		name    string
		mm      failedStartMM
		wantErr string
	}{
		{
			name:    "the minimega script fails",
			mm:      failedStartMM{scriptErr: errors.New("host is not in namespace: e2e-head")},
			wantErr: "reading minimega script: host is not in namespace: e2e-head",
		},
		{
			name:    "the VMs fail to launch",
			mm:      failedStartMM{launchErr: errors.New("vm launch failed")},
			wantErr: "launching experiment VMs: vm launch failed",
		},
		{
			name:    "minimega leaves a VM in ERROR",
			mm:      failedStartMM{vmError: true},
			wantErr: "launching experiment VMs: 1 error occurred",
		},
		{
			name:    "the VLANs cannot be read",
			mm:      failedStartMM{vlansErr: errors.New("bad vlans table")},
			wantErr: "processing experiment VLANs: bad vlans table",
		},
		{
			name: "clearing the namespace fails too",
			mm: failedStartMM{
				scriptErr: errors.New("host is not in namespace: e2e-head"),
				clearErr:  errors.New("minimega is gone"),
			},
			wantErr: "2 errors occurred:\n\t* reading minimega script: host is not in namespace: e2e-head\n\t" +
				"* cleaning up the failed start: killing experiment VMs: minimega is gone",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var (
				events = new(failedStartEvents)
				fake   = tt.mm
			)

			fake.startMM = new(startMM)
			fake.events = events

			originalMM, originalFiles, originalStore := mm.DefaultMM, file.DefaultClusterFiles, store.DefaultStore

			t.Cleanup(func() {
				mm.DefaultMM = originalMM                //nolint:reassign // restore test double
				file.DefaultClusterFiles = originalFiles //nolint:reassign // restore test double
				store.DefaultStore = originalStore       //nolint:reassign // restore test double
			})

			mm.DefaultMM = fake                                         //nolint:reassign // install test double
			file.DefaultClusterFiles = failedStartFiles{events: events} //nolint:reassign // install test double

			cfg := startTestConfig(t, false, false)
			delete(cfg.Spec, "scenario")

			ctrl := gomock.NewController(t)

			m := store.NewMockStore(ctrl)
			m.EXPECT().Get(gomock.Any()).DoAndReturn(func(c *store.Config) error {
				*c = cfg

				return nil
			}).AnyTimes()
			m.EXPECT().Update(gomock.Any()).Times(0)

			store.DefaultStore = m //nolint:reassign // install test double

			err := experiment.Start(notes.Context(context.Background(), false), experiment.StartWithName(startTestExp))
			if err == nil || !strings.HasPrefix(err.Error(), tt.wantErr) {
				t.Fatalf("Start returned %v, want an error starting with %q", err, tt.wantErr)
			}

			cleared := slices.Index(events.events, "clear namespace "+startTestExp)
			if cleared < 0 {
				t.Fatalf("the failed start did not clear the namespace: %q", events.events)
			}

			// Like Stop, the start leaves the snapshots of VMs it could not kill.
			if fake.clearErr != nil {
				return
			}

			for _, vm := range []string{"idle", "timer"} {
				snapshot := "file delete head_" + startTestExp + "_" + vm + "_snapshot"

				if !slices.Contains(events.events[cleared:], snapshot) {
					t.Errorf("the failed start did not delete the snapshot of %s after clearing the namespace: %q", vm, events.events)
				}
			}
		})
	}
}

// TestStartFailureRunsCleanupApps is a regression test: a start that minimega
// refused after the pre-start apps ran cleared the namespace but ran no
// cleanup apps, so what those apps set up outside minimega, such as the tap
// app's host taps, network namespaces and NAT rules, stayed behind.
func TestStartFailureRunsCleanupApps(t *testing.T) {
	tests := []struct {
		name string
		mm   failedStartMM
	}{
		{name: "the minimega script fails", mm: failedStartMM{scriptErr: errors.New("host is not in namespace: e2e-head")}},
		{name: "the VMs fail to launch", mm: failedStartMM{launchErr: errors.New("vm launch failed")}},
		{name: "the VLANs cannot be read", mm: failedStartMM{vlansErr: errors.New("bad vlans table")}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			events := new(failedStartEvents)

			rec := new(startRecord)
			rec.onCleanup = func() { events.add("cleanup apps") }

			registerStartApp(t, rec)

			fake := tt.mm
			fake.startMM = new(startMM)
			fake.events = events

			cfg := startTestConfig(t, false, false)
			installStartDoubles(t, fake, cfg)
			useBoltStore(t, cfg)

			err := experiment.Start(notes.Context(context.Background(), false), experiment.StartWithName(startTestExp))
			if err == nil {
				t.Fatal("Start of an experiment minimega refused returned no error")
			}

			if rec.cleanups != 1 {
				t.Errorf("cleanup apps ran %d times, want once", rec.cleanups)
			}

			cleanup := slices.Index(events.events, "cleanup apps")
			cleared := slices.Index(events.events, "clear namespace "+startTestExp)

			if cleanup < 0 || cleared < cleanup {
				t.Errorf("the failed start ran %q, want the cleanup apps, then the namespace clear", events.events)
			}

			if got := storedStart(t); got != "" {
				t.Errorf("stored start time %q, want the experiment stopped", got)
			}
		})
	}
}
