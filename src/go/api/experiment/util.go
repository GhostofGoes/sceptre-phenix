package experiment

import (
	"fmt"
	"path"
	"strings"

	"github.com/hashicorp/go-multierror"

	"phenix/util/mm"
)

// vmStateError is the state minimega reports for a VM whose launch or start
// failed.
const vmStateError = "ERROR"

// launchErrors returns one error per VM that minimega reports in the ERROR
// state, carrying the reason minimega recorded in the VM's error tag, and one
// per expected VM that minimega does not list at all, or nil when every
// expected VM launched.
func launchErrors(vms mm.VMs, expected []string) error {
	var (
		errs   *multierror.Error
		listed = make(map[string]struct{}, len(vms))
	)

	for _, vm := range vms {
		listed[vm.Name] = struct{}{}

		if vm.State != vmStateError {
			continue
		}

		reason := vm.Tags["error"]
		if reason == "" {
			reason = "minimega recorded no reason"
		}

		errs = multierror.Append(errs, fmt.Errorf("VM %s is in the ERROR state: %s", vm.Name, reason))
	}

	for _, name := range expected {
		if _, ok := listed[name]; !ok {
			errs = multierror.Append(errs, fmt.Errorf("VM %s was not launched by minimega", name))
		}
	}

	return errs.ErrorOrNil()
}

// RequireRunning returns an error wrapping ErrExperimentNotRunning unless the
// named experiment runs in minimega. A stopped experiment's namespace is
// cleared and a dry run never had one, and minimega creates a namespace for
// any command sent to it, so VM operations must not reach minimega for either.
func RequireRunning(name string) error {
	exp, err := Get(name)
	if err != nil {
		return err
	}

	if !exp.Running() {
		return fmt.Errorf("%w: %s", ErrExperimentNotRunning, name)
	}

	if exp.DryRun() {
		return fmt.Errorf("%w: %s is a dry run", ErrExperimentNotRunning, name)
	}

	return nil
}

func ClusterNodes(exp string) ([]string, error) {
	nodeMap := make(map[string]struct{})

	spec, err := Get(exp)
	if err != nil {
		return nil, ErrExperimentNotFound
	}

	if !spec.Running() {
		return nil, ErrExperimentNotRunning
	}

	for _, node := range spec.Status.Schedules() {
		nodeMap[node] = struct{}{}
	}

	var nodes []string

	for node := range nodeMap {
		nodes = append(nodes, node)
	}

	return nodes, nil
}

// captureInProgress reports whether one of the captures is writing the
// experiment file at filePath (relative to the experiment's files directory).
func captureInProgress(captures []mm.Capture, filePath string) bool {
	want := "/files/" + path.Clean(strings.TrimPrefix(filePath, "/"))

	for _, c := range captures {
		if strings.HasSuffix(path.Clean(c.Filepath), want) {
			return true
		}
	}

	return false
}
