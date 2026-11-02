package scheduler

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"phenix/store"
	"phenix/types"
	ifaces "phenix/types/interfaces"
	"phenix/util/mm"
	"phenix/util/shell"
)

// clusterMM fakes minimega's schedulable cluster hosts; other mm.MM methods panic.
type clusterMM struct {
	mm.MM

	hosts mm.Hosts
}

func (m clusterMM) GetClusterHosts(bool) (mm.Hosts, error) {
	return append(mm.Hosts(nil), m.hosts...), nil
}

// schedulerSpec is an experiment spec with VMs vm1 and vm2 on VLAN EXP, and
// the given schedules.
func schedulerSpec(t *testing.T, schedules map[string]any) ifaces.ExperimentSpec { //nolint:ireturn // the spec type
	t.Helper()

	node := func(name string) map[string]any {
		return map[string]any{
			"type":     "VirtualMachine",
			"general":  map[string]any{"hostname": name},
			"hardware": map[string]any{"vcpus": 1, "memory": 512, "os_type": "linux"},
			"network":  map[string]any{"interfaces": []map[string]any{{"name": "eth0", "vlan": "EXP"}}},
		}
	}

	if schedules == nil {
		schedules = map[string]any{}
	}

	exp, err := types.DecodeExperimentFromConfig(store.Config{
		Version:  "phenix.sandia.gov/v1",
		Kind:     "Experiment",
		Metadata: store.ConfigMetadata{Name: "exp"},
		Spec: map[string]any{
			"experimentName": "exp",
			"schedules":      schedules,
			"topology":       map[string]any{"nodes": []map[string]any{node("vm1"), node("vm2")}},
		},
	})
	if err != nil {
		t.Fatalf("types.DecodeExperimentFromConfig() error = %v", err)
	}

	return exp.Spec
}

func TestSchedulersWithoutSchedulableHosts(t *testing.T) { //nolint:paralleltest // replaces mm.DefaultMM
	head := mm.Host{Name: "head", MemTotal: 4096, CPUs: 4}

	tests := []struct {
		name      string
		hosts     mm.Hosts
		schedules map[string]any
		wantErr   error
		want      map[string]string
	}{
		{name: "fails with no hosts", wantErr: ErrNoSchedulableHosts},
		{
			name:  "schedules every VM on the only host",
			hosts: mm.Hosts{head},
			want:  map[string]string{"vm1": "head", "vm2": "head"},
		},
		{
			name:      "needs no host when every VM is scheduled",
			schedules: map[string]any{"vm1": "compute1", "vm2": "compute1"},
			want:      map[string]string{"vm1": "compute1", "vm2": "compute1"},
		},
	}

	for _, name := range []string{"round-robin", "subnet-compute", "isolate-experiment"} {
		for _, test := range tests {
			if name == "isolate-experiment" && test.schedules != nil {
				continue // isolates the experiment again on a free host
			}

			t.Run(name+"/"+test.name, func(t *testing.T) {
				original := mm.DefaultMM
				t.Cleanup(func() { mm.DefaultMM = original }) //nolint:reassign // restore test double

				mm.DefaultMM = clusterMM{hosts: test.hosts} //nolint:reassign // install test double

				spec := schedulerSpec(t, test.schedules)

				err := Schedule(name, spec)
				if !errors.Is(err, test.wantErr) || (test.wantErr == nil && err != nil) {
					t.Fatalf("Schedule(%s) error = %v, want %v", name, err, test.wantErr)
				}

				if test.wantErr != nil {
					return
				}

				if diff := cmp.Diff(test.want, spec.Schedules()); diff != "" {
					t.Errorf("Schedule(%s) schedules mismatch (-want +got):\n%s", name, diff)
				}
			})
		}
	}
}

// schedulerFakeShell finds the user schedulers named in commands and answers
// each run with out and err. Other shell.Shell methods panic.
type schedulerFakeShell struct {
	shell.Shell

	commands []string
	out      []byte
	err      error

	mu sync.Mutex
	// the commands looked for
	asked []string
}

func (s *schedulerFakeShell) FindCommandsWithPrefix(prefix string) []string {
	var names []string

	for _, c := range s.commands {
		if name, ok := strings.CutPrefix(c, prefix); ok {
			names = append(names, name)
		}
	}

	return names
}

func (s *schedulerFakeShell) CommandExists(name string) bool {
	s.mu.Lock()
	s.asked = append(s.asked, name)
	s.mu.Unlock()

	return slices.Contains(s.commands, name)
}

func (s *schedulerFakeShell) ExecCommand(context.Context, ...shell.Option) ([]byte, []byte, error) {
	return s.out, nil, s.err
}

// useSchedulerShell installs fake as the shell until the test ends.
func useSchedulerShell(t *testing.T, fake *schedulerFakeShell) {
	t.Helper()

	original := shell.DefaultShell
	t.Cleanup(func() { shell.DefaultShell = original }) //nolint:reassign // restore test double

	shell.DefaultShell = fake //nolint:reassign // install test double
}

// List names the built-in schedulers and the user schedulers on the PATH.
//
//nolint:paralleltest // replaces shell.DefaultShell
func TestList(t *testing.T) {
	builtIn := []string{"isolate-experiment", "round-robin", "subnet-compute"}

	for name, tc := range map[string]struct {
		commands []string
		want     []string
	}{
		"built-in only": {want: builtIn},
		"user scheduler": {
			commands: []string{"phenix-scheduler-custom", "phenix-other"},
			want:     append([]string{"custom"}, builtIn...),
		},
	} {
		t.Run(name, func(t *testing.T) {
			useSchedulerShell(t, &schedulerFakeShell{commands: tc.commands})

			got := List()
			slices.Sort(got)

			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("List() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// Schedule runs a scheduler it does not know as the user scheduler
// phenix-scheduler-<name>, and takes the schedules it answers.
//
//nolint:paralleltest // replaces shell.DefaultShell and mm.DefaultMM
func TestScheduleRunsUserSchedulers(t *testing.T) {
	for name, tc := range map[string]struct {
		installed bool
		out       string
		err       error
		fails     bool
		wantErr   error
		want      map[string]string
	}{
		"not installed":   {fails: true, wantErr: ErrUserSchedulerNotFound},
		"fails":           {installed: true, err: errors.New("exit status 1"), fails: true},
		"answers no JSON": {installed: true, out: "scheduled", fails: true},
		"schedules the VMs": {
			installed: true, out: `{"spec":{"schedules":{"vm1":"compute1","vm2":"compute2"}}}`,
			want: map[string]string{"vm1": "compute1", "vm2": "compute2"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			original := mm.DefaultMM
			t.Cleanup(func() { mm.DefaultMM = original }) //nolint:reassign // restore test double

			mm.DefaultMM = clusterMM{hosts: mm.Hosts{{Name: "compute1"}}} //nolint:reassign // install test double

			fake := &schedulerFakeShell{out: []byte(tc.out), err: tc.err}
			if tc.installed {
				fake.commands = []string{"phenix-scheduler-custom"}
			}

			useSchedulerShell(t, fake)

			spec := schedulerSpec(t, nil)
			err := Schedule("custom", spec)

			if (err != nil) != tc.fails || (tc.wantErr != nil && !errors.Is(err, tc.wantErr)) {
				t.Fatalf("Schedule(custom) error = %v, want error %t (%v)", err, tc.fails, tc.wantErr)
			}

			if diff := cmp.Diff(tc.want, spec.Schedules(), cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("Schedule(custom) schedules mismatch (-want +got):\n%s", diff)
			}

			if diff := cmp.Diff([]string{"phenix-scheduler-custom"}, fake.asked); diff != "" {
				t.Errorf("Schedule(custom) looked for commands mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
