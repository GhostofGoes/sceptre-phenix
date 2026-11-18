package app

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runGuardedScript runs a script made of the run-once guard and a line that
// reports it ran, with the guard's marker directory and boot ID file in dir.
func runGuardedScript(t *testing.T, dir string) string {
	t.Helper()

	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Fatalf("the run-once guard test needs bash: %v", err)
	}

	script := filepath.Join(dir, "script.sh")
	body := "#!/bin/bash\n" +
		startupC2RunOnceGuard(filepath.Join(dir, "ran"), filepath.Join(dir, "boot_id"), "interfaces") +
		"echo ran\n"

	if err := os.WriteFile(script, []byte(body), 0o600); err != nil {
		t.Fatalf("writing guarded script: %v", err)
	}

	out, err := exec.CommandContext(t.Context(), bash, script).CombinedOutput()
	if err != nil {
		t.Fatalf("running guarded script: %v\n%s", err, out)
	}

	return strings.TrimSpace(string(out))
}

// TestStartupC2RunOnceGuard is a regression test: minimega gives a cc exec to
// a miniccc client each time it connects, so a Linux startup script delivered
// over C2 ran again on every reconnect, and the interfaces script reset the
// guest's links in the middle of the experiment.
func TestStartupC2RunOnceGuard(t *testing.T) {
	dir := t.TempDir()
	bootID := filepath.Join(dir, "boot_id")

	setBoot := func(id string) {
		t.Helper()

		if err := os.WriteFile(bootID, []byte(id+"\n"), 0o600); err != nil {
			t.Fatalf("writing boot ID: %v", err)
		}
	}

	setBoot("boot-1")

	if got := runGuardedScript(t, dir); got != "ran" {
		t.Fatalf("first run in a boot printed %q, want ran", got)
	}

	if got := runGuardedScript(t, dir); got != "" {
		t.Fatalf("second run in the same boot printed %q, want nothing", got)
	}

	setBoot("boot-2")

	if got := runGuardedScript(t, dir); got != "ran" {
		t.Fatalf("first run after a reboot printed %q, want ran", got)
	}

	// Without a boot ID the guard cannot tell boots apart, so it lets the
	// script run, as it did before the guard.
	setBoot("")

	for range 2 {
		if got := runGuardedScript(t, dir); got != "ran" {
			t.Fatalf("run without a boot ID printed %q, want ran", got)
		}
	}
}

func TestStartupStagesLinuxC2ScriptsWithRunOnceGuard(t *testing.T) {
	node := newStartupTestNode(t, "linux1", osLinux, true, 0)
	exp, mmDir := newStartupTestExperiment(t, node, nil)

	runStartupPreStart(t, exp, mmDir)

	for _, kind := range []string{"hostname", "timezone", "interfaces"} {
		name := "linux1-" + kind + ".sh"

		generated, err := os.ReadFile(filepath.Join(exp.Spec.BaseDir(), "startup", name))
		if err != nil {
			t.Fatalf("reading generated %s: %v", name, err)
		}

		staged, err := os.ReadFile(filepath.Join(mmDir, "exp1", name))
		if err != nil {
			t.Fatalf("reading staged %s: %v", name, err)
		}

		want := insertAfterShebang(generated, startupC2RunOnceGuard(startupC2RanDir, linuxBootIDFile, kind))
		if !bytes.Equal(staged, want) {
			t.Errorf("staged %s =\n%s\nwant\n%s", name, staged, want)
		}

		if !bytes.HasPrefix(staged, []byte("#!")) {
			t.Errorf("staged %s no longer starts with its #! line:\n%s", name, staged)
		}

		if !strings.Contains(string(staged), startupC2RanDir+"/"+kind+".boot") {
			t.Errorf("staged %s does not record its own run:\n%s", name, staged)
		}
	}
}

// The Windows startup script keeps its own done flag in the registry.
func TestStartupStagesWindowsC2ScriptUnchanged(t *testing.T) {
	node := newStartupTestNode(t, "win1", osWindows, true, 0)
	exp, mmDir := newStartupTestExperiment(t, node, nil)

	runStartupPreStart(t, exp, mmDir)

	generated, err := os.ReadFile(filepath.Join(exp.Spec.BaseDir(), "startup", "win1-startup.ps1"))
	if err != nil {
		t.Fatalf("reading generated Windows script: %v", err)
	}

	staged, err := os.ReadFile(filepath.Join(mmDir, "exp1", "win1-startup.ps1"))
	if err != nil {
		t.Fatalf("reading staged Windows script: %v", err)
	}

	if !bytes.Equal(staged, generated) {
		t.Errorf("staged Windows script differs from the generated one:\n%s", staged)
	}
}

func TestInsertAfterShebang(t *testing.T) {
	for _, tc := range []struct{ script, want string }{
		{script: "#!/bin/bash\necho a\n", want: "#!/bin/bash\nX\necho a\n"},
		{script: "#!/bin/bash", want: "#!/bin/bash\nX\n"},
		{script: "echo a\n", want: "X\necho a\n"},
		{script: "", want: "X\n"},
	} {
		if got := string(insertAfterShebang([]byte(tc.script), "X\n")); got != tc.want {
			t.Errorf("insertAfterShebang(%q) = %q, want %q", tc.script, got, tc.want)
		}
	}
}
