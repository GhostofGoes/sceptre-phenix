package experiment_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"phenix/api/experiment"
)

// TestStartRefusesTwoModesForOneBaseDirSource is a regression test: two
// nodes that inject one source in the experiment's base directory with
// different permissions both set them on that file as the minimega script was
// rendered, so both nodes got the mode set last.
func TestStartRefusesTwoModesForOneBaseDirSource(t *testing.T) {
	rec := new(startRecord)
	registerStartApp(t, rec)

	cfg := startTestConfig(t, false, false)

	baseDir, _ := cfg.Spec["baseDir"].(string)
	if err := os.WriteFile(filepath.Join(baseDir, "f"), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}

	nodes, _ := cfg.Spec["topology"].(map[string]any)["nodes"].([]map[string]any)
	for i, perms := range []string{"0700", "0644"} {
		nodes[i]["general"].(map[string]any)["snapshot"] = true
		nodes[i]["injections"] = []map[string]any{{"src": "f", "dst": "/etc/x", "permissions": perms}}
	}

	events := new(failedStartEvents)
	installStartDoubles(t, failedStartMM{startMM: new(startMM), events: events}, cfg)
	useBoltStore(t, cfg)

	err := experiment.Start(context.Background(), experiment.StartWithName(startTestExp))

	want := `node idle's injection to "/etc/x" and node timer's injection to "/etc/x" give source`
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("Start() = %v, want an error with %q", err, want)
	}

	if rec.cleanups != 1 {
		t.Errorf("cleanup apps ran %d times, want once", rec.cleanups)
	}

	if len(events.events) != 0 {
		t.Errorf("the refused start sent minimega %q", events.events)
	}
}
