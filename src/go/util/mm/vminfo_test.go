package mm

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/activeshadow/libminimega/minicli"

	"phenix/util/mm/mmcli"
)

var vmInfoHeader = []string{ //nolint:gochecknoglobals // test fixture
	"uuid", "name", "state", "uptime", "vlan", "tap", "ip", "memory", "vcpus",
	"disks", "snapshot", "cdrom", "tags",
}

// fakeVM is one row of the fake `vm info` table.
type fakeVM struct {
	host, name, uptime, disk string
	snapshot                 bool
}

func (v fakeVM) row() []string {
	snapshot := "false"
	if v.snapshot {
		snapshot = "true"
	}

	return []string{
		"uuid-" + v.name, v.name, "RUNNING", v.uptime, "[EXP (101)]", "[mega_tap1]",
		"[10.0.0.1]", "2048", "2", v.disk + ",virtio,writeback", snapshot, "", `{"a":"b"}`,
	}
}

// vmInfoCluster answers the commands GetVMInfo sends for the given VMs.
type vmInfoCluster struct {
	mu  sync.Mutex
	vms []fakeVM
}

func (c *vmInfoCluster) set(vms ...fakeVM) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.vms = vms
}

func (c *vmInfoCluster) reply(cmd fakeCommand) []*minicli.Response {
	c.mu.Lock()
	defer c.mu.Unlock()

	switch {
	case cmd.base == "host":
		return []*minicli.Response{tabular("head", []string{"name", "cpus"}, []string{"head", "8"})}
	case cmd.base == ccClientCmd:
		return []*minicli.Response{tabular("head", []string{"uuid"}, []string{"uuid-a"})}
	case cmd.base == "capture":
		return []*minicli.Response{tabular("head", []string{"interface", "path"},
			[]string{"a:0", "/a0.pcap"},
			[]string{"b:1", "/b1.pcap"},
			[]string{"a:1", "/a1.pcap"},
		)}
	case cmd.base == vmInfoCmd:
		byHost := make(map[string][][]string)

		for _, vm := range c.vms {
			byHost[vm.host] = append(byHost[vm.host], vm.row())
		}

		resps := make([]*minicli.Response, 0, len(byHost))

		for host, rows := range byHost {
			resps = append(resps, tabular(host, vmInfoHeader, rows...))
		}

		return resps
	case strings.Contains(cmd.base, "disk info "):
		disk := cmd.base[strings.Index(cmd.base, "disk info ")+len("disk info "):]

		return []*minicli.Response{
			tabular("any", []string{"image", "backingfile"}, []string{disk, "/base/" + disk}),
		}
	}

	return nil
}

func TestGetVMInfoCommandsAndDiskCache(t *testing.T) { //nolint:paralleltest // shares the fake minimega
	cluster := new(vmInfoCluster)
	cluster.set(
		fakeVM{host: "compute1", name: "a", uptime: "1h0m0s", disk: "snap-a", snapshot: true},
		fakeVM{host: "head", name: "b", uptime: "1h0m0s", disk: "snap-b", snapshot: true},
		fakeVM{host: "compute1", name: "c", uptime: "1h0m0s", disk: "plain-c", snapshot: false},
	)

	received := useFakeMinimega(t, cluster.reply)

	vms := Minimega{}.GetVMInfo(NS("exp"))
	vms.SortByName(true)

	if len(vms) != 3 {
		t.Fatalf("got %d VMs, want 3", len(vms))
	}

	wantCaptures := map[string][]Capture{
		"a": {{VM: "a", Interface: 0, Filepath: "/a0.pcap"}, {VM: "a", Interface: 1, Filepath: "/a1.pcap"}},
		"b": {{VM: "b", Interface: 1, Filepath: "/b1.pcap"}},
		"c": nil,
	}

	wantDisks := map[string]string{"a": "/base/snap-a", "b": "/base/snap-b", "c": "plain-c"}

	for _, vm := range vms {
		if !reflect.DeepEqual(vm.Captures, wantCaptures[vm.Name]) {
			t.Errorf("VM %s captures = %#v, want %#v", vm.Name, vm.Captures, wantCaptures[vm.Name])
		}

		if vm.Disk != wantDisks[vm.Name] {
			t.Errorf("VM %s disk = %q, want %q", vm.Name, vm.Disk, wantDisks[vm.Name])
		}

		if vm.CCActive != (vm.Name == "a") {
			t.Errorf("VM %s ccActive = %v", vm.Name, vm.CCActive)
		}

		if !reflect.DeepEqual(vm.IPv4, []string{"10.0.0.1"}) || vm.Uptime != 3600 || vm.RAM != 2048 {
			t.Errorf("VM %s parsed wrong: %#v", vm.Name, vm)
		}
	}

	cmds := received()

	for prefix, want := range map[string]int{
		"host":                         1, // headnode lookup, then cached
		ccClientCmd:                    1,
		vmInfoCmd:                      1,
		"capture":                      1, // once for the namespace, not per VM
		"disk info":                    1, // headnode VM
		"mesh send compute1 disk info": 1,
	} {
		if got := countCommands(cmds, prefix); got != want {
			t.Errorf("sent %d %q commands, want %d (all: %v)", got, prefix, want, cmds)
		}
	}

	// Listing again reuses the snapshot disk details and headnode.
	_ = Minimega{}.GetVMInfo(NS("exp"))

	cmds = received()
	if got := len(cmds); got != 9 {
		t.Errorf("second listing sent %d more commands, want 3 (all: %v)", got-6, cmds)
	}

	// A relaunched VM (a much younger uptime) is asked about again.
	cluster.set(
		fakeVM{host: "compute1", name: "a", uptime: "5s", disk: "snap-a", snapshot: true},
		fakeVM{host: "head", name: "b", uptime: "1h0m0s", disk: "snap-b", snapshot: true},
	)

	_ = Minimega{}.GetVMInfo(NS("exp"))

	if got := countCommands(received(), "mesh send compute1 disk info"); got != 2 {
		t.Errorf("relaunched VM's disk was asked about %d times, want 2", got)
	}

	// Killing VMs forgets everything known about the namespace.
	_ = Minimega{}.KillVM(NS("exp"), VMName("b"))
	_ = Minimega{}.GetVMInfo(NS("exp"))

	if got := countCommands(received(), "disk info"); got != 2 {
		t.Errorf("headnode VM's disk was asked about %d times after a kill, want 2", got)
	}
}

func TestGetVMInfoMergesConcurrentCalls(t *testing.T) { //nolint:paralleltest // shares the fake minimega
	const callers = 4

	var (
		cluster = new(vmInfoCluster)
		release = make(chan struct{})
	)

	cluster.set(fakeVM{host: "head", name: "a", uptime: "1m0s", disk: "plain", snapshot: false})

	received := useFakeMinimega(t, func(cmd fakeCommand) []*minicli.Response {
		if cmd.base == "hold" {
			<-release
		}

		return cluster.reply(cmd)
	})

	// Hold the shared connection so the callers below queue up behind it.
	held := make(chan error, 1)

	go func() {
		cmd := mmcli.NewCommand()
		cmd.Command = "hold"

		held <- mmcli.ErrorResponse(mmcli.Run(cmd))
	}()

	waitFor(t, func() bool { return countCommands(received(), "hold") == 1 })

	var (
		wg      sync.WaitGroup
		results = make([]VMs, callers)
	)

	for i := range callers {
		wg.Add(1)

		go func() {
			defer wg.Done()

			results[i] = Minimega{}.GetVMInfo(NS("exp"))
		}()
	}

	waitFor(t, func() bool { return vmInfoFlights.waiting("exp\x00") == callers-1 })

	close(release)
	wg.Wait()

	if err := <-held; err != nil {
		t.Fatalf("holding command failed: %v", err)
	}

	if got := countCommands(received(), vmInfoCmd); got != 1 {
		t.Fatalf("sent %d vm info commands for %d concurrent callers, want 1", got, callers)
	}

	for i, vms := range results {
		if len(vms) != 1 || vms[0].Name != "a" {
			t.Fatalf("caller %d got %#v", i, vms)
		}
	}

	// Each caller has its own copy.
	results[0][0].IPv4[0] = "changed"

	if results[1][0].IPv4[0] != "10.0.0.1" {
		t.Fatal("callers share a result's slices")
	}
}

func TestHeadnodeIsLookedUpOnce(t *testing.T) { //nolint:paralleltest // shares the fake minimega
	cluster := new(vmInfoCluster)
	received := useFakeMinimega(t, cluster.reply)

	for range 3 {
		if got := (Minimega{}).Headnode(); got != "head" {
			t.Fatalf("Headnode() = %q, want %q", got, "head")
		}
	}

	if got := countCommands(received(), "host"); got != 1 {
		t.Fatalf("sent %d host commands, want 1", got)
	}
}

func TestC2ClientUsesNarrowVMInfo(t *testing.T) { //nolint:paralleltest // shares the fake minimega
	received := useFakeMinimega(t, func(cmd fakeCommand) []*minicli.Response {
		switch cmd.base {
		case vmInfoCmd:
			// minicli's name filter folds case, so both come back
			return []*minicli.Response{tabular("head", []string{"name", "uuid"},
				[]string{"VM1", "uuid-upper"},
				[]string{"vm1", "uuid-lower"},
			)}
		case ccClientCmd:
			return []*minicli.Response{tabular("head", []string{"uuid"}, []string{"uuid-lower"})}
		}

		return nil
	})

	name, uuid, err := Minimega{}.c2Client(NewC2Options(
		C2NS("exp"), C2VM("vm1"), C2Context(context.Background()), C2IDClientsByUUID(),
	))
	if err != nil {
		t.Fatalf("c2Client: %v", err)
	}

	if name != "vm1" || uuid != "uuid-lower" {
		t.Fatalf("c2Client = %q, %q; want the exact name match", name, uuid)
	}

	cmds := received()

	if len(cmds) != 2 {
		t.Fatalf("sent %v, want only a vm info and a cc client", cmds)
	}

	if want := `.record false namespace "exp" .columns "name","uuid" .filter name=vm1 vm info`; cmds[0].raw != want {
		t.Fatalf("vm info command = %q, want %q", cmds[0].raw, want)
	}
}

func TestGetClusterHostsMeasuresDiskOncePerHost(t *testing.T) { //nolint:paralleltest // shares the fake minimega
	received := useFakeMinimega(t, func(cmd fakeCommand) []*minicli.Response {
		header := []string{"name", "cpus"}

		switch {
		case cmd.base == "host" && cmd.namespace == "minimega":
			return []*minicli.Response{tabular("head", header, []string{"head", "8"})}
		case cmd.base == "host" && cmd.namespace == "__phenix__":
			return []*minicli.Response{
				tabular("compute1", header, []string{"compute1", "16"}),
				tabular("compute2", header, []string{"compute2", "16"}),
			}
		case strings.HasPrefix(cmd.base, "mesh send compute1 shell "):
			return []*minicli.Response{text("compute1", "0=11% 1=12%\n")}
		case strings.HasPrefix(cmd.base, "mesh send compute2 shell "):
			// df failed for the first path
			return []*minicli.Response{text("compute2", "0= 1=22%\n")}
		case strings.HasPrefix(cmd.base, "shell "):
			return []*minicli.Response{text("head", "0=1% 1=2%\n")}
		}

		return nil
	})

	hosts, err := Minimega{}.GetClusterHosts(false)
	if err != nil {
		t.Fatalf("GetClusterHosts: %v", err)
	}

	want := map[string]DiskUsage{
		"compute1": {Phenix: 11, Minimega: 12},
		"compute2": {Phenix: 0, Minimega: 22},
		"head":     {Phenix: 1, Minimega: 2},
	}

	if len(hosts) != len(want) {
		t.Fatalf("got hosts %#v", hosts)
	}

	for _, host := range hosts {
		if host.DiskUsage != want[host.Name] {
			t.Errorf("host %s disk usage = %#v, want %#v", host.Name, host.DiskUsage, want[host.Name])
		}
	}

	if got := countCommands(received(), "mesh send") + countCommands(received(), "shell"); got != 3 {
		t.Fatalf("sent %d disk usage commands for 3 hosts, want 3", got)
	}

	// compute2's failed path is measured again next time; the others are cached.
	if _, err := (Minimega{}).GetClusterHosts(false); err != nil {
		t.Fatalf("GetClusterHosts: %v", err)
	}

	if got := countCommands(received(), "mesh send compute2 shell"); got != 2 {
		t.Fatalf("compute2 measured %d times, want 2", got)
	}

	if got := countCommands(received(), "mesh send compute1 shell"); got != 1 {
		t.Fatalf("compute1 measured %d times, want 1", got)
	}
}

func TestNarrowVMQueries(t *testing.T) { //nolint:paralleltest // shares the fake minimega
	received := useFakeMinimega(t, func(cmd fakeCommand) []*minicli.Response {
		if cmd.base != vmInfoCmd {
			return nil
		}

		if strings.Contains(cmd.raw, `.columns "ip"`) {
			if strings.Contains(cmd.raw, "name=missing") {
				return nil
			}

			return []*minicli.Response{tabular("compute1", []string{"ip"}, []string{"[10.0.0.1, ]"})}
		}

		return []*minicli.Response{
			tabular("compute1", []string{"name"}, []string{"a"}),
			tabular("compute2", []string{"name"}, []string{"b"}),
		}
	})

	hosts := Minimega{}.GetVMHosts(NS("exp"))
	if want := map[string]string{"a": "compute1", "b": "compute2"}; !reflect.DeepEqual(hosts, want) {
		t.Fatalf("GetVMHosts = %v, want %v", hosts, want)
	}

	ips, err := Minimega{}.GetVMIPv4(NS("exp"), VMName("a"))
	if err != nil || !reflect.DeepEqual(ips, []string{"10.0.0.1", ""}) {
		t.Fatalf("GetVMIPv4 = %#v, %v", ips, err)
	}

	if _, err := (Minimega{}).GetVMIPv4(NS("exp"), VMName("missing")); err == nil {
		t.Fatal("GetVMIPv4 found a missing VM")
	}

	for _, cmd := range received() {
		if !strings.Contains(cmd.raw, ".columns") {
			t.Fatalf("sent a vm info without columns: %q", cmd.raw)
		}
	}
}

func TestCloneVMsKeepsNilAndCopies(t *testing.T) {
	t.Parallel()

	if cloneVMs(nil) != nil {
		t.Fatal("cloneVMs(nil) is not nil")
	}

	vms := VMs{{Name: "a", IPv4: []string{"1.1.1.1"}, Tags: map[string]string{"k": "v"}}, {Name: "b"}}
	clone := cloneVMs(vms)

	before, _ := json.Marshal(vms)
	after, _ := json.Marshal(clone)

	if string(before) != string(after) {
		t.Fatalf("clone marshals differently:\n%s\n%s", before, after)
	}

	clone[0].IPv4[0] = "changed"
	clone[0].Tags["k"] = "changed"

	if vms[0].IPv4[0] != "1.1.1.1" || vms[0].Tags["k"] != "v" {
		t.Fatal("clone shares slices or maps with the original")
	}

	if hosts := cloneHosts(Hosts{{Name: "h", Load: []string{"1"}}}); hosts[0].Load[0] != "1" {
		t.Fatal("cloneHosts lost data")
	}
}

func TestGroupCapturesByVM(t *testing.T) {
	t.Parallel()

	grouped := groupCapturesByVM([]Capture{
		{VM: "a", Interface: 0}, {VM: "b", Interface: 0}, {VM: "a", Interface: 2},
	})

	if want := []Capture{{VM: "a", Interface: 0}, {VM: "a", Interface: 2}}; !reflect.DeepEqual(grouped["a"], want) {
		t.Fatalf("grouped[a] = %v, want %v", grouped["a"], want)
	}

	if grouped["missing"] != nil {
		t.Fatal("a VM without captures should get nil")
	}
}

func TestParseList(t *testing.T) {
	t.Parallel()

	for in, want := range map[string][]string{
		"":          nil,
		"[]":        nil,
		"[a]":       {"a"},
		"[a, b]":    {"a", "b"},
		"[a, , c]":  {"a", "", "c"},
		"[EXP (1)]": {"EXP (1)"},
	} {
		if got := parseList(in); !reflect.DeepEqual(got, want) {
			t.Errorf("parseList(%q) = %#v, want %#v", in, got, want)
		}
	}
}

func TestVMLaunchSameAs(t *testing.T) {
	t.Parallel()

	now := time.Now()
	base := vmLaunch{ns: "exp", name: "a", uuid: "u", host: "h", disk: "d", at: now}

	later := base
	later.at = now.Add(launchTolerance / 2)

	relaunched := base
	relaunched.at = now.Add(2 * launchTolerance)

	moved := base
	moved.host = "other"

	unknown := base
	unknown.at = time.Time{}

	for name, tc := range map[string]struct {
		other vmLaunch
		want  bool
	}{
		"same":       {base, true},
		"jitter":     {later, true},
		"relaunched": {relaunched, false},
		"moved":      {moved, false},
		"unknown":    {unknown, false},
	} {
		if got := base.sameAs(tc.other); got != tc.want {
			t.Errorf("%s: sameAs = %v, want %v", name, got, tc.want)
		}
	}
}

func TestDiskUsageCommandAndParse(t *testing.T) {
	t.Parallel()

	cmd := diskUsageCommand("/phenix", "/tmp/minimega")
	want := `bash -c "echo 0=$(df /phenix | awk '{print $(NF-1)}' | tail -1) ` +
		`1=$(df /tmp/minimega | awk '{print $(NF-1)}' | tail -1)"`

	if cmd != want {
		t.Fatalf("diskUsageCommand =\n%s\nwant\n%s", cmd, want)
	}

	for resp, want := range map[string]map[int]float64{
		"0=42% 1=17%": {0: 42, 1: 17},
		"0= 1=17%":    {1: 17},
		"0=Use% 1=5%": {1: 5},
		"":            {},
		"0=1% 7=2%":   {0: 1},
	} {
		if got := parseDiskUsage(resp, 2); !reflect.DeepEqual(got, want) {
			t.Errorf("parseDiskUsage(%q) = %v, want %v", resp, got, want)
		}
	}
}

// waitFor polls cond until it holds, failing the test after a few seconds.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)

	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for condition")
		}

		time.Sleep(time.Millisecond)
	}
}
