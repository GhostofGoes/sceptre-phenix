//go:build e2e

package e2e

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"syscall"
	"testing"
)

// Helpers the scorch-guest and scorch-night shards share: the `phenix
// experiment scorch` process, the status file a component writes, and readers
// of what a run leaves.

// scorchProc is a `phenix experiment scorch` run the test watches while it
// runs and can signal.
type scorchProc struct {
	phenixProc

	cmd *exec.Cmd
}

// terminate sends the CLI SIGTERM, which it handles by canceling the run.
func (p *scorchProc) terminate(t *testing.T) {
	t.Helper()

	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("sending SIGTERM to the SCORCH run: %v", err)
	}
}

// scorchShell runs a script in a guest of experiment exp and returns the
// key=value lines it prints.
func scorchShell(t *testing.T, exp, vm, script string) map[string]string {
	t.Helper()

	kv := make(map[string]string)

	for line := range strings.SplitSeq(guestShell(t, exp, vm, script), "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			kv[k] = v
		}
	}

	return kv
}

// scorchStatus is the status file a Python component writes for each
// stage it runs (ComponentBase.execute_stage).
type scorchStatus struct {
	Experiment string   `json:"experiment_name"`
	Run        int      `json:"scorch_run_index"`
	Component  string   `json:"component"`
	Loop       int      `json:"loop"`
	Count      int      `json:"count"`
	Stage      string   `json:"stage"`
	Return     any      `json:"return"`
	Logs       []string `json:"logs"`
}

// failure returns the error a failed stage returned, or "".
func (s scorchStatus) failure() string {
	if ret := fmt.Sprint(s.Return); strings.HasPrefix(ret, "Error occurred") {
		return ret
	}

	return ""
}

func (s scorchStatus) logged(msg string) bool {
	return slices.ContainsFunc(s.Logs, func(line string) bool { return strings.Contains(line, msg) })
}

// scorchArchive lists the entries of a gzipped tar archive.
func scorchArchive(t *testing.T, path string) []string {
	t.Helper()

	f, err := os.Open(path) //nolint:gosec // product output
	if err != nil {
		t.Errorf("opening %s: %v", path, err)

		return nil
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Errorf("%s is not gzipped: %v", path, err)

		return nil
	}

	var (
		tr    = tar.NewReader(gz)
		names []string
	)

	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return names
		}

		if err != nil {
			t.Errorf("reading %s: %v", path, err)

			return names
		}

		names = append(names, h.Name)
	}
}

func scorchRead(t *testing.T, path string) string {
	t.Helper()

	data, err := os.ReadFile(path) //nolint:gosec // product output
	if err != nil {
		t.Errorf("reading %s: %v", path, err)
	}

	return string(data)
}

func scorchReadJSON(t *testing.T, path string, v any) bool {
	t.Helper()

	data, err := os.ReadFile(path) //nolint:gosec // product output
	if err != nil {
		t.Errorf("reading %s: %v", path, err)

		return false
	}

	if err := json.Unmarshal(data, v); err != nil {
		t.Errorf("%s is not the JSON expected: %v\n%s", path, err, data)

		return false
	}

	return true
}

// scorchJSONLines reads a JSON lines file.
func scorchJSONLines(t *testing.T, path string) []map[string]any {
	t.Helper()

	var rows []map[string]any

	for line := range strings.SplitSeq(scorchRead(t, path), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}

		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Errorf("%s: line %q is not JSON: %v", path, line, err)

			continue
		}

		rows = append(rows, row)
	}

	return rows
}

// scorchWantFile fails unless path is a non-empty file.
func scorchWantFile(t *testing.T, path string) {
	t.Helper()

	fi, err := os.Stat(path)

	switch {
	case err != nil:
		t.Errorf("%v", err)
	case fi.IsDir() || fi.Size() == 0:
		t.Errorf("%s is empty or not a file", path)
	}
}
