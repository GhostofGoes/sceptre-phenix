package experiment_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/golang/mock/gomock"

	"phenix/api/config"
	"phenix/api/experiment"
	"phenix/app"
	"phenix/store"
	"phenix/types"
	"phenix/util/mm"
	"phenix/util/notes"
)

const c2DelaysTestApp = "c2-delays-test-app"

// c2DelaysApp adds the node c2DelaysAdded at configure, as the caldera app
// adds its server.
type c2DelaysApp struct{ *noopApp }

//nolint:gochecknoglobals // the app registry is global, so the app is registered once
var (
	c2DelaysAppOnce sync.Once
	c2DelaysAdded   string
)

func (c2DelaysApp) Configure(_ context.Context, exp *types.Experiment) error {
	if c2DelaysAdded != "" {
		node := exp.Spec.Topology().AddNode("VirtualMachine", c2DelaysAdded)
		node.AddHardware("linux", 1, 512)
		node.Hardware().AddDrive("caldera.qc2", 1)
	}

	return nil
}

// c2DelaysNode is a node that waits on the C2 clients of the given hosts.
func c2DelaysNode(hostname, image string, waits ...string) map[string]any {
	node := map[string]any{
		"type":     "VirtualMachine",
		"general":  map[string]any{"hostname": hostname},
		"hardware": map[string]any{"os_type": "linux", "drives": []map[string]any{{"image": image}}},
	}

	if len(waits) > 0 {
		c2 := make([]map[string]any, len(waits))

		for i, host := range waits {
			c2[i] = map[string]any{"hostname": host}
		}

		node["delay"] = map[string]any{"c2": c2}
	}

	return node
}

// TestCreateChecksC2DelaysAfterTheConfigureApps is a regression test:
// experiment create checked delay.c2 hostnames before the configure apps ran,
// so it refused a wait on a node an app adds at configure, such as the
// caldera app's server.
func TestCreateChecksC2DelaysAfterTheConfigureApps(t *testing.T) {
	c2DelaysAppOnce.Do(func() {
		err := app.RegisterUserApp(c2DelaysTestApp, func() app.App { return c2DelaysApp{&noopApp{name: c2DelaysTestApp}} })
		if err != nil {
			t.Fatalf("registering test app: %v", err)
		}
	})

	for _, tc := range []struct {
		name, added, want string
	}{
		{name: "node an app adds", added: "caldera-0"},
		{name: "node nobody adds", want: `validating node agent-1: delay.c2 hostname "caldera-0" is not a node in the topology`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c2DelaysAdded = tc.added

			var (
				ctrl = gomock.NewController(t)
				m    = store.NewMockStore(ctrl)
			)

			m.EXPECT().Get(gomock.Any()).Return(store.ErrNotExist).AnyTimes()
			m.EXPECT().List(gomock.Any()).Return(store.Configs{}, nil).AnyTimes()

			if tc.want == "" {
				m.EXPECT().Create(gomock.Any()).Return(nil).Times(1)
			}

			useStore(t, m)

			_, err := config.Create(config.CreateFromConfig(&store.Config{
				Version:  "phenix.sandia.gov/v1",
				Kind:     "Experiment",
				Metadata: store.ConfigMetadata{Name: "c2-delays"},
				Spec: map[string]any{
					"experimentName": "c2-delays",
					"baseDir":        t.TempDir(),
					"topology": map[string]any{
						"nodes": []map[string]any{c2DelaysNode("agent-1", "agent.qc2", "caldera-0")},
					},
					"scenario": map[string]any{"apps": []map[string]any{{"name": c2DelaysTestApp}}},
				},
			}))

			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("creating experiment: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("creating experiment = %v, want an error with %q", err, tc.want)
			}
		})
	}
}

// TestStartRefusesC2DelayOnANodeThatWillNotBoot checks that a start refuses a
// delay.c2 on a node the startup app marked do-not-boot at pre-start because
// its image is missing (after create's check): minimega never launches that
// node, so the wait would never end. The refusal names the reason, sends
// minimega nothing, and runs the cleanup apps for what the pre-start apps set
// up.
func TestStartRefusesC2DelayOnANodeThatWillNotBoot(t *testing.T) {
	rec := new(startRecord)
	registerStartApp(t, rec)

	recorder := useNotBootingC2Experiment(t)

	err := experiment.Start(context.Background(), experiment.StartWithName("c2-dnb"))

	want := `validating node b: delay.c2 hostname "a" is a do-not-boot node, which minimega does not launch (missing disk image)`
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("Start() = %v, want an error with %q", err, want)
	}

	if len(recorder.commands) != 0 {
		t.Errorf("Start() sent minimega %q", recorder.commands)
	}

	if rec.cleanups != 1 {
		t.Errorf("cleanup apps ran %d times after the refused start, want once", rec.cleanups)
	}
}

// TestDryRunStartWarnsOfC2DelayOnANodeThatWillNotBoot checks that a dry run
// reports the refusal of a delay.c2 on a node the startup app marked
// do-not-boot (its disk image is missing on this host) as a warning and
// succeeds, because a dry run waits for no VM and the image may exist where
// the experiment really starts.
func TestDryRunStartWarnsOfC2DelayOnANodeThatWillNotBoot(t *testing.T) {
	registerStartApp(t, new(startRecord))

	recorder := useNotBootingC2Experiment(t)

	// A dry run renders the minimega script, whose snapshot names hold the
	// head node's name.
	mm.DefaultMM = headnodeMM{recorder} //nolint:reassign // install test double

	ctx := notes.Context(context.Background(), false)

	if err := experiment.Start(ctx, experiment.StartWithName("c2-dnb"), experiment.StartWithDryRun(true)); err != nil {
		t.Fatalf("dry-run Start() = %v, want a warning only", err)
	}

	want := `delay.c2 hostname "a" is a do-not-boot node, which minimega does not launch (missing disk image)`

	var warned bool

	for _, w := range notes.Warnings(ctx, true) {
		warned = warned || strings.Contains(w.Error(), want)
	}

	if !warned {
		t.Errorf("dry-run Start() warned %v, want a warning with %q", notes.Warnings(ctx, true), want)
	}

	if len(recorder.commands) != 0 {
		t.Errorf("dry-run Start() sent minimega %q", recorder.commands)
	}
}

// headnodeMM is a commandRecordingMM that also names the head node.
type headnodeMM struct{ *commandRecordingMM }

func (headnodeMM) Headnode() string { return "headnode" }

// useNotBootingC2Experiment stores the experiment c2-dnb, whose node b waits
// on a C2 client of node a, whose disk image is missing, until the test ends,
// and installs a minimega double that records the commands it gets.
func useNotBootingC2Experiment(t *testing.T) *commandRecordingMM {
	t.Helper()

	baseDir := t.TempDir()

	if err := os.WriteFile(filepath.Join(baseDir, "present.qc2"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	useBoltStore(t, store.Config{
		Version:  "phenix.sandia.gov/v1",
		Kind:     "Experiment",
		Metadata: store.ConfigMetadata{Name: "c2-dnb"},
		Spec: map[string]any{
			"experimentName": "c2-dnb",
			"baseDir":        baseDir,
			"topology": map[string]any{
				"nodes": []map[string]any{
					c2DelaysNode("a", filepath.Join(baseDir, "missing.qc2")),
					c2DelaysNode("b", filepath.Join(baseDir, "present.qc2"), "a"),
				},
			},
			"scenario": map[string]any{"apps": []map[string]any{{"name": startTestApp}}},
		},
	})

	originalMM := mm.DefaultMM
	t.Cleanup(func() { mm.DefaultMM = originalMM }) //nolint:reassign // restore test double

	recorder := new(commandRecordingMM)
	mm.DefaultMM = recorder //nolint:reassign // install test double

	return recorder
}

// TestStartRefusingAC2DelayRunsCleanupApps is a regression test: a start that
// refused a C2 delay once the pre-start apps had run returned without running
// the cleanup apps, so what those apps set up, such as the tap app's host
// taps, stayed behind.
func TestStartRefusingAC2DelayRunsCleanupApps(t *testing.T) {
	rec := new(startRecord)
	registerStartApp(t, rec)

	cfg := startTestConfig(t, false, false)

	// Topology validation leaves a hostname no node has yet to the check
	// start runs once the pre-start apps ran.
	nodes, _ := cfg.Spec["topology"].(map[string]any)["nodes"].([]map[string]any)
	nodes[1]["delay"] = map[string]any{"c2": []map[string]any{{"hostname": "nosuch"}}}

	events := new(failedStartEvents)
	installStartDoubles(t, failedStartMM{startMM: new(startMM), events: events}, cfg)
	useBoltStore(t, cfg)

	err := experiment.Start(context.Background(), experiment.StartWithName(startTestExp))

	want := `validating node timer: delay.c2 hostname "nosuch" is not a node in the topology`
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("Start() = %v, want an error with %q", err, want)
	}

	if rec.cleanups != 1 {
		t.Errorf("cleanup apps ran %d times, want once", rec.cleanups)
	}

	if len(events.events) != 0 {
		t.Errorf("the refused start sent minimega %q", events.events)
	}

	if got := storedStart(t); got != "" {
		t.Errorf("stored start time %q, want the experiment stopped", got)
	}
}
