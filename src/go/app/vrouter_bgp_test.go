package app

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/mitchellh/mapstructure"
	"gopkg.in/yaml.v3"
	"pgregory.net/rapid"

	ifaces "phenix/types/interfaces"
	v1 "phenix/types/version/v1"
	v2 "phenix/types/version/v2"
	"phenix/util/testenv"
)

// bgpTestNets returns the interfaces of the router that the bgp tests
// configure: two with a static address and one that gets its address by DHCP.
func bgpTestNets() []ifaces.NodeNetworkInterface {
	return (&v1.Network{InterfacesF: []*v1.Interface{
		{NameF: "WAN0", TypeF: "ethernet", ProtoF: "static", VLANF: "wan", AddressF: "198.18.0.2", MaskF: 30},
		{NameF: "LAN0", TypeF: "ethernet", ProtoF: "static", VLANF: "lan", AddressF: "10.20.30.1", MaskF: 24},
		{NameF: "DHCP0", TypeF: "ethernet", ProtoF: "dhcp", VLANF: "dhcp"},
	}}).Interfaces()
}

// bgpMetadata returns host metadata whose `bgp` value is the YAML in text.
func bgpMetadata(t *testing.T, text string) map[string]any {
	t.Helper()

	var value any

	err := yaml.Unmarshal([]byte(text), &value)
	if err != nil {
		t.Fatalf("yaml.Unmarshal(%q) error = %v", text, err)
	}

	return map[string]any{"bgp": value}
}

// bgpFixture returns the host metadata in testdata/vrouter/name.yaml.
func bgpFixture(t *testing.T, name string) map[string]any {
	t.Helper()

	body, err := os.ReadFile(filepath.Join("testdata", "vrouter", name+".yaml"))
	if err != nil {
		t.Fatal(err)
	}

	var md map[string]any

	err = yaml.Unmarshal(body, &md)
	if err != nil {
		t.Fatalf("yaml.Unmarshal(%s.yaml) error = %v", name, err)
	}

	return md
}

// bgpFixtureLines returns the lines of testdata/vrouter/name.lines.
func bgpFixtureLines(t *testing.T, name string) []string {
	t.Helper()

	body, err := os.ReadFile(filepath.Join("testdata", "vrouter", name+".lines"))
	if err != nil {
		t.Fatal(err)
	}

	return strings.Split(strings.TrimSuffix(string(body), "\n"), "\n")
}

// bgpNeighbor returns `bgp` metadata with one neighbor that has the given keys.
func bgpNeighbor(keys string) string {
	return "localAS: 64512\nneighbors:\n  - address: 198.18.0.1\n    remoteAS: 64500\n    " + keys + "\n"
}

// bgpLists is `bgp` metadata that defines one name of each kind.
const bgpLists = `localAS: 64512
bfdProfiles: [{name: BFD}]
prefixLists: [{name: PL, rules: [{rule: 10, action: permit, prefix: 10.0.0.0/8}]}]
communityLists: [{name: CL, rules: [{rule: 10, action: permit, regex: "_64512:1_"}]}]
routeMaps: [{name: RM, rules: [{rule: 10, action: permit}]}]
`

// bgpNeighborWithLists is bgpNeighbor in a block that defines the names of bgpLists.
func bgpNeighborWithLists(keys string) string {
	return bgpLists + "neighbors:\n  - address: 198.18.0.1\n    remoteAS: 64500\n    " + keys + "\n"
}

// bgpPrefixRules returns `bgp` metadata with the prefix list PL of the given rules.
func bgpPrefixRules(rules ...string) string {
	return "localAS: 64512\nprefixLists:\n  - name: PL\n    rules:\n      - " + strings.Join(rules, "\n      - ") + "\n"
}

// bgpCommunityRule returns `bgp` metadata with the community list CL of one rule.
func bgpCommunityRule(rule string) string {
	return "localAS: 64512\ncommunityLists:\n  - name: CL\n    rules:\n      - " + rule + "\n"
}

// bgpRouteMapRule returns `bgp` metadata that defines the names of bgpLists,
// and the route map RM2 whose rule 10 has the given keys.
func bgpRouteMapRule(keys string) string {
	lists, _, _ := strings.Cut(bgpLists, "routeMaps:")

	return lists +
		"routeMaps:\n  - name: RM\n    rules: [{rule: 10, action: permit}]\n" +
		"  - name: RM2\n    rules:\n      - {rule: 10, action: permit, " + keys + "}\n"
}

type bgpCase struct {
	name   string
	text   string   // the YAML of the `bgp` value
	config []string // the `config` lines of the same host
	ok     bool
	want   []string // lines that an accepted block must give
}

func bgpTopLevelCases() []bgpCase {
	return []bgpCase{
		{name: "local AS only", text: "localAS: 64512", ok: true, want: []string{"set protocols bgp system-as 64512"}},
		{name: "local AS 1", text: "localAS: 1", ok: true},
		{name: "largest local AS", text: "localAS: 4294967294", ok: true},
		{name: "no local AS", text: "routerID: 10.255.0.1"},
		{name: "empty block", text: "{}"},
		{name: "null block", text: "null"},
		{name: "local AS 0", text: "localAS: 0"},
		{name: "reserved local AS", text: "localAS: 4294967295"},
		{name: "negative local AS", text: "localAS: -1"},
		{name: "local AS is text", text: "localAS: sixty"},
		{name: "local AS is a fraction", text: "localAS: 64512.5"},
		{name: "block is a list", text: "- localAS: 64512"},
		{name: "misspelled key", text: "localAS: 64512\nrouterIdentifier: 10.255.0.1"},
		{name: "two keys that differ in case", text: "localAS: 64512\nrouterID: 10.255.0.1\nrouterId: 10.255.0.1"},
		{
			// mapstructure matches keys without regard to case, as it does for the app's other metadata.
			name: "key in another case", text: "localAS: 64512\nrouterId: 10.255.0.1", ok: true,
			want: []string{"set protocols bgp parameters router-id 10.255.0.1"},
		},
		{name: "neighbors is not a list", text: "localAS: 64512\nneighbors: 5"},

		{
			name: "router ID", text: "localAS: 64512\nrouterID: 10.255.0.1", ok: true,
			want: []string{"set protocols bgp parameters router-id 10.255.0.1"},
		},
		{name: "router ID out of range", text: "localAS: 64512\nrouterID: 10.0.0.256"},
		{name: "router ID is IPv6", text: "localAS: 64512\nrouterID: 2001:db8::1"},
		{name: "router ID is IPv4 in IPv6", text: "localAS: 64512\nrouterID: ::ffff:10.0.0.1"},
		{name: "router ID is a name", text: "localAS: 64512\nrouterID: WAN0"},
		{name: "router ID with a command", text: "localAS: 64512\nrouterID: 10.0.0.1; reboot"},

		{
			name: "multipath limits", text: "localAS: 64512\nmultipath: {ebgp: 1, ibgp: 256}", ok: true,
			want: []string{
				"set protocols bgp address-family ipv4-unicast maximum-paths ebgp 1",
				"set protocols bgp address-family ipv4-unicast maximum-paths ibgp 256",
			},
		},
		{name: "multipath ebgp 0", text: "localAS: 64512\nmultipath: {ebgp: 0}"},
		{name: "multipath ebgp too large", text: "localAS: 64512\nmultipath: {ebgp: 257}"},
		{name: "multipath ibgp 0", text: "localAS: 64512\nmultipath: {ibgp: 0}"},
		{name: "multipath ibgp too large", text: "localAS: 64512\nmultipath: {ibgp: 257}"},
		{name: "multipath unknown key", text: "localAS: 64512\nmultipath: {relax: true}"},

		{
			name: "network", text: "localAS: 64512\nnetworks: [198.18.105.0/24, 0.0.0.0/0]", ok: true,
			want: []string{"set protocols bgp address-family ipv4-unicast network 198.18.105.0/24"},
		},
		{name: "network with host bits", text: "localAS: 64512\nnetworks: [10.1.2.3/8]"},
		{name: "network without a length", text: "localAS: 64512\nnetworks: [10.0.0.0]"},
		{name: "network is IPv6", text: "localAS: 64512\nnetworks: ['2001:db8::/32']"},
		{name: "network with a command", text: "localAS: 64512\nnetworks: ['10.0.0.0/8; reboot']"},

		{
			name: "redistribute every protocol",
			text: "localAS: 64512\nredistribute: [{protocol: connected}, {protocol: static}, {protocol: ospf}, " +
				"{protocol: kernel}, {protocol: rip, metric: 0}]",
			ok: true,
			want: []string{
				"set protocols bgp address-family ipv4-unicast redistribute connected",
				"set protocols bgp address-family ipv4-unicast redistribute rip metric 0",
			},
		},
		{
			name: "redistribute with a route map", text: bgpLists + "redistribute: [{protocol: static, routeMap: RM, metric: 4294967295}]",
			ok: true,
			want: []string{
				"set protocols bgp address-family ipv4-unicast redistribute static route-map RM",
				"set protocols bgp address-family ipv4-unicast redistribute static metric 4294967295",
			},
		},
		{name: "redistribute bgp", text: "localAS: 64512\nredistribute: [{protocol: bgp}]"},
		{name: "redistribute isis", text: "localAS: 64512\nredistribute: [{protocol: isis}]"},
		{name: "redistribute no protocol", text: "localAS: 64512\nredistribute: [{metric: 10}]"},
		{name: "redistribute twice", text: "localAS: 64512\nredistribute: [{protocol: static}, {protocol: static, metric: 5}]"},
		{name: "redistribute unknown route map", text: "localAS: 64512\nredistribute: [{protocol: static, routeMap: RM}]"},
		{name: "redistribute negative metric", text: "localAS: 64512\nredistribute: [{protocol: static, metric: -1}]"},
		{name: "redistribute metric too large", text: "localAS: 64512\nredistribute: [{protocol: static, metric: 4294967296}]"},
	}
}

func bgpNeighborCases() []bgpCase {
	const base = "set protocols bgp neighbor 198.18.0.1 "

	return []bgpCase{
		{
			name: "plain neighbor", text: bgpNeighbor("shutdown: false"), ok: true,
			want: []string{base + "remote-as 64500", base + "address-family ipv4-unicast"},
		},
		{name: "no address", text: "localAS: 64512\nneighbors: [{remoteAS: 64500}]"},
		{name: "address is IPv6", text: "localAS: 64512\nneighbors: [{address: '2001:db8::1', remoteAS: 64500}]"},
		{name: "address is a name", text: "localAS: 64512\nneighbors: [{address: isp, remoteAS: 64500}]"},
		{name: "address is a prefix", text: "localAS: 64512\nneighbors: [{address: 198.18.0.1/30, remoteAS: 64500}]"},
		{name: "address is the router's own", text: "localAS: 64512\nneighbors: [{address: 198.18.0.2, remoteAS: 64500}]"},
		{
			name: "address twice",
			text: "localAS: 64512\nneighbors: [{address: 198.18.0.1, remoteAS: 64500}, {address: 198.18.0.1, remoteAS: 64501}]",
		},
		{name: "no remote AS", text: "localAS: 64512\nneighbors: [{address: 198.18.0.1}]"},
		{name: "remote AS 0", text: "localAS: 64512\nneighbors: [{address: 198.18.0.1, remoteAS: 0}]"},
		{name: "reserved remote AS", text: "localAS: 64512\nneighbors: [{address: 198.18.0.1, remoteAS: 4294967295}]"},
		{name: "misspelled neighbor key", text: bgpNeighbor("remoteASN: 64500")},

		{
			name: "description with shell characters", text: bgpNeighbor(`description: "ISP #1; $(reboot) & co | <x>"`), ok: true,
			want: []string{base + "description 'ISP #1; $(reboot) & co | <x>'"},
		},
		{name: "description with a single quote", text: bgpNeighbor(`description: "ISP's router"`)},
		// The quotes pair up, so only the rule for the text itself refuses it.
		{name: "description with a quoted word", text: bgpNeighbor(`description: "'ISP' router"`)},
		{name: "description with a newline", text: bgpNeighbor(`description: "ISP\nreboot"`)},
		{name: "description with a tab", text: bgpNeighbor(`description: "ISP\t1"`)},
		{name: "description longer than VyOS takes", text: bgpNeighbor("description: " + strings.Repeat("d", 256))},
		{name: "longest description", text: bgpNeighbor("description: " + strings.Repeat("d", 255)), ok: true},

		{
			name: "update source address",
			text: bgpNeighbor("updateSource: 10.255.0.1"),
			ok:   true,
			want: []string{base + "update-source 10.255.0.1"},
		},
		{
			name: "update source interface",
			text: bgpNeighbor("updateSource: WAN0"),
			ok:   true,
			want: []string{base + "update-source 198.18.0.2"},
		},
		{name: "update source unknown interface", text: bgpNeighbor("updateSource: WAN9")},
		{name: "update source interface with no address", text: bgpNeighbor("updateSource: DHCP0")},
		{name: "update source is IPv6", text: bgpNeighbor("updateSource: '2001:db8::1'")},

		{
			name: "multihop limits", text: bgpNeighbor("ebgpMultihop: 255"), ok: true,
			want: []string{base + "ebgp-multihop 255"},
		},
		{name: "multihop 1", text: bgpNeighbor("ebgpMultihop: 1"), ok: true},
		{name: "multihop 0", text: bgpNeighbor("ebgpMultihop: 0")},
		{name: "multihop too large", text: bgpNeighbor("ebgpMultihop: 256")},

		{
			name: "password with shell characters", text: bgpNeighbor(`password: "p$(id);|&#x"`), ok: true,
			want: []string{base + "password 'p$(id);|&#x'"},
		},
		{name: "longest password", text: bgpNeighbor("password: " + strings.Repeat("p", 80)), ok: true},
		{name: "empty password", text: bgpNeighbor(`password: ""`)},
		{name: "password with a single quote", text: bgpNeighbor(`password: "it's"`)},
		{name: "password in single quotes", text: bgpNeighbor(`password: "'ab'"`)},
		{name: "password with a newline", text: bgpNeighbor(`password: "a\nb"`)},
		{name: "password with a space", text: bgpNeighbor(`password: "a b"`)},
		{name: "password too long", text: bgpNeighbor("password: " + strings.Repeat("p", 81))},
		{name: "password is a number", text: bgpNeighbor("password: 12345678")},

		{name: "shutdown", text: bgpNeighbor("shutdown: true"), ok: true, want: []string{base + "shutdown"}},

		{
			name: "timers", text: bgpNeighbor("timers: {keepalive: 3, holdtime: 9, connectRetry: 5}"), ok: true,
			want: []string{base + "timers keepalive 3", base + "timers holdtime 9", base + "timers connect 5"},
		},
		{
			name: "connect retry alone",
			text: bgpNeighbor("timers: {connectRetry: 65535}"),
			ok:   true,
			want: []string{base + "timers connect 65535"},
		},
		{
			name: "hold time off",
			text: bgpNeighbor("timers: {keepalive: 60, holdtime: 0}"),
			ok:   true,
			want: []string{base + "timers holdtime 0"},
		},
		{name: "largest timers", text: bgpNeighbor("timers: {keepalive: 65534, holdtime: 65535}"), ok: true},
		{name: "empty timers", text: bgpNeighbor("timers: {}"), ok: true},
		{name: "keepalive alone", text: bgpNeighbor("timers: {keepalive: 3}")},
		{name: "holdtime alone", text: bgpNeighbor("timers: {holdtime: 9}")},
		{name: "holdtime 1", text: bgpNeighbor("timers: {keepalive: 1, holdtime: 1}")},
		{name: "holdtime 2", text: bgpNeighbor("timers: {keepalive: 1, holdtime: 2}")},
		{name: "holdtime too large", text: bgpNeighbor("timers: {keepalive: 3, holdtime: 65536}")},
		{name: "keepalive equals holdtime", text: bgpNeighbor("timers: {keepalive: 9, holdtime: 9}")},
		{name: "keepalive above holdtime", text: bgpNeighbor("timers: {keepalive: 10, holdtime: 9}")},
		{name: "keepalive 0", text: bgpNeighbor("timers: {keepalive: 0, holdtime: 0}")},
		{name: "connect retry 0", text: bgpNeighbor("timers: {connectRetry: 0}")},
		{name: "connect retry too large", text: bgpNeighbor("timers: {connectRetry: 65536}")},
		{name: "misspelled timer", text: bgpNeighbor("timers: {keepalives: 3, holdtime: 9}")},

		{name: "bfd", text: bgpNeighbor("bfd: true"), ok: true, want: []string{base + "bfd"}},
		{
			name: "bfd profile", text: bgpNeighborWithLists("bfdProfile: BFD"), ok: true,
			want: []string{base + "bfd profile BFD"},
		},
		{name: "bfd with its profile", text: bgpNeighborWithLists("bfd: true\n    bfdProfile: BFD"), ok: true},
		{name: "bfd profile not defined", text: bgpNeighbor("bfdProfile: BFD")},
		{name: "bfd off with a profile", text: bgpNeighborWithLists("bfd: false\n    bfdProfile: BFD")},
		{
			name: "bfd profile of a config line", text: bgpNeighbor("bfdProfile: FAST"), ok: true,
			config: []string{"set protocols bfd profile FAST interval multiplier 5"},
		},

		{
			name: "address family options", text: bgpNeighbor("nexthopSelf: true\n    softReconfigurationInbound: true"), ok: true,
			want: []string{
				base + "address-family ipv4-unicast nexthop-self",
				base + "address-family ipv4-unicast soft-reconfiguration inbound",
			},
		},
		{
			name: "default originate", text: bgpNeighbor("defaultOriginate: true"), ok: true,
			want: []string{base + "address-family ipv4-unicast default-originate"},
		},
		{
			name: "default originate route map", text: bgpNeighborWithLists("defaultOriginateRouteMap: RM"), ok: true,
			want: []string{base + "address-family ipv4-unicast default-originate route-map RM"},
		},
		{name: "default originate route map not defined", text: bgpNeighbor("defaultOriginateRouteMap: RM")},
		{
			name: "default originate off with a route map",
			text: bgpNeighborWithLists("defaultOriginate: false\n    defaultOriginateRouteMap: RM"),
		},

		{
			name: "import and export", ok: true,
			text: bgpNeighborWithLists("import: {routeMap: RM, prefixList: PL}\n    export: {routeMap: RM, prefixList: PL}"),
			want: []string{
				base + "address-family ipv4-unicast route-map import RM",
				base + "address-family ipv4-unicast route-map export RM",
				base + "address-family ipv4-unicast prefix-list import PL",
				base + "address-family ipv4-unicast prefix-list export PL",
			},
		},
		{name: "import route map not defined", text: bgpNeighbor("import: {routeMap: RM}")},
		{name: "export route map not defined", text: bgpNeighbor("export: {routeMap: RM}")},
		{name: "import prefix list not defined", text: bgpNeighbor("import: {prefixList: PL}")},
		{name: "export prefix list not defined", text: bgpNeighbor("export: {prefixList: PL}")},
		{name: "import a route map as a prefix list", text: bgpNeighborWithLists("import: {prefixList: RM}")},
		{name: "import unknown key", text: bgpNeighborWithLists("import: {communityList: CL}")},
		{
			name: "import names of config lines", text: bgpNeighbor("import: {routeMap: IN, prefixList: NETS}"), ok: true,
			config: []string{
				"set policy route-map IN rule 10 action permit",
				"set policy prefix-list NETS rule 10 action permit",
				"set policy prefix-list NETS rule 10 prefix 10.0.0.0/8",
			},
		},
		{
			name: "a config line of another kind does not define the name", text: bgpNeighbor("import: {routeMap: IN}"),
			config: []string{"set policy prefix-list IN rule 10 action permit", "set policy route-map INNER rule 10 action permit"},
		},
		{
			name: "a delete line does not define the name", text: bgpNeighbor("import: {routeMap: IN}"),
			config: []string{"delete policy route-map IN rule 10"},
		},
		{
			name: "a name that is not a name, though a config line has it", text: bgpNeighbor(`import: {routeMap: "IN; reboot"}`),
			config: []string{"set policy route-map 'IN; reboot' rule 10 action permit"},
		},
	}
}

func bgpListCases() []bgpCase {
	return []bgpCase{
		{
			name: "bfd profile limits",
			ok:   true,
			text: "localAS: 64512\nbfdProfiles: [{name: A, transmitInterval: 10, receiveInterval: 60000, multiplier: 2}, {name: B_b-2, multiplier: 255}]",
			want: []string{
				"set protocols bfd profile A interval transmit 10",
				"set protocols bfd profile A interval receive 60000",
				"set protocols bfd profile A interval multiplier 2",
				"set protocols bfd profile B_b-2 interval multiplier 255",
			},
		},
		{
			name: "bfd profile with no values",
			text: "localAS: 64512\nbfdProfiles: [{name: A}]",
			ok:   true,
			want: []string{"set protocols bfd profile A"},
		},
		{
			name: "bfd profile with the longest name",
			text: "localAS: 64512\nbfdProfiles: [{name: " + strings.Repeat("A", 32) + "}]",
			ok:   true,
		},
		{name: "bfd profile name longer than VyOS takes", text: "localAS: 64512\nbfdProfiles: [{name: " + strings.Repeat("A", 33) + "}]"},
		{name: "bfd profile without a name", text: "localAS: 64512\nbfdProfiles: [{multiplier: 3}]"},
		{name: "bfd profile name starts with a digit", text: "localAS: 64512\nbfdProfiles: [{name: 1A}]"},
		{name: "bfd profile name with a space", text: "localAS: 64512\nbfdProfiles: [{name: A B}]"},
		{name: "bfd profile name with a command", text: "localAS: 64512\nbfdProfiles: [{name: 'A;reboot'}]"},
		{name: "bfd profile twice", text: "localAS: 64512\nbfdProfiles: [{name: A}, {name: A}]"},
		{name: "bfd transmit too small", text: "localAS: 64512\nbfdProfiles: [{name: A, transmitInterval: 9}]"},
		{name: "bfd transmit too large", text: "localAS: 64512\nbfdProfiles: [{name: A, transmitInterval: 60001}]"},
		{name: "bfd receive too small", text: "localAS: 64512\nbfdProfiles: [{name: A, receiveInterval: 9}]"},
		{name: "bfd receive too large", text: "localAS: 64512\nbfdProfiles: [{name: A, receiveInterval: 60001}]"},
		{name: "bfd multiplier too small", text: "localAS: 64512\nbfdProfiles: [{name: A, multiplier: 1}]"},
		{name: "bfd multiplier too large", text: "localAS: 64512\nbfdProfiles: [{name: A, multiplier: 256}]"},

		{
			name: "prefix list",
			ok:   true,
			text: bgpPrefixRules(
				"{rule: 1, action: deny, prefix: 10.0.0.0/8, ge: 8, le: 32}",
				"{rule: 65535, action: permit, prefix: 0.0.0.0/0, le: 0}",
			),
			want: []string{
				"set policy prefix-list PL rule 1 action deny",
				"set policy prefix-list PL rule 1 prefix 10.0.0.0/8",
				"set policy prefix-list PL rule 1 ge 8",
				"set policy prefix-list PL rule 1 le 32",
				"set policy prefix-list PL rule 65535 le 0",
			},
		},
		{
			name: "prefix list ge equals le",
			text: bgpPrefixRules("{rule: 10, action: permit, prefix: 10.0.0.0/8, ge: 24, le: 24}"),
			ok:   true,
		},
		{name: "prefix list without rules", text: "localAS: 64512\nprefixLists: [{name: PL}]"},
		{
			name: "prefix list name with a dot",
			text: "localAS: 64512\nprefixLists: [{name: P.L, rules: [{rule: 10, action: permit, prefix: 10.0.0.0/8}]}]",
		},
		{
			name: "prefix list twice",
			text: "localAS: 64512\nprefixLists: [{name: PL, rules: [{rule: 10, action: permit, prefix: 10.0.0.0/8}]}, " +
				"{name: PL, rules: [{rule: 10, action: permit, prefix: 10.0.0.0/8}]}]",
		},
		{name: "prefix rule 0", text: bgpPrefixRules("{rule: 0, action: permit, prefix: 10.0.0.0/8}")},
		{name: "prefix rule too large", text: bgpPrefixRules("{rule: 65536, action: permit, prefix: 10.0.0.0/8}")},
		{name: "prefix rule without a number", text: bgpPrefixRules("{action: permit, prefix: 10.0.0.0/8}")},
		{
			name: "prefix rule twice",
			text: bgpPrefixRules("{rule: 10, action: permit, prefix: 10.0.0.0/8}", "{rule: 10, action: deny, prefix: 172.16.0.0/12}"),
		},
		{name: "prefix rule action accept", text: bgpPrefixRules("{rule: 10, action: accept, prefix: 10.0.0.0/8}")},
		{name: "prefix rule without an action", text: bgpPrefixRules("{rule: 10, prefix: 10.0.0.0/8}")},
		{name: "prefix rule without a prefix", text: bgpPrefixRules("{rule: 10, action: permit}")},
		{name: "prefix rule with host bits", text: bgpPrefixRules("{rule: 10, action: permit, prefix: 10.1.2.3/8}")},
		{name: "prefix rule is IPv6", text: bgpPrefixRules("{rule: 10, action: permit, prefix: '2001:db8::/32'}")},
		{name: "prefix rule ge below the length", text: bgpPrefixRules("{rule: 10, action: permit, prefix: 10.0.0.0/8, ge: 7}")},
		{name: "prefix rule le below the length", text: bgpPrefixRules("{rule: 10, action: permit, prefix: 10.0.0.0/8, le: 7}")},
		{name: "prefix rule le below ge", text: bgpPrefixRules("{rule: 10, action: permit, prefix: 10.0.0.0/8, ge: 24, le: 16}")},
		{name: "prefix rule le above 32", text: bgpPrefixRules("{rule: 10, action: permit, prefix: 10.0.0.0/8, le: 33}")},
		{name: "prefix rule ge above 32", text: bgpPrefixRules("{rule: 10, action: permit, prefix: 10.0.0.0/8, ge: 33}")},
		{name: "prefix rule misspelled key", text: bgpPrefixRules("{rule: 10, action: permit, prefix: 10.0.0.0/8, lte: 24}")},
		{
			// VyOS refuses a commit with two rules of one list that match the same.
			name: "prefix rules that are the same",
			text: bgpPrefixRules(
				"{rule: 10, action: permit, prefix: 10.0.0.0/8, le: 24}",
				"{rule: 20, action: permit, prefix: 10.0.0.0/8, le: 24}",
			),
		},
		{
			name: "prefix rules that differ in the action", ok: true,
			text: bgpPrefixRules("{rule: 10, action: deny, prefix: 0.0.0.0/0}", "{rule: 15, action: permit, prefix: 0.0.0.0/0, le: 24}"),
		},

		{
			name: "community list", text: bgpCommunityRule(`{rule: 10, action: deny, regex: "^(64512|64513):[0-9]+$ #;"}`), ok: true,
			want: []string{
				"set policy community-list CL rule 10 action deny",
				"set policy community-list CL rule 10 regex '^(64512|64513):[0-9]+$ #;'",
			},
		},
		{name: "community list without rules", text: "localAS: 64512\ncommunityLists: [{name: CL, rules: []}]"},
		{
			name: "community list name with a slash",
			text: "localAS: 64512\ncommunityLists: [{name: C/L, rules: [{rule: 10, action: permit, regex: x}]}]",
		},
		{
			name: "community list twice",
			text: "localAS: 64512\ncommunityLists: [{name: CL, rules: [{rule: 10, action: permit, regex: x}]}, " +
				"{name: CL, rules: [{rule: 10, action: permit, regex: y}]}]",
		},
		{name: "community rule 0", text: bgpCommunityRule("{rule: 0, action: permit, regex: x}")},
		{
			name: "community rule twice",
			text: bgpCommunityRule("{rule: 10, action: permit, regex: x}\n      - {rule: 10, action: permit, regex: y}"),
		},
		{name: "community rule action drop", text: bgpCommunityRule("{rule: 10, action: drop, regex: x}")},
		{name: "community rule without a regex", text: bgpCommunityRule("{rule: 10, action: permit}")},
		{name: "community rule with an empty regex", text: bgpCommunityRule(`{rule: 10, action: permit, regex: ""}`)},
		{name: "community regex with a single quote", text: bgpCommunityRule(`{rule: 10, action: permit, regex: "a'b"}`)},
		{name: "community regex in single quotes", text: bgpCommunityRule(`{rule: 10, action: permit, regex: "'ab'"}`)},
		{name: "community regex with a newline", text: bgpCommunityRule(`{rule: 10, action: permit, regex: "a\nb"}`)},

		{
			name: "route map", ok: true,
			text: bgpRouteMapRule(
				`match: {prefixList: PL, communityList: CL}, ` +
					`set: {localPreference: 4294967295, community: "65535:65535", src: 10.0.0.1, metric: 4294967295, asPathPrepend: [1, 4294967294]}`,
			),
			want: []string{
				"set policy route-map RM2 rule 10 action permit",
				"set policy route-map RM2 rule 10 match ip address prefix-list PL",
				"set policy route-map RM2 rule 10 match community community-list CL",
				"set policy route-map RM2 rule 10 set local-preference 4294967295",
				"set policy route-map RM2 rule 10 set community replace 65535:65535",
				"set policy route-map RM2 rule 10 set src 10.0.0.1",
				"set policy route-map RM2 rule 10 set metric 4294967295",
				"set policy route-map RM2 rule 10 set as-path prepend '1 4294967294'",
			},
		},
		{
			name: "route map zero values",
			text: bgpRouteMapRule(`set: {localPreference: 0, metric: 0, community: "0:0", asPathPrepend: []}`),
			ok:   true,
			want: []string{
				"set policy route-map RM2 rule 10 set local-preference 0",
				"set policy route-map RM2 rule 10 set metric 0",
				"set policy route-map RM2 rule 10 set community replace 0:0",
			},
		},
		{
			name: "route map well-known community", text: bgpRouteMapRule("set: {community: no-export}"), ok: true,
			want: []string{"set policy route-map RM2 rule 10 set community replace no-export"},
		},
		{
			name: "route map names of config lines", text: "localAS: 64512\nrouteMaps: [{name: RM, rules: " +
				"[{rule: 10, action: permit, match: {prefixList: NETS, communityList: TAGS}}]}]",
			ok: true,
			config: []string{
				"set policy prefix-list NETS rule 10 action permit",
				"set policy community-list TAGS rule 10 action permit",
			},
		},
		{name: "route map without rules", text: "localAS: 64512\nrouteMaps: [{name: RM}]"},
		{name: "route map name with a plus", text: "localAS: 64512\nrouteMaps: [{name: R+M, rules: [{rule: 10, action: permit}]}]"},
		{name: "route map rule too large", text: "localAS: 64512\nrouteMaps: [{name: RM, rules: [{rule: 65536, action: permit}]}]"},
		{
			name: "route map rule twice",
			text: "localAS: 64512\nrouteMaps: [{name: RM, rules: [{rule: 10, action: permit}, {rule: 10, action: deny}]}]",
		},
		{name: "route map rule without an action", text: "localAS: 64512\nrouteMaps: [{name: RM, rules: [{rule: 10}]}]"},
		{
			name: "route map prefix list not defined",
			text: "localAS: 64512\nrouteMaps: [{name: RM, rules: [{rule: 10, action: permit, match: {prefixList: PL}}]}]",
		},
		{
			name: "route map community list not defined",
			text: "localAS: 64512\nrouteMaps: [{name: RM, rules: [{rule: 10, action: permit, match: {communityList: CL}}]}]",
		},
		{name: "route map match unknown key", text: bgpRouteMapRule("match: {asPath: X}")},
		{name: "route map set unknown key", text: bgpRouteMapRule("set: {localPref: 200}")},
		{name: "route map negative local preference", text: bgpRouteMapRule("set: {localPreference: -1}")},
		{name: "route map local preference too large", text: bgpRouteMapRule("set: {localPreference: 4294967296}")},
		{name: "route map negative metric", text: bgpRouteMapRule("set: {metric: -1}")},
		{name: "route map metric too large", text: bgpRouteMapRule("set: {metric: 4294967296}")},
		{name: "route map community internet", text: bgpRouteMapRule("set: {community: internet}")},
		{name: "route map community with one number", text: bgpRouteMapRule(`set: {community: "64512"}`)},
		{name: "route map community number too large", text: bgpRouteMapRule(`set: {community: "65536:1"}`)},
		{name: "route map community with a sign", text: bgpRouteMapRule(`set: {community: "+1:1"}`)},
		{name: "route map two communities", text: bgpRouteMapRule(`set: {community: "64512:1 64512:2"}`)},
		{name: "route map community with a command", text: bgpRouteMapRule(`set: {community: "64512:1;reboot"}`)},
		{name: "route map src is IPv6", text: bgpRouteMapRule("set: {src: '2001:db8::1'}")},
		{name: "route map src is a prefix", text: bgpRouteMapRule("set: {src: 10.0.0.0/8}")},
		{name: "route map prepend AS 0", text: bgpRouteMapRule("set: {asPathPrepend: [0]}")},
		{name: "route map prepend reserved AS", text: bgpRouteMapRule("set: {asPathPrepend: [64512, 4294967295]}")},
		{name: "route map prepend is text", text: bgpRouteMapRule("set: {asPathPrepend: '64512 64512'}")},
	}
}

// TestBGPLines gives each rule of the `bgp` metadata a block that it accepts
// and a block that it refuses. Every refusal wraps ErrBGPConfig.
func TestBGPLines(t *testing.T) {
	t.Parallel()

	for _, tt := range slices.Concat(bgpTopLevelCases(), bgpNeighborCases(), bgpListCases()) {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := bgpLines(bgpMetadata(t, tt.text), osTypeVyos, bgpTestNets(), tt.config)
			if !tt.ok {
				if !errors.Is(err, ErrBGPConfig) {
					t.Fatalf("bgpLines(%q) = %q, %v, want ErrBGPConfig", tt.text, got, err)
				}

				return
			}

			if err != nil {
				t.Fatalf("bgpLines(%q) error = %v, want nil", tt.text, err)
			}

			for _, want := range tt.want {
				if !slices.Contains(got, want) {
					t.Errorf("bgpLines(%q) has no line %q:\n%s", tt.text, want, strings.Join(got, "\n"))
				}
			}
		})
	}
}

// TestBGPLinesNamesPerKind checks that names are unique for each kind only:
// a prefix list and a route map can have one name, and two route maps cannot.
func TestBGPLinesNamesPerKind(t *testing.T) {
	t.Parallel()

	const (
		rule   = "rules: [{rule: 10, action: permit}]"
		prefix = "rules: [{rule: 10, action: permit, prefix: 10.0.0.0/8}]"
	)

	shared := "localAS: 64512\nbfdProfiles: [{name: X}]\nprefixLists: [{name: X, " + prefix + "}]\n" +
		"communityLists: [{name: X, rules: [{rule: 10, action: permit, regex: x}]}]\nrouteMaps: [{name: X, " + rule + "}]\n"

	_, err := bgpLines(bgpMetadata(t, shared), osTypeVyos, nil, nil)
	if err != nil {
		t.Errorf("bgpLines() with one name for four kinds error = %v, want nil", err)
	}

	twice := "localAS: 64512\nrouteMaps: [{name: X, " + rule + "}, {name: X, " + rule + "}]\n"

	_, err = bgpLines(bgpMetadata(t, twice), osTypeVyos, nil, nil)
	if !errors.Is(err, ErrBGPConfig) {
		t.Errorf("bgpLines() with two route maps of one name error = %v, want ErrBGPConfig", err)
	}
}

// TestBGPLinesAbsent checks that metadata with no `bgp` key gives no lines,
// for any os_type.
func TestBGPLinesAbsent(t *testing.T) {
	t.Parallel()

	for _, osType := range []string{osTypeVyos, "vyatta", "minirouter"} {
		got, err := bgpLines(map[string]any{"config": []any{"set system host-name a"}}, osType, bgpTestNets(), nil)
		if got != nil || err != nil {
			t.Errorf("bgpLines() without bgp on %s = %q, %v, want nil, nil", osType, got, err)
		}
	}
}

// TestBGPLinesRenderers checks that `bgp` is refused for an os_type with no
// renderer, and that the message lists the os_types that have one. The text is
// checked because it is what tells the author which routers can take `bgp`.
func TestBGPLinesRenderers(t *testing.T) {
	t.Parallel()

	md := bgpMetadata(t, "localAS: 64512")

	for _, osType := range []string{"vyatta", "minirouter", "linux", ""} {
		_, err := bgpLines(md, osType, nil, nil)
		if !errors.Is(err, ErrBGPConfig) {
			t.Errorf("bgpLines() on os_type %q error = %v, want ErrBGPConfig", osType, err)

			continue
		}

		for supported := range bgpRenderers {
			if !strings.Contains(err.Error(), supported) {
				t.Errorf("bgpLines() error = %q, want it to name the supported os_type %s", err, supported)
			}
		}
	}

	if _, err := bgpLines(md, "VyOS", nil, nil); err != nil {
		t.Errorf("bgpLines() on os_type VyOS error = %v, want nil", err)
	}

	if _, ok := bgpRenderers[osTypeVyos]; !ok || len(bgpRenderers) != 1 {
		t.Errorf("bgpRenderers has %d renderers, want one, for %s", len(bgpRenderers), osTypeVyos)
	}
}

// TestBGPLinesErrorsNameThePlace checks that an error says which neighbor,
// list or rule is wrong. The text is checked because it is how the author of a
// long block finds the mistake.
func TestBGPLinesErrorsNameThePlace(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		text string
		want []string
	}{
		{name: "neighbor", text: bgpNeighbor("ebgpMultihop: 0"), want: []string{"neighbor 198.18.0.1", "ebgpMultihop"}},
		{
			name: "neighbor reference",
			text: bgpNeighbor("import: {routeMap: NOPE}"),
			want: []string{"neighbor 198.18.0.1", "route map NOPE"},
		},
		{
			name: "prefix list rule", text: bgpPrefixRules("{rule: 20, action: permit, prefix: 10.1.2.3/8}"),
			want: []string{"prefix list PL", "rule 20", "10.0.0.0/8"},
		},
		{
			name: "community list rule",
			text: bgpCommunityRule("{rule: 30, action: allow, regex: x}"),
			want: []string{"community list CL", "rule 30"},
		},
		{
			name: "route map rule",
			text: bgpRouteMapRule("match: {prefixList: NOPE}"),
			want: []string{"route map RM2", "rule 10", "prefix list NOPE"},
		},
		{
			name: "bfd profile",
			text: "localAS: 64512\nbfdProfiles: [{name: SLOW, multiplier: 1}]",
			want: []string{"BFD profile SLOW", "multiplier"},
		},
		{name: "network", text: "localAS: 64512\nnetworks: [10.1.2.3/8]", want: []string{"10.1.2.3/8", "10.0.0.0/8"}},
		{name: "IPv6", text: "localAS: 64512\nnetworks: ['2001:db8::/32']", want: []string{"only IPv4 unicast is supported"}},
		{
			name: "IPv6 neighbor",
			text: "localAS: 64512\nneighbors: [{address: '2001:db8::1', remoteAS: 1}]",
			want: []string{"only IPv4 unicast is supported"},
		},
		{name: "unknown key", text: "localAS: 64512\nrouterIdentifier: 10.0.0.1", want: []string{"routerIdentifier"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := bgpLines(bgpMetadata(t, tt.text), osTypeVyos, bgpTestNets(), nil)
			if !errors.Is(err, ErrBGPConfig) {
				t.Fatalf("bgpLines(%q) error = %v, want ErrBGPConfig", tt.text, err)
			}

			for _, want := range tt.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("bgpLines(%q) error = %q, want it to contain %q", tt.text, err, want)
				}
			}
		})
	}
}

// TestBGPLinesErrorsHidePassword checks that no error repeats the password of
// a neighbor, whatever is wrong with the password or with the rest.
func TestBGPLinesErrorsHidePassword(t *testing.T) {
	t.Parallel()

	const secret = "s3cretKEY"

	for _, keys := range []string{
		`password: "` + secret + `'"`,
		`password: "` + secret + ` x"`,
		`password: "` + secret + `\n"`,
		"password: " + secret + strings.Repeat("x", 80),
		"password: [" + secret + "]",
		"password: {" + secret + ": 1}",
		"password: " + secret + "\n    ebgpMultihop: 0",
		"password: " + secret + "\n    import: {routeMap: NOPE}",
		"password: " + secret + "\n    description: " + strings.Repeat("d", 300),
	} {
		_, err := bgpLines(bgpMetadata(t, bgpNeighbor(keys)), osTypeVyos, bgpTestNets(), nil)
		if !errors.Is(err, ErrBGPConfig) {
			t.Errorf("bgpLines() with neighbor keys %q error = %v, want ErrBGPConfig", keys, err)
		} else if strings.Contains(err.Error(), secret) {
			t.Errorf("bgpLines() error = %q, want it not to contain the password", err)
		}
	}
}

// TestBGPLinesJSONNumbers checks that a block decoded from JSON, where every
// number is a float64, gives the lines of the same block decoded from YAML.
func TestBGPLinesJSONNumbers(t *testing.T) {
	t.Parallel()

	fromYAML := bgpFixture(t, "bgp-all")

	body, err := json.Marshal(fromYAML)
	if err != nil {
		t.Fatal(err)
	}

	var fromJSON map[string]any

	err = json.Unmarshal(body, &fromJSON)
	if err != nil {
		t.Fatal(err)
	}

	want, err := bgpLines(fromYAML, osTypeVyos, bgpTestNets(), nil)
	if err != nil || len(want) == 0 {
		t.Fatalf("bgpLines(YAML) = %d lines, %v, want lines and no error", len(want), err)
	}

	got, err := bgpLines(fromJSON, osTypeVyos, bgpTestNets(), nil)
	if err != nil {
		t.Fatalf("bgpLines(JSON) error = %v", err)
	}

	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("bgpLines(JSON) mismatch (-YAML +JSON):\n%s", diff)
	}
}

// TestVyosConfigNames checks which `config` lines define a name that the
// `bgp` block can use.
func TestVyosConfigNames(t *testing.T) {
	t.Parallel()

	got := vyosConfigNames([]string{
		"set policy route-map RM rule 10 action permit",
		"set policy route-map RM rule 10 set local-preference 200",
		"set policy prefix-list PL rule 10 prefix 10.0.0.0/8",
		"set policy community-list CL rule 10 regex '_64512:1_'",
		"set protocols bfd profile BFD interval multiplier 3",
		"set policy route-map ALONE",
		"set policy prefix-list6 PL6 rule 10 prefix 2001:db8::/32",
		"set policy as-path-list AP rule 10 regex '^64512_'",
		"set protocols bfd peer 10.0.0.1 profile PEER",
		"delete policy route-map GONE rule 10",
		"set policy route-map  SPACED rule 10 action permit",
	})

	want := bgpNames{
		routeMaps:      map[string]bool{"RM": true},
		prefixLists:    map[string]bool{"PL": true},
		communityLists: map[string]bool{"CL": true},
		bfdProfiles:    map[string]bool{"BFD": true},
	}

	if diff := cmp.Diff(want, got, cmp.AllowUnexported(bgpNames{})); diff != "" {
		t.Errorf("vyosConfigNames() mismatch (-want +got):\n%s", diff)
	}
}

// unsetBGPFields returns the paths of the fields below v that have their zero
// value. Of a list it looks at the first item only.
func unsetBGPFields(v reflect.Value, path string) []string {
	switch v.Kind() { //nolint:exhaustive // the default case takes every kind that holds no other value
	case reflect.Pointer:
		if v.IsNil() {
			return []string{path}
		}

		// A pointer says whether the key is set, so the value it points to can be zero.
		if v.Elem().Kind() != reflect.Struct {
			return nil
		}

		return unsetBGPFields(v.Elem(), path)
	case reflect.Struct:
		var unset []string

		for i := range v.NumField() {
			unset = append(unset, unsetBGPFields(v.Field(i), path+"."+v.Type().Field(i).Name)...)
		}

		return unset
	case reflect.Slice:
		if v.Len() == 0 {
			return []string{path}
		}

		return unsetBGPFields(v.Index(0), path+"[0]")
	default:
		if v.IsZero() {
			return []string{path}
		}

		return nil
	}
}

// TestBGPFixtureSetsEveryField checks that bgp-all.yaml sets every field of
// BGPConfig, so that the golden test below renders every field. A new field
// fails here until the fixture and its lines have it.
func TestBGPFixtureSetsEveryField(t *testing.T) {
	t.Parallel()

	cfg, err := decodeBGPConfig(bgpFixture(t, "bgp-all")["bgp"])
	if err != nil {
		t.Fatalf("decodeBGPConfig(bgp-all.yaml) error = %v", err)
	}

	if unset := unsetBGPFields(reflect.ValueOf(cfg), "bgp"); len(unset) != 0 {
		t.Errorf("bgp-all.yaml does not set %s", strings.Join(unset, ", "))
	}
}

// TestVyosBGPLinesGolden renders the fixture that sets every field, and
// compares the lines and their order with the lines written by hand from the
// VyOS interface definitions.
func TestVyosBGPLinesGolden(t *testing.T) {
	t.Parallel()

	got, err := bgpLines(bgpFixture(t, "bgp-all"), osTypeVyos, bgpTestNets(), nil)
	if err != nil {
		t.Fatalf("bgpLines(bgp-all.yaml) error = %v", err)
	}

	if diff := cmp.Diff(bgpFixtureLines(t, "bgp-all"), got); diff != "" {
		t.Errorf("bgpLines(bgp-all.yaml) mismatch (-want +got):\n%s", diff)
	}
}

// TestVyosBGPLinesParity compares the renderer with lines that ran on VyOS
// routers. The .lines files hold what the WAN generator of the CherryMountain
// topology (tools/wangen/wangen/vyos.py in sceptre-phenix-topologies) writes
// for an ISP and for the edge of the head office: every line of the router's
// `config` list that starts with `set policy`, `set protocols`, or
// `set system ip protocol`. The .yaml files hold the same routers as `bgp`
// metadata. The renderer must give the same set of lines, in any order, but
// for the lines listed here, which the model does not cover on purpose.
func TestVyosBGPLinesParity(t *testing.T) {
	t.Parallel()

	const (
		static = "a static route: the topology's routes or `config` carry it, and `bgp` holds only BGP and its policy"
		zebra  = "it applies a route map to the routes that BGP gives the kernel, which is a setting of the system and not of BGP"
	)

	tests := []struct {
		host       string
		notCovered map[string]string // line -> why the model leaves it out
	}{
		{host: "tumbleweed", notCovered: map[string]string{
			"set protocols static route 198.18.101.0/24 blackhole distance 250": static,
		}},
		{host: "fr-fw-edge", notCovered: map[string]string{
			"set protocols static route 198.18.105.0/24 blackhole distance 250": static,
			"set protocols static route 10.10.0.0/16 blackhole distance 250":    static,
			"set protocols static route 10.20.0.0/16 blackhole distance 250":    static,
			"set protocols static route 10.30.0.0/16 blackhole distance 250":    static,
			"set protocols static route 10.40.0.0/16 blackhole distance 250":    static,
			"set protocols static route 10.0.0.0/8 blackhole distance 254":      static,
			"set protocols static route 172.16.0.0/12 blackhole distance 254":   static,
			"set protocols static route 192.168.0.0/16 blackhole distance 254":  static,
			"set system ip protocol bgp route-map SET-SOURCE":                   zebra,
		}},
	}

	for _, tt := range tests {
		t.Run(tt.host, func(t *testing.T) {
			t.Parallel()

			var want []string

			all := bgpFixtureLines(t, "bgp-"+tt.host)

			for _, line := range all {
				if _, ok := tt.notCovered[line]; !ok {
					want = append(want, line)
				}
			}

			for line := range tt.notCovered {
				if !slices.Contains(all, line) {
					t.Errorf("bgp-%s.lines has no line %q, which the test lists as not covered", tt.host, line)
				}
			}

			got, err := bgpLines(bgpFixture(t, "bgp-"+tt.host), osTypeVyos, nil, nil)
			if err != nil {
				t.Fatalf("bgpLines(bgp-%s.yaml) error = %v", tt.host, err)
			}

			slices.Sort(want)
			slices.Sort(got)

			if diff := cmp.Diff(slices.Compact(want), slices.Compact(got)); diff != "" {
				t.Errorf("bgpLines(bgp-%s.yaml) is not the set of lines of bgp-%s.lines (-want +got):\n%s", tt.host, tt.host, diff)
			}
		})
	}
}

// TestVyosBGPLinesAreConfigLines checks that every line of every fixture is a
// line that the `config` list would take.
func TestVyosBGPLinesAreConfigLines(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"bgp-all", "bgp-tumbleweed", "bgp-fr-fw-edge"} {
		got, err := bgpLines(bgpFixture(t, name), osTypeVyos, bgpTestNets(), nil)
		if err != nil {
			t.Errorf("bgpLines(%s.yaml) error = %v", name, err)
		}

		if len(got) == 0 {
			t.Errorf("bgpLines(%s.yaml) gave no lines", name)
		}

		for _, line := range got {
			if err := checkVyosConfigLine(line); err != nil {
				t.Errorf("checkVyosConfigLine(%q) of %s.yaml = %v, want nil", line, name, err)
			}
		}
	}
}

// TestVyosBGPLinesGuard checks the second guard of the renderer. A block that
// was not checked, with a text that would end its single quotes, gives no
// lines, and the error does not hold the password.
func TestVyosBGPLinesGuard(t *testing.T) {
	t.Parallel()

	password := "s3cretKEY';reboot;'"

	for name, neighbor := range map[string]BGPNeighbor{
		"description": {Address: "198.18.0.1", RemoteAS: 64500, Description: "a'; reboot; '"},
		"password":    {Address: "198.18.0.1", RemoteAS: 64500, Password: &password},
		"address":     {Address: "198.18.0.1; reboot", RemoteAS: 64500},
		"route map":   {Address: "198.18.0.1", RemoteAS: 64500, Import: BGPNeighborPolicy{RouteMap: "$(reboot)"}},
	} {
		got, err := vyosBGPLines(BGPConfig{LocalAS: 64512, Neighbors: []BGPNeighbor{neighbor}})
		if got != nil || !errors.Is(err, ErrVyosConfigLine) {
			t.Errorf("vyosBGPLines() with an unchecked %s = %q, %v, want nil, ErrVyosConfigLine", name, got, err)
		} else if strings.Contains(err.Error(), "s3cretKEY") {
			t.Errorf("vyosBGPLines() error = %q, want it not to contain the password", err)
		}
	}
}

// TestBGPConfigCheckWithoutNames checks that a block is checked against its
// own names when nothing else defines one.
func TestBGPConfigCheckWithoutNames(t *testing.T) {
	t.Parallel()

	cfg, err := decodeBGPConfig(bgpFixture(t, "bgp-all")["bgp"])
	if err != nil {
		t.Fatalf("decodeBGPConfig(bgp-all.yaml) error = %v", err)
	}

	if err := cfg.check(bgpTestNets(), bgpNames{}); err != nil {
		t.Errorf("check() of bgp-all.yaml error = %v, want nil", err)
	}

	cfg.Neighbors[0].Import.RouteMap = "NOPE"

	if err := cfg.check(bgpTestNets(), bgpNames{}); err == nil {
		t.Error("check() with a route map that nothing defines error = nil, want an error")
	}
}

// bgpTextMetadata returns `bgp` metadata that puts text into each value that
// the renderer writes in single quotes.
func bgpTextMetadata(description, password, regex string) map[string]any {
	return map[string]any{"bgp": map[string]any{
		"localAS": 64512,
		"neighbors": []any{map[string]any{
			"address": "198.18.0.1", "remoteAS": 64500, "description": description, "password": password,
		}},
		"communityLists": []any{map[string]any{
			"name":  "CL",
			"rules": []any{map[string]any{"rule": 10, "action": "permit", "regex": regex}},
		}},
	}}
}

// bgpTextLines returns the lines that carry the texts of bgpTextMetadata: the
// regex, then the description, then the password.
func bgpTextLines(lines []string) []string {
	var found []string

	for _, line := range lines {
		for _, word := range []string{" description ", " password ", " regex "} {
			if strings.Contains(line, word) {
				found = append(found, line)
			}
		}
	}

	return found
}

// TestVyosBGPLinesTextMatchesBash puts words a shell acts on into the
// description, the password and the regex. bash must read every line of a
// block that is accepted as one command, with the text as its last word.
func TestVyosBGPLinesTextMatchesBash(t *testing.T) {
	t.Parallel()
	testenv.RequireCommands(t, "bash")

	dir := vrouterConfigBashDir(t)
	words := append(vrouterConfigSpecial(), "a", "b1", "é", "-x", "--", "a=b")

	rapid.Check(t, func(t *rapid.T) {
		text := func(label string, sep string) string {
			return strings.Join(rapid.SliceOfN(rapid.SampledFrom(words), 1, 4).Draw(t, label), sep)
		}

		description, password, regex := text("description", " "), text("password", ""), text("regex", " ")

		lines, err := bgpLines(bgpTextMetadata(description, password, regex), osTypeVyos, nil, nil)
		if err != nil {
			if !errors.Is(err, ErrBGPConfig) {
				t.Fatalf("bgpLines() error = %v, want ErrBGPConfig", err)
			}

			return
		}

		found := bgpTextLines(lines)
		if len(found) != 3 {
			t.Fatalf("bgpLines() gave %d lines with a text, want 3: %q", len(found), found)
		}

		for i, want := range []string{regex, description, password} {
			if got := vrouterConfigWords(found[i]); got[len(got)-1] != want {
				t.Fatalf("last word of %q = %q, want %q", found[i], got[len(got)-1], want)
			}

			diff, err := vrouterConfigBashDiff(dir, found[i])
			if err != nil {
				t.Fatalf("accepted text: %v", err)
			}

			if diff != "" {
				t.Fatalf("bash words of %q (-want +got):\n%s", found[i], diff)
			}
		}
	})
}

// bgpRouterMetadata is `bgp` metadata for the router of vrouterNode.
const bgpRouterMetadata = `localAS: 64512
routerID: 10.0.0.1
neighbors:
  - address: 192.168.1.2
    remoteAS: 64500
    updateSource: eth1
    import: {routeMap: FROM-ISP}
`

// TestVrouterPreStartBGP checks that the boot script of a VyOS router has the
// rendered `bgp` metadata after the system section and before the extra
// config, so that a `config` line can change what `bgp` wrote, and that a name
// defined only by a `config` line and an interface name are resolved.
func TestVrouterPreStartBGP(t *testing.T) {
	t.Parallel()

	md := bgpMetadata(t, bgpRouterMetadata)
	md["config"] = []any{
		"set policy route-map FROM-ISP rule 10 action permit",
		"delete protocols bgp neighbor 192.168.1.2 update-source",
	}

	got := renderVrouter(t, vrouterExperiment(t, vrouterScenario(vrouterApp(md)), vrouterNode("rtr", osTypeVyos)))

	want := "set system host-name rtr\n" +
		"# ---------------------------------- BGP ----------------------------------\n" +
		"set protocols bgp system-as 64512\n" +
		"set protocols bgp parameters router-id 10.0.0.1\n" +
		"set protocols bgp neighbor 192.168.1.2 remote-as 64500\n" +
		"set protocols bgp neighbor 192.168.1.2 update-source 192.168.1.1\n" +
		"set protocols bgp neighbor 192.168.1.2 address-family ipv4-unicast route-map import FROM-ISP\n" +
		"# ------------------------------ Extra config -----------------------------\n" +
		"set policy route-map FROM-ISP rule 10 action permit\n" +
		"delete protocols bgp neighbor 192.168.1.2 update-source\n" +
		"commit\n" +
		"save\n" +
		"exit"

	if !strings.HasSuffix(strings.TrimRight(got, "\n"), want) {
		t.Errorf("rendered config does not end with the system section, BGP, the extra config and the commit:\n%s", got)
	}

	const width = len("# --------------------------------- System --------------------------------")
	for line := range strings.SplitSeq(got, "\n") {
		if strings.HasPrefix(line, "# -") && len(line) != width {
			t.Errorf("section comment %q is %d characters wide, want %d", line, len(line), width)
		}
	}
}

// TestVrouterPreStartBGPWithoutConfig checks that the BGP section comes before
// the commit when the router has no extra config.
func TestVrouterPreStartBGPWithoutConfig(t *testing.T) {
	t.Parallel()

	md := bgpMetadata(t, "localAS: 64512")
	got := renderVrouter(t, vrouterExperiment(t, vrouterScenario(vrouterApp(md)), vrouterNode("rtr", osTypeVyos)))

	want := "# ---------------------------------- BGP ----------------------------------\n" +
		"set protocols bgp system-as 64512\n" +
		"commit\n" +
		"save\n" +
		"exit"

	if !strings.HasSuffix(strings.TrimRight(got, "\n"), want) {
		t.Errorf("rendered config does not end with the BGP section and the commit:\n%s", got)
	}
}

// TestVrouterPreStartBGPAbsent checks that a router without `bgp` metadata
// gets no BGP section, whether or not the scenario names the router.
func TestVrouterPreStartBGPAbsent(t *testing.T) {
	t.Parallel()

	for _, scenario := range []*v2.ScenarioSpec{nil, vrouterScenario(vrouterApp(map[string]any{}))} {
		for _, osType := range []string{osTypeVyos, "vyatta"} {
			got := renderVrouter(t, vrouterExperiment(t, scenario, vrouterNode("rtr", osType)))
			if strings.Contains(got, " BGP ") || strings.Contains(got, "protocols bgp") {
				t.Errorf("rendered %s config has BGP without bgp metadata:\n%s", osType, got)
			}
		}
	}
}

// TestVrouterPreStartBGPHosts checks that `bgp` is refused on a host that
// PreStart does not render with a BGP renderer: PreStart would drop the block
// without a word.
func TestVrouterPreStartBGPHosts(t *testing.T) {
	t.Parallel()

	external := true

	tests := []struct {
		name string
		node func() *v1.Node
	}{
		{name: "no node of that name", node: func() *v1.Node { return vrouterNode("other", osTypeVyos) }},
		{name: "not a router or firewall", node: func() *v1.Node {
			n := vrouterNode("rtr", osTypeVyos)
			n.TypeF = "VirtualMachine"

			return n
		}},
		{name: "external", node: func() *v1.Node {
			n := vrouterNode("rtr", osTypeVyos)
			n.ExternalF = &external

			return n
		}},
		{name: "default apps off", node: func() *v1.Node {
			n := vrouterNode("rtr", osTypeVyos)
			n.AnnotationsF = map[string]any{defaultAppsAnnotation: false}

			return n
		}},
		{name: "vyatta", node: func() *v1.Node { return vrouterNode("rtr", "vyatta") }},
		{name: "minirouter", node: func() *v1.Node { return vrouterNode("rtr", "minirouter") }},
		{name: "linux", node: func() *v1.Node { return vrouterNode("rtr", osLinux) }},
		{name: "unknown os_type", node: func() *v1.Node { return vrouterNode("rtr", "junos") }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			exp := vrouterExperiment(t, vrouterScenario(vrouterApp(bgpMetadata(t, "localAS: 64512"))), tt.node())

			var v Vrouter

			err := v.PreStart(t.Context(), exp)
			if !errors.Is(err, ErrBGPConfig) {
				t.Errorf("PreStart() error = %v, want ErrBGPConfig", err)
			}

			if errors.Is(err, ErrVyosConfigLine) {
				t.Errorf("PreStart() error = %v, want it not to be ErrVyosConfigLine", err)
			}
		})
	}
}

// TestVrouterPreStartBGPFirewall checks that a VyOS firewall takes `bgp` as a
// VyOS router does.
func TestVrouterPreStartBGPFirewall(t *testing.T) {
	t.Parallel()

	node := vrouterNode("rtr", "VyOS")
	node.TypeF = "Firewall"

	md := bgpMetadata(t, "localAS: 64512")
	got := renderVrouter(t, vrouterExperiment(t, vrouterScenario(vrouterApp(md)), node))

	if !strings.Contains(got, "\nset protocols bgp system-as 64512\n") {
		t.Errorf("rendered config of a VyOS firewall has no BGP:\n%s", got)
	}
}

// TestVrouterPreStartBGPErrors checks that PreStart refuses a `bgp` block that
// is not valid for the router, and names the host.
func TestVrouterPreStartBGPErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		text   string
		config []any
	}{
		{name: "not valid", text: "localAS: 0"},
		{name: "unknown key", text: "localAS: 64512\nneighbours: []"},
		{name: "route map not defined", text: bgpRouterMetadata},
		{
			name: "route map of another name in config", text: bgpRouterMetadata,
			config: []any{"set policy route-map FROM-ISP2 rule 10 action permit"},
		},
		{
			name: "interface of another router",
			text: "localAS: 64512\nneighbors: [{address: 192.168.1.2, remoteAS: 64500, updateSource: WAN0}]",
		},
		{name: "the router's own address", text: "localAS: 64512\nneighbors: [{address: 192.168.1.1, remoteAS: 64500}]"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			md := bgpMetadata(t, tt.text)
			if tt.config != nil {
				md["config"] = tt.config
			}

			exp := vrouterExperiment(t, vrouterScenario(vrouterApp(md)), vrouterNode("rtr", osTypeVyos))

			var v Vrouter

			err := v.PreStart(t.Context(), exp)
			if !errors.Is(err, ErrBGPConfig) {
				t.Fatalf("PreStart() with bgp %q error = %v, want ErrBGPConfig", tt.text, err)
			}

			if !strings.Contains(err.Error(), "host rtr") {
				t.Errorf("PreStart() error = %q, want it to name the host rtr", err)
			}
		})
	}
}

// vrouterDocPath is the page that documents the vrouter app.
const vrouterDocPath = "../../../docs/content/apps.md"

// vrouterDocExample returns the YAML example of vrouterDocPath that has the
// given title, without its indentation.
func vrouterDocExample(t *testing.T, title string) map[string]any {
	t.Helper()

	page, err := os.ReadFile(vrouterDocPath)
	if err != nil {
		t.Fatal(err)
	}

	_, after, found := strings.Cut(string(page), "``` yaml title=\""+title+"\"\n")
	if !found {
		t.Fatalf("%s has no YAML example with the title %q", vrouterDocPath, title)
	}

	var (
		body   strings.Builder
		indent = -1
	)

	for line := range strings.Lines(after) {
		if strings.TrimSpace(line) == "```" {
			break
		}

		if indent < 0 {
			indent = len(line) - len(strings.TrimLeft(line, " "))
		}

		body.WriteString(line[min(indent, len(line)-1):])
	}

	var example map[string]any

	err = yaml.Unmarshal([]byte(body.String()), &example)
	if err != nil {
		t.Fatalf("yaml.Unmarshal(example %q of %s) error = %v", title, vrouterDocPath, err)
	}

	return example
}

// TestVrouterDocsBGPExamples checks that the examples of the vrouter app in
// the docs are metadata that a VyOS router with the interfaces eth0 and eth1
// takes.
func TestVrouterDocsBGPExamples(t *testing.T) {
	t.Parallel()

	full := vrouterDocExample(t, "vrouter example")

	var doc struct {
		Spec struct {
			Apps []struct {
				Hosts []struct {
					Metadata map[string]any `mapstructure:"metadata"`
				} `mapstructure:"hosts"`
			} `mapstructure:"apps"`
		} `mapstructure:"spec"`
	}

	err := mapstructure.Decode(full, &doc)
	if err != nil || len(doc.Spec.Apps) != 1 || len(doc.Spec.Apps[0].Hosts) != 1 {
		t.Fatalf("the vrouter example of %s is not one app with one host (error = %v)", vrouterDocPath, err)
	}

	examples := map[string]map[string]any{
		"vrouter example":        doc.Spec.Apps[0].Hosts[0].Metadata,
		"vrouter bgp example":    vrouterDocExample(t, "vrouter bgp example"),
		"vrouter config example": vrouterDocExample(t, "vrouter config example"),
	}

	for title, md := range examples {
		config, err := vyosConfigLines(md, true)
		if err != nil {
			t.Errorf("vyosConfigLines(%s) error = %v, want nil", title, err)
		}

		lines, err := bgpLines(md, osTypeVyos, vrouterNetIfaces(), config)
		if err != nil {
			t.Errorf("bgpLines(%s) error = %v, want nil", title, err)
		}

		if _, ok := md["bgp"]; ok == (len(lines) == 0) {
			t.Errorf("bgpLines(%s) gave %d lines, and the example has bgp: %t", title, len(lines), ok)
		}
	}
}

// bgpKeys returns the keys of the `bgp` metadata: the mapstructure tags of typ
// and of the types of its fields.
func bgpKeys(typ reflect.Type) []string {
	for typ.Kind() == reflect.Pointer || typ.Kind() == reflect.Slice {
		typ = typ.Elem()
	}

	if typ.Kind() != reflect.Struct {
		return nil
	}

	var keys []string

	for i := range typ.NumField() {
		keys = append(keys, typ.Field(i).Tag.Get("mapstructure"))
		keys = append(keys, bgpKeys(typ.Field(i).Type)...)
	}

	return keys
}

// TestVrouterDocsListBGPKeys checks that the docs name every key of the `bgp`
// metadata, and that the full example uses every key.
func TestVrouterDocsListBGPKeys(t *testing.T) {
	t.Parallel()

	page, err := os.ReadFile(vrouterDocPath)
	if err != nil {
		t.Fatal(err)
	}

	_, example, _ := strings.Cut(string(page), "``` yaml title=\"vrouter example\"\n")
	example, options, _ := strings.Cut(example, "```")

	keys := bgpKeys(reflect.TypeFor[BGPConfig]())
	if len(keys) < 50 {
		t.Fatalf("bgpKeys(BGPConfig) found %d keys, want all of them", len(keys))
	}

	for _, key := range keys {
		if !strings.Contains(options, "`"+key+"`") {
			t.Errorf("the vrouter options in %s do not name the bgp key %s", vrouterDocPath, key)
		}

		if !strings.Contains(example, " "+key+":") {
			t.Errorf("the vrouter example in %s does not use the bgp key %s", vrouterDocPath, key)
		}
	}
}
