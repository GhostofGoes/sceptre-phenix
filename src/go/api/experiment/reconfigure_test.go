package experiment_test

import (
	"testing"

	"github.com/golang/mock/gomock"

	"phenix/api/experiment"
	"phenix/store"
	"phenix/util/mm"
)

type commandRecordingMM struct {
	mm.MM

	commands []string
}

func (m *commandRecordingMM) MeshSend(_, _, command string) error {
	m.commands = append(m.commands, command)

	return nil
}

const reconfigureTestExperiment = `
apiVersion: phenix.sandia.gov/v1
kind: Experiment
metadata:
  name: reconfigure-test
spec:
  experimentName: reconfigure-test
  defaultBridge: phenix
  topology:
    nodes:
    - type: VirtualMachine
      general:
        hostname: host-1
      hardware:
        os_type: linux
        drives:
        - image: test.qc2
`

// TestReconfigureSendsNoMinimegaCommands is a regression test: reconfigure
// sent ns del-bridge in the experiment's namespace, which is cleared while the
// experiment is stopped, so the command never deleted a bridge and only made
// minimega create the namespace again.
func TestReconfigureSendsNoMinimegaCommands(t *testing.T) {
	cfg, err := store.NewConfigFromYAML([]byte(reconfigureTestExperiment))
	if err != nil {
		t.Fatalf("parsing experiment: %v", err)
	}

	ctrl := gomock.NewController(t)

	m := store.NewMockStore(ctrl)
	m.EXPECT().Get(gomock.Any()).DoAndReturn(func(c *store.Config) error {
		*c = *cfg
		return nil
	}).AnyTimes()
	m.EXPECT().List(gomock.Any()).Return(store.Configs{*cfg}, nil).AnyTimes()
	m.EXPECT().Update(gomock.Any()).Return(nil).Times(1)

	fake := new(commandRecordingMM)

	originalStore, originalMM := store.DefaultStore, mm.DefaultMM
	t.Cleanup(func() {
		store.DefaultStore = originalStore //nolint:reassign // restore default
		mm.DefaultMM = originalMM          //nolint:reassign // restore default
	})

	store.DefaultStore = m //nolint:reassign // install test double
	mm.DefaultMM = fake    //nolint:reassign // install test double

	if err := experiment.Reconfigure("reconfigure-test"); err != nil {
		t.Fatalf("reconfiguring experiment: %v", err)
	}

	if len(fake.commands) != 0 {
		t.Fatalf("expected no minimega commands, got %q", fake.commands)
	}
}
