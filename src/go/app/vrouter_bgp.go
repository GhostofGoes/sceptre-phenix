package app

import (
	"errors"
	"fmt"
	"maps"
	"math"
	"net/netip"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/mitchellh/mapstructure"

	ifaces "phenix/types/interfaces"
)

// The `bgp` metadata of a vrouter host has three parts, so that a new kind of
// router needs only the third:
//
//  1. The model ([BGPConfig] and the types it holds) has no word of any
//     router's configuration language. decodeBGPConfig reads it and refuses a
//     key that it does not know.
//  2. [BGPConfig.check] refuses a block that no router could run: a value out
//     of range, an address that is not IPv4, a name that nothing defines.
//  3. A renderer turns a checked block into the configuration of one os_type.
//     bgpRenderers holds one for each os_type that has one. It may refuse
//     what its own router cannot take.
//
// bgpLines runs the three for one router. Every error it returns wraps
// [ErrBGPConfig].

// Limits of the model. The VyOS interface definitions (vyos-1x, branch
// current) and FRR agree on them.
const (
	bgpMaxAS            = math.MaxUint32 - 1 // 4294967295 is reserved (RFC 7300)
	bgpMaxPaths         = 256
	bgpMaxMultihop      = 255
	bgpMinHoldtime      = 3 // seconds; 0 turns the hold timer off (RFC 4271)
	bgpMinBFDInterval   = 10
	bgpMaxBFDInterval   = 60000
	bgpMinBFDMultiplier = 2
	bgpMaxBFDMultiplier = 255
	bgpMaxPrefixLen     = 32
	bgpMaxPasswordLen   = 80 // bytes of a TCP MD5 key on Linux (TCP_MD5SIG_MAXKEYLEN)
	bgpMaxWholeFloat    = 1 << 53
)

// Limits of VyOS only (vyos-1x, branch current).
const (
	vyosMaxBFDProfileName = 32  // interface-definitions/protocols_bfd.xml.in
	vyosMaxDescription    = 255 // interface-definitions/include/generic-description.xml.i
)

// The kinds of name that one part of a `bgp` block defines and another uses.
const (
	bgpKindRouteMap      = "route map"
	bgpKindPrefixList    = "prefix list"
	bgpKindCommunityList = "community list"
	bgpKindBFDProfile    = "BFD profile"
)

const (
	bgpPermit = "permit"
	bgpDeny   = "deny"
)

// bgpNamePattern matches the name of a BFD profile, a prefix list, a community
// list, or a route map. Every router this app configures takes such a name
// unquoted.
var bgpNamePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]*$`)

// bgpRenderers holds, for each os_type that can take `bgp` metadata, the
// function that writes a checked block as that router's configuration. To
// support another router (Vyatta, minirouter, FRR on Linux), add its function
// here and print the lines it returns in the router's template or commands.
var bgpRenderers = map[string]func(BGPConfig) ([]string, error){ //nolint:gochecknoglobals // registry of renderers
	osTypeVyos: vyosBGPLines,
}

// BGPConfig is the `bgp` metadata of a vrouter host: one BGP speaker for IPv4
// unicast, with the lists and route maps that its neighbors use.
type BGPConfig struct {
	LocalAS            int64              `mapstructure:"localAS"`
	RouterID           string             `mapstructure:"routerID"`
	LogNeighborChanges bool               `mapstructure:"logNeighborChanges"`
	Multipath          BGPMultipath       `mapstructure:"multipath"`
	Networks           []string           `mapstructure:"networks"`
	Redistribute       []BGPRedistribute  `mapstructure:"redistribute"`
	Neighbors          []BGPNeighbor      `mapstructure:"neighbors"`
	BFDProfiles        []BFDProfile       `mapstructure:"bfdProfiles"`
	PrefixLists        []BGPPrefixList    `mapstructure:"prefixLists"`
	CommunityLists     []BGPCommunityList `mapstructure:"communityLists"`
	RouteMaps          []BGPRouteMap      `mapstructure:"routeMaps"`
}

// BGPMultipath holds how many equal paths BGP installs for one prefix.
type BGPMultipath struct {
	EBGP        *int64 `mapstructure:"ebgp"`
	IBGP        *int64 `mapstructure:"ibgp"`
	ASPathRelax bool   `mapstructure:"asPathRelax"`
}

// BGPRedistribute brings the routes of another protocol into BGP.
type BGPRedistribute struct {
	Protocol string `mapstructure:"protocol"`
	RouteMap string `mapstructure:"routeMap"`
	Metric   *int64 `mapstructure:"metric"`
}

// BGPNeighbor is one BGP session. A pointer says whether the key is set,
// where that differs from the zero value.
type BGPNeighbor struct {
	Address                    string            `mapstructure:"address"`
	RemoteAS                   int64             `mapstructure:"remoteAS"`
	Description                string            `mapstructure:"description"`
	UpdateSource               string            `mapstructure:"updateSource"`
	EBGPMultihop               *int64            `mapstructure:"ebgpMultihop"`
	Password                   *string           `mapstructure:"password"` //nolint:gosec // G117: no error or log holds the value
	Shutdown                   bool              `mapstructure:"shutdown"`
	Timers                     BGPTimers         `mapstructure:"timers"`
	BFD                        *bool             `mapstructure:"bfd"`
	BFDProfile                 string            `mapstructure:"bfdProfile"`
	NexthopSelf                bool              `mapstructure:"nexthopSelf"`
	SoftReconfigurationInbound bool              `mapstructure:"softReconfigurationInbound"`
	DefaultOriginate           *bool             `mapstructure:"defaultOriginate"`
	DefaultOriginateRouteMap   string            `mapstructure:"defaultOriginateRouteMap"`
	Import                     BGPNeighborPolicy `mapstructure:"import"`
	Export                     BGPNeighborPolicy `mapstructure:"export"`
}

// BGPTimers holds the timers of a neighbor, in seconds.
type BGPTimers struct {
	Keepalive    *int64 `mapstructure:"keepalive"`
	Holdtime     *int64 `mapstructure:"holdtime"`
	ConnectRetry *int64 `mapstructure:"connectRetry"`
}

// BGPNeighborPolicy names what filters the routes of a neighbor in one direction.
type BGPNeighborPolicy struct {
	RouteMap   string `mapstructure:"routeMap"`
	PrefixList string `mapstructure:"prefixList"`
}

// BFDProfile holds BFD timers under a name. The intervals are in milliseconds.
type BFDProfile struct {
	Name             string `mapstructure:"name"`
	TransmitInterval *int64 `mapstructure:"transmitInterval"`
	ReceiveInterval  *int64 `mapstructure:"receiveInterval"`
	Multiplier       *int64 `mapstructure:"multiplier"`
}

// BGPPrefixList is a list of rules that match IPv4 prefixes.
type BGPPrefixList struct {
	Name  string          `mapstructure:"name"`
	Rules []BGPPrefixRule `mapstructure:"rules"`
}

// BGPPrefixRule matches Prefix, or the prefixes inside it whose length is
// from GE to LE.
type BGPPrefixRule struct {
	Rule   int64  `mapstructure:"rule"`
	Action string `mapstructure:"action"`
	Prefix string `mapstructure:"prefix"`
	GE     *int64 `mapstructure:"ge"`
	LE     *int64 `mapstructure:"le"`
}

// BGPCommunityList is a list of rules that match the communities of a route.
type BGPCommunityList struct {
	Name  string             `mapstructure:"name"`
	Rules []BGPCommunityRule `mapstructure:"rules"`
}

// BGPCommunityRule matches the communities of a route with a regular expression.
type BGPCommunityRule struct {
	Rule   int64  `mapstructure:"rule"`
	Action string `mapstructure:"action"`
	Regex  string `mapstructure:"regex"`
}

// BGPRouteMap is a list of rules that accept, refuse, or change routes.
type BGPRouteMap struct {
	Name  string            `mapstructure:"name"`
	Rules []BGPRouteMapRule `mapstructure:"rules"`
}

// BGPRouteMapRule is one rule of a route map.
type BGPRouteMapRule struct {
	Rule   int64            `mapstructure:"rule"`
	Action string           `mapstructure:"action"`
	Match  BGPRouteMapMatch `mapstructure:"match"`
	Set    BGPRouteMapSet   `mapstructure:"set"`
}

// BGPRouteMapMatch names the lists that a route must match.
type BGPRouteMapMatch struct {
	PrefixList    string `mapstructure:"prefixList"`
	CommunityList string `mapstructure:"communityList"`
}

// BGPRouteMapSet holds what a route map rule changes in a route it permits.
type BGPRouteMapSet struct {
	LocalPreference *int64  `mapstructure:"localPreference"`
	Community       string  `mapstructure:"community"`
	Src             string  `mapstructure:"src"`
	Metric          *int64  `mapstructure:"metric"`
	ASPathPrepend   []int64 `mapstructure:"asPathPrepend"`
}

// bgpNames holds the names, by kind, that a `bgp` block can use.
type bgpNames struct {
	routeMaps, prefixLists, communityLists, bfdProfiles map[string]bool
}

// bgpLines returns the `bgp` metadata of a router as lines of its
// configuration, or nil if md has none. osType is the os_type of the router,
// nets are its topology interfaces, and config holds the lines of the `config`
// metadata of the same host, which can define names that the block uses. Every
// error wraps [ErrBGPConfig]. No error holds the password of a neighbor: the
// lines do.
func bgpLines(md map[string]any, osType string, nets []ifaces.NodeNetworkInterface, config []string) ([]string, error) {
	raw, ok := md["bgp"]
	if !ok {
		return nil, nil
	}

	render, ok := bgpRenderers[strings.ToLower(osType)]
	if !ok {
		return nil, fmt.Errorf(
			"%w: bgp is only supported for os_type %s, and the os_type is %q",
			ErrBGPConfig, strings.Join(bgpOSTypes(), ", "), osType,
		)
	}

	cfg, err := decodeBGPConfig(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrBGPConfig, err)
	}

	// The lines of `config` are VyOS commands, and only a VyOS router has them.
	err = cfg.check(nets, vyosConfigNames(config))
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrBGPConfig, err)
	}

	lines, err := render(cfg)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrBGPConfig, err)
	}

	return lines, nil
}

// bgpOSTypes returns the os_types that have a BGP renderer, in order.
func bgpOSTypes() []string {
	return slices.Sorted(maps.Keys(bgpRenderers))
}

// decodeBGPConfig reads the value of the `bgp` key. A key that the model does
// not have is an error, so that a misspelled key is not dropped without a word.
func decodeBGPConfig(raw any) (BGPConfig, error) {
	// The wrapper gives the errors of mapstructure the name of the key.
	var doc struct {
		BGP BGPConfig `mapstructure:"bgp"`
	}

	decoder, err := mapstructure.NewDecoder(&mapstructure.DecoderConfig{
		DecodeHook:  mapstructure.DecodeHookFuncKind(wholeNumber),
		ErrorUnused: true,
		Result:      &doc,
	})
	if err != nil {
		return doc.BGP, fmt.Errorf("making the decoder: %w", err)
	}

	err = decoder.Decode(map[string]any{"bgp": raw})
	if err != nil {
		var list *mapstructure.Error
		if errors.As(err, &list) {
			// Its own text spreads the errors over several lines.
			return doc.BGP, errors.New(strings.Join(list.Errors, "; "))
		}

		return doc.BGP, fmt.Errorf("decoding: %w", err)
	}

	return doc.BGP, nil
}

// wholeNumber is a mapstructure hook that refuses a number with a fraction for
// a field that holds a whole number. Every number of a config that came
// through JSON is a float64, and mapstructure would cut the fraction off.
func wholeNumber(from, to reflect.Kind, data any) (any, error) {
	if (from != reflect.Float32 && from != reflect.Float64) || to < reflect.Int || to > reflect.Uint64 {
		return data, nil
	}

	if f := reflect.ValueOf(data).Float(); f != math.Trunc(f) || math.Abs(f) > bgpMaxWholeFloat {
		return nil, fmt.Errorf("%v is not a whole number", data)
	}

	return data, nil
}

// vyosConfigNames returns the names that the lines of a `config` list define:
// a line that starts with `set policy route-map NAME `, and the same for
// `policy prefix-list`, `policy community-list`, and `protocols bfd profile`.
func vyosConfigNames(lines []string) bgpNames {
	names := bgpNames{
		routeMaps:      map[string]bool{},
		prefixLists:    map[string]bool{},
		communityLists: map[string]bool{},
		bfdProfiles:    map[string]bool{},
	}

	kinds := []struct {
		prefix string
		names  map[string]bool
	}{
		{prefix: "set policy route-map ", names: names.routeMaps},
		{prefix: "set policy prefix-list ", names: names.prefixLists},
		{prefix: "set policy community-list ", names: names.communityLists},
		{prefix: "set protocols bfd profile ", names: names.bfdProfiles},
	}

	for _, line := range lines {
		for _, kind := range kinds {
			rest, ok := strings.CutPrefix(line, kind.prefix)
			if !ok {
				continue
			}

			if name, _, more := strings.Cut(rest, " "); more && bgpNamePattern.MatchString(name) {
				kind.names[name] = true
			}
		}
	}

	return names
}

// check refuses a block that no router could run, and replaces each
// updateSource that names an interface in nets with the address of that
// interface. known holds the names that something outside the block defines.
// check adds the names of the block to it.
func (c *BGPConfig) check(nets []ifaces.NodeNetworkInterface, known bgpNames) error {
	for _, names := range []*map[string]bool{&known.routeMaps, &known.prefixLists, &known.communityLists, &known.bfdProfiles} {
		if *names == nil {
			*names = map[string]bool{}
		}
	}

	err := c.checkSpeaker()
	if err != nil {
		return err
	}

	err = c.checkLists(known)
	if err != nil {
		return err
	}

	seen := map[string]bool{}

	for i := range c.Redistribute {
		r := &c.Redistribute[i]

		err := r.check(known)
		if err != nil {
			return fmt.Errorf("redistribute %s: %w", r.Protocol, err)
		}

		if seen[r.Protocol] {
			return fmt.Errorf("redistribute has the protocol %s more than once", r.Protocol)
		}

		seen[r.Protocol] = true
	}

	clear(seen)

	for i := range c.Neighbors {
		n := &c.Neighbors[i]

		_, err := bgpAddr("address", n.Address)
		if err != nil {
			return fmt.Errorf("neighbor %d: %w", i+1, err)
		}

		if seen[n.Address] {
			return fmt.Errorf("neighbor %s is in the list more than once", n.Address)
		}

		seen[n.Address] = true

		err = n.check(nets, known)
		if err != nil {
			return fmt.Errorf("neighbor %s: %w", n.Address, err)
		}
	}

	return nil
}

// checkSpeaker checks what the block says of the router itself.
func (c *BGPConfig) checkSpeaker() error {
	err := bgpRange("localAS", &c.LocalAS, 1, bgpMaxAS)
	if err != nil {
		return err
	}

	if c.RouterID != "" {
		_, err := bgpAddr("routerID", c.RouterID)
		if err != nil {
			return err
		}
	}

	err = bgpRange("multipath ebgp", c.Multipath.EBGP, 1, bgpMaxPaths)
	if err != nil {
		return err
	}

	err = bgpRange("multipath ibgp", c.Multipath.IBGP, 1, bgpMaxPaths)
	if err != nil {
		return err
	}

	for _, network := range c.Networks {
		_, err := bgpPrefix("network", network)
		if err != nil {
			return err
		}
	}

	return nil
}

// checkLists checks the BFD profiles, the lists and the route maps, and adds
// their names to known. The route maps come last, because their rules name
// the lists.
func (c *BGPConfig) checkLists(known bgpNames) error {
	err := bgpDefine(bgpKindBFDProfile, known.bfdProfiles, c.BFDProfiles,
		func(p BFDProfile) string { return p.Name }, BFDProfile.check)
	if err != nil {
		return err
	}

	err = bgpDefine(bgpKindPrefixList, known.prefixLists, c.PrefixLists,
		func(l BGPPrefixList) string { return l.Name }, BGPPrefixList.check)
	if err != nil {
		return err
	}

	err = bgpDefine(bgpKindCommunityList, known.communityLists, c.CommunityLists,
		func(l BGPCommunityList) string { return l.Name }, BGPCommunityList.check)
	if err != nil {
		return err
	}

	return bgpDefine(bgpKindRouteMap, known.routeMaps, c.RouteMaps,
		func(m BGPRouteMap) string { return m.Name },
		func(m BGPRouteMap) error { return m.check(known) })
}

// bgpDefine checks the name of each item, that no two items have one name,
// and each item with check. It adds the names to known.
func bgpDefine[T any](kind string, known map[string]bool, items []T, nameOf func(T) string, check func(T) error) error {
	defined := map[string]bool{}

	for _, item := range items {
		name := nameOf(item)

		switch {
		case !bgpNamePattern.MatchString(name):
			return fmt.Errorf("%s name %q is not a letter and then letters, digits, - or _", kind, name)
		case defined[name]:
			return fmt.Errorf("%s %s is defined more than once", kind, name)
		}

		defined[name] = true
		known[name] = true

		err := check(item)
		if err != nil {
			return fmt.Errorf("%s %s: %w", kind, name, err)
		}
	}

	return nil
}

func (p BFDProfile) check() error {
	err := bgpRange("transmitInterval", p.TransmitInterval, bgpMinBFDInterval, bgpMaxBFDInterval)
	if err != nil {
		return err
	}

	err = bgpRange("receiveInterval", p.ReceiveInterval, bgpMinBFDInterval, bgpMaxBFDInterval)
	if err != nil {
		return err
	}

	return bgpRange("multiplier", p.Multiplier, bgpMinBFDMultiplier, bgpMaxBFDMultiplier)
}

func (l BGPPrefixList) check() error {
	var (
		numbers []int64
		matches = map[string]int64{}
	)

	for _, r := range l.Rules {
		err := r.check()
		if err != nil {
			return fmt.Errorf("rule %d: %w", r.Rule, err)
		}

		numbers = append(numbers, r.Rule)

		// VyOS refuses the commit when two rules of one list say the same.
		match := fmt.Sprint(r.Action, " ", r.Prefix, " ", bgpOptional(r.GE), " ", bgpOptional(r.LE))
		if other, ok := matches[match]; ok && other != r.Rule {
			return fmt.Errorf("rule %d is the same as rule %d", r.Rule, other)
		}

		matches[match] = r.Rule
	}

	return bgpRuleNumbers(numbers)
}

func (r BGPPrefixRule) check() error {
	err := bgpAction(r.Action)
	if err != nil {
		return err
	}

	prefix, err := bgpPrefix("prefix", r.Prefix)
	if err != nil {
		return err
	}

	// prefix length <= ge <= le <= 32, as FRR has it
	low := int64(prefix.Bits())

	if r.GE != nil {
		err := bgpRange("ge", r.GE, low, bgpMaxPrefixLen)
		if err != nil {
			return fmt.Errorf("%w (from the length of the prefix to %d)", err, bgpMaxPrefixLen)
		}

		low = *r.GE
	}

	err = bgpRange("le", r.LE, low, bgpMaxPrefixLen)
	if err != nil {
		return fmt.Errorf("%w (from ge, or the length of the prefix, to %d)", err, bgpMaxPrefixLen)
	}

	return nil
}

func (l BGPCommunityList) check() error {
	numbers := make([]int64, 0, len(l.Rules))

	for _, r := range l.Rules {
		err := bgpAction(r.Action)
		if err == nil {
			err = bgpText("regex", r.Regex, false)
		}

		if err != nil {
			return fmt.Errorf("rule %d: %w", r.Rule, err)
		}

		numbers = append(numbers, r.Rule)
	}

	return bgpRuleNumbers(numbers)
}

func (m BGPRouteMap) check(known bgpNames) error {
	numbers := make([]int64, 0, len(m.Rules))

	for _, r := range m.Rules {
		err := r.check(known)
		if err != nil {
			return fmt.Errorf("rule %d: %w", r.Rule, err)
		}

		numbers = append(numbers, r.Rule)
	}

	return bgpRuleNumbers(numbers)
}

func (r BGPRouteMapRule) check(known bgpNames) error {
	err := bgpAction(r.Action)
	if err != nil {
		return err
	}

	err = bgpReference(bgpKindPrefixList, known.prefixLists, r.Match.PrefixList)
	if err != nil {
		return err
	}

	err = bgpReference(bgpKindCommunityList, known.communityLists, r.Match.CommunityList)
	if err != nil {
		return err
	}

	return r.Set.check()
}

func (s BGPRouteMapSet) check() error {
	err := bgpRange("localPreference", s.LocalPreference, 0, math.MaxUint32)
	if err != nil {
		return err
	}

	err = bgpRange("metric", s.Metric, 0, math.MaxUint32)
	if err != nil {
		return err
	}

	if s.Community != "" {
		err := bgpCommunity(s.Community)
		if err != nil {
			return err
		}
	}

	if s.Src != "" {
		_, err := bgpAddr("src", s.Src)
		if err != nil {
			return err
		}
	}

	for _, as := range s.ASPathPrepend {
		err := bgpRange("asPathPrepend", &as, 1, bgpMaxAS)
		if err != nil {
			return err
		}
	}

	return nil
}

func (r BGPRedistribute) check(known bgpNames) error {
	if !slices.Contains([]string{"connected", "static", "ospf", "kernel", "rip"}, r.Protocol) {
		return fmt.Errorf("protocol %q is not connected, static, ospf, kernel or rip", r.Protocol)
	}

	err := bgpReference(bgpKindRouteMap, known.routeMaps, r.RouteMap)
	if err != nil {
		return err
	}

	return bgpRange("metric", r.Metric, 0, math.MaxUint32)
}

// check checks a neighbor whose address is valid. It replaces an updateSource
// that names an interface in nets with the address of the interface.
func (n *BGPNeighbor) check(nets []ifaces.NodeNetworkInterface, known bgpNames) error {
	// VyOS refuses the commit when a neighbor has an address of the router.
	for _, iface := range nets {
		if iface.Address() == n.Address {
			return fmt.Errorf("the address is the address of the router's own interface %s", iface.Name())
		}
	}

	err := n.checkSession(nets)
	if err != nil {
		return err
	}

	if n.BFD != nil && !*n.BFD && n.BFDProfile != "" {
		return errors.New("bfd is false, and bfdProfile turns BFD on: remove one of the two")
	}

	if n.DefaultOriginate != nil && !*n.DefaultOriginate && n.DefaultOriginateRouteMap != "" {
		return errors.New("defaultOriginate is false, and defaultOriginateRouteMap turns it on: remove one of the two")
	}

	references := []struct {
		kind  string
		known map[string]bool
		name  string
	}{
		{kind: bgpKindBFDProfile, known: known.bfdProfiles, name: n.BFDProfile},
		{kind: bgpKindRouteMap, known: known.routeMaps, name: n.DefaultOriginateRouteMap},
		{kind: bgpKindRouteMap, known: known.routeMaps, name: n.Import.RouteMap},
		{kind: bgpKindRouteMap, known: known.routeMaps, name: n.Export.RouteMap},
		{kind: bgpKindPrefixList, known: known.prefixLists, name: n.Import.PrefixList},
		{kind: bgpKindPrefixList, known: known.prefixLists, name: n.Export.PrefixList},
	}

	for _, reference := range references {
		err := bgpReference(reference.kind, reference.known, reference.name)
		if err != nil {
			return err
		}
	}

	return nil
}

// checkSession checks what a neighbor says of the session itself.
func (n *BGPNeighbor) checkSession(nets []ifaces.NodeNetworkInterface) error {
	err := bgpRange("remoteAS", &n.RemoteAS, 1, bgpMaxAS)
	if err != nil {
		return err
	}

	err = bgpText("description", n.Description, true)
	if err != nil {
		return err
	}

	err = n.resolveUpdateSource(nets)
	if err != nil {
		return err
	}

	err = bgpRange("ebgpMultihop", n.EBGPMultihop, 1, bgpMaxMultihop)
	if err != nil {
		return err
	}

	if n.Password != nil {
		// The same rule as vyosPassword, and what a TCP MD5 key can hold.
		err := bgpText("password", *n.Password, false)
		if err != nil {
			return err
		}

		if len(*n.Password) > bgpMaxPasswordLen {
			return fmt.Errorf("password is longer than %d bytes", bgpMaxPasswordLen)
		}
	}

	return n.Timers.check()
}

// resolveUpdateSource checks that UpdateSource is an IPv4 address, or replaces
// the name of an interface in nets with the address of that interface.
func (n *BGPNeighbor) resolveUpdateSource(nets []ifaces.NodeNetworkInterface) error {
	if n.UpdateSource == "" {
		return nil
	}

	if _, err := netip.ParseAddr(n.UpdateSource); err == nil {
		_, err := bgpAddr("updateSource", n.UpdateSource)

		return err
	}

	names := make([]string, 0, len(nets))

	for _, iface := range nets {
		names = append(names, iface.Name())

		if iface.Name() != n.UpdateSource {
			continue
		}

		addr, err := bgpAddr("address", iface.Address())
		if err != nil {
			return fmt.Errorf("updateSource names the interface %s: %w", iface.Name(), err)
		}

		n.UpdateSource = addr.String()

		return nil
	}

	return fmt.Errorf(
		"updateSource %q is not an IPv4 address or an interface of the router (%s)",
		n.UpdateSource, strings.Join(names, ", "),
	)
}

func (t BGPTimers) check() error {
	if (t.Keepalive == nil) != (t.Holdtime == nil) {
		return errors.New("timers need keepalive and holdtime together: a router ignores one without the other")
	}

	if t.Keepalive != nil {
		keepalive, holdtime := *t.Keepalive, *t.Holdtime

		switch {
		case holdtime != 0 && (holdtime < bgpMinHoldtime || holdtime > math.MaxUint16):
			return fmt.Errorf("timers holdtime is %d, want 0 or %d to %d", holdtime, bgpMinHoldtime, math.MaxUint16)
		case keepalive < 1 || keepalive > math.MaxUint16:
			return fmt.Errorf("timers keepalive is %d, want 1 to %d", keepalive, math.MaxUint16)
		case holdtime != 0 && keepalive >= holdtime:
			return fmt.Errorf("timers keepalive %d is not below holdtime %d", keepalive, holdtime)
		}
	}

	return bgpRange("timers connectRetry", t.ConnectRetry, 1, math.MaxUint16)
}

// bgpRange refuses a value that is set and is not from low to high.
func bgpRange(what string, value *int64, low, high int64) error {
	if value != nil && (*value < low || *value > high) {
		return fmt.Errorf("%s is %d, want %d to %d", what, *value, low, high)
	}

	return nil
}

// bgpOptional returns a value that is set as text, and "" for one that is not.
func bgpOptional(value *int64) string {
	if value == nil {
		return ""
	}

	return strconv.FormatInt(*value, 10)
}

// bgpAddr returns the IPv4 address in text.
func bgpAddr(what, text string) (netip.Addr, error) {
	addr, err := netip.ParseAddr(text)

	switch {
	case err != nil:
		return addr, fmt.Errorf("%s %q is not an IPv4 address", what, text)
	case !addr.Is4():
		return addr, fmt.Errorf("%s %s is not an IPv4 address: only IPv4 unicast is supported", what, text)
	default:
		return addr, nil
	}
}

// bgpPrefix returns the IPv4 prefix in text, which must have no bit set after
// its length.
func bgpPrefix(what, text string) (netip.Prefix, error) {
	prefix, err := netip.ParsePrefix(text)

	switch {
	case err != nil:
		return prefix, fmt.Errorf("%s %q is not an IPv4 prefix, such as 10.0.0.0/8", what, text)
	case !prefix.Addr().Is4():
		return prefix, fmt.Errorf("%s %s is not an IPv4 prefix: only IPv4 unicast is supported", what, text)
	case prefix != prefix.Masked():
		return prefix, fmt.Errorf("%s %s has bits set after its length: write %s", what, text, prefix.Masked())
	default:
		return prefix, nil
	}
}

// bgpText refuses a text that a script cannot hold in single quotes, and an
// empty one unless empty is true. The error does not hold the text, which can
// be a password.
func bgpText(what, text string, empty bool) error {
	switch {
	case text == "" && !empty:
		return fmt.Errorf("%s is empty", what)
	case !singleQuotable(text):
		return fmt.Errorf("%s has a single quote or a control character", what)
	default:
		return nil
	}
}

func bgpAction(action string) error {
	if action != bgpPermit && action != bgpDeny {
		return fmt.Errorf("action %q is not %s or %s", action, bgpPermit, bgpDeny)
	}

	return nil
}

// bgpRuleNumbers refuses a list with no rule, a number that is not from 1 to
// 65535, and a number that two rules have.
func bgpRuleNumbers(numbers []int64) error {
	if len(numbers) == 0 {
		return errors.New("it has no rules")
	}

	seen := map[int64]bool{}

	for _, number := range numbers {
		switch {
		case number < 1 || number > math.MaxUint16:
			return fmt.Errorf("rule %d: the number of a rule is from 1 to %d", number, math.MaxUint16)
		case seen[number]:
			return fmt.Errorf("rule %d is in the list more than once", number)
		}

		seen[number] = true
	}

	return nil
}

// bgpReference refuses a name that nothing defines. VyOS refuses the whole
// commit when a route map, a prefix list, or a community list that BGP or a
// route map names does not exist, and the router then boots with no
// configuration at all. So phenix must catch it here. (VyOS does not check the
// BFD profile of a neighbor. FRR then runs the session with its default
// timers, which is not what the author asked for.) The empty name is no
// reference.
func bgpReference(kind string, known map[string]bool, name string) error {
	switch {
	case name == "":
		return nil
	case !bgpNamePattern.MatchString(name):
		return fmt.Errorf("%s name %q is not a letter and then letters, digits, - or _", kind, name)
	case !known[name]:
		return fmt.Errorf("%s %s is not defined in bgp or by a line of config", kind, name)
	default:
		return nil
	}
}

// bgpWellKnownCommunities returns the names that stand for a community. They
// are the names that VyOS takes (include/policy/community-value-list.xml.i),
// which has `local-as` and no `internet`.
func bgpWellKnownCommunities() []string {
	return []string{
		"local-as", "no-advertise", "no-export", "graceful-shutdown", "accept-own", "route-filter-translated-v4",
		"route-filter-v4", "route-filter-translated-v6", "route-filter-v6", "llgr-stale", "no-llgr",
		"accept-own-nexthop", "blackhole", "no-peer",
	}
}

// bgpCommunity refuses a text that is not one community: AA:NN in decimal with
// each number from 0 to 65535, or a well-known name.
func bgpCommunity(text string) error {
	names := bgpWellKnownCommunities()
	if slices.Contains(names, text) {
		return nil
	}

	part := func(digits string) bool {
		number, err := strconv.ParseUint(digits, 10, 16)

		return err == nil && strconv.FormatUint(number, 10) == digits
	}

	if high, low, ok := strings.Cut(text, ":"); ok && part(high) && part(low) {
		return nil
	}

	return fmt.Errorf(
		"community %q is not AA:NN, with each number from 0 to %d, or one of %s",
		text, math.MaxUint16, strings.Join(names, ", "),
	)
}

// vyosLines collects VyOS `set` commands.
type vyosLines []string

// set adds the command `set` with the given words.
func (l *vyosLines) set(words ...any) {
	*l = append(*l, "set "+strings.TrimSuffix(fmt.Sprintln(words...), "\n"))
}

// optional adds a command that ends with a value, if the value is set.
func (l *vyosLines) optional(value *int64, words ...any) {
	if value != nil {
		l.set(append(words, *value)...)
	}
}

// vyosQuote returns text as one word of a VyOS script. The text must have
// passed bgpText.
func vyosQuote(text string) string {
	return "'" + text + "'"
}

// vyosBGPLines writes a checked block as `set` commands of VyOS 1.4 and later,
// in an order that reads well: the BFD profiles, the lists, the route maps,
// and then BGP with its neighbors in the order of the block. VyOS itself takes
// the lines in any order, because it checks nothing before the commit. The
// function refuses what the model allows and VyOS does not take. Every node
// and value range was read in the interface definitions of vyos-1x (branch
// current).
//
// A value that is free text is in single quotes. The other values are
// numbers, addresses, names that match bgpNamePattern, or words from a fixed
// list. As a second guard, every line must pass checkVyosConfigLine, as a line
// of `config` must.
func vyosBGPLines(cfg BGPConfig) ([]string, error) {
	var lines vyosLines

	for _, p := range cfg.BFDProfiles {
		if len(p.Name) > vyosMaxBFDProfileName {
			return nil, fmt.Errorf("%s %s: VyOS takes a name of at most %d characters", bgpKindBFDProfile, p.Name, vyosMaxBFDProfileName)
		}

		lines.bfdProfile(p)
	}

	for _, l := range cfg.PrefixLists {
		for _, r := range l.Rules {
			base := fmt.Sprintf("policy prefix-list %s rule %d", l.Name, r.Rule)

			lines.set(base, "action", r.Action)
			lines.set(base, "prefix", r.Prefix)
			lines.optional(r.GE, base, "ge")
			lines.optional(r.LE, base, "le")
		}
	}

	for _, l := range cfg.CommunityLists {
		for _, r := range l.Rules {
			base := fmt.Sprintf("policy community-list %s rule %d", l.Name, r.Rule)

			lines.set(base, "action", r.Action)
			lines.set(base, "regex", vyosQuote(r.Regex))
		}
	}

	for _, m := range cfg.RouteMaps {
		for _, r := range m.Rules {
			lines.routeMapRule(m.Name, r)
		}
	}

	lines.speaker(cfg)

	for _, n := range cfg.Neighbors {
		err := lines.neighbor(n)
		if err != nil {
			return nil, fmt.Errorf("neighbor %s: %w", n.Address, err)
		}
	}

	for i, line := range lines {
		err := checkVyosConfigLine(line)
		if err != nil {
			// The line is not in the error: it can hold a password.
			return nil, fmt.Errorf("line %d of the rendered bgp: %w", i, err)
		}
	}

	return lines, nil
}

func (l *vyosLines) bfdProfile(p BFDProfile) {
	base := "protocols bfd profile " + p.Name

	if p.TransmitInterval == nil && p.ReceiveInterval == nil && p.Multiplier == nil {
		l.set(base)
	}

	l.optional(p.TransmitInterval, base, "interval transmit")
	l.optional(p.ReceiveInterval, base, "interval receive")
	l.optional(p.Multiplier, base, "interval multiplier")
}

func (l *vyosLines) routeMapRule(name string, r BGPRouteMapRule) {
	base := fmt.Sprintf("policy route-map %s rule %d", name, r.Rule)

	l.set(base, "action", r.Action)

	if r.Match.PrefixList != "" {
		l.set(base, "match ip address prefix-list", r.Match.PrefixList)
	}

	if r.Match.CommunityList != "" {
		l.set(base, "match community community-list", r.Match.CommunityList)
	}

	l.optional(r.Set.LocalPreference, base, "set local-preference")

	if r.Set.Community != "" {
		l.set(base, "set community replace", r.Set.Community)
	}

	if r.Set.Src != "" {
		l.set(base, "set src", r.Set.Src)
	}

	l.optional(r.Set.Metric, base, "set metric")

	if len(r.Set.ASPathPrepend) != 0 {
		l.set(base, "set as-path prepend", vyosQuote(strings.Trim(fmt.Sprint(r.Set.ASPathPrepend), "[]")))
	}
}

// speaker adds the BGP instance without its neighbors.
func (l *vyosLines) speaker(cfg BGPConfig) {
	const (
		parameters = "protocols bgp parameters"
		afi        = "protocols bgp address-family ipv4-unicast"
	)

	l.set("protocols bgp system-as", cfg.LocalAS)

	if cfg.RouterID != "" {
		l.set(parameters, "router-id", cfg.RouterID)
	}

	if cfg.LogNeighborChanges {
		l.set(parameters, "log-neighbor-changes")
	}

	if cfg.Multipath.ASPathRelax {
		l.set(parameters, "bestpath as-path multipath-relax")
	}

	for _, network := range cfg.Networks {
		l.set(afi, "network", network)
	}

	for _, r := range cfg.Redistribute {
		// A command with an option also turns the redistribution on.
		if r.RouteMap == "" && r.Metric == nil {
			l.set(afi, "redistribute", r.Protocol)
		}

		if r.RouteMap != "" {
			l.set(afi, "redistribute", r.Protocol, "route-map", r.RouteMap)
		}

		l.optional(r.Metric, afi, "redistribute", r.Protocol, "metric")
	}

	l.optional(cfg.Multipath.EBGP, afi, "maximum-paths ebgp")
	l.optional(cfg.Multipath.IBGP, afi, "maximum-paths ibgp")
}

// neighbor adds one neighbor. It refuses what VyOS does not take: a
// description of more than 255 characters, and a password with a space, for
// which VyOS refuses the whole commit.
func (l *vyosLines) neighbor(n BGPNeighbor) error {
	base := "protocols bgp neighbor " + n.Address

	l.set(base, "remote-as", n.RemoteAS)

	if n.Description != "" {
		if utf8.RuneCountInString(n.Description) > vyosMaxDescription {
			return fmt.Errorf("VyOS takes a description of at most %d characters", vyosMaxDescription)
		}

		l.set(base, "description", vyosQuote(n.Description))
	}

	l.optional(n.Timers.Keepalive, base, "timers keepalive")
	l.optional(n.Timers.Holdtime, base, "timers holdtime")
	l.optional(n.Timers.ConnectRetry, base, "timers connect")

	// A command that names a profile also turns BFD on.
	switch {
	case n.BFDProfile != "":
		l.set(base, "bfd profile", n.BFDProfile)
	case n.BFD != nil && *n.BFD:
		l.set(base, "bfd")
	}

	if n.UpdateSource != "" {
		l.set(base, "update-source", n.UpdateSource)
	}

	l.optional(n.EBGPMultihop, base, "ebgp-multihop")

	if n.Password != nil {
		if strings.Contains(*n.Password, " ") {
			return errors.New("VyOS takes no password with a space")
		}

		l.set(base, "password", vyosQuote(*n.Password))
	}

	if n.Shutdown {
		l.set(base, "shutdown")
	}

	l.neighborAFI(base+" address-family ipv4-unicast", n)

	return nil
}

// neighborAFI adds what a neighbor does with IPv4 unicast routes. VyOS turns
// the address family on for a neighbor only when the neighbor has the node
// (it writes `no bgp default ipv4-unicast`), so a neighbor with no option gets
// the bare node.
func (l *vyosLines) neighborAFI(base string, n BGPNeighbor) {
	before := len(*l)

	if n.NexthopSelf {
		l.set(base, "nexthop-self")
	}

	if n.SoftReconfigurationInbound {
		l.set(base, "soft-reconfiguration inbound")
	}

	// A command that names a route map also turns the default route on.
	switch {
	case n.DefaultOriginateRouteMap != "":
		l.set(base, "default-originate route-map", n.DefaultOriginateRouteMap)
	case n.DefaultOriginate != nil && *n.DefaultOriginate:
		l.set(base, "default-originate")
	}

	for _, option := range []struct{ node, name string }{
		{node: "route-map import", name: n.Import.RouteMap},
		{node: "route-map export", name: n.Export.RouteMap},
		{node: "prefix-list import", name: n.Import.PrefixList},
		{node: "prefix-list export", name: n.Export.PrefixList},
	} {
		if option.name != "" {
			l.set(base, option.node, option.name)
		}
	}

	if len(*l) == before {
		l.set(base)
	}
}
