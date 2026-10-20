package experiment_test

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"

	"github.com/golang/mock/gomock"

	"phenix/api/config"
	"phenix/api/experiment"
	"phenix/app"
	"phenix/store"
	"phenix/types"
	"phenix/util/common"
	"phenix/util/mm"
)

const (
	bridgeModeTestExp = "bridge-auto"
	bridgeModeTestApp = "bridge-mode-test-app"
)

// bridgeModeApp records the interface bridges its configure stage sees.
type bridgeModeApp struct{ *noopApp }

func (bridgeModeApp) Configure(_ context.Context, exp *types.Experiment) error {
	bridgeModeSeen = interfaceBridges(exp)

	return nil
}

//nolint:gochecknoglobals // the app registry is global, so the app is registered once
var (
	bridgeModeAppOnce sync.Once
	bridgeModeSeen    []string
)

// bridgeModeTopology has one node whose interfaces are on the given bridges:
// "" names no bridge, phenix counts as none, any other is the topology's own.
func bridgeModeTopology(bridges ...string) map[string]any {
	ifaces := make([]map[string]any, len(bridges))

	for i, bridge := range bridges {
		ifaces[i] = map[string]any{
			"name":    fmt.Sprintf("eth%d", i),
			"type":    "ethernet",
			"proto":   "static",
			"address": fmt.Sprintf("10.0.0.%d", i+1),
			"mask":    24,
			"vlan":    "LAN",
			"bridge":  bridge,
		}
	}

	return map[string]any{
		"nodes": []map[string]any{{
			"type":     "VirtualMachine",
			"general":  map[string]any{"hostname": "host-1"},
			"hardware": map[string]any{"os_type": "linux", "drives": []map[string]any{{"image": "test.qc2"}}},
			"network":  map[string]any{"interfaces": ifaces},
		}},
	}
}

// useAutoBridgeMode switches to the auto bridge mode and installs a minimega
// double that panics on any call, for the test.
func useAutoBridgeMode(t *testing.T) {
	t.Helper()

	originalMode, originalMM := common.BridgeMode, mm.DefaultMM

	t.Cleanup(func() {
		common.BridgeMode = originalMode //nolint:reassign // restore global config
		mm.DefaultMM = originalMM        //nolint:reassign // restore test double
	})

	common.BridgeMode = common.BridgeModeAuto //nolint:reassign // global config under test
	mm.DefaultMM = new(commandRecordingMM)    //nolint:reassign // install test double
}

func interfaceBridges(exp *types.Experiment) []string {
	var bridges []string

	for _, node := range exp.Spec.Topology().Nodes() {
		for _, iface := range node.Network().Interfaces() {
			bridges = append(bridges, iface.Bridge())
		}
	}

	return bridges
}

// wantStoredBridges checks the default bridge and the interface bridges of
// the stored experiment.
func wantStoredBridges(t *testing.T, c *store.Config, want ...string) {
	t.Helper()

	if c == nil {
		t.Fatal("no experiment was stored")
	}

	exp, err := types.DecodeExperimentFromConfig(*c)
	if err != nil {
		t.Fatalf("decoding stored experiment: %v", err)
	}

	if got := exp.Spec.DefaultBridge(); got != bridgeModeTestExp {
		t.Errorf("spec.defaultBridge = %q, want %q", got, bridgeModeTestExp)
	}

	if got := interfaceBridges(exp); !slices.Equal(got, want) {
		t.Errorf("stored interface bridges %q, want %q", got, want)
	}
}

// TestCreateAutoBridgeModeBridgesInterfaces is a regression test: in the
// auto bridge mode, create set the default bridge only after Init had put the
// interfaces that name no bridge on phenix, so the configure apps saw them
// there.
func TestCreateAutoBridgeModeBridgesInterfaces(t *testing.T) {
	bridgeModeAppOnce.Do(func() {
		err := app.RegisterUserApp(bridgeModeTestApp, func() app.App { return bridgeModeApp{&noopApp{name: bridgeModeTestApp}} })
		if err != nil {
			t.Fatalf("registering test app: %v", err)
		}
	})

	bridgeModeSeen = nil

	useAutoBridgeMode(t)

	var (
		ctrl   = gomock.NewController(t)
		m      = store.NewMockStore(ctrl)
		stored *store.Config
	)

	m.EXPECT().Get(gomock.Any()).Return(store.ErrNotExist).AnyTimes()
	m.EXPECT().List(gomock.Any()).Return(store.Configs{}, nil).AnyTimes()
	m.EXPECT().Create(gomock.Any()).DoAndReturn(func(c *store.Config) error {
		stored = c

		return nil
	}).Times(1)

	useStore(t, m)

	_, err := config.Create(config.CreateFromConfig(&store.Config{
		Version:  "phenix.sandia.gov/v1",
		Kind:     "Experiment",
		Metadata: store.ConfigMetadata{Name: bridgeModeTestExp},
		Spec: map[string]any{
			"experimentName": bridgeModeTestExp,
			"baseDir":        t.TempDir(),
			"defaultBridge":  "phenix",
			"topology":       bridgeModeTopology("", "phenix", "pinned"),
			"scenario":       map[string]any{"apps": []map[string]any{{"name": bridgeModeTestApp}}},
		},
	}))
	if err != nil {
		t.Fatalf("creating experiment: %v", err)
	}

	want := []string{bridgeModeTestExp, bridgeModeTestExp, "pinned"}

	if !slices.Equal(bridgeModeSeen, want) {
		t.Errorf("the configure apps saw interface bridges %q, want %q", bridgeModeSeen, want)
	}

	wantStoredBridges(t, stored, want...)
}

// TestUpdateAutoBridgeModeBridgesInterfaces is a regression test: in the
// auto bridge mode, an update such as reconfigure set the default bridge only
// after Init had left the interfaces on the old default bridge, so the next
// start put the VMs there.
func TestUpdateAutoBridgeModeBridgesInterfaces(t *testing.T) {
	useAutoBridgeMode(t)

	// The experiment as a manual-mode create stores it: on phenix, with each
	// interface's bridge marked as set by phenix or by the topology.
	topo := bridgeModeTopology("phenix", "pinned")
	ifaces := topo["nodes"].([]map[string]any)[0]["network"].(map[string]any)["interfaces"].([]map[string]any)
	ifaces[0]["bridge_set_in_topo"] = false
	ifaces[1]["bridge_set_in_topo"] = true

	current := store.Config{
		Version:  "phenix.sandia.gov/v1",
		Kind:     "Experiment",
		Metadata: store.ConfigMetadata{Name: bridgeModeTestExp},
		Spec: map[string]any{
			"experimentName": bridgeModeTestExp,
			"baseDir":        t.TempDir(),
			"defaultBridge":  "phenix",
			"topology":       topo,
		},
	}

	var (
		ctrl   = gomock.NewController(t)
		m      = store.NewMockStore(ctrl)
		stored *store.Config
	)

	m.EXPECT().Get(gomock.Any()).DoAndReturn(func(c *store.Config) error {
		*c = current

		return nil
	}).AnyTimes()
	m.EXPECT().List(gomock.Any()).Return(store.Configs{current}, nil).AnyTimes()
	m.EXPECT().Update(gomock.Any()).DoAndReturn(func(c *store.Config) error {
		stored = c

		return nil
	}).Times(1)

	useStore(t, m)

	if err := experiment.Reconfigure(bridgeModeTestExp); err != nil {
		t.Fatalf("reconfiguring experiment: %v", err)
	}

	wantStoredBridges(t, stored, bridgeModeTestExp, "pinned")
}
