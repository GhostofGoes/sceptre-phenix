package config_test

import (
	"errors"
	"path/filepath"
	"testing"

	"phenix/api/config"
	"phenix/store"
)

// useUpdateTestStore installs a BoltDB store holding the topologies lab and
// other, which differ in their one node's hostname, until the test ends.
func useUpdateTestStore(t *testing.T) {
	t.Helper()

	original := store.DefaultStore
	t.Cleanup(func() { store.DefaultStore = original }) //nolint:reassign // restore default

	db := store.NewBoltDB()
	if err := db.Init(store.Endpoint("bolt://" + filepath.Join(t.TempDir(), "store.bdb"))); err != nil {
		t.Fatalf("initializing BoltDB: %v", err)
	}

	store.DefaultStore = db //nolint:reassign // install test store

	for _, name := range []string{"lab", "other"} {
		c := validateTestTopology(t, "phenix.sandia.gov/v1", name,
			"{nodes: [{type: VirtualMachine, general: {hostname: "+name+"-host}, hardware: {os_type: linux, drives: [{image: a.qc2}]}}]}")
		if err := store.Create(c); err != nil {
			t.Fatalf("storing topology %s: %v", name, err)
		}
	}
}

// storedHostname returns the hostname of the stored topology's one node, or
// "" when the store holds no topology of that name.
func storedHostname(t *testing.T, name string) string {
	t.Helper()

	c, _ := store.NewConfig("topology/" + name)
	if err := store.Get(c); err != nil {
		if !errors.Is(err, store.ErrNotExist) {
			t.Fatalf("getting topology %s: %v", name, err)
		}

		return ""
	}

	nodes, _ := c.Spec["nodes"].([]any)
	if len(nodes) != 1 {
		t.Fatalf("topology %s has %d nodes, want 1", name, len(nodes))
	}

	node, _ := nodes[0].(map[string]any)
	general, _ := node["general"].(map[string]any)
	hostname, _ := general["hostname"].(string)

	return hostname
}

// TestUpdateRefusesRenameOntoExistingConfig is a regression test: a config
// update that renamed lab to the name of the existing config other replaced
// other with lab's content and kept lab, because the store's update of the
// new name succeeded and the rename path never ran.
func TestUpdateRefusesRenameOntoExistingConfig(t *testing.T) {
	useUpdateTestStore(t)

	c, _ := store.NewConfig("topology/lab")
	if err := store.Get(c); err != nil {
		t.Fatalf("getting topology lab: %v", err)
	}

	c.Metadata.Name = "other"

	if err := config.Update("topology/lab", c); !errors.Is(err, store.ErrExist) {
		t.Errorf("renaming lab onto other returned %v, want ErrExist", err)
	}

	if got := storedHostname(t, "other"); got != "other-host" {
		t.Errorf("topology other holds hostname %q after the refused rename, want other-host", got)
	}

	if got := storedHostname(t, "lab"); got != "lab-host" {
		t.Errorf("topology lab holds hostname %q after the refused rename, want lab-host", got)
	}
}

// TestUpdateRenamesConfig checks that a rename to a free name still moves the
// config.
func TestUpdateRenamesConfig(t *testing.T) {
	useUpdateTestStore(t)

	c, _ := store.NewConfig("topology/lab")
	if err := store.Get(c); err != nil {
		t.Fatalf("getting topology lab: %v", err)
	}

	c.Metadata.Name = "renamed"

	if err := config.Update("topology/lab", c); err != nil {
		t.Fatalf("renaming lab: %v", err)
	}

	if got := storedHostname(t, "renamed"); got != "lab-host" {
		t.Errorf("topology renamed holds hostname %q, want lab-host", got)
	}

	if got := storedHostname(t, "lab"); got != "" {
		t.Errorf("topology lab is still stored (hostname %q) after the rename", got)
	}
}
