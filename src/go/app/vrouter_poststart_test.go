package app

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/activeshadow/libminimega/minicli"
	"github.com/activeshadow/libminimega/miniclient"

	"phenix/types"
	v1 "phenix/types/version/v1"
	v2 "phenix/types/version/v2"
	"phenix/util/mm/mmcli"
)

// installRouterCommands replaces the minimega router command sender for the
// test. answer gives each host's answer to a command, namespaced as minimega
// receives it.
func installRouterCommands(t *testing.T, answer func(cmd string) minicli.Responses) {
	t.Helper()

	original := vrouterRunMM
	t.Cleanup(func() { vrouterRunMM = original })

	vrouterRunMM = func(cmd *mmcli.Command) chan *miniclient.Response {
		if cmd.Namespace != "exp1" {
			t.Errorf("router command %q sent in namespace %q, want exp1", cmd.Command, cmd.Namespace)
		}

		out := make(chan *miniclient.Response, 1)
		out <- &miniclient.Response{Resp: answer(cmd.Command)}
		close(out)

		return out
	}
}

// oneHost answers each command from the one host of the namespace, with the
// error send returns.
func oneHost(send func(cmd string) error) func(string) minicli.Responses {
	return func(cmd string) minicli.Responses {
		resp := &minicli.Response{Host: "h1"}

		if err := send(cmd); err != nil {
			resp.Error = err.Error()
		}

		return minicli.Responses{resp}
	}
}

// newMinirouterTestExperiment builds an experiment with one minirouter, rtr,
// that has static, DHCP and firewalled interfaces, a static route, and DHCP
// and DNS settings in its vrouter app metadata.
func newMinirouterTestExperiment(t *testing.T, metadata map[string]any) *types.Experiment {
	t.Helper()

	node := newStartupTestNode(t, "rtr", osMinirouter, true, 1)
	node.TypeF = "Router"
	node.NetworkF = &v1.Network{
		InterfacesF: []*v1.Interface{
			{NameF: "eth0", TypeF: "ethernet", ProtoF: "static", VLANF: "LAN", AddressF: "10.10.0.254", MaskF: 24},
			{
				NameF: "eth1", TypeF: "ethernet", ProtoF: "static", VLANF: "LAN2",
				AddressF: "10.20.0.254", MaskF: 24, GatewayF: "10.20.0.1", RulesetInF: "lan2-in",
			},
			{NameF: "eth2", TypeF: "ethernet", ProtoF: "dhcp", VLANF: "WAN"},
		},
		RoutesF: []v1.Route{{DestinationF: "10.99.0.0/24", NextF: "10.20.0.1"}},
		RulesetsF: []*v1.Ruleset{{
			NameF:    "lan2-in",
			DefaultF: "accept",
			RulesF: []*v1.Rule{{
				IDF:          10,
				ActionF:      "drop",
				ProtocolF:    "tcp",
				SourceF:      &v1.AddrPort{AddressF: "10.20.0.0/24"},
				DestinationF: &v1.AddrPort{AddressF: "10.10.0.1", PortF: 22},
			}},
		}},
	}

	scenario := &v2.ScenarioSpec{AppsF: []*v2.ScenarioApp{{
		NameF:  appNameVrouter,
		HostsF: []*v2.ScenarioAppHost{{HostnameF: "rtr", MetadataF: metadata}},
	}}}

	exp, _ := newStartupTestExperiment(t, node, scenario)

	return exp
}

// minirouterTestMetadata is vrouter app metadata with DHCP on 10.10.0.254 and a
// DNS record.
func minirouterTestMetadata() map[string]any {
	return map[string]any{
		"dhcp": []any{map[string]any{
			"listenAddress": "10.10.0.254",
			"ranges":        []any{map[string]any{"lowAddress": "10.10.0.100", "highAddress": "10.10.0.110"}},
			"defaultRoute":  "10.10.0.254",
			"dnsServers":    []any{"10.10.0.254"},
		}},
		"dns": map[string]any{"10.10.0.1": "a.e2e.local"},
	}
}

func runVrouterPostStart(exp *types.Experiment, opts ...Option) error {
	v := new(Vrouter)
	_ = v.Init(opts...)

	return v.PostStart(context.Background(), exp)
}

// TestVrouterPostStartDryRun is a regression test: a dry run launches no VMs,
// but vrouter still sent its router commands for every minirouter, which
// minimega refused, so a dry run of any minirouter experiment failed and
// created the experiment's minimega namespace.
func TestVrouterPostStartDryRun(t *testing.T) {
	var sent []string

	installRouterCommands(t, oneHost(func(cmd string) error {
		sent = append(sent, cmd)

		return nil
	}))

	t.Run("dry run sends nothing", func(t *testing.T) {
		sent = nil

		if err := runVrouterPostStart(newMinirouterTestExperiment(t, minirouterTestMetadata()), DryRun(true)); err != nil {
			t.Fatalf("dry-run vrouter PostStart: %v", err)
		}

		if len(sent) != 0 {
			t.Errorf("dry run sent router commands %q, want none", sent)
		}
	})

	t.Run("dry run still checks app metadata", func(t *testing.T) {
		sent = nil

		err := runVrouterPostStart(newMinirouterTestExperiment(t, map[string]any{"dhcp": "10.10.0.254"}), DryRun(true))
		if err == nil || !strings.Contains(err.Error(), "decoding DHCP config") {
			t.Errorf("dry-run vrouter PostStart with bad dhcp metadata = %v, want a DHCP decoding error", err)
		}
	})

	t.Run("start sends the router config", func(t *testing.T) {
		sent = nil

		if err := runVrouterPostStart(newMinirouterTestExperiment(t, minirouterTestMetadata())); err != nil {
			t.Fatalf("vrouter PostStart: %v", err)
		}

		for _, want := range []string{
			"router rtr gw 10.20.0.1",
			"router rtr interface 0 10.10.0.254/24",
			"router rtr interface 1 10.20.0.254/24",
			"router rtr interface 2 dhcp",
			"router rtr route static 10.99.0.0/24 10.20.0.1",
			"router rtr fw chain lan2-in action drop 10.20.0.0/24 10.10.0.1:22 tcp",
			"router rtr fw chain lan2-in default action accept",
			"router rtr fw chain lan2-in apply in 1",
			"router rtr dhcp 10.10.0.254 range 10.10.0.100 10.10.0.110",
			"router rtr dhcp 10.10.0.254 router 10.10.0.254",
			"router rtr dhcp 10.10.0.254 dns 10.10.0.254",
			"router rtr dns 10.10.0.1 a.e2e.local",
		} {
			if !slices.Contains(sent, want) {
				t.Errorf("router commands lack %q: %q", want, sent)
			}
		}

		if len(sent) == 0 || sent[len(sent)-1] != "router rtr commit" {
			t.Errorf("router commands do not end with the commit: %q", sent)
		}
	})
}

// fakeRouters keeps minimega's staged router state as far as a repeated
// post-start depends on it: router subcommands create the router and append
// to its config, an address can be staged once, "clear router" needs an
// existing router, and commit snapshots the staged config.
type fakeRouters struct {
	staged  map[string][]string
	commits [][]string
}

func (f *fakeRouters) send(cmd string) error {
	fields := strings.Fields(cmd)

	switch {
	case len(fields) >= 3 && fields[0] == "clear" && fields[1] == "router":
		vm := fields[2]

		staged, ok := f.staged[vm]
		if !ok {
			return fmt.Errorf("no such router %v", vm)
		}

		if len(fields) == 4 && fields[3] == "gw" {
			f.staged[vm] = slices.DeleteFunc(staged, func(c string) bool {
				return strings.HasPrefix(c, "router "+vm+" gw ")
			})

			return nil
		}

		// like minimega, a whole-router clear keeps the gateway
		f.staged[vm] = slices.DeleteFunc(staged, func(c string) bool {
			return !strings.HasPrefix(c, "router "+vm+" gw ")
		})
	case len(fields) >= 3 && fields[0] == "router":
		vm := fields[1]

		if fields[2] == "commit" {
			f.commits = append(f.commits, slices.Clone(f.staged[vm]))

			return nil
		}

		if fields[2] == "interface" && slices.Contains(f.staged[vm], cmd) {
			return fmt.Errorf("IP %v already exists", fields[4])
		}

		f.staged[vm] = append(f.staged[vm], cmd)
	default:
		return fmt.Errorf("unexpected command %q", cmd)
	}

	return nil
}

// TestVrouterPostStartIsIdempotent is a regression test: minimega keeps a
// router's staged config across commits, so re-triggering vrouter's
// post-start failed with "IP ... already exists" on the first interface, and
// would have duplicated firewall rules and DNS records past it.
func TestVrouterPostStartIsIdempotent(t *testing.T) {
	for _, hosts := range []int{1, 2} {
		t.Run(fmt.Sprintf("%d namespace hosts", hosts), func(t *testing.T) {
			routers := &fakeRouters{staged: make(map[string][]string)}

			answer := oneHost(routers.send)

			if hosts == 2 {
				answer = func(cmd string) minicli.Responses {
					resps := oneHost(routers.send)(cmd)

					// minimega drops "vm not found" answers from the hosts
					// without the VM, except for clear router's own message.
					if strings.HasPrefix(cmd, "clear router rtr") {
						resps = append(resps, &minicli.Response{Host: "h2", Error: "no such vm rtr"})
					}

					return resps
				}
			}

			installRouterCommands(t, answer)

			exp := newMinirouterTestExperiment(t, minirouterTestMetadata())

			for run := 1; run <= 2; run++ {
				if err := runVrouterPostStart(exp); err != nil {
					t.Fatalf("vrouter PostStart run %d: %v", run, err)
				}
			}

			if len(routers.commits) != 2 {
				t.Fatalf("got %d router commits, want 2", len(routers.commits))
			}

			first, second := routers.commits[0], routers.commits[1]

			if !slices.Contains(first, "router rtr interface 0 10.10.0.254/24") {
				t.Errorf("first commit lacks the router's interfaces: %q", first)
			}

			if !slices.Equal(first, second) {
				t.Errorf("second commit staged\n%q\nwant the first commit's\n%q", second, first)
			}
		})
	}
}

// TestClearRouterError checks that the answers of namespace hosts without the
// VM, which minimega passes on for "clear router", do not fail the clear.
func TestClearRouterError(t *testing.T) {
	answer := func(host, err string) *minicli.Response {
		return &minicli.Response{Host: host, Error: err}
	}

	tests := []struct {
		name    string
		answers minicli.Responses
		want    string // "": no error
	}{
		{name: "cleared", answers: minicli.Responses{answer("h1", "")}},
		{name: "cleared beside a host without the VM", answers: minicli.Responses{answer("h1", ""), answer("h2", "no such vm rtr")}},
		{name: "no host has the VM", answers: minicli.Responses{answer("h1", "no such vm rtr")}, want: "no such vm rtr"},
		{name: "no answer", want: "no such vm rtr"},
		{
			name:    "other errors are kept",
			answers: minicli.Responses{answer("h1", "no such router rtr"), answer("h2", "no such vm rtr")},
			want:    "1 error occurred:\n\t* no such router rtr\n\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			responses := make(chan *miniclient.Response, 1)
			responses <- &miniclient.Response{Resp: tt.answers}
			close(responses)

			var got string

			if err := clearRouterError(responses, "rtr"); err != nil {
				got = err.Error()
			}

			if got != tt.want {
				t.Errorf("clearRouterError = %q, want %q", got, tt.want)
			}
		})
	}
}
