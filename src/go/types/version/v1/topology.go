package v1

import (
	"fmt"
	"slices"
	"strings"

	"github.com/hashicorp/go-multierror"

	ifaces "phenix/types/interfaces"
)

type TopologySpec struct {
	IncludeTopologiesF []string `json:"includeTopologies" mapstructure:"includeTopologies" structs:"includeTopologies" yaml:"includeTopologies"`
	NodesF             []*Node  `json:"nodes"             mapstructure:"nodes"             structs:"nodes"             yaml:"nodes"`
}

func (t *TopologySpec) IncludedTopologies() []string {
	if t == nil {
		return nil
	}

	return t.IncludeTopologiesF
}

func (t *TopologySpec) Nodes() []ifaces.NodeSpec {
	if t == nil {
		return nil
	}

	nodes := make([]ifaces.NodeSpec, len(t.NodesF))

	for i, n := range t.NodesF {
		nodes[i] = n
	}

	return nodes
}

func (t *TopologySpec) BootableNodes() []ifaces.NodeSpec {
	if t == nil {
		return nil
	}

	var bootable []ifaces.NodeSpec

	for _, n := range t.NodesF {
		var dnb bool

		if n.GeneralF.DoNotBootF != nil {
			dnb = *n.GeneralF.DoNotBootF
		}

		if dnb {
			continue
		}

		bootable = append(bootable, n)
	}

	return bootable
}

func (t *TopologySpec) SchedulableNodes(platform string) []ifaces.NodeSpec {
	if t == nil {
		return nil
	}

	var schedulable []ifaces.NodeSpec

	for _, n := range t.NodesF {
		if !n.External() {
			schedulable = append(schedulable, n)
		}
	}

	return schedulable
}

func (t TopologySpec) FindNodeByName(name string) ifaces.NodeSpec { //nolint:ireturn // interface
	for _, node := range t.NodesF {
		if node.GeneralF.HostnameF == name {
			return node
		}
	}

	return nil
}

// FindNodesWithLabels finds all nodes in the topology containing at least one
// of the labels provided. Take note that the node does not have to have all the
// labels provided, just one.
func (t TopologySpec) FindNodesWithLabels(labels ...string) []ifaces.NodeSpec {
	var nodes []ifaces.NodeSpec

	for _, n := range t.NodesF {
		for _, l := range labels {
			if _, ok := n.LabelsF[l]; ok {
				nodes = append(nodes, n)

				break
			}
		}
	}

	return nodes
}

func (t TopologySpec) FindDelayedNodes() []ifaces.NodeSpec {
	var nodes []ifaces.NodeSpec

	for _, n := range t.NodesF {
		if n.Delayed() != "" {
			nodes = append(nodes, n)
		}
	}

	return nodes
}

func (t TopologySpec) FindNodesWithVLAN(vlan string) []ifaces.NodeSpec {
	var nodes []ifaces.NodeSpec

	for _, n := range t.NodesF {
		for _, i := range n.NetworkF.InterfacesF {
			if i.VLAN() == vlan {
				nodes = append(nodes, n)

				break
			}
		}
	}

	return nodes
}

func (t *TopologySpec) AddNode(typ, hostname string) ifaces.NodeSpec { //nolint:ireturn // interface
	n := &Node{ //nolint:exhaustruct // partial initialization
		TypeF: typ,
		GeneralF: &General{ //nolint:exhaustruct // partial initialization
			HostnameF: hostname,
		},
	}

	t.NodesF = append(t.NodesF, n)

	return n
}

func (t *TopologySpec) RemoveNode(hostname string) {
	idx := -1

	for i, node := range t.NodesF {
		if node.GeneralF.HostnameF == hostname {
			idx = i

			break
		}
	}

	if idx != -1 {
		t.NodesF = append(t.NodesF[:idx], t.NodesF[idx+1:]...)
	}
}

func (t TopologySpec) HasCommands() bool {
	for _, node := range t.Nodes() {
		if len(node.Commands()) > 0 {
			return true
		}
	}

	return false
}

func (t *TopologySpec) Init(bridge string) error {
	var errs error

	seen := make(map[string]struct{})

	for _, n := range t.NodesF {
		hostname := n.GeneralF.HostnameF

		// Hostnames must be unique across the topology (including case-insensitively)
		hostnameLower := strings.ToLower(hostname)
		if _, ok := seen[hostnameLower]; ok {
			errs = multierror.Append(
				errs,
				fmt.Errorf("duplicate node hostname %q", hostname),
			)
		} else {
			seen[hostnameLower] = struct{}{}
		}

		err := n.validate()
		if err != nil {
			errs = multierror.Append(
				errs,
				fmt.Errorf("validating node %s: %w", hostname, err),
			)
		}

		n.setDefaults(bridge)
	}

	if err := t.validateC2Delays(false); err != nil {
		errs = multierror.Append(errs, err)
	}

	return errs
}

// launched reports whether minimega launches the node, so its miniccc client
// can register: external and do-not-boot nodes are left out of the script.
func (n Node) launched() bool {
	if n.External() {
		return false
	}

	return n.GeneralF.DoNotBootF == nil || !*n.GeneralF.DoNotBootF
}

// c2Waits returns the delay.c2 hostnames, empty ones included, of a node
// whose C2 delay experiment start enforces: none for a node minimega does not
// launch or that the user starts, since a user delay wins over a C2 one.
func (n Node) c2Waits() []string {
	if !n.launched() || n.DelayF == nil || n.DelayF.UserF {
		return nil
	}

	hosts := make([]string, 0, len(n.DelayF.C2F))

	for _, c2 := range n.DelayF.C2F {
		hosts = append(hosts, c2.HostnameF)
	}

	return hosts
}

// ValidateC2Delays checks that every delay.c2 hostname experiment start waits
// on is set and names a node whose miniccc client can register: a node
// minimega launches other than the waiting node itself, with C2 delays that
// do not wait on each other. Experiment start waits for those clients with no
// deadline, so anything else would keep the VM, and a synchronous start,
// waiting forever. Hostnames match case-insensitively, as minimega's VM
// lookup does.
//
// Topology validation (Init) cannot know two of these: a configure app may
// add the node a hostname names, as the caldera app adds its server, and the
// startup app marks a node do_not_boot at pre-start while its image is
// missing and boots it again once the image exists. Init leaves both to this
// check, which experiment create runs once the configure apps ran and
// experiment start once the pre-start apps ran.
func (t TopologySpec) ValidateC2Delays() error {
	return t.validateC2Delays(true)
}

// validateC2Delays refuses what ValidateC2Delays refuses; unless final, it
// accepts a hostname that names no node or a node the startup app marked
// do_not_boot.
func (t TopologySpec) validateC2Delays(final bool) error {
	nodes := make(map[string]*Node, len(t.NodesF))

	for _, n := range t.NodesF {
		nodes[strings.ToLower(n.GeneralF.HostnameF)] = n
	}

	var errs error

	for _, n := range t.NodesF {
		for i, host := range n.c2Waits() {
			var reason string

			switch other, ok := nodes[strings.ToLower(host)]; {
			case host == "":
				// Experiment start skips it, so it would silently not wait.
				errs = multierror.Append(errs, fmt.Errorf(
					"validating node %s: delay.c2[%d] has no hostname", n.GeneralF.HostnameF, i,
				))

				continue
			case !ok:
				if !final {
					continue
				}

				reason = "is not a node in the topology"
			case other == n:
				reason = "is the node itself, which never starts while it waits"
			case other.External():
				reason = "is an external node, which minimega does not launch"
			case !other.launched():
				why, _ := other.AnnotationsF[DoNotBootReasonAnnotation].(string)
				if why != "" && !final {
					continue
				}

				reason = "is a do-not-boot node, which minimega does not launch"

				if why != "" {
					reason += " (" + why + ")"
				}
			default:
				continue
			}

			errs = multierror.Append(errs, fmt.Errorf(
				"validating node %s: delay.c2 hostname %q %s",
				n.GeneralF.HostnameF, host, reason,
			))
		}
	}

	if cycle := t.c2DelayCycle(nodes); cycle != nil {
		errs = multierror.Append(errs, fmt.Errorf(
			"delay.c2 of nodes %s wait on each other, so none of them starts",
			strings.Join(cycle, " -> "),
		))
	}

	return errs
}

// c2DelayCycle returns the hostnames of a cycle of C2 delays between distinct
// nodes, its first node repeated at the end, or nil when there is none.
func (t TopologySpec) c2DelayCycle(nodes map[string]*Node) []string {
	const (
		visiting = 1
		done     = 2
	)

	var (
		state = make(map[*Node]int, len(nodes))
		path  []*Node
		visit func(*Node) []string
	)

	visit = func(n *Node) []string {
		state[n] = visiting
		path = append(path, n)

		for _, host := range n.c2Waits() {
			other, ok := nodes[strings.ToLower(host)]
			if !ok || other == n {
				continue
			}

			switch state[other] {
			case visiting:
				var cycle []string

				for _, p := range path[slices.Index(path, other):] {
					cycle = append(cycle, p.GeneralF.HostnameF)
				}

				return append(cycle, other.GeneralF.HostnameF)
			case done:
				continue
			}

			if cycle := visit(other); cycle != nil {
				return cycle
			}
		}

		path = path[:len(path)-1]
		state[n] = done

		return nil
	}

	for _, n := range t.NodesF {
		if state[n] != 0 {
			continue
		}

		if cycle := visit(n); cycle != nil {
			return cycle
		}
	}

	return nil
}
