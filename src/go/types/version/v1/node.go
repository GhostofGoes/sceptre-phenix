package v1

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	ifaces "phenix/types/interfaces"
	"phenix/util/mm"
	"phenix/util/mm/mmcli"
	"phenix/util/plog"
)

// DoNotBootReasonAnnotation records why the startup app set a node's
// do_not_boot, so the flag can be cleared when the reason goes away. A
// do_not_boot without it was set by the user and is kept.
const DoNotBootReasonAnnotation = "phenix/do-not-boot-reason"

type Node struct {
	AnnotationsF map[string]any    `json:"annotations" mapstructure:"annotations" structs:"annotations" yaml:"annotations"`
	LabelsF      map[string]string `json:"labels"      mapstructure:"labels"      structs:"labels"      yaml:"labels"`
	TypeF        string            `json:"type"        mapstructure:"type"        structs:"type"        yaml:"type"`
	GeneralF     *General          `json:"general"     mapstructure:"general"     structs:"general"     yaml:"general"`
	HardwareF    *Hardware         `json:"hardware"    mapstructure:"hardware"    structs:"hardware"    yaml:"hardware"`
	NetworkF     *Network          `json:"network"     mapstructure:"network"     structs:"network"     yaml:"network"`
	InjectionsF  []*Injection      `json:"injections"  mapstructure:"injections"  structs:"injections"  yaml:"injections"`
	DeletionsF   []*Deletion       `json:"deletions"   mapstructure:"deletions"   structs:"deletions"   yaml:"deletions"`
	AdvancedF    map[string]string `json:"advanced"    mapstructure:"advanced"    structs:"advanced"    yaml:"advanced"`
	OverridesF   map[string]string `json:"overrides"   mapstructure:"overrides"   structs:"overrides"   yaml:"overrides"`
	DelayF       *Delay            `json:"delay"       mapstructure:"delay"       structs:"delay"       yaml:"delay"`
	CommandsF    []string          `json:"commands"    mapstructure:"commands"    structs:"commands"    yaml:"commands"`
	ExternalF    *bool             `json:"external"    mapstructure:"external"    structs:"external"    yaml:"external"`
}

func (n Node) Annotations() map[string]any {
	return n.AnnotationsF
}

func (n Node) Labels() map[string]string {
	return n.LabelsF
}

func (n Node) Type() string {
	return n.TypeF
}

func (n Node) General() ifaces.NodeGeneral { //nolint:ireturn // interface
	return n.GeneralF
}

func (n Node) Hardware() ifaces.NodeHardware { //nolint:ireturn // interface
	return n.HardwareF
}

func (n Node) Network() ifaces.NodeNetwork { //nolint:ireturn // interface
	return n.NetworkF
}

func (n Node) Injections() []ifaces.NodeInjection {
	injects := make([]ifaces.NodeInjection, len(n.InjectionsF))

	for i, j := range n.InjectionsF {
		injects[i] = j
	}

	return injects
}

func (n Node) Deletions() []ifaces.NodeDeletion {
	deletions := make([]ifaces.NodeDeletion, len(n.DeletionsF))

	for i, j := range n.DeletionsF {
		deletions[i] = j
	}

	return deletions
}

func (n Node) Delay() ifaces.NodeDelay { //nolint:ireturn // interface
	if n.DelayF == nil {
		return new(Delay)
	}

	return n.DelayF
}

func (n Node) Advanced() map[string]string {
	return n.AdvancedF
}

func (n Node) Overrides() map[string]string {
	return n.OverridesF
}

func (n Node) Commands() []string {
	return n.CommandsF
}

func (n Node) External() bool {
	// The topology schema uses the `external` key as a way to determine which of
	// the two node schemas to apply to a configuration. The value is ignored, but
	// the key must be provided in order to use the less stringent node schema.
	// Here, if the key is provided, even if the value is false, we consider it to
	// be an external node.
	//
	// NOTE: the `Node.validate` function should error out if external was
	// intentionally set to false by a user.
	return n.ExternalF != nil
}

func (n *Node) SetInjections(injections []ifaces.NodeInjection) {
	injects := make([]*Injection, len(injections))

	for i, j := range injections {
		inj, _ := j.(*Injection)
		injects[i] = inj
	}

	n.InjectionsF = injects
}

func (n *Node) SetDeletions(deletions []ifaces.NodeDeletion) {
	deletionList := make([]*Deletion, len(deletions))

	for i, j := range deletions {
		del, _ := j.(*Deletion)
		deletionList[i] = del
	}

	n.DeletionsF = deletionList
}

func (n *Node) SetType(t string) {
	n.TypeF = t
}

func (n *Node) SetLabels(m map[string]string) {
	n.LabelsF = m
}

func (n *Node) AddAnnotation(k string, i any) {
	if n.AnnotationsF == nil {
		n.AnnotationsF = make(map[string]any)
	}

	n.AnnotationsF[k] = i
}

func (n *Node) AddTimerDelay(delay string) {
	n.DelayF.TimerF = delay
}

func (n *Node) AddUserDelay(delay bool) {
	n.DelayF.UserF = delay
}

func (n *Node) AddC2Delay(hostname string, useuuid bool) {
	n.DelayF = new(Delay)
	d := new(C2Delay)
	d.HostnameF = hostname
	d.UseUUIDF = useuuid
	n.DelayF.C2F = append(n.DelayF.C2F, *d)
}

func (n *Node) AddLabel(k, v string) {
	if n.LabelsF == nil {
		n.LabelsF = make(map[string]string)
	}

	n.LabelsF[k] = v
}

func (n *Node) AddHardware(os string, vcpu, memory int) ifaces.NodeHardware { //nolint:ireturn // interface
	h := &Hardware{ //nolint:exhaustruct // partial initialization
		OSTypeF: os,
		VCPUF:   vcpu,
		MemoryF: memory,
	}

	n.HardwareF = h

	return h
}

func (n *Node) AddNetworkInterface(typ, name, vlan string) ifaces.NodeNetworkInterface { //nolint:ireturn // interface
	i := &Interface{ //nolint:exhaustruct // partial initialization
		TypeF: typ,
		NameF: name,
		VLANF: vlan,
	}

	if n.NetworkF == nil {
		n.NetworkF = new(Network)
	}

	n.NetworkF.InterfacesF = append(n.NetworkF.InterfacesF, i)

	return i
}

func (n *Node) AddNetworkRoute(dest, next string, cost int) {
	r := Route{
		DestinationF: dest,
		NextF:        next,
		CostF:        &cost,
	}

	if n.NetworkF == nil {
		n.NetworkF = new(Network)
	}

	n.NetworkF.RoutesF = append(n.NetworkF.RoutesF, r)
}

func (n *Node) AddNetworkNAT(nats []map[string][]string) {
	for _, nat := range nats {
		natConfig := new(NAT)
		for out, ins := range nat {
			natConfig.OutF = out
			natConfig.InF = append(natConfig.InF, ins...)
		}

		n.NetworkF.NATF = append(n.NetworkF.NATF, *natConfig)
	}
}

func (n *Node) AddNetworkOSPF(routerID string, dead, hello, retrans int, areas map[int][]string) {
	n.NetworkF.OSPFF = new(OSPF)
	n.NetworkF.OSPFF.RouterIDF = routerID
	n.NetworkF.OSPFF.DeadIntervalF = &dead
	n.NetworkF.OSPFF.HelloIntervalF = &hello
	n.NetworkF.OSPFF.RetransmissionIntervalF = &retrans

	for id, networks := range areas {
		area := new(Area)
		area.AreaIDF = &id

		for _, net := range networks {
			areaNetwork := AreaNetwork{NetworkF: net}
			area.AreaNetworksF = append(area.AreaNetworksF, areaNetwork)
		}

		n.NetworkF.OSPFF.AreasF = append(n.NetworkF.OSPFF.AreasF, *area)
	}
}

func (n *Node) AddInject(src, dst, perms, desc string) {
	if _, ok := n.LabelsF["disable-injects"]; ok {
		return
	}

	var exists bool

	for _, inject := range n.InjectionsF {
		if inject.DstF == dst {
			inject.SrcF = src
			inject.PermissionsF = perms
			inject.DescriptionF = desc

			exists = true

			break
		}
	}

	if !exists {
		n.InjectionsF = append(n.InjectionsF, &Injection{
			SrcF:         src,
			DstF:         dst,
			PermissionsF: perms,
			DescriptionF: desc,
		})
	}
}

func (n *Node) AddDeletion(path, desc string) {
	if _, ok := n.LabelsF["disable-injects"]; ok {
		return
	}

	var exists bool

	for _, deletion := range n.DeletionsF {
		if deletion.PathF == path {
			deletion.DescriptionF = desc

			exists = true

			break
		}
	}

	if !exists {
		n.DeletionsF = append(n.DeletionsF, &Deletion{
			PathF:        path,
			DescriptionF: desc,
		})
	}
}

func (n *Node) SetAdvanced(adv map[string]string) {
	n.AdvancedF = adv
}

func (n *Node) AddAdvanced(config, value string) {
	if n.AdvancedF == nil {
		n.AdvancedF = make(map[string]string)
	}

	n.AdvancedF[config] = value
}

func (n *Node) AddOverride(match, replace string) {
	if n.OverridesF == nil {
		n.OverridesF = make(map[string]string)
	}

	n.OverridesF[match] = replace
}

func (n *Node) AddCommand(cmd string) {
	// avoid duplicates
	if slices.Contains(n.CommandsF, cmd) {
		return
	}

	n.CommandsF = append(n.CommandsF, cmd)
}

// SetCommands replaces the node command list.
func (n *Node) SetCommands(commands []string) {
	n.CommandsF = commands
}

func (n Node) GetAnnotation(a string) (any, bool) {
	if n.AnnotationsF == nil {
		return nil, false
	}

	for k := range n.AnnotationsF {
		if k == a {
			return n.AnnotationsF[k], true
		}
	}

	return nil, false
}

func (n Node) Delayed() string {
	if n.DelayF == nil {
		return ""
	}

	// Report the delay that experiment start enforces when several are set:
	// a user delay wins over a C2 delay, which wins over a timer.
	if n.DelayF.UserF {
		return "user"
	}

	// Experiment start skips an empty C2 hostname, so a C2 delay of empty
	// hostnames alone leaves the timer, if any, in force.
	var hosts []string

	for _, host := range n.DelayF.C2F {
		if host.Hostname() != "" {
			hosts = append(hosts, host.Hostname())
		}
	}

	if len(hosts) > 0 {
		return "cc:" + strings.Join(hosts, ",")
	}

	if n.DelayF.TimerF != "" {
		return "timer:" + n.DelayF.TimerF
	}

	return ""
}

type General struct {
	HostnameF    string `json:"hostname"    mapstructure:"hostname"    structs:"hostname"    yaml:"hostname"`
	DescriptionF string `json:"description" mapstructure:"description" structs:"description" yaml:"description"`
	VMTypeF      string `json:"vm_type"     mapstructure:"vm_type"     structs:"vm_type"     yaml:"vm_type"`
	SnapshotF    *bool  `json:"snapshot"    mapstructure:"snapshot"    structs:"snapshot"    yaml:"snapshot"`
	DoNotBootF   *bool  `json:"do_not_boot" mapstructure:"do_not_boot" structs:"do_not_boot" yaml:"do_not_boot"`
}

func (g *General) Hostname() string {
	if g == nil {
		return ""
	}

	return g.HostnameF
}

func (g *General) Description() string {
	if g == nil {
		return ""
	}

	return g.DescriptionF
}

func (g *General) VMType() string {
	if g == nil {
		return ""
	}

	return g.VMTypeF
}

func (g *General) Snapshot() *bool {
	if g == nil {
		return nil
	}

	if g.SnapshotF == nil {
		snapshot := false

		return &snapshot
	}

	return g.SnapshotF
}

func (g *General) SetSnapshot(b bool) {
	g.SnapshotF = &b
}

func (g *General) DoNotBoot() *bool {
	if g == nil {
		return nil
	}

	if g.DoNotBootF == nil {
		dnb := false

		return &dnb
	}

	return g.DoNotBootF
}

func (g *General) SetDoNotBoot(b bool) {
	g.DoNotBootF = &b
}

type Hardware struct {
	CPUF    string   `json:"cpu"     mapstructure:"cpu"     structs:"cpu"     yaml:"cpu"`
	VCPUF   int      `json:"vcpus"   mapstructure:"vcpus"   structs:"vcpus"   yaml:"vcpus"`
	MemoryF int      `json:"memory"  mapstructure:"memory"  structs:"memory"  yaml:"memory"`
	OSTypeF string   `json:"os_type" mapstructure:"os_type" structs:"os_type" yaml:"os_type"`
	DrivesF []*Drive `json:"drives"  mapstructure:"drives"  structs:"drives"  yaml:"drives"`
}

func (h *Hardware) CPU() string {
	if h == nil {
		return ""
	}

	return h.CPUF
}

func (h *Hardware) VCPU() int {
	if h == nil {
		return 0
	}

	return h.VCPUF
}

func (h *Hardware) Memory() int {
	if h == nil {
		return 0
	}

	return h.MemoryF
}

func (h *Hardware) OSType() string {
	if h == nil {
		return ""
	}

	return h.OSTypeF
}

func (h *Hardware) Drives() []ifaces.NodeDrive {
	if h == nil {
		return nil
	}

	drives := make([]ifaces.NodeDrive, len(h.DrivesF))

	for i, d := range h.DrivesF {
		drives[i] = d
	}

	return drives
}

func (h *Hardware) SetVCPU(v int) {
	h.VCPUF = v
}

func (h *Hardware) SetMemory(m int) {
	h.MemoryF = m
}

func (h *Hardware) AddDrive(disk string, part int) ifaces.NodeDrive { //nolint:ireturn // interface
	d := &Drive{ //nolint:exhaustruct // partial initialization
		ImageF:           disk,
		InjectPartitionF: &part,
	}

	h.DrivesF = append(h.DrivesF, d)

	return d
}

type Drive struct {
	ImageF           string `json:"image"            mapstructure:"image"            structs:"image"            yaml:"image"`
	IfaceF           string `json:"interface"        mapstructure:"interface"        structs:"interface"        yaml:"interface"`
	CacheModeF       string `json:"cache_mode"       mapstructure:"cache_mode"       structs:"cache_mode"       yaml:"cache_mode"`
	InjectPartitionF *int   `json:"inject_partition" mapstructure:"inject_partition" structs:"inject_partition" yaml:"inject_partition"`
}

func (d Drive) Image() string {
	return d.ImageF
}

func (d Drive) Interface() string {
	return d.IfaceF
}

func (d Drive) CacheMode() string {
	return d.CacheModeF
}

func (d Drive) InjectPartition() *int {
	if d.InjectPartitionF != nil {
		return d.InjectPartitionF
	}

	part := 1

	return &part
}

func (d *Drive) SetImage(i string) {
	d.ImageF = i
}

func (d *Drive) SetInjectPartition(p *int) {
	d.InjectPartitionF = p
}

type Injection struct {
	SrcF         string `json:"src"         mapstructure:"src"         structs:"src"         yaml:"src"`
	DstF         string `json:"dst"         mapstructure:"dst"         structs:"dst"         yaml:"dst"`
	DescriptionF string `json:"description" mapstructure:"description" structs:"description" yaml:"description"`
	PermissionsF string `json:"permissions" mapstructure:"permissions" structs:"permissions" yaml:"permissions"`
}

func (i Injection) Src() string {
	return i.SrcF
}

func (i Injection) Dst() string {
	return i.DstF
}

func (i Injection) Description() string {
	return i.DescriptionF
}

func (i Injection) Permissions() string {
	return i.PermissionsF
}

type Deletion struct {
	PathF        string `json:"path"        mapstructure:"path"        structs:"path"        yaml:"path"`
	DescriptionF string `json:"description" mapstructure:"description" structs:"description" yaml:"description"`
}

func (del Deletion) Path() string {
	return del.PathF
}

func (del Deletion) Description() string {
	return del.DescriptionF
}

func (n Node) validate() error {
	if n.ExternalF != nil {
		if external := *n.ExternalF; !external {
			return errors.New(
				"the external key should not be included for internal nodes (even if set to false)",
			)
		}

		return nil
	}

	// External nodes are skipped above because minimega never launches them.
	var osType string
	if n.HardwareF != nil {
		osType = n.HardwareF.OSTypeF
	}

	warning, err := checkHostnameKeywords(n.GeneralF.HostnameF, osType)
	if err != nil {
		return err
	}

	if warning != "" {
		plog.Warn(
			plog.TypeSystem,
			"node hostname may cause problems; consider renaming the node",
			"node", n.GeneralF.HostnameF,
			"reason", warning,
		)
	}

	if n.DelayF != nil && n.DelayF.TimerF != "" {
		// Timer() reads an unparsable duration as 0, which would start the
		// VM right away instead of delaying it.
		if d, err := time.ParseDuration(n.DelayF.TimerF); err != nil || d < 0 {
			return fmt.Errorf(
				"delay timer %q is not a valid duration; use a number with a unit, such as 30s or 5m",
				n.DelayF.TimerF,
			)
		}
	}

	if err := CheckInjections(n.Injections()); err != nil {
		return err
	}

	if err := CheckDeletions(n.Deletions()); err != nil {
		return err
	}

	if err := CheckAdvanced(n.AdvancedF); err != nil {
		return err
	}

	if err := CheckCommands(n.CommandsF); err != nil {
		return err
	}

	if err := n.checkScriptWords(); err != nil {
		return err
	}

	if n.NetworkF == nil {
		return nil
	}

	var (
		gateways   int
		seenIfaces = make(map[string]struct{})
	)

	for _, iface := range n.NetworkF.InterfacesF {
		if iface == nil {
			continue
		}

		// ensure interface name only used once for each node
		if name := iface.NameF; name != "" {
			if _, ok := seenIfaces[name]; ok {
				return fmt.Errorf("interface name %q is defined more than once", name)
			}

			seenIfaces[name] = struct{}{}
		}

		if iface.GatewayF == "" {
			continue
		}

		// gateway is not relevant for DHCP or manual proto
		switch strings.ToLower(iface.ProtoF) {
		case "dhcp", "manual":
			return fmt.Errorf(
				"interface %q sets a gateway but uses the %q protocol, where the gateway is ignored",
				iface.NameF, iface.ProtoF,
			)
		}

		// Only one interface may define a gateway.
		gateways++
		if gateways > 1 {
			return errors.New("internal nodes should not have more than one gateway interface")
		}
	}

	return nil
}

func (n *Node) setDefaults(bridge string) {
	if n.External() {
		return
	}

	if n.GeneralF.VMTypeF == "" {
		n.GeneralF.VMTypeF = "kvm"
	}

	if n.GeneralF.SnapshotF == nil {
		snapshot := true
		n.GeneralF.SnapshotF = &snapshot
	}

	if n.GeneralF.DoNotBootF == nil {
		dnb := false
		n.GeneralF.DoNotBootF = &dnb
	}

	if n.HardwareF.CPUF == "" {
		n.HardwareF.CPUF = "Broadwell"
	}

	if n.HardwareF.VCPUF == 0 {
		n.HardwareF.VCPUF = 1
	}

	if n.HardwareF.MemoryF == 0 {
		n.HardwareF.MemoryF = 512
	}

	if n.HardwareF.OSTypeF == "" {
		n.HardwareF.OSTypeF = "linux"
	}

	if n.AdvancedF == nil {
		n.AdvancedF = make(map[string]string)
	}

	if n.OverridesF == nil {
		n.OverridesF = make(map[string]string)
	}

	if n.NetworkF != nil {
		n.NetworkF.SetDefaults(bridge)
	}
}

type Delay struct {
	TimerF string    `json:"timer" mapstructure:"timer" structs:"timer" yaml:"timer"`
	UserF  bool      `json:"user"  mapstructure:"user"  structs:"user"  yaml:"user"`
	C2F    []C2Delay `json:"c2"    mapstructure:"c2"    structs:"c2"    yaml:"c2"`
}

func (delay Delay) Timer() time.Duration {
	if delay.TimerF == "" {
		return 0
	}

	d, _ := time.ParseDuration(delay.TimerF)

	return d
}

func (delay Delay) User() bool {
	return delay.UserF
}

func (delay Delay) C2() []ifaces.NodeC2Delay {
	delays := make([]ifaces.NodeC2Delay, len(delay.C2F))

	for i, d := range delay.C2F {
		delays[i] = d
	}

	return delays
}

type C2Delay struct {
	HostnameF string `json:"hostname" mapstructure:"hostname" structs:"hostname" yaml:"hostname"`
	UseUUIDF  bool   `json:"useUUID"  mapstructure:"useUUID"  structs:"useUUID"  yaml:"useUUID"`
}

func (c C2Delay) Hostname() string {
	return c.HostnameF
}

func (c C2Delay) UseUUID() bool {
	return c.UseUUIDF
}

// FileInjects returns the injections of the node as the src:dst pairs of the
// minimega script's `disk inject ... files` line. It refuses an injection
// without a source, naming the node, which fails the script's rendering.
func (n Node) FileInjects(baseDir string) (string, error) {
	pairs, err := InjectPairs(baseDir, n.Injections())
	if err != nil {
		return "", fmt.Errorf("node %s: %w", n.General().Hostname(), err)
	}

	return strings.Join(pairs, " "), nil
}

// CheckInjections refuses a null injection and one that names no source. A
// relative source is resolved against the experiment's base directory, so an
// empty one, such as the null src of an app's output, would be rendered as
// that directory, and minimega would copy all of it into the VM. It also
// refuses a source or destination minimega's disk inject would get wrong (see
// checkDiskPath).
func CheckInjections(injections []ifaces.NodeInjection) error {
	for i, inject := range injections {
		if j, ok := inject.(*Injection); inject == nil || (ok && j == nil) {
			return fmt.Errorf("injection %d is null", i+1)
		}

		if inject.Src() == "" {
			return fmt.Errorf("injection to %q has no source", inject.Dst())
		}

		if err := checkDiskPath("source", inject.Src(), ':', false); err != nil {
			return fmt.Errorf("injection to %q: %w", inject.Dst(), err)
		}

		if err := checkDiskPath("destination", inject.Dst(), ':', true); err != nil {
			return fmt.Errorf("injection of %q: %w", inject.Src(), err)
		}

		if perms := inject.Permissions(); perms != "" {
			if _, err := injectionMode(perms); err != nil {
				return fmt.Errorf("injection to %q: %w", inject.Dst(), err)
			}
		}
	}

	return nil
}

// CheckDeletions refuses a null deletion, one that names no path, which
// minimega would read as the whole disk partition, and a path minimega's disk
// inject would get wrong (see checkDiskPath).
func CheckDeletions(deletions []ifaces.NodeDeletion) error {
	for i, del := range deletions {
		if d, ok := del.(*Deletion); del == nil || (ok && d == nil) {
			return fmt.Errorf("deletion %d is null", i+1)
		}

		if del.Path() == "" {
			return fmt.Errorf("deletion %d has no path", i+1)
		}

		if err := checkDiskPath("deletion path", del.Path(), ',', true); err != nil {
			return err
		}
	}

	return nil
}

// checkDiskPath refuses a path of an injection or a deletion that minimega's
// `disk inject` would not get as written: one with a control character, which
// could also end the line of the minimega script; with a '$', which minimega
// expands from its own environment; or with sep, where minimega splits its
// argument: at the ':' between an injection's source and destination, and at
// the ',' between deletions. A path in the VM must not contain a ".."
// element: minimega joins it to where it mounted the VM's disk on the host,
// as root, so the path could reach the host's own files.
func checkDiskPath(kind, path string, sep rune, inVM bool) error {
	switch {
	case strings.ContainsFunc(path, unicode.IsControl):
		return fmt.Errorf("%s %q contains a control character", kind, path)
	case strings.ContainsRune(path, '$'):
		return fmt.Errorf("%s %q contains '$', which minimega expands", kind, path)
	case strings.ContainsRune(path, sep):
		return fmt.Errorf("%s %q contains %q, where minimega splits its disk inject argument", kind, path, sep)
	case inVM && slices.Contains(strings.Split(path, "/"), ".."):
		return fmt.Errorf("%s %q contains '..', which would leave the VM's disk", kind, path)
	}

	return nil
}

// InjectPairs returns injections as the quoted src:dst pairs of a minimega
// `disk inject ... files` command, the same for the start-time script and for
// a redeploy that replicates injections. A relative source is relative to
// baseDir, the experiment's base directory. An injection with permissions
// gets a source with them (see injectionSource), since minimega copies the
// source's mode. It refuses the injections CheckInjections refuses, and two
// that give one source in baseDir different permissions (see sourceModes).
func InjectPairs(baseDir string, injections []ifaces.NodeInjection) ([]string, error) {
	if err := CheckInjections(injections); err != nil {
		return nil, err
	}

	pairs := make([]string, len(injections))
	modes := make(sourceModes)

	for i, inject := range injections {
		src := injectionPath(baseDir, inject.Src())

		if perms := inject.Permissions(); perms != "" {
			mode, err := injectionMode(perms)
			if err != nil {
				return nil, fmt.Errorf("injection to %q: %w", inject.Dst(), err)
			}

			if err := modes.claim(baseDir, src, mode, fmt.Sprintf("the injection to %q", inject.Dst())); err != nil {
				return nil, err
			}

			src, err = injectionSource(baseDir, src, mode)
			if err != nil {
				return nil, fmt.Errorf("injection to %q: %w", inject.Dst(), err)
			}
		}

		pairs[i] = `"` + mmcli.Escape(src) + `":"` + mmcli.Escape(inject.Dst()) + `"`
	}

	return pairs, nil
}

// CheckInjectionModes refuses injections into the nodes the start-time
// minimega script injects into that give one source in the experiment's base
// directory, baseDir, different permissions: phenix sets them on that file
// itself as it renders the script, and minimega injects into every node once
// the script is rendered, so each node would get the mode set last.
// Injections InjectPairs refuses are left for it to report.
func CheckInjectionModes(baseDir string, nodes []ifaces.NodeSpec) error {
	modes := make(sourceModes)

	for _, node := range nodes {
		if !scriptInjectsInto(node) {
			continue
		}

		for _, inject := range node.Injections() {
			if j, ok := inject.(*Injection); inject == nil || (ok && j == nil) || inject.Permissions() == "" {
				continue
			}

			mode, err := injectionMode(inject.Permissions())
			if err != nil {
				continue
			}

			what := fmt.Sprintf("node %s's injection to %q", node.General().Hostname(), inject.Dst())

			if err := modes.claim(baseDir, injectionPath(baseDir, inject.Src()), mode, what); err != nil {
				return err
			}
		}
	}

	return nil
}

// scriptInjectsInto reports whether the start-time minimega script injects
// files into node: a node minimega launches from a snapshot of its first
// drive, with an inject partition.
func scriptInjectsInto(node ifaces.NodeSpec) bool {
	if n, ok := node.(*Node); node == nil || (ok && n == nil) || node.External() {
		return false
	}

	if dnb := node.General().DoNotBoot(); dnb != nil && *dnb {
		return false
	}

	if snapshot := node.General().Snapshot(); snapshot == nil || !*snapshot {
		return false
	}

	drives := node.Hardware().Drives()
	if len(drives) == 0 {
		return false
	}

	if d, ok := drives[0].(*Drive); drives[0] == nil || (ok && d == nil) {
		return false
	}

	part := drives[0].InjectPartition()

	return part == nil || *part != 0
}

// injectionPath returns the path of an injection's source: a relative one is
// relative to baseDir, the experiment's base directory.
func injectionPath(baseDir, src string) string {
	if !strings.HasPrefix(src, "/") {
		return baseDir + "/" + src
	}

	return src
}

// sourceModes records, by resolved path, the permissions injections give the
// sources in the experiment's base directory. phenix sets them on such a
// source itself (see injectionSource), so two injections that give one source
// different permissions would both inject it with the mode set last.
type sourceModes map[string]sourceMode

type sourceMode struct {
	mode os.FileMode
	what string // the injection, such as `the injection to "/etc/x"`
}

// claim records that the injection what gives src mode, and refuses a
// different mode another injection gave the same source in baseDir. A source
// that does not resolve is left for injectionSource to report.
func (m sourceModes) claim(baseDir, src string, mode os.FileMode, what string) error {
	resolved, err := filepath.EvalSymlinks(src)
	if err != nil {
		return nil //nolint:nilerr // injectionSource reports it
	}

	base, err := filepath.EvalSymlinks(baseDir)
	if err != nil || !isWithin(base, resolved) {
		return nil //nolint:nilerr // only a source in the base directory is changed in place
	}

	if prior, ok := m[resolved]; ok && prior.mode != mode {
		return fmt.Errorf(
			"%s and %s give source %s permissions %04o and %04o; phenix sets them on that file in the "+
				"experiment's base directory itself, so both would get the mode set last",
			prior.what, what, src, prior.mode, mode,
		)
	}

	m[resolved] = sourceMode{mode: mode, what: what}

	return nil
}

// injectionMode parses an injection's permissions: an octal mode of the
// permission bits alone, such as 0644.
func injectionMode(perms string) (os.FileMode, error) {
	mode, err := strconv.ParseUint(perms, 8, 32)
	if err != nil || len(perms) > 4 || mode > uint64(fs.ModePerm) {
		return 0, fmt.Errorf("permissions %q are not an octal mode from 0000 to 0777, such as 0644", perms)
	}

	return os.FileMode(mode), nil
}

// injectionSource returns the file minimega injects from src with mode,
// since minimega copies a source's mode into the VM. phenix runs as root, so
// it changes the mode only of a source in the experiment's base directory,
// which phenix and its apps write, and injects a copy, which it writes there
// with mode, of a regular file anywhere else, instead of changing the mode of
// a host file the topology names. It leaves a missing source for minimega to
// report.
func injectionSource(baseDir, src string, mode os.FileMode) (string, error) {
	resolved, err := filepath.EvalSymlinks(src)
	if errors.Is(err, fs.ErrNotExist) {
		return src, nil
	}

	if err != nil {
		return "", fmt.Errorf("resolving source %s: %w", src, err)
	}

	if base, err := filepath.EvalSymlinks(baseDir); err == nil && isWithin(base, resolved) {
		if err := os.Chmod(resolved, mode); err != nil {
			return "", fmt.Errorf("setting the permissions of source %s: %w", src, err)
		}

		return src, nil
	}

	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("reading source %s: %w", src, err)
	}

	if !info.Mode().IsRegular() {
		return "", fmt.Errorf(
			"source %s is outside the experiment's base directory and not a regular file, so phenix cannot apply the "+
				"injection's permissions to a copy of it", src,
		)
	}

	sum := sha256.Sum256([]byte(resolved))
	staged := filepath.Join(baseDir, "injections", fmt.Sprintf("%x-%04o-%s", sum[:6], mode, filepath.Base(resolved)))

	if err := copyFileWithMode(resolved, staged, mode); err != nil {
		return "", fmt.Errorf("copying source %s to apply the injection's permissions: %w", src, err)
	}

	return staged, nil
}

// isWithin reports whether path is dir or below it; both are clean.
func isWithin(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)

	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}

// copyFileWithMode copies src to dst, which it replaces, with mode.
func copyFileWithMode(src, dst string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return err //nolint:wrapcheck // wrapped by the caller
	}

	in, err := os.Open(src) //nolint:gosec // the topology names the file to inject
	if err != nil {
		return err //nolint:wrapcheck // wrapped by the caller
	}

	defer func() { _ = in.Close() }()

	out, err := os.CreateTemp(filepath.Dir(dst), "."+filepath.Base(dst)+".*")
	if err != nil {
		return err //nolint:wrapcheck // wrapped by the caller
	}

	defer func() { _ = os.Remove(out.Name()) }()

	_, err = io.Copy(out, in)
	if err == nil {
		err = out.Chmod(mode)
	}

	if closeErr := out.Close(); err == nil {
		err = closeErr
	}

	if err == nil {
		err = os.Rename(out.Name(), dst)
	}

	return err //nolint:wrapcheck // wrapped by the caller
}

// trimLine drops the whitespace a value of a minimega script line ends with,
// such as the line break a YAML block scalar (`|` or `>`) ends with, which
// the script line ends anyway.
func trimLine(value string) string {
	return strings.TrimRightFunc(value, unicode.IsSpace)
}

// CheckAdvanced refuses an advanced setting with a control character other
// than the whitespace it ends with (see trimLine). The minimega script
// renders each setting as its own `vm config <key> <value>` line, unquoted so
// a value can hold several arguments, so a line break would start another
// minimega command.
func CheckAdvanced(advanced map[string]string) error {
	for _, key := range slices.Sorted(maps.Keys(advanced)) {
		if strings.ContainsFunc(key+trimLine(advanced[key]), unicode.IsControl) {
			return fmt.Errorf("advanced setting %q contains a control character", key)
		}
	}

	return nil
}

// AdvancedConfig returns the node's advanced settings for the minimega
// script, without the whitespace they end with. It refuses the settings
// CheckAdvanced refuses, naming the node, which fails the script's rendering.
func (n Node) AdvancedConfig() (map[string]string, error) {
	if err := CheckAdvanced(n.AdvancedF); err != nil {
		return nil, fmt.Errorf("node %s: %w", n.General().Hostname(), err)
	}

	advanced := make(map[string]string, len(n.AdvancedF))

	for key, value := range n.AdvancedF {
		advanced[key] = trimLine(value)
	}

	return advanced, nil
}

// CheckCommands refuses a C2 command with a control character other than the
// whitespace it ends with (see trimLine). The minimega C2 script renders each
// command as its own `cc <command>` line, unquoted so a command can hold
// several arguments, so a line break would start another minimega command.
func CheckCommands(commands []string) error {
	for i, command := range commands {
		if strings.ContainsFunc(trimLine(command), unicode.IsControl) {
			return fmt.Errorf("command %d %q contains a control character", i+1, command)
		}
	}

	return nil
}

// CCCommands returns the node's C2 commands for the minimega C2 script,
// without the whitespace they end with. It refuses the commands
// CheckCommands refuses, naming the node, which fails the script's rendering.
func (n Node) CCCommands() ([]string, error) {
	if err := CheckCommands(n.CommandsF); err != nil {
		return nil, fmt.Errorf("node %s: %w", n.General().Hostname(), err)
	}

	commands := make([]string, len(n.CommandsF))

	for i, command := range n.CommandsF {
		commands[i] = trimLine(command)
	}

	return commands, nil
}

// CheckScriptWord refuses value, a node's what, when it is not empty and a
// minimega script cannot hold it unquoted, as one argument or as one
// comma-separated item of one (see mm.CheckArgument). A line break, for
// example, would end the script's line, so the rest would run as its own
// minimega command, such as `shell`.
func CheckScriptWord(what, value string) error {
	if value == "" {
		return nil
	}

	if err := mm.CheckArgument(value); err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}

	return nil
}

// checkScriptWords refuses a hostname, VM type, CPU, drive or interface of
// the node that the minimega scripts write unquoted and CheckScriptWord
// refuses.
func (n Node) checkScriptWords() error {
	for _, word := range [][2]string{
		{"hostname", n.General().Hostname()},
		{"VM type", n.General().VMType()},
		{"CPU", n.Hardware().CPU()},
	} {
		if err := CheckScriptWord(word[0], word[1]); err != nil {
			return err
		}
	}

	if n.HardwareF != nil {
		for i, drive := range n.HardwareF.DrivesF {
			if err := drive.checkScriptWords(i); err != nil {
				return err
			}
		}
	}

	if n.NetworkF != nil {
		for _, iface := range n.NetworkF.InterfacesF {
			if err := iface.checkScriptWords(); err != nil {
				return err
			}
		}
	}

	return nil
}

// ScriptWord returns value, the node's what, for a minimega script, which
// writes it unquoted. It refuses a value CheckScriptWord refuses, naming the
// node, which fails the script's rendering.
func (n Node) ScriptWord(what, value string) (string, error) {
	if err := CheckScriptWord(what, value); err != nil {
		return "", fmt.Errorf("node %s: %w", n.General().Hostname(), err)
	}

	return value, nil
}

// DiskConfig returns the node's drives as the argument of the minimega
// script's `vm config disk` line (see Hardware.DiskConfig), naming the node in
// an error, which fails the script's rendering.
func (n Node) DiskConfig(snapshot string) (string, error) {
	if n.HardwareF == nil {
		return "", nil
	}

	config, err := n.HardwareF.DiskConfig(snapshot)
	if err != nil {
		return "", fmt.Errorf("node %s: %w", n.General().Hostname(), err)
	}

	return config, nil
}

// InterfaceConfig returns the node's interfaces as the argument of the
// minimega script's `vm config net` line (see Network.InterfaceConfig), naming
// the node in an error, which fails the script's rendering.
func (n Node) InterfaceConfig() (string, error) {
	if n.NetworkF == nil {
		return "", nil
	}

	config, err := n.NetworkF.InterfaceConfig()
	if err != nil {
		return "", fmt.Errorf("node %s: %w", n.General().Hostname(), err)
	}

	return config, nil
}

// FileDeletions returns the deletions of the node as the quoted paths of the
// minimega script's `disk inject ... delete files` line. It refuses the
// deletions CheckDeletions refuses, naming the node, which fails the script's
// rendering.
func (n Node) FileDeletions() (string, error) {
	if err := CheckDeletions(n.Deletions()); err != nil {
		return "", fmt.Errorf("node %s: %w", n.General().Hostname(), err)
	}

	deletions := make([]string, len(n.DeletionsF))

	for i, deletion := range n.DeletionsF {
		deletions[i] = `"` + mmcli.Escape(deletion.PathF) + `"`
	}

	return strings.Join(deletions, ","), nil
}

// DiskConfig returns the drives as the argument of a minimega `vm config
// disk` command, with snapshot, when set, in place of the first drive's
// image. It refuses a drive whose image, interface or cache mode
// CheckScriptWord refuses, since the minimega scripts write the argument
// unquoted.
func (h Hardware) DiskConfig(snapshot string) (string, error) {
	configs := make([]string, len(h.DrivesF))

	for i, d := range h.DrivesF {
		if d == nil {
			return "", fmt.Errorf("drive %d is null", i+1)
		}

		if err := d.checkScriptWords(i); err != nil {
			return "", err
		}

		config := []string{d.ImageF}

		if i == 0 && snapshot != "" {
			config[0] = snapshot
		}

		if d.IfaceF != "" {
			config = append(config, d.IfaceF)
		}

		if d.CacheModeF == "" {
			if snapshot != "" {
				config = append(config, "writeback")
			}
		} else {
			config = append(config, d.CacheModeF)
		}

		configs[i] = strings.Join(config, ",")
	}

	return strings.Join(configs, " "), nil
}

// checkScriptWords refuses the image, interface or cache mode of drive i, from
// 0, when CheckScriptWord refuses it.
func (d *Drive) checkScriptWords(i int) error {
	if d == nil {
		return nil
	}

	for _, word := range [][2]string{
		{"image", d.ImageF},
		{"interface", d.IfaceF},
		{"cache mode", d.CacheModeF},
	} {
		if err := CheckScriptWord(fmt.Sprintf("drive %d %s", i+1, word[0]), word[1]); err != nil {
			return err
		}
	}

	return nil
}

func (d Drive) GetInjectPartition() int {
	if d.InjectPartitionF == nil {
		return 1
	}

	return *d.InjectPartitionF
}
