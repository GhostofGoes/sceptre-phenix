package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"phenix/store"
	"phenix/types"
	"phenix/util/common"
	"phenix/util/mm"
	"phenix/util/plog"
)

type runtimeMM struct {
	mm.MM

	hosts mm.Hosts
	vms   mm.VMs
}

func (m runtimeMM) GetClusterHosts(bool) (mm.Hosts, error) {
	return m.hosts, nil
}

func (m runtimeMM) GetVMInfo(...mm.Option) mm.VMs {
	return m.vms
}

func TestPopulateRuntimeIncludesVMTapsInExperimentJSON(t *testing.T) {
	originalMM := mm.DefaultMM
	t.Cleanup(func() { mm.DefaultMM = originalMM }) //nolint:reassign // restore test double

	hosts := mm.Hosts{{Name: "compute-0"}}
	vms := mm.VMs{{
		Name: "router",
		Host: "compute-0",
		Taps: []string{"mega_tap101", "mega_tap102"},
	}}
	mm.DefaultMM = runtimeMM{hosts: hosts, vms: vms} //nolint:reassign // install test double

	exp := types.NewExperiment(store.ConfigMetadata{Name: "tap-test"})
	exp.Spec.SetExperimentName("tap-test")

	if err := PopulateRuntime(exp, true); err != nil {
		t.Fatalf("populating experiment runtime details: %v", err)
	}

	if len(exp.Hosts) != 1 || exp.Hosts[0].Name != hosts[0].Name {
		t.Fatalf("unexpected runtime hosts: %#v", exp.Hosts)
	}

	if len(exp.VMs) != 1 || exp.VMs[0].Name != vms[0].Name {
		t.Fatalf("unexpected runtime VMs: %#v", exp.VMs)
	}

	data, err := json.Marshal(exp)
	if err != nil {
		t.Fatalf("marshaling experiment: %v", err)
	}

	var payload struct {
		VMs []struct {
			Taps []string `json:"taps"`
		} `json:"vms"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("unmarshaling experiment JSON: %v", err)
	}

	if len(payload.VMs) != 1 || len(payload.VMs[0].Taps) != 2 {
		t.Fatalf("VM taps missing from experiment JSON: %s", data)
	}
}

type countingRuntimeMM struct {
	mm.MM

	vmInfoCalls int
}

func (*countingRuntimeMM) GetClusterHosts(bool) (mm.Hosts, error) {
	return mm.Hosts{{Name: "compute-0"}}, nil
}

func (m *countingRuntimeMM) GetVMInfo(...mm.Option) mm.VMs {
	m.vmInfoCalls++

	return mm.VMs{{Name: "router", Host: "compute-0"}}
}

// TestUserAppQueriesVMsOnlyWhenTheyCanExist is a regression test: every user
// app stage ran vm info in the experiment's namespace, so configure, pre-start
// and dry-run stages, and a post-start or cleanup stage triggered for a stopped
// experiment, made minimega create a namespace that held no VMs. So did the
// cleanup of a real start refused after pre-start while the stored run was a
// dry run.
func TestUserAppQueriesVMsOnlyWhenTheyCanExist(t *testing.T) {
	dir := t.TempDir()

	//nolint:gosec // test user app must be executable
	if err := os.WriteFile(
		filepath.Join(dir, UserAppPrefix+"runtime-test"),
		[]byte("#!/bin/sh\ncat >/dev/null\n"),
		0o700,
	); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	tests := []struct {
		action       Action
		dryRun       bool
		stopped      bool // the stage was triggered for a stopped experiment
		storedDryRun bool // the stored run is a dry run, but this stage's is not
		want         int
	}{
		{action: ActionConfigure, want: 0},
		{action: ActionPreStart, want: 0},
		{action: ActionPostStart, want: 1},
		{action: ActionRunning, want: 1},
		{action: ActionCleanup, want: 1},
		{action: ActionPreStart, dryRun: true, want: 0},
		{action: ActionPostStart, dryRun: true, want: 0},
		{action: ActionRunning, dryRun: true, want: 0},
		{action: ActionCleanup, dryRun: true, want: 0},
		{action: ActionPostStart, stopped: true, want: 0},
		{action: ActionCleanup, stopped: true, want: 0},
		{action: ActionCleanup, storedDryRun: true, want: 0},
	}

	for _, tt := range tests {
		name := fmt.Sprintf("%s/dryrun=%t/stopped=%t/storedDryRun=%t", tt.action, tt.dryRun, tt.stopped, tt.storedDryRun)
		t.Run(name, func(t *testing.T) {
			fake := new(countingRuntimeMM)

			originalMM := mm.DefaultMM
			t.Cleanup(func() { mm.DefaultMM = originalMM }) //nolint:reassign // restore test double

			mm.DefaultMM = fake //nolint:reassign // install test double

			var u UserApp
			if err := u.Init(Name("runtime-test"), DryRun(tt.dryRun)); err != nil {
				t.Fatal(err)
			}

			exp := types.NewExperiment(store.ConfigMetadata{Name: "runtime-test"})
			exp.Spec.SetExperimentName("runtime-test")

			// The experiment runs from post-start on, and in its cleanup.
			if !tt.stopped {
				exp.Status.SetStartTime("2026-10-18T00:00:00Z")

				if tt.dryRun || tt.storedDryRun {
					exp.Status.SetStartTime("2026-10-18T00:00:00Z-DRYRUN")
				}
			}

			if err := u.shellOut(context.Background(), tt.action, exp); err != nil {
				t.Fatalf("running user app: %v", err)
			}

			if fake.vmInfoCalls != tt.want {
				t.Fatalf("expected %d vm info queries, got %d", tt.want, fake.vmInfoCalls)
			}

			if len(exp.Hosts) != 1 {
				t.Fatalf("expected the cluster hosts in the app input, got %#v", exp.Hosts)
			}

			if len(exp.VMs) != tt.want {
				t.Fatalf("expected %d runtime VMs, got %#v", tt.want, exp.VMs)
			}
		})
	}
}

// TestUserAppGetsTheHostnameSuffixes is a regression test: apps trimmed only
// the default suffixes -minimega and -phenix from host names to tell the
// headnode, since phenix passed them no --hostname-suffixes.
func TestUserAppGetsTheHostnameSuffixes(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "suffixes")

	//nolint:gosec // test user app must be executable
	if err := os.WriteFile(
		filepath.Join(dir, UserAppPrefix+"suffixes-test"),
		[]byte("#!/bin/sh\ncat >/dev/null\nprintf %s \"$PHENIX_HOSTNAME_SUFFIXES\" > "+out+"\n"),
		0o700,
	); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	originalMM, originalSuffixes := mm.DefaultMM, common.HostnameSuffixes
	t.Cleanup(func() {
		mm.DefaultMM = originalMM                  //nolint:reassign // restore test double
		common.HostnameSuffixes = originalSuffixes //nolint:reassign // restore configuration
	})

	mm.DefaultMM = new(countingRuntimeMM) //nolint:reassign // install test double
	common.HostnameSuffixes = "-foo,-bar" //nolint:reassign // configure the test

	var u UserApp
	if err := u.Init(Name("suffixes-test")); err != nil {
		t.Fatal(err)
	}

	exp := types.NewExperiment(store.ConfigMetadata{Name: "suffixes-test"})
	exp.Spec.SetExperimentName("suffixes-test")

	if err := u.shellOut(context.Background(), ActionConfigure, exp); err != nil {
		t.Fatalf("running user app: %v", err)
	}

	got, err := os.ReadFile(out) //nolint:gosec // test file
	if err != nil || string(got) != "-foo,-bar" {
		t.Errorf("the app got PHENIX_HOSTNAME_SUFFIXES %q (%v), want -foo,-bar", got, err)
	}
}

// TestUserAppRefusesInjectionWithoutSource is a regression test: a user app
// that returned an injection with an empty or null src, as the ot-sim app did
// for TLS files nobody configured, had its experiment accepted, and the
// minimega script then rendered the source as "<base dir>/", so minimega
// copied the experiment's whole base directory into the VM. The stage fails,
// naming the app, the node and the destination, and keeps the spec.
func TestUserAppRefusesInjectionWithoutSource(t *testing.T) {
	var (
		dir    = t.TempDir()
		output = filepath.Join(dir, "output.json")
	)

	//nolint:gosec // test user app must be executable
	if err := os.WriteFile(
		filepath.Join(dir, UserAppPrefix+"inject-test"),
		[]byte("#!/bin/sh\ncat >/dev/null\ncat '"+output+"'\n"),
		0o700,
	); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	originalMM := mm.DefaultMM
	t.Cleanup(func() { mm.DefaultMM = originalMM }) //nolint:reassign // restore test double

	mm.DefaultMM = new(countingRuntimeMM) //nolint:reassign // install test double

	const node = `{"type": "VirtualMachine", "general": {"hostname": "rtu-1"}%s, "injections": %s}`

	tests := []struct {
		name       string
		action     Action
		injections string
		external   bool
		wantErr    string
	}{
		{
			name:       "null source at pre-start",
			action:     ActionPreStart,
			injections: `[{"src": "rtu-1/config.xml", "dst": "/etc/ot-sim/config.xml"}, {"src": null, "dst": "/etc/ot-sim/ca.pem"}]`,
			wantErr:    `user app inject-test returned an experiment phenix refuses: node rtu-1: injection to "/etc/ot-sim/ca.pem" has no source`,
		},
		{
			name:       "empty source at configure",
			action:     ActionConfigure,
			injections: `[{"src": "", "dst": "/etc/ot-sim/ca.pem"}]`,
			wantErr:    `node rtu-1: injection to "/etc/ot-sim/ca.pem" has no source`,
		},
		{
			name:       "null injection",
			action:     ActionPreStart,
			injections: `[null]`,
			wantErr:    "node rtu-1: injection 1 is null",
		},
		{
			name:       "deletion that leaves the disk",
			action:     ActionConfigure,
			injections: `[], "deletions": [{"path": "/../../etc/passwd"}]`,
			wantErr:    `node rtu-1: deletion path "/../../etc/passwd" contains '..'`,
		},
		{
			name:       "external node",
			action:     ActionPreStart,
			injections: `[{"src": null, "dst": "/etc/ot-sim/ca.pem"}]`,
			external:   true,
		},
		{
			name:       "every injection has a source",
			action:     ActionPreStart,
			injections: `[{"src": "rtu-1/config.xml", "dst": "/etc/ot-sim/config.xml"}]`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var external string
			if tt.external {
				external = `, "external": true`
			}

			out := `{"spec": {"experimentName": "inject-test", "topology": {"nodes": [` +
				fmt.Sprintf(node, external, tt.injections) + `]}}}`

			if err := os.WriteFile(output, []byte(out), 0o600); err != nil {
				t.Fatal(err)
			}

			var u UserApp
			if err := u.Init(Name("inject-test")); err != nil {
				t.Fatal(err)
			}

			exp := types.NewExperiment(store.ConfigMetadata{Name: "inject-test"})
			exp.Spec.SetExperimentName("inject-test")

			original := exp.Spec

			err := u.shellOut(context.Background(), tt.action, exp)

			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("running user app: %v", err)
				}

				if nodes := exp.Spec.Topology().Nodes(); len(nodes) != 1 || len(nodes[0].Injections()) != 1 {
					t.Errorf("the experiment did not take the app's topology: %#v", nodes)
				}

				return
			}

			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("running user app = %v, want an error with %q", err, tt.wantErr)
			}

			if exp.Spec != original {
				t.Errorf("the experiment took the spec the app returned")
			}
		})
	}
}

// TestUserAppCleanupKeepsGoingWithARefusedSpec is a regression test: the
// cleanup stage of an app that passed on an injection phenix refuses, such as
// one an older phenix stored without a source, failed, and Stop then skipped
// the cleanup of every later app, so the tap app's host taps were left
// behind. Nothing renders the spec of a stopping experiment, so the stage
// warns, keeps the spec, and the next start refuses it.
func TestUserAppCleanupKeepsGoingWithARefusedSpec(t *testing.T) {
	var (
		dir    = t.TempDir()
		output = filepath.Join(dir, "output.json")
	)

	//nolint:gosec // test user app must be executable
	if err := os.WriteFile(
		filepath.Join(dir, UserAppPrefix+"cleanup-test"),
		[]byte("#!/bin/sh\ncat >/dev/null\ncat '"+output+"'\n"),
		0o700,
	); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	originalMM := mm.DefaultMM
	t.Cleanup(func() { mm.DefaultMM = originalMM }) //nolint:reassign // restore test double

	mm.DefaultMM = new(countingRuntimeMM) //nolint:reassign // install test double

	out := `{"spec": {"experimentName": "cleanup-test", "topology": {"nodes": [` +
		`{"type": "VirtualMachine", "general": {"hostname": "rtu-1"}, "injections": [{"dst": "/etc/ot-sim/ca.pem"}]}` +
		`]}}}`

	if err := os.WriteFile(output, []byte(out), 0o600); err != nil {
		t.Fatal(err)
	}

	var logs bytes.Buffer

	plog.AddHandler(t.Name(), slog.NewTextHandler(&logs, nil))
	t.Cleanup(func() { plog.RemoveHandler(t.Name()) })

	var u UserApp
	if err := u.Init(Name("cleanup-test")); err != nil {
		t.Fatal(err)
	}

	exp := types.NewExperiment(store.ConfigMetadata{Name: "cleanup-test"})
	exp.Spec.SetExperimentName("cleanup-test")

	if err := u.shellOut(context.Background(), ActionCleanup, exp); err != nil {
		t.Fatalf("cleanup stage = %v, want it to keep going", err)
	}

	if nodes := exp.Spec.Topology().Nodes(); len(nodes) != 1 || nodes[0].General().Hostname() != "rtu-1" {
		t.Errorf("the experiment did not take the spec the app returned: %#v", nodes)
	}

	for _, want := range []string{"the next configure or start refuses", "app=cleanup-test", "has no source"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("logs lack %q:\n%s", want, logs.String())
		}
	}
}
