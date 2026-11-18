package vm

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	v1 "phenix/types/version/v1"
)

// TestRedeployFilesIncludesDeletions is a regression test: a redeploy that
// replicates injections replayed a node's injections but not its deletions,
// so the files the topology deletes came back with the recreated snapshot.
func TestRedeployFilesIncludesDeletions(t *testing.T) {
	node := &v1.Node{
		InjectionsF: []*v1.Injection{{SrcF: "/phenix/motd", DstF: "/etc/motd"}},
		DeletionsF:  []*v1.Deletion{{PathF: "/e2e-base-marker"}, {PathF: "/etc/motd"}},
	}

	injects, deletions, err := redeployFiles(node, "/phenix/experiments/exp")
	if err != nil {
		t.Fatalf("redeployFiles() = %v", err)
	}

	if want := []string{`"/phenix/motd":"/etc/motd"`}; !reflect.DeepEqual(injects, want) {
		t.Errorf("redeploy injections = %v, want %v", injects, want)
	}

	if want := []string{"/e2e-base-marker", "/etc/motd"}; !reflect.DeepEqual(deletions, want) {
		t.Errorf("redeploy deletions = %v, want %v", deletions, want)
	}
}

// TestRedeployFilesRendersInjectionsAsTheStart is a regression test: a
// redeploy that replicates injections sent each injection as a bare src:dst
// pair, so a relative source was not looked up in the experiment's base
// directory, a path with a space split the pair, and the injection's
// permissions were not applied, while the start-time script renders them with
// FileInjects.
func TestRedeployFilesRendersInjectionsAsTheStart(t *testing.T) {
	baseDir := t.TempDir()

	if err := os.Mkdir(filepath.Join(baseDir, "files"), 0o750); err != nil {
		t.Fatal(err)
	}

	src := filepath.Join(baseDir, "files", "motd")

	if err := os.WriteFile(src, []byte("v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	node := &v1.Node{
		InjectionsF: []*v1.Injection{
			{SrcF: "files/motd", DstF: "/etc/motd", PermissionsF: "0644"},
			{SrcF: "/phenix/start.ps1", DstF: "ProgramData/Microsoft/Windows/Start Menu/start.ps1"},
		},
	}

	injects, _, err := redeployFiles(node, baseDir)
	if err != nil {
		t.Fatalf("redeployFiles() = %v", err)
	}

	want := []string{
		`"` + src + `":"/etc/motd"`,
		`"/phenix/start.ps1":"ProgramData/Microsoft/Windows/Start Menu/start.ps1"`,
	}

	if !reflect.DeepEqual(injects, want) {
		t.Errorf("redeploy injections = %q, want %q", injects, want)
	}

	if start, err := node.FileInjects(baseDir); err != nil || start != want[0]+" "+want[1] {
		t.Errorf("start-time injections = %q (%v), want the redeploy's %q", start, err, want)
	}

	fi, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}

	if mode := fi.Mode().Perm(); mode != 0o644 {
		t.Errorf("injection source %s has mode %o, want the injection's 644", src, mode)
	}
}

// TestRedeployFilesRefusesInjectionWithoutSource checks that a redeploy that
// replicates injections refuses an injection without a source, as the
// start-time script does, instead of injecting the experiment's whole base
// directory.
func TestRedeployFilesRefusesInjectionWithoutSource(t *testing.T) {
	node := &v1.Node{
		GeneralF:    &v1.General{HostnameF: "rtu-1"},
		InjectionsF: []*v1.Injection{{DstF: "/etc/ot-sim/ca.pem"}},
	}

	injects, _, err := redeployFiles(node, "/phenix/experiments/exp")

	want := `node rtu-1: injection to "/etc/ot-sim/ca.pem" has no source`
	if err == nil || err.Error() != want {
		t.Errorf("redeployFiles() = %q, %v, want the error %q", injects, err, want)
	}
}

// TestRedeployFilesRefusesDeletionsMinimegaSplitsOrExpands checks that a
// redeploy that replicates injections refuses, as the start-time script does,
// a deletion path minimega's `disk inject ... delete files` would split at a
// comma or expand at a '$', before RedeployVM kills the VM: /data/a,/etc/x
// would delete /data/a and /etc/x, and /Windows/$Temp would delete /Windows/.
func TestRedeployFilesRefusesDeletionsMinimegaSplitsOrExpands(t *testing.T) {
	for path, want := range map[string]string{
		"/data/a,/etc/x": `node rtu-1: deletion path "/data/a,/etc/x" contains ','`,
		"/Windows/$Temp": `node rtu-1: deletion path "/Windows/$Temp" contains '$'`,
	} {
		node := &v1.Node{
			GeneralF:   &v1.General{HostnameF: "rtu-1"},
			DeletionsF: []*v1.Deletion{{PathF: path}},
		}

		if _, deletions, err := redeployFiles(node, "/phenix/experiments/exp"); err == nil ||
			!strings.HasPrefix(err.Error(), want) {
			t.Errorf("redeployFiles() of the deletion %s = %q, %v, want an error starting %q", path, deletions, err, want)
		}
	}
}
