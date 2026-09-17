package config_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/golang/mock/gomock"

	"phenix/api/config"
	"phenix/store"
)

func TestListError(t *testing.T) {
	configs := store.Configs(
		[]store.Config{
			{
				Version: "phenix.sandia.gov/v1",
				Kind:    "Experiment",
				Metadata: store.ConfigMetadata{
					Name: "test-experiment",
				},
			},
		},
	)

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	m := store.NewMockStore(ctrl)
	m.EXPECT().
		List(gomock.Eq("Topology"), gomock.Eq("Scenario"), gomock.Eq("Experiment"), gomock.Eq("Image")).
		Return(configs, nil).
		AnyTimes()

	store.DefaultStore = m //nolint:reassign // mocking

	_, err := config.List("blech")
	if err == nil {
		t.Log("expected error")
		t.FailNow()
	}
}

func TestUpdateFromPath(t *testing.T) {
	const body = `apiVersion: phenix.sandia.gov/v1
kind: Topology
metadata:
  name: test-topology
spec:
  nodes:
    - type: VirtualMachine
      general:
        hostname: test-node
      hardware:
        os_type: linux
        drives:
          - image: ubuntu.qc2
`

	path := filepath.Join(t.TempDir(), "topology.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing config: %v", err)
	}

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	m := store.NewMockStore(ctrl)
	m.EXPECT().Get(gomock.Any()).DoAndReturn(func(c *store.Config) error {
		c.Metadata.Created = "created"
		return nil
	})
	m.EXPECT().Update(gomock.Any()).DoAndReturn(func(c *store.Config) error {
		if c.FullName() != "Topology/test-topology" {
			t.Errorf("unexpected config name: %s", c.FullName())
		}
		if c.Metadata.Created != "created" {
			t.Errorf("created timestamp not preserved: %q", c.Metadata.Created)
		}
		return nil
	})

	previous := store.DefaultStore
	store.DefaultStore = m //nolint:reassign // mocking
	t.Cleanup(func() {
		store.DefaultStore = previous //nolint:reassign // restore global test state
	})

	c, err := config.UpdateFromPath(path)
	if err != nil {
		t.Fatalf("updating config from path: %v", err)
	}

	if c.FullName() != "Topology/test-topology" {
		t.Fatalf("unexpected updated config: %s", c.FullName())
	}
}

func TestUpdateFromPathPreservesExperimentState(t *testing.T) {
	const body = `apiVersion: phenix.sandia.gov/v1
kind: Experiment
metadata:
  name: test-experiment
spec:
  topology:
    nodes:
      - type: VirtualMachine
        general:
          hostname: test-node
        hardware:
          os_type: linux
          drives:
            - image: ubuntu.qc2
`

	path := filepath.Join(t.TempDir(), "experiment.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing config: %v", err)
	}

	status := map[string]any{"startTime": "running"}

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	m := store.NewMockStore(ctrl)
	gomock.InOrder(
		m.EXPECT().Get(gomock.Any()).DoAndReturn(func(c *store.Config) error {
			c.Metadata.Name = "test-experiment"
			c.Status = status
			return nil
		}),
		m.EXPECT().Get(gomock.Any()).DoAndReturn(func(c *store.Config) error {
			c.Metadata.Created = "created"
			return nil
		}),
		m.EXPECT().Update(gomock.Any()).DoAndReturn(func(c *store.Config) error {
			if c.Status["startTime"] != "running" {
				t.Errorf("experiment status not preserved: %#v", c.Status)
			}
			if c.Spec["experimentName"] != "test-experiment" {
				t.Errorf("experiment name not restored: %#v", c.Spec["experimentName"])
			}
			return nil
		}),
	)

	previous := store.DefaultStore
	store.DefaultStore = m //nolint:reassign // mocking
	t.Cleanup(func() {
		store.DefaultStore = previous //nolint:reassign // restore global test state
	})

	if _, err := config.UpdateFromPath(path); err != nil {
		t.Fatalf("updating experiment from path: %v", err)
	}
}

func TestUpdateFromPathMissingConfig(t *testing.T) {
	const body = `apiVersion: phenix.sandia.gov/v1
kind: Topology
metadata:
  name: missing
spec:
  nodes: []
`

	path := filepath.Join(t.TempDir(), "topology.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing config: %v", err)
	}

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	m := store.NewMockStore(ctrl)
	m.EXPECT().Get(gomock.Any()).Return(store.ErrNotExist)

	previous := store.DefaultStore
	store.DefaultStore = m //nolint:reassign // mocking
	t.Cleanup(func() {
		store.DefaultStore = previous //nolint:reassign // restore global test state
	})

	if _, err := config.UpdateFromPath(path); !errors.Is(err, store.ErrNotExist) {
		t.Fatalf("expected missing config error, got: %v", err)
	}
}

func TestCreateEnv(t *testing.T) {
	expected := store.Config{
		Version: "phenix.sandia.gov/v1",
		Kind:    "Topology",
		Metadata: store.ConfigMetadata{
			Name: "foobar-test-experiment",
		},
	}

	cfg := `
	{
		"apiVersion": "phenix.sandia.gov/v1",
		"kind": "Topology",
		"metadata": {
			"name": "${BRANCH_NAME}-test-experiment"
		}
	}
	`

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	m := store.NewMockStore(ctrl)
	m.EXPECT().Create(gomock.Eq(&expected)).Return(nil).AnyTimes()

	store.DefaultStore = m //nolint:reassign // mocking

	t.Setenv("BRANCH_NAME", "foobar")
	options := []config.CreateOption{config.CreateFromJSON([]byte(cfg))}

	_, err := config.Create(options...)
	if err != nil {
		t.Log(err)
		t.FailNow()
	}
}
