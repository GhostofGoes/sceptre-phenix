//go:build e2e

package e2e

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// experiment is one phenix experiment created for a test. Its topology (and
// scenario, when given) are created as configs named after the experiment,
// and everything is stopped and deleted when the test ends.
type experiment struct {
	t    *testing.T
	name string
}

// newExperiment creates the topology (and optional scenario) configs from
// YAML and an experiment named name from them. The YAML may use {{name}},
// which is replaced with the experiment name. createArgs are passed to
// `phenix experiment create`.
func newExperiment(t *testing.T, name, topology, scenario string, createArgs ...string) *experiment {
	t.Helper()

	topology = strings.ReplaceAll(topology, "{{name}}", name)
	phenix(t, "config", "create", writeFile(t, name+"-topology.yaml", topology))

	t.Cleanup(func() { _, _ = phenixRun("config", "delete", "topology/"+name) })

	args := []string{"experiment", "create", name, "--topology", name}

	if scenario != "" {
		scenario = strings.ReplaceAll(scenario, "{{name}}", name)
		phenix(t, "config", "create", writeFile(t, name+"-scenario.yaml", scenario))

		t.Cleanup(func() { _, _ = phenixRun("config", "delete", "scenario/"+name) })

		args = append(args, "--scenario", name)
	}

	phenix(t, append(args, createArgs...)...)

	e := &experiment{t: t, name: name}

	// Registered after the config cleanups, so it runs first.
	t.Cleanup(e.teardown)

	return e
}

// teardown stops the experiment if it is running and deletes it, and records
// what minimega and the host still hold for it when that fails.
func (e *experiment) teardown() {
	e.stopAndDelete()
	e.clearNamespace()
}

// stopAndDelete is the part of teardown before clearNamespace, for a shard
// that fails a test on a namespace the delete leaves.
func (e *experiment) stopAndDelete() {
	if e.running() {
		if out, err := phenixRun("experiment", "stop", e.name); err != nil {
			e.t.Errorf("cleanup: stopping %s: %v\n%s", e.name, err, out)
		}
	}

	if out, err := phenixRun("experiment", "delete", e.name, "--force"); err != nil {
		e.t.Errorf("cleanup: deleting %s: %v\n%s", e.name, err, out)
	}
}

// clearNamespace clears the experiment's minimega namespace when it outlived
// the delete (phenix's delete leaves minimega alone when the experiment is not
// running), logging the VMs it still holds, so that they cannot reach a later
// test. A namespaced command re-creates a cleared namespace empty, so an empty
// one is cleared without a word.
func (e *experiment) clearNamespace() {
	resp, err := mmRun("", "namespace")
	if err != nil {
		e.t.Errorf("cleanup: listing minimega's namespaces: %v", err)

		return
	}

	if !slices.ContainsFunc(mmRows(resp), func(row map[string]string) bool { return row["namespace"] == e.name }) {
		return
	}

	if resp, err := mmRun(e.name, "vm info"); err == nil {
		var vms []string

		for _, row := range mmRows(resp) {
			vms = append(vms, row["name"]+" "+row["state"])
		}

		if len(vms) > 0 {
			e.t.Logf("cleanup: minimega still holds the VMs %v of %s after it was deleted; clearing the namespace", vms, e.name)
		}
	}

	if _, err := mmRun("", "clear namespace "+e.name); err != nil {
		e.t.Errorf("cleanup: clearing minimega namespace %s: %v", e.name, err)
	}
}

// start runs `phenix experiment start` and fails the test on error.
func (e *experiment) start(args ...string) string {
	e.t.Helper()

	return phenix(e.t, append([]string{"experiment", "start", e.name}, args...)...)
}

// expDoc is the part of an experiment config the tests read.
type expDoc struct {
	Spec   map[string]any `json:"spec"`
	Status struct {
		StartTime string            `json:"startTime"`
		Schedules map[string]string `json:"schedules"`
		VLANs     map[string]int    `json:"vlans"`
		Apps      map[string]any    `json:"apps"`
	} `json:"status"`
}

func (e *experiment) running() bool {
	out, err := phenixStdout("config", "get", "experiment/"+e.name, "--output", "json")
	if err != nil {
		return false
	}

	var d expDoc
	if err := json.Unmarshal([]byte(jsonPart(out)), &d); err != nil {
		return false
	}

	return d.Status.StartTime != ""
}

// jsonPart returns the first JSON document in phenix's output. phenix can
// print log lines before it and warnings after it (it re-reads its log file
// on every command), so everything around the document is dropped. phenix
// prints the document on lines of its own, so a '{' that starts a line is
// preferred: a log line can carry a JSON-looking attribute such as {} before
// the document.
func jsonPart(out string) string {
	i := strings.Index(out, "{")
	if i < 0 {
		return out
	}

	if !strings.HasPrefix(out, "{") {
		if j := strings.Index(out, "\n{"); j >= 0 {
			i = j + 1
		}
	}

	var doc json.RawMessage
	if err := json.NewDecoder(strings.NewReader(out[i:])).Decode(&doc); err == nil {
		return string(doc)
	}

	return out[i:]
}

// topology wraps nodes in a Topology config named {{name}}.
func topology(nodes ...string) string {
	return `apiVersion: phenix.sandia.gov/v1
kind: Topology
metadata:
  name: {{name}}
spec:
  nodes:` + strings.Join(nodes, "")
}
