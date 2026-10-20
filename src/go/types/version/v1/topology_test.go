package v1

import (
	"strings"
	"testing"
)

func TestTopologyInitHostnames(t *testing.T) {
	// minimal node that survives setDefaults (which dereferences HardwareF).
	node := func(hostname string) *Node {
		return &Node{
			GeneralF:  &General{HostnameF: hostname},
			HardwareF: &Hardware{},
		}
	}

	t.Run("rejects duplicate hostnames", func(t *testing.T) {
		topo := &TopologySpec{
			NodesF: []*Node{node("dup"), node("other"), node("dup")},
		}

		err := topo.Init("phenix")
		if err == nil {
			t.Fatalf("expected an error, got nil")
		}

		if !strings.Contains(err.Error(), `duplicate node hostname "dup"`) {
			t.Fatalf("error %q does not name the duplicate hostname", err.Error())
		}
	})

	t.Run("rejects hostnames differing only in case", func(t *testing.T) {
		topo := &TopologySpec{
			NodesF: []*Node{node("ADServer"), node("adserver")},
		}

		err := topo.Init("phenix")
		if err == nil {
			t.Fatalf("expected an error, got nil")
		}

		if !strings.Contains(err.Error(), `duplicate node hostname "adserver"`) {
			t.Fatalf("error %q does not name the duplicate hostname", err.Error())
		}
	})

	t.Run("allows unique hostnames", func(t *testing.T) {
		topo := &TopologySpec{
			NodesF: []*Node{node("a"), node("b"), node("c")},
		}

		if err := topo.Init("phenix"); err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
	})
}

func TestTopologyInitC2Delays(t *testing.T) {
	// vm returns a minimal node; c2 lists the hostnames it waits on.
	vm := func(hostname string, c2 ...string) *Node {
		n := &Node{
			GeneralF:  &General{HostnameF: hostname},
			HardwareF: &Hardware{},
		}

		if len(c2) > 0 {
			n.DelayF = &Delay{}

			for _, h := range c2 {
				n.DelayF.C2F = append(n.DelayF.C2F, C2Delay{HostnameF: h})
			}
		}

		return n
	}

	dnb := func(n *Node) *Node {
		n.GeneralF.DoNotBootF = boolPtr(true)
		return n
	}

	external := func(n *Node) *Node {
		n.ExternalF = boolPtr(true)
		return n
	}

	user := func(n *Node) *Node {
		n.DelayF.UserF = true
		return n
	}

	missingImage := func(n *Node) *Node {
		n.AnnotationsF = map[string]any{DoNotBootReasonAnnotation: "missing disk image"}
		return dnb(n)
	}

	tests := []struct {
		name  string
		nodes []*Node
		// init lists substrings of Init's error, final those of
		// ValidateC2Delays's; none means it succeeds. Init leaves out what
		// apps may change, so it refuses a subset of what ValidateC2Delays
		// refuses.
		init, final []string
	}{
		{
			name:  "waits on a launched node",
			nodes: []*Node{vm("a"), vm("b", "a")},
		},
		{
			name:  "hostname matches regardless of case",
			nodes: []*Node{vm("Server"), vm("b", "server")},
		},
		{
			name:  "chain of C2 delays",
			nodes: []*Node{vm("a"), vm("b", "a"), vm("c", "b")},
		},
		{
			name:  "empty hostname",
			nodes: []*Node{vm("a"), vm("b", "a", "")},
			init:  []string{"validating node b: delay.c2[1] has no hostname"},
			final: []string{"validating node b: delay.c2[1] has no hostname"},
		},
		{
			name:  "user delay wins over the C2 delay",
			nodes: []*Node{user(vm("a", "nosuchvm"))},
		},
		{
			name:  "do-not-boot node's own C2 delay is never waited on",
			nodes: []*Node{dnb(vm("a", "nosuchvm"))},
		},
		{
			name:  "waits on a user-delayed node",
			nodes: []*Node{user(vm("a", "")), vm("b", "a")},
		},
		{
			name:  "unknown hostname, which a configure app may add",
			nodes: []*Node{vm("badc2", "nosuchvm")},
			final: []string{`validating node badc2: delay.c2 hostname "nosuchvm" is not a node in the topology`},
		},
		{
			name:  "waits on itself",
			nodes: []*Node{vm("a", "A")},
			init:  []string{`validating node a: delay.c2 hostname "A" is the node itself`},
			final: []string{`validating node a: delay.c2 hostname "A" is the node itself`},
		},
		{
			name:  "waits on a do-not-boot node",
			nodes: []*Node{dnb(vm("off")), vm("b", "off")},
			init:  []string{`validating node b: delay.c2 hostname "off" is a do-not-boot node, which minimega does not launch`},
			final: []string{`validating node b: delay.c2 hostname "off" is a do-not-boot node, which minimega does not launch`},
		},
		{
			name:  "waits on a node the startup app marked do-not-boot",
			nodes: []*Node{missingImage(vm("off")), vm("b", "off")},
			final: []string{`delay.c2 hostname "off" is a do-not-boot node, which minimega does not launch (missing disk image)`},
		},
		{
			name:  "waits on an external node",
			nodes: []*Node{external(vm("ext")), vm("b", "ext")},
			init:  []string{`validating node b: delay.c2 hostname "ext" is an external node`},
			final: []string{`validating node b: delay.c2 hostname "ext" is an external node`},
		},
		{
			name:  "C2 delays that wait on each other",
			nodes: []*Node{vm("x"), vm("a", "x", "b"), vm("b", "c"), vm("c", "a")},
			init:  []string{"delay.c2 of nodes a -> b -> c -> a wait on each other"},
			final: []string{"delay.c2 of nodes a -> b -> c -> a wait on each other"},
		},
		{
			name:  "every invalid hostname is reported",
			nodes: []*Node{vm("a", "nope", "a"), vm("b", "gone")},
			init:  []string{`"a" is the node itself`},
			final: []string{`"nope" is not a node`, `"a" is the node itself`, `"gone" is not a node`},
		},
	}

	check := func(t *testing.T, what string, err error, want []string) {
		t.Helper()

		if len(want) == 0 {
			if err != nil {
				t.Fatalf("%s error = %v, want nil", what, err)
			}

			return
		}

		if err == nil {
			t.Fatalf("%s error = nil, want one containing %q", what, want)
		}

		for _, w := range want {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("%s error = %q, want it to contain %q", what, err, w)
			}
		}
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			topo := &TopologySpec{NodesF: test.nodes}

			check(t, "Init()", topo.Init("phenix"), test.init)
			check(t, "ValidateC2Delays()", topo.ValidateC2Delays(), test.final)
		})
	}
}
