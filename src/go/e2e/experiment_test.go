//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
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

// adopt registers the cleanup of an experiment the test creates outside
// newExperiment (with REST, a workflow, or a create that is expected to
// fail). Unlike newExperiment's cleanup it tolerates an experiment that was
// never created or that the test already deleted, and it removes <files
// dir>/<name>, which holds the startup scripts phenix stages for C2 delivery
// and the C2 response directory minimega creates for the namespace (and
// leaves behind).
func adopt(t *testing.T, name string) *experiment {
	t.Helper()

	e := &experiment{t: t, name: name}

	t.Cleanup(func() { _ = os.RemoveAll(filepath.Join(cfg.imagesDir, name)) })

	t.Cleanup(func() {
		if _, err := phenixRun("config", "get", "experiment/"+name); err != nil {
			return
		}

		e.teardown()
	})

	return e
}

// with returns the experiment bound to t. A subtest calls the methods that
// fail the test through it: failing the test that created the experiment
// from a subtest stops that test and silently skips its later subtests.
func (e *experiment) with(t *testing.T) *experiment {
	t.Helper()

	return &experiment{t: t, name: e.name}
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

// stop runs `phenix experiment stop` and fails the test on error.
func (e *experiment) stop() string {
	e.t.Helper()

	return phenix(e.t, "experiment", "stop", e.name)
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

// doc returns the experiment config as phenix stores it.
func (e *experiment) doc() expDoc {
	e.t.Helper()

	out, err := phenixStdout("config", "get", "experiment/"+e.name, "--output", "json")
	if err != nil {
		e.t.Fatalf("phenix config get experiment/%s: %v\n%s", e.name, err, out)
	}

	var d expDoc
	if err := json.Unmarshal([]byte(jsonPart(out)), &d); err != nil {
		e.t.Fatalf("decoding experiment %s: %v\n%s", e.name, err, out)
	}

	return d
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

// mmScript returns the minimega script phenix rendered for the experiment.
func (e *experiment) mmScript() string {
	e.t.Helper()

	data, err := os.ReadFile(filepath.Join("/phenix/experiments", e.name, "mm_files", e.name+".mm"))
	if err != nil {
		e.t.Fatalf("reading rendered minimega script: %v", err)
	}

	return string(data)
}

// waitRunning waits until minimega reports every named VM RUNNING.
func (e *experiment) waitRunning(timeout time.Duration, vms ...string) {
	e.t.Helper()

	eventually(e.t, timeout, time.Second, fmt.Sprintf("VMs %v RUNNING", vms), func() (bool, string) {
		info := vmInfo(e.t, e.name)

		for _, vm := range vms {
			if info[vm]["state"] != "RUNNING" {
				return false, fmt.Sprintf("%s is %q", vm, info[vm]["state"])
			}
		}

		return true, ""
	})
}

// node returns a VirtualMachine topology node with one static interface on
// VLAN vlan, booting image.
func node(hostname, image, vlan, address string) string {
	return fmt.Sprintf(`
  - type: VirtualMachine
    general:
      hostname: %s
    hardware:
      vcpus: 1
      memory: 1024
      os_type: linux
      drives:
      - image: %s
    network:
      interfaces:
      - name: eth0
        type: ethernet
        proto: static
        vlan: %s
        address: %s
        mask: 24`, hostname, image, vlan, address)
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
