package app

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/golang/mock/gomock"

	"phenix/store"
	"phenix/types"
	"phenix/util/mm"
	"phenix/util/tap"
)

// TestTapCleanupNoopWhenStatusMissing reproduces the bug where stopping an
// experiment before a post-start tap app has run (e.g. because the
// experiment was stopped while waiting on delayed VMs) causes Cleanup to
// fail with "missing status for app tap", even though the app never
// created any taps that need to be cleaned up.
func TestTapCleanupNoopWhenStatusMissing(t *testing.T) {
	exp := types.NewExperiment(store.ConfigMetadata{Name: "tap-cleanup-test"})
	exp.Spec.SetExperimentName("tap-cleanup-test")

	var tapApp Tap

	if err := tapApp.Cleanup(context.Background(), exp); err != nil {
		t.Fatalf("expected no error cleaning up tap app with no recorded status, got: %v", err)
	}
}

// TestTapCleanupDeletesRecordedTaps verifies Cleanup still parses and
// processes status when the tap app previously recorded taps it created.
func TestTapCleanupDeletesRecordedTaps(t *testing.T) {
	exp := types.NewExperiment(store.ConfigMetadata{Name: "tap-cleanup-test"})
	exp.Spec.SetExperimentName("tap-cleanup-test")

	// No taps recorded, but the status entry itself exists -- this should
	// still be treated as a legitimate (empty) status rather than "missing".
	exp.Status.SetAppStatus("tap", TapAppStatus{Host: "compute-0"})

	var tapApp Tap

	if err := tapApp.Cleanup(context.Background(), exp); err != nil {
		t.Fatalf("expected no error cleaning up tap app with empty taps, got: %v", err)
	}
}

// tapAppTestMM is a test double for mm.MM that records the host queries and
// the taps the tap app creates and deletes.
type tapAppTestMM struct {
	mm.MM

	hosts mm.Hosts

	// failVLAN makes creating the tap on this VLAN alias fail.
	failVLAN string

	// exists, when set, tracks the taps on the hosts: deleting one that does
	// not exist fails, as minimega's `tap delete` does. failDelete names taps
	// whose next delete fails.
	exists     map[string]bool
	failDelete map[string]bool

	// failDeleteFirst makes the first delete of the first tap created fail.
	failDeleteFirst bool

	calls []string
}

func (m *tapAppTestMM) GetNamespaceHosts(string) (mm.Hosts, error) {
	m.calls = append(m.calls, "hosts")

	return m.hosts, nil
}

// TapVLAN records "create <tap> on <host>" or "delete <tap> on <host>". mm keeps
// the tap options unexported, so they are read by reflection.
func (m *tapAppTestMM) TapVLAN(opts ...mm.TapOption) error {
	o := reflect.ValueOf(mm.NewTapOptions(opts...))
	name, host := o.FieldByName("name").String(), o.FieldByName("host").String()

	if o.FieldByName("untap").Bool() {
		m.calls = append(m.calls, "delete "+name+" on "+host)

		if m.failDelete[name] {
			delete(m.failDelete, name)

			return errors.New("tap delete failed")
		}

		if created := m.created(); m.failDeleteFirst && len(created) > 0 && created[0] == name {
			m.failDeleteFirst = false

			return errors.New("tap delete failed")
		}

		if m.exists != nil {
			if !m.exists[name] {
				return errors.New("tap not found: " + name)
			}

			delete(m.exists, name)
		}

		return nil
	}

	m.calls = append(m.calls, "create "+name+" on "+host)

	if m.exists != nil {
		m.exists[name] = true
	}

	if m.failVLAN != "" && strings.HasSuffix(o.FieldByName("vlan").String(), "//"+m.failVLAN) {
		return errors.New("tap create failed")
	}

	return nil
}

func (m *tapAppTestMM) MeshShell(string, string) error {
	return nil
}

// created returns the names of the taps created, in order.
func (m *tapAppTestMM) created() []string {
	var names []string

	for _, c := range m.calls {
		if name, ok := strings.CutPrefix(c, "create "); ok {
			names = append(names, strings.Fields(name)[0])
		}
	}

	return names
}

// installTapAppTest installs fake as mm.DefaultMM and a store mock that lists
// no other experiments.
func installTapAppTest(t *testing.T, fake *tapAppTestMM) {
	t.Helper()

	ctrl := gomock.NewController(t)

	m := store.NewMockStore(ctrl)
	m.EXPECT().List(gomock.Any()).Return(store.Configs{}, nil).AnyTimes()

	originalMM, originalStore := mm.DefaultMM, store.DefaultStore
	t.Cleanup(func() {
		mm.DefaultMM = originalMM          //nolint:reassign // restore test double
		store.DefaultStore = originalStore //nolint:reassign // restore test double
	})

	mm.DefaultMM = fake    //nolint:reassign // install test double
	store.DefaultStore = m //nolint:reassign // install test double
}

// tapAppExperiment builds an experiment whose scenario has the tap app with
// the given taps.
func tapAppExperiment(t *testing.T, taps ...map[string]any) *types.Experiment {
	t.Helper()

	exp, err := types.DecodeExperimentFromConfig(store.Config{
		Version:  "phenix.sandia.gov/v1",
		Kind:     "Experiment",
		Metadata: store.ConfigMetadata{Name: "tap-test"},
		Spec: map[string]any{
			"experimentName": "tap-test",
			"scenario": map[string]any{
				"apps": []map[string]any{{"name": "tap", "metadata": map[string]any{"taps": taps}}},
			},
		},
	})
	if err != nil {
		t.Fatalf("decoding experiment: %v", err)
	}

	return exp
}

// TestTapAppRefusesInvalidTapsBeforeCreatingAny checks that a configuration
// with an invalid later tap is refused before any tap is created, instead of
// after the earlier taps and their namespaces exist with no status to clean
// them up.
func TestTapAppRefusesInvalidTapsBeforeCreatingAny(t *testing.T) {
	for _, tc := range []struct {
		name string
		taps []map[string]any
		want string
	}{
		{
			name: "two taps on one VLAN",
			taps: []map[string]any{
				{"vlan": "LAN", "ip": "10.10.0.201/24"},
				{"vlan": "LAN", "ip": "10.10.0.202/24"},
			},
			want: "tap already created for VLAN LAN",
		},
		{
			name: "external access without a mask",
			taps: []map[string]any{
				{"vlan": "LAN", "ip": "10.10.0.201/24"},
				{"vlan": "WAN", "ip": "10.20.0.201", "externalAccess": map[string]any{"enabled": true}},
			},
			want: `"10.20.0.201"`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &tapAppTestMM{hosts: mm.Hosts{{Name: "compute-0"}}}
			installTapAppTest(t, fake)

			exp := tapAppExperiment(t, tc.taps...)

			var tapApp Tap

			err := tapApp.PostStart(context.Background(), exp)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("PostStart error = %v, want one containing %s", err, tc.want)
			}

			if len(fake.calls) != 0 {
				t.Errorf("PostStart sent %v to minimega, want nothing", fake.calls)
			}

			if _, ok := exp.Status.AppStatus()["tap"]; ok {
				t.Errorf("PostStart recorded status %v for a refused configuration", exp.Status.AppStatus()["tap"])
			}
		})
	}
}

// TestTapAppDeletesCreatedTapsWhenALaterTapFails checks that the taps created
// before a later tap fails are deleted again, since a failed stage records no
// status for Cleanup.
func TestTapAppDeletesCreatedTapsWhenALaterTapFails(t *testing.T) {
	fake := &tapAppTestMM{hosts: mm.Hosts{{Name: "compute-0"}}, failVLAN: "WAN"}
	installTapAppTest(t, fake)

	exp := tapAppExperiment(t,
		map[string]any{"vlan": "LAN", "ip": "10.10.0.201/24"},
		map[string]any{"vlan": "WAN", "ip": "10.20.0.201/24"},
	)

	var tapApp Tap

	err := tapApp.PostStart(context.Background(), exp)
	if err == nil || !strings.Contains(err.Error(), "creating host tap for VLAN WAN") {
		t.Fatalf("PostStart error = %v, want one for VLAN WAN", err)
	}

	created := fake.created()
	if len(created) != 2 {
		t.Fatalf("PostStart created %v, want the LAN and WAN taps", created)
	}

	if !slices.Contains(fake.calls, "delete "+created[0]+" on compute-0") {
		t.Errorf("PostStart left the LAN tap %s behind: %v", created[0], fake.calls)
	}

	if _, ok := exp.Status.AppStatus()["tap"]; ok {
		t.Errorf("PostStart recorded status %v for a failed stage", exp.Status.AppStatus()["tap"])
	}
}

// TestTapAppFailsWithoutNamespaceHosts checks that an empty host list, which
// minimega gives when it cannot be queried, fails the stage with an error
// instead of panicking while picking a host.
func TestTapAppFailsWithoutNamespaceHosts(t *testing.T) {
	fake := new(tapAppTestMM)
	installTapAppTest(t, fake)

	exp := tapAppExperiment(t, map[string]any{"vlan": "LAN", "ip": "10.10.0.201/24"})

	var tapApp Tap

	err := tapApp.PostStart(context.Background(), exp)
	if err == nil || !strings.Contains(err.Error(), "no minimega hosts found for experiment tap-test") {
		t.Fatalf("PostStart error = %v, want one saying experiment tap-test has no minimega hosts", err)
	}

	if created := fake.created(); len(created) != 0 {
		t.Errorf("PostStart created %v without a host", created)
	}
}

// TestTapAppRetriggerReplacesRecordedTaps checks that running the stage again,
// as phenix experiment trigger does, deletes the taps the status records
// before creating new ones, so the replaced status does not lose them.
func TestTapAppRetriggerReplacesRecordedTaps(t *testing.T) {
	fake := &tapAppTestMM{hosts: mm.Hosts{{Name: "compute-0"}}}
	installTapAppTest(t, fake)

	exp := tapAppExperiment(t, map[string]any{"vlan": "LAN", "ip": "10.10.0.201/24"})
	exp.Status.SetAppStatus("tap", TapAppStatus{
		Host: "compute-1",
		Taps: []*tap.Tap{{VLAN: "LAN", IP: "10.10.0.201/24", Name: "old00000-tapapp"}},
	})

	var tapApp Tap

	if err := tapApp.PostStart(context.Background(), exp); err != nil {
		t.Fatalf("PostStart: %v", err)
	}

	if len(fake.calls) == 0 || fake.calls[0] != "delete old00000-tapapp on compute-1" {
		t.Errorf("PostStart sent %v, want the recorded tap deleted from compute-1 first", fake.calls)
	}

	created := fake.created()
	if len(created) != 1 {
		t.Fatalf("PostStart created %v, want one tap", created)
	}

	var status TapAppStatus
	if err := exp.Status.ParseAppStatus("tap", &status); err != nil {
		t.Fatalf("parsing tap app status: %v", err)
	}

	if status.Host != "compute-0" || len(status.Taps) != 1 || status.Taps[0].Name != created[0] {
		t.Errorf("tap app status = %+v, want only the new tap %s on compute-0", status, created[0])
	}
}

// TestTapAppDryRunLeavesMinimegaAlone checks that a dry run creates no host
// taps and sends nothing to minimega, whose host query alone would create the
// experiment's namespace, while it still refuses an invalid configuration.
func TestTapAppDryRunLeavesMinimegaAlone(t *testing.T) {
	for _, tc := range []struct {
		name string
		taps []map[string]any
		want string
	}{
		{
			name: "valid taps",
			taps: []map[string]any{{"vlan": "LAN", "ip": "10.10.0.201/24", "externalAccess": map[string]any{"enabled": true}}},
		},
		{
			name: "two taps on one VLAN",
			taps: []map[string]any{{"vlan": "LAN", "ip": "10.10.0.201/24"}, {"vlan": "LAN", "ip": "10.10.0.202/24"}},
			want: "tap already created for VLAN LAN",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &tapAppTestMM{hosts: mm.Hosts{{Name: "compute-0"}}}
			installTapAppTest(t, fake)

			exp := tapAppExperiment(t, tc.taps...)

			var tapApp Tap
			if err := tapApp.Init(Name("tap"), DryRun(true)); err != nil {
				t.Fatalf("Init: %v", err)
			}

			err := tapApp.PostStart(context.Background(), exp)

			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("dry-run PostStart: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("dry-run PostStart error = %v, want one containing %q", err, tc.want)
			}

			if len(fake.calls) != 0 {
				t.Errorf("dry-run PostStart sent %v to minimega, want nothing", fake.calls)
			}

			if _, ok := exp.Status.AppStatus()["tap"]; ok {
				t.Errorf("dry-run PostStart recorded status %v for taps it did not create", exp.Status.AppStatus()["tap"])
			}

			if err := tapApp.Cleanup(context.Background(), exp); err != nil || len(fake.calls) != 0 {
				t.Errorf("dry-run Cleanup = %v and sent %v, want nil and nothing", err, fake.calls)
			}
		})
	}
}

// TestTapAppPartialDeleteRecordsOnlyTheTapsLeft is a regression test: when a
// trigger deleted some of the recorded taps and failed on another, the status
// still listed the deleted ones, and since minimega fails to delete a tap
// that is gone, every later trigger and the cleanup failed on them.
func TestTapAppPartialDeleteRecordsOnlyTheTapsLeft(t *testing.T) {
	fake := &tapAppTestMM{
		hosts:      mm.Hosts{{Name: "compute-0"}},
		exists:     map[string]bool{"old00001-tapapp": true, "old00002-tapapp": true},
		failDelete: map[string]bool{"old00002-tapapp": true},
	}
	installTapAppTest(t, fake)

	exp := tapAppExperiment(t, map[string]any{"vlan": "LAN", "ip": "10.10.0.201/24"})
	exp.Status.SetAppStatus("tap", TapAppStatus{
		Host: "compute-1",
		Taps: []*tap.Tap{
			{VLAN: "LAN", IP: "10.10.0.201/24", Name: "old00001-tapapp"},
			{VLAN: "WAN", IP: "10.20.0.201/24", Name: "old00002-tapapp"},
		},
	})

	var tapApp Tap

	err := tapApp.PostStart(context.Background(), exp)
	if err == nil || !strings.Contains(err.Error(), "deleting the host taps created before") {
		t.Fatalf("first trigger error = %v, want the failed delete", err)
	}

	var status TapAppStatus
	if err := exp.Status.ParseAppStatus("tap", &status); err != nil {
		t.Fatalf("parsing tap app status: %v", err)
	}

	if status.Host != "compute-1" || len(status.Taps) != 1 || status.Taps[0].Name != "old00002-tapapp" {
		t.Fatalf("tap app status after the failed delete = %+v, want only old00002-tapapp on compute-1", status)
	}

	if err := tapApp.PostStart(context.Background(), exp); err != nil {
		t.Fatalf("second trigger: %v", err)
	}

	if err := tapApp.Cleanup(context.Background(), exp); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}

	if len(fake.exists) != 0 {
		t.Errorf("taps left on the hosts: %v (minimega calls %v)", fake.exists, fake.calls)
	}
}

// TestTapAppSecondCleanupDeletesNothing is a regression test: a cleanup that
// deleted every recorded tap kept them in the status, so a second cleanup,
// such as a stop after phenix experiment trigger cleanup, failed on the taps
// minimega no longer had and skipped the cleanup of later apps.
func TestTapAppSecondCleanupDeletesNothing(t *testing.T) {
	fake := &tapAppTestMM{
		hosts:  mm.Hosts{{Name: "compute-0"}},
		exists: map[string]bool{"old00001-tapapp": true},
	}
	installTapAppTest(t, fake)

	exp := tapAppExperiment(t, map[string]any{"vlan": "LAN", "ip": "10.10.0.201/24"})
	exp.Status.SetAppStatus("tap", TapAppStatus{
		Host: "compute-0",
		Taps: []*tap.Tap{{VLAN: "LAN", IP: "10.10.0.201/24", Name: "old00001-tapapp"}},
	})

	var tapApp Tap

	for i := range 2 {
		if err := tapApp.Cleanup(context.Background(), exp); err != nil {
			t.Fatalf("cleanup %d: %v (minimega calls %v)", i+1, err, fake.calls)
		}
	}

	if _, ok := exp.Status.AppStatus()["tap"]; ok {
		t.Errorf("tap app status after the cleanup = %v, want none", exp.Status.AppStatus()["tap"])
	}
}

// TestTapAppRecordsTapsAFailedRollbackLeft checks that a failed stage whose
// rollback could not delete a tap records that tap, so a later trigger or
// the cleanup deletes it.
func TestTapAppRecordsTapsAFailedRollbackLeft(t *testing.T) {
	fake := &tapAppTestMM{
		hosts:           mm.Hosts{{Name: "compute-0"}},
		failVLAN:        "WAN",
		failDeleteFirst: true,
		exists:          map[string]bool{},
	}
	installTapAppTest(t, fake)

	exp := tapAppExperiment(t,
		map[string]any{"vlan": "LAN", "ip": "10.10.0.201/24"},
		map[string]any{"vlan": "WAN", "ip": "10.20.0.201/24"},
	)

	var tapApp Tap

	err := tapApp.PostStart(context.Background(), exp)
	if err == nil || !strings.Contains(err.Error(), "creating host tap for VLAN WAN") {
		t.Fatalf("PostStart error = %v, want one for VLAN WAN", err)
	}

	created := fake.created()
	if len(created) != 2 {
		t.Fatalf("PostStart created %v, want the LAN and WAN taps", created)
	}

	var status TapAppStatus
	if err := exp.Status.ParseAppStatus("tap", &status); err != nil {
		t.Fatalf("parsing tap app status: %v", err)
	}

	if status.Host != "compute-0" || len(status.Taps) != 1 || status.Taps[0].Name != created[0] {
		t.Fatalf("tap app status = %+v, want only the LAN tap %s the rollback left", status, created[0])
	}

	if err := tapApp.Cleanup(context.Background(), exp); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}

	if fake.exists[created[0]] {
		t.Errorf("Cleanup left the LAN tap %s behind: %v", created[0], fake.calls)
	}
}
