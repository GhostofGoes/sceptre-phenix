package app

import (
	"context"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"

	"phenix/store"
	"phenix/types"
	v1 "phenix/types/version/v1"
	v2 "phenix/types/version/v2"
)

func TestStartupPreStartC2TriggerMatrix(t *testing.T) {
	tests := []struct {
		name             string
		annotations      map[string]any
		snapshot         bool
		injectPartition  int
		wantCommands     bool
		wantStartupInj   bool
		wantStagedScript bool
	}{
		{
			name:             "annotation enabled on injectable node",
			annotations:      map[string]any{startupViaCCAnnotation: true},
			snapshot:         true,
			injectPartition:  1,
			wantCommands:     true,
			wantStartupInj:   false,
			wantStagedScript: true,
		},
		{
			name:             "inject partition zero",
			snapshot:         true,
			injectPartition:  0,
			wantCommands:     true,
			wantStartupInj:   true,
			wantStagedScript: true,
		},
		{
			name:            "snapshot false without annotation",
			snapshot:        false,
			injectPartition: 1,
			wantCommands:    false,
			wantStartupInj:  true,
		},
		{
			name:            "normal injectable node",
			snapshot:        true,
			injectPartition: 1,
			wantCommands:    false,
			wantStartupInj:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			node := newStartupTestNode(t, "linux1", osLinux, tt.snapshot, tt.injectPartition)
			node.AnnotationsF = tt.annotations

			exp, mmDir := newStartupTestExperiment(t, node, nil)
			runStartupPreStart(t, exp, mmDir)

			wantCommands := []string{
				"send exp1/linux1-hostname.sh",
				"exec bash /tmp/miniccc/files/exp1/linux1-hostname.sh",
				"send exp1/linux1-timezone.sh",
				"exec bash /tmp/miniccc/files/exp1/linux1-timezone.sh",
				"send exp1/linux1-interfaces.sh",
				"exec bash /tmp/miniccc/files/exp1/linux1-interfaces.sh",
			}

			for _, command := range wantCommands {
				if got := hasCommand(node, command); got != tt.wantCommands {
					t.Fatalf("command %q present = %v, want %v", command, got, tt.wantCommands)
				}
			}

			for _, dst := range []string{linuxHostnameInjectDst, linuxTimezoneInjectDst, linuxIfaceInjectDst} {
				if got := hasInjection(node, dst); got != tt.wantStartupInj {
					t.Fatalf("injection %q present = %v, want %v", dst, got, tt.wantStartupInj)
				}
			}

			staged := filepath.Join(mmDir, "exp1", "linux1-hostname.sh")
			_, err := os.Stat(staged)
			if tt.wantStagedScript && err != nil {
				t.Fatalf("expected staged script %s: %v", staged, err)
			}
			if !tt.wantStagedScript && !os.IsNotExist(err) {
				t.Fatalf("unexpected staged script %s", staged)
			}
		})
	}
}

func TestStartupPreStartWindowsC2Commands(t *testing.T) {
	node := newStartupTestNode(t, "win1", osWindows, true, 0)
	exp, mmDir := newStartupTestExperiment(t, node, nil)

	runStartupPreStart(t, exp, mmDir)

	wantSend := "send exp1/win1-startup.ps1"
	wantExec := "exec cmd /c 'powershell.exe -noprofile -executionpolicy bypass -file /tmp/miniccc/files/exp1/win1-startup.ps1'"

	if !hasCommand(node, wantSend) {
		t.Fatalf("missing command %q", wantSend)
	}
	if !hasCommand(node, wantExec) {
		t.Fatalf("missing command %q", wantExec)
	}
	if !hasInjection(node, windowsStartupInjectDst) {
		t.Fatalf("expected Windows startup script injection to remain for inject partition 0")
	}
	if hasInjection(node, windowsStartupWrapperDst) {
		t.Fatalf("unexpected Windows startup wrapper injection with C2 delivery")
	}
	if hasInjection(node, windowsSchedulerDst) || hasInjection(node, "/"+windowsSchedulerDst) {
		t.Fatalf("unexpected Windows Start Menu scheduler injection with C2 delivery")
	}

	staged := filepath.Join(mmDir, "exp1", "win1-startup.ps1")
	if _, err := os.Stat(staged); err != nil {
		t.Fatalf("expected staged Windows script %s: %v", staged, err)
	}
}

func TestStartupPreStartWindowsWrapperInjections(t *testing.T) {
	tests := []struct {
		name        string
		annotations map[string]any
		wantWrapper bool
	}{
		{
			name:        "disk injection",
			wantWrapper: true,
		},
		{
			name:        "startup via cc suppresses wrapper",
			annotations: map[string]any{startupViaCCAnnotation: true},
			wantWrapper: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			node := newStartupTestNode(t, "win1", osWindows, true, 1)
			node.AnnotationsF = tt.annotations

			exp, mmDir := newStartupTestExperiment(t, node, nil)
			runStartupPreStart(t, exp, mmDir)

			for _, dst := range []string{windowsStartupWrapperDst, windowsSchedulerDst} {
				if got := hasInjection(node, dst); got != tt.wantWrapper {
					t.Fatalf("injection %q present = %v, want %v", dst, got, tt.wantWrapper)
				}
			}

			if !tt.wantWrapper {
				return
			}

			startupDir := filepath.Join(exp.Spec.BaseDir(), "startup")

			for _, asset := range []string{windowsStartupWrapperAsset, windowsSchedulerAsset} {
				if _, err := os.Stat(filepath.Join(startupDir, asset)); err != nil {
					t.Fatalf("expected restored asset %s: %v", asset, err)
				}
			}
		})
	}
}

func TestStartupPreStartAnnotationPreservesUnrelatedInjections(t *testing.T) {
	node := newStartupTestNode(t, "linux1", osLinux, true, 1)
	node.AnnotationsF = map[string]any{startupViaCCAnnotation: true}
	node.InjectionsF = []*v1.Injection{
		{SrcF: "user-file", DstF: "/etc/user-file"},
		{SrcF: "old-hostname", DstF: linuxHostnameInjectDst},
		{SrcF: "old-wrapper", DstF: windowsStartupWrapperDst},
		{SrcF: "old-scheduler", DstF: windowsSchedulerDst},
	}

	exp, mmDir := newStartupTestExperiment(t, node, nil)
	runStartupPreStart(t, exp, mmDir)

	if !hasInjection(node, "/etc/user-file") {
		t.Fatalf("expected unrelated user injection to remain")
	}
	if hasInjection(node, linuxHostnameInjectDst) {
		t.Fatalf("unexpected startup app injection")
	}
	if hasInjection(node, windowsStartupWrapperDst) {
		t.Fatalf("unexpected Windows startup wrapper injection")
	}
	if hasInjection(node, windowsSchedulerDst) {
		t.Fatalf("unexpected scheduler injection")
	}
	if !hasCommand(node, "send exp1/linux1-hostname.sh") {
		t.Fatalf("expected startup C2 send command")
	}
}

func TestStartupPreStartRemovesWindowsWrapperInjectionsForC2Delivery(t *testing.T) {
	node := newStartupTestNode(t, "win1", osWindows, true, 0)
	node.InjectionsF = []*v1.Injection{
		{SrcF: "user-file", DstF: "/etc/user-file"},
		{SrcF: "old-wrapper", DstF: windowsStartupWrapperDst},
		{SrcF: "old-scheduler", DstF: windowsSchedulerDst},
		{SrcF: "old-scheduler-abs", DstF: "/" + windowsSchedulerDst},
	}

	exp, mmDir := newStartupTestExperiment(t, node, nil)
	runStartupPreStart(t, exp, mmDir)

	if !hasInjection(node, "/etc/user-file") {
		t.Fatalf("expected unrelated user injection to remain")
	}
	if !hasInjection(node, windowsStartupInjectDst) {
		t.Fatalf("expected current Windows startup injection to remain")
	}
	if hasInjection(node, windowsStartupWrapperDst) {
		t.Fatalf("unexpected Windows startup wrapper injection with C2 delivery")
	}
	if hasInjection(node, windowsSchedulerDst) || hasInjection(node, "/"+windowsSchedulerDst) {
		t.Fatalf("unexpected Windows Start Menu scheduler injection with C2 delivery")
	}
	if !hasCommand(node, "send exp1/win1-startup.ps1") {
		t.Fatalf("expected startup C2 command")
	}
}

func TestStartupPreStartC2CommandsAreIdempotent(t *testing.T) {
	node := newStartupTestNode(t, "linux1", osLinux, true, 0)
	exp, mmDir := newStartupTestExperiment(t, node, nil)

	runStartupPreStart(t, exp, mmDir)
	first := append([]string(nil), node.CommandsF...)

	runStartupPreStart(t, exp, mmDir)
	second := append([]string(nil), node.CommandsF...)

	if !reflect.DeepEqual(first, second) {
		t.Fatalf("commands changed after second PreStart:\nfirst:  %#v\nsecond: %#v", first, second)
	}
	if len(second) != 6 {
		t.Fatalf("expected 6 startup C2 commands, got %d: %#v", len(second), second)
	}
}

func TestStartupPreStartRemovesStaleC2Commands(t *testing.T) {
	stale := []string{
		"send exp1/linux1-hostname.sh",
		"exec bash /tmp/miniccc/files/exp1/linux1-hostname.sh",
		"exec-once bash /tmp/miniccc/files/exp1/linux1-hostname.sh",
		"send exp1/linux1-startup.ps1",
		"exec cmd /c 'powershell.exe -noprofile -executionpolicy bypass -file /tmp/miniccc/files/exp1/linux1-startup.ps1'",
		"exec-once cmd /c 'powershell.exe -noprofile -executionpolicy bypass -file /tmp/miniccc/files/exp1/linux1-startup.ps1'",
	}

	node := newStartupTestNode(t, "linux1", osLinux, true, 1)
	node.CommandsF = append(slices.Clone(stale), "exec df -h")

	exp, mmDir := newStartupTestExperiment(t, node, nil)
	runStartupPreStart(t, exp, mmDir)

	for _, command := range stale {
		if hasCommand(node, command) {
			t.Errorf("unexpected stale startup C2 command %q", command)
		}
	}
	if !hasCommand(node, "exec df -h") {
		t.Fatalf("expected user command to remain")
	}
}

// TestStartupPreStartRemovesC2CommandsOfAPreviousHostname is a regression
// test: only the current hostname's startup commands were replaced, so a
// renamed node kept its old hostname's, and with exec those ran first and
// renamed the guest to its old hostname.
func TestStartupPreStartRemovesC2CommandsOfAPreviousHostname(t *testing.T) {
	user := []string{
		"exec df -h",
		"send exp1/notes-hostname.txt",
		"send other/web1-hostname.sh",
		"exec bash /tmp/miniccc/files/exp1/-hostname.sh",
		"exec bash /tmp/miniccc/files/exp1/web1-hostname.sh --check",
	}

	node := newStartupTestNode(t, "web2", osLinux, true, 0)
	node.CommandsF = append([]string{
		"send exp1/web1-hostname.sh",
		"exec bash /tmp/miniccc/files/exp1/web1-hostname.sh",
		"send exp1/web1-interfaces.sh",
		"exec-once bash /tmp/miniccc/files/exp1/web1-interfaces.sh",
		"send exp1/web1-startup.ps1",
		"exec cmd /c 'powershell.exe -noprofile -executionpolicy bypass -file /tmp/miniccc/files/exp1/web1-startup.ps1'",
	}, user...)

	exp, mmDir := newStartupTestExperiment(t, node, nil)
	runStartupPreStart(t, exp, mmDir)

	want := append(slices.Clone(user),
		"send exp1/web2-hostname.sh",
		"exec bash /tmp/miniccc/files/exp1/web2-hostname.sh",
		"send exp1/web2-timezone.sh",
		"exec bash /tmp/miniccc/files/exp1/web2-timezone.sh",
		"send exp1/web2-interfaces.sh",
		"exec bash /tmp/miniccc/files/exp1/web2-interfaces.sh",
	)

	if !slices.Equal(node.CommandsF, want) {
		t.Fatalf("commands =\n%q\nwant\n%q", node.CommandsF, want)
	}
}

// TestStartupPreStartReplacesExecOnceC2Commands is a regression test: the
// startup app queued its C2 scripts with exec-once, which minimega marks sent
// when phenix reads the cc script at start, before any guest connects, so no
// guest ever ran them. They are queued with exec, and the exec-once commands
// an experiment stored before are replaced, not kept beside them.
func TestStartupPreStartReplacesExecOnceC2Commands(t *testing.T) {
	node := newStartupTestNode(t, "linux1", osLinux, true, 0)
	node.CommandsF = []string{
		"send exp1/linux1-hostname.sh",
		"exec-once bash /tmp/miniccc/files/exp1/linux1-hostname.sh",
		"exec df -h",
	}

	exp, mmDir := newStartupTestExperiment(t, node, nil)
	runStartupPreStart(t, exp, mmDir)

	want := []string{
		"exec df -h",
		"send exp1/linux1-hostname.sh",
		"exec bash /tmp/miniccc/files/exp1/linux1-hostname.sh",
		"send exp1/linux1-timezone.sh",
		"exec bash /tmp/miniccc/files/exp1/linux1-timezone.sh",
		"send exp1/linux1-interfaces.sh",
		"exec bash /tmp/miniccc/files/exp1/linux1-interfaces.sh",
	}

	if !slices.Equal(node.CommandsF, want) {
		t.Fatalf("commands = %#v, want %#v", node.CommandsF, want)
	}
}

func TestStartupViaCCAnnotationValues(t *testing.T) {
	tests := []struct {
		name string
		val  any
		want bool
	}{
		{name: "bool true", val: true, want: true},
		{name: "bool false", val: false, want: false},
		{name: "string true", val: "true", want: true},
		{name: "string false", val: "false", want: false},
		{name: "string zero", val: "0", want: false},
		{name: "int zero", val: 0, want: false},
		{name: "int one", val: 1, want: true},
		// A number read back from the store is a float64.
		{name: "float64 zero", val: float64(0), want: false},
		{name: "float64 one", val: float64(1), want: true},
		{name: "int64 zero", val: int64(0), want: false},
		{name: "uint64 zero", val: uint64(0), want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			node := newStartupTestNode(t, "linux1", osLinux, true, 1)
			node.AnnotationsF = map[string]any{startupViaCCAnnotation: tt.val}

			if got := startupViaCCEnabled(node); got != tt.want {
				t.Fatalf("startupViaCCEnabled = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestStartupPreStartLinuxDomainScriptUsesC2(t *testing.T) {
	node := newStartupTestNode(t, "linux1", osLinux, true, 0)
	scenario := &v2.ScenarioSpec{AppsF: []*v2.ScenarioApp{
		{
			NameF: "startup",
			HostsF: []*v2.ScenarioAppHost{
				{
					HostnameF: "linux1",
					MetadataF: map[string]any{
						"domain_controller": map[string]any{
							"username": "admin",
							"password": "password",
							"domain":   "example.test",
						},
					},
				},
			},
		},
	}}

	exp, mmDir := newStartupTestExperiment(t, node, scenario)
	runStartupPreStart(t, exp, mmDir)

	if !hasCommand(node, "send exp1/linux1-domain.sh") {
		t.Fatalf("expected domain script send command")
	}
	if !hasCommand(node, "exec bash /tmp/miniccc/files/exp1/linux1-domain.sh") {
		t.Fatalf("expected domain script exec command")
	}
	if _, err := os.Stat(filepath.Join(mmDir, "exp1", "linux1-domain.sh")); err != nil {
		t.Fatalf("expected staged domain script: %v", err)
	}
}

// newStartupTestNode builds a minimal VM node with a real test disk path.
func newStartupTestNode(t *testing.T, hostname, osType string, snapshot bool, injectPartition int) *v1.Node {
	t.Helper()

	image := filepath.Join(t.TempDir(), hostname+".qc2")
	if err := os.WriteFile(image, []byte("disk"), 0o600); err != nil {
		t.Fatalf("creating test image: %v", err)
	}

	return &v1.Node{
		TypeF: "VirtualMachine",
		GeneralF: &v1.General{
			HostnameF:  hostname,
			SnapshotF:  &snapshot,
			DoNotBootF: boolPtr(false),
		},
		HardwareF: &v1.Hardware{
			OSTypeF: osType,
			DrivesF: []*v1.Drive{
				{
					ImageF:           image,
					InjectPartitionF: &injectPartition,
				},
			},
		},
		NetworkF: &v1.Network{},
	}
}

// newStartupTestExperiment builds a minimal experiment and redirects minimega file staging.
func newStartupTestExperiment(t *testing.T, node *v1.Node, scenario *v2.ScenarioSpec) (*types.Experiment, string) {
	t.Helper()

	root := t.TempDir()
	mmDir := filepath.Join(root, "images")

	oldStartupMMFullPath := startupMMFullPath
	startupMMFullPath = func(rel string) string {
		return filepath.Join(mmDir, filepath.FromSlash(rel))
	}

	t.Cleanup(func() {
		startupMMFullPath = oldStartupMMFullPath
	})

	spec := &v1.ExperimentSpec{
		ExperimentNameF: "exp1",
		BaseDirF:        filepath.Join(root, "experiments", "exp1"),
		TopologyF:       &v1.TopologySpec{NodesF: []*v1.Node{node}},
		ScenarioF:       scenario,
	}
	if err := spec.Init(); err != nil {
		t.Fatalf("initializing test experiment spec: %v", err)
	}

	status := &v1.ExperimentStatus{}
	if err := status.Init(); err != nil {
		t.Fatalf("initializing test experiment status: %v", err)
	}

	return &types.Experiment{
		Metadata: store.ConfigMetadata{Name: "exp1"},
		Spec:     spec,
		Status:   status,
	}, mmDir
}

// runStartupPreStart creates the staged file directory and invokes startup PreStart.
func runStartupPreStart(t *testing.T, exp *types.Experiment, mmDir string) {
	t.Helper()

	if err := os.MkdirAll(mmDir, 0o750); err != nil {
		t.Fatalf("creating minimega file directory: %v", err)
	}

	if err := (Startup{}).PreStart(context.Background(), exp); err != nil {
		t.Fatalf("running startup PreStart: %v", err)
	}
}

// hasCommand reports whether the node contains command.
func hasCommand(node *v1.Node, command string) bool {
	return slices.Contains(node.CommandsF, command)
}

// hasInjection reports whether the node contains an injection targeting dst.
func hasInjection(node *v1.Node, dst string) bool {
	for _, injection := range node.InjectionsF {
		if injection.DstF == dst {
			return true
		}
	}

	return false
}

// boolPtr returns a pointer to v.
func boolPtr(v bool) *bool {
	return &v
}

// TestStartupDoNotBootForMissingImageIsNotSticky is a regression test: the
// startup app marked a node do-not-boot when its image was missing and nothing
// ever cleared the flag, so the node stayed down after the image appeared. It
// also looked for relative images under <phenix base>/images instead of
// minimega's files directory.
func TestStartupDoNotBootForMissingImageIsNotSticky(t *testing.T) {
	node := newStartupTestNode(t, "linux1", "linux", true, 1)
	node.HardwareF.DrivesF[0].ImageF = "late.qc2" // relative: minimega's files directory

	exp, mmDir := newStartupTestExperiment(t, node, nil)

	runStartupPreStart(t, exp, mmDir)

	if !*node.General().DoNotBoot() {
		t.Fatal("node with a missing image was not marked do-not-boot")
	}

	if err := os.WriteFile(filepath.Join(mmDir, "late.qc2"), []byte("disk"), 0o600); err != nil {
		t.Fatal(err)
	}

	runStartupPreStart(t, exp, mmDir)

	if *node.General().DoNotBoot() {
		t.Fatal("node is still do-not-boot after its image appeared in minimega's files directory")
	}

	if _, ok := node.GetAnnotation(DoNotBootReasonAnnotation); ok {
		t.Error("do-not-boot reason annotation was not removed")
	}
}

// TestStartupRefusesAnImageItCannotCheck is a regression test: the startup
// app treated only a missing file as a missing image, so an image path
// through a regular file, or with a file name that is too long, left the node
// bootable for minimega to fail its launch.
func TestStartupRefusesAnImageItCannotCheck(t *testing.T) {
	t.Run("path through a file", func(t *testing.T) {
		node := newStartupTestNode(t, "linux1", "linux", true, 1)
		exp, mmDir := newStartupTestExperiment(t, node, nil)

		if err := os.MkdirAll(mmDir, 0o750); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(filepath.Join(mmDir, "base.qc2"), []byte("disk"), 0o600); err != nil {
			t.Fatal(err)
		}

		node.HardwareF.DrivesF[0].ImageF = "base.qc2/x.qc2"

		runStartupPreStart(t, exp, mmDir)

		if !*node.General().DoNotBoot() {
			t.Fatal("node whose image path runs through a file was not marked do-not-boot")
		}
	})

	t.Run("file name too long", func(t *testing.T) {
		node := newStartupTestNode(t, "linux1", "linux", true, 1)
		node.HardwareF.DrivesF[0].ImageF = strings.Repeat("x", 300) + ".qc2"

		exp, mmDir := newStartupTestExperiment(t, node, nil)

		if err := os.MkdirAll(mmDir, 0o750); err != nil {
			t.Fatal(err)
		}

		err := (Startup{}).PreStart(context.Background(), exp)
		if err == nil || !strings.Contains(err.Error(), `node "linux1": checking disk image`) {
			t.Fatalf("PreStart() = %v, want the image refused", err)
		}
	})
}

// TestStartupKeepsNodesWhoseImageItCannotCheckButNeedNot is a regression
// test: any error checking a disk image failed the start, also for a node
// that does not boot, and for a path phenix may not read but minimega, which
// may run as another user, can.
func TestStartupKeepsNodesWhoseImageItCannotCheckButNeedNot(t *testing.T) {
	stat := func(err error) func(string) (fs.FileInfo, error) {
		return func(name string) (fs.FileInfo, error) {
			return nil, &fs.PathError{Op: "stat", Path: name, Err: err}
		}
	}

	t.Run("denied path", func(t *testing.T) {
		original := statImage
		statImage = stat(syscall.EACCES)

		t.Cleanup(func() { statImage = original })

		node := newStartupTestNode(t, "linux1", "linux", true, 1)
		exp, mmDir := newStartupTestExperiment(t, node, nil)

		runStartupPreStart(t, exp, mmDir)

		if dnb := node.General().DoNotBoot(); dnb != nil && *dnb {
			t.Fatal("node whose image phenix may not read was marked do-not-boot")
		}
	})

	t.Run("do-not-boot node", func(t *testing.T) {
		node := newStartupTestNode(t, "linux1", "linux", true, 1)
		node.GeneralF.DoNotBootF = boolPtr(true)
		node.HardwareF.DrivesF[0].ImageF = strings.Repeat("x", 300) + ".qc2"

		exp, mmDir := newStartupTestExperiment(t, node, nil)

		runStartupPreStart(t, exp, mmDir)

		if !*node.General().DoNotBoot() {
			t.Fatal("the startup app cleared the do-not-boot of a node whose image it cannot check")
		}
	})
}

func TestStartupKeepsAUsersDoNotBoot(t *testing.T) {
	node := newStartupTestNode(t, "linux1", "linux", true, 1)
	node.GeneralF.DoNotBootF = boolPtr(true)

	exp, mmDir := newStartupTestExperiment(t, node, nil)

	runStartupPreStart(t, exp, mmDir)

	if !*node.General().DoNotBoot() {
		t.Fatal("the startup app cleared a do-not-boot the user set")
	}
}

// TestStagedStartupScriptsMatchEveryStagedScript checks that the globs
// experiment delete removes cover every script PreStart stages for C2
// delivery, so none is left in minimega's files directory.
func TestStagedStartupScriptsMatchEveryStagedScript(t *testing.T) {
	domain := &v2.ScenarioSpec{AppsF: []*v2.ScenarioApp{
		{
			NameF: "startup",
			HostsF: []*v2.ScenarioAppHost{
				{
					HostnameF: "linux1",
					MetadataF: map[string]any{
						"domain_controller": map[string]any{
							"username": "admin",
							"password": "password",
							"domain":   "example.test",
						},
					},
				},
			},
		},
	}}

	tests := []struct {
		name     string
		node     *v1.Node
		scenario *v2.ScenarioSpec
		want     int
	}{
		{name: "linux with a domain", node: newStartupTestNode(t, "linux1", osLinux, true, 0), scenario: domain, want: 4},
		{name: "windows", node: newStartupTestNode(t, "win1", osWindows, true, 0), want: 1},
	}

	globs, err := StagedStartupScripts("exp1")
	if err != nil {
		t.Fatalf("getting staged startup script globs: %v", err)
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exp, mmDir := newStartupTestExperiment(t, tt.node, tt.scenario)
			runStartupPreStart(t, exp, mmDir)

			entries, err := os.ReadDir(filepath.Join(mmDir, "exp1"))
			if err != nil {
				t.Fatalf("listing staged scripts: %v", err)
			}

			if len(entries) != tt.want {
				t.Fatalf("PreStart staged %d scripts, want %d", len(entries), tt.want)
			}

			for _, entry := range entries {
				staged := path.Join("exp1", entry.Name())

				matched := slices.ContainsFunc(globs, func(glob string) bool {
					ok, _ := path.Match(glob, staged)
					return ok
				})

				if !matched {
					t.Errorf("staged script %s matches none of %v", staged, globs)
				}
			}
		})
	}
}

func TestStagedStartupScriptsRequiresExperimentName(t *testing.T) {
	if globs, err := StagedStartupScripts(""); err == nil {
		t.Fatalf("expected an error for an empty experiment name, got globs %v", globs)
	}
}

// TestStagedStartupScriptsRefusesNamesOutsideTheExperimentDirectory is a
// regression test: the experiment names "." and "..", which experiment names
// may be, made the startup app stage its scripts in minimega's files
// directory or its parent, and experiment delete glob for them there.
func TestStagedStartupScriptsRefusesNamesOutsideTheExperimentDirectory(t *testing.T) {
	for _, name := range []string{".", "..", "a/b", "a*", "a?", "a[b]"} {
		if globs, err := StagedStartupScripts(name); err == nil {
			t.Errorf("StagedStartupScripts(%q) = %v, want an error", name, globs)
		}
	}

	if _, err := StagedStartupScripts("exp.1"); err != nil {
		t.Errorf("StagedStartupScripts(%q): %v", "exp.1", err)
	}

	node := newStartupTestNode(t, "linux1", osLinux, true, 0)
	exp, mmDir := newStartupTestExperiment(t, node, nil)
	exp.Spec.SetExperimentName("..")

	if err := os.MkdirAll(mmDir, 0o750); err != nil {
		t.Fatalf("creating minimega file directory: %v", err)
	}

	if err := (Startup{}).PreStart(context.Background(), exp); err == nil {
		t.Fatal("PreStart staged C2 startup scripts for an experiment named ..")
	}

	if _, err := os.Stat(filepath.Join(filepath.Dir(mmDir), "linux1-hostname.sh")); err == nil {
		t.Fatal("PreStart staged a script outside minimega's files directory")
	}
}

// TestStartupKeepsAUsersDoNotBootWhenImageAppears is a regression test: the
// startup app recorded its missing-image reason on a node the user had
// already marked do-not-boot, so the node booted once its image appeared.
func TestStartupKeepsAUsersDoNotBootWhenImageAppears(t *testing.T) {
	node := newStartupTestNode(t, "linux1", "linux", true, 1)
	node.GeneralF.DoNotBootF = boolPtr(true)
	node.HardwareF.DrivesF[0].ImageF = "late.qc2" // relative: minimega's files directory

	exp, mmDir := newStartupTestExperiment(t, node, nil)

	runStartupPreStart(t, exp, mmDir)

	if _, ok := node.GetAnnotation(DoNotBootReasonAnnotation); ok {
		t.Error("the startup app recorded a reason for a do-not-boot the user set")
	}

	if err := os.WriteFile(filepath.Join(mmDir, "late.qc2"), []byte("disk"), 0o600); err != nil {
		t.Fatal(err)
	}

	runStartupPreStart(t, exp, mmDir)

	if !*node.General().DoNotBoot() {
		t.Fatal("the startup app cleared a do-not-boot the user set once the image appeared")
	}
}
