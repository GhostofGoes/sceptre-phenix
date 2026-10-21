package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"phenix/store"
	"phenix/tmpl"
	"phenix/types"
	"phenix/util"
	"phenix/util/mm"
	"phenix/util/notes"
	"phenix/util/plog"
	"phenix/util/pubsub"

	"github.com/mitchellh/mapstructure"

	ifaces "phenix/types/interfaces"
	v1 "phenix/types/version/v1"
)

const (
	appNameStartup                = "startup"
	tunnelConfigPartsPortOnly     = 1
	tunnelConfigPartsPortHost     = 2
	tunnelConfigPartsPortHostDest = 3

	startupViaCCAnnotation = "phenix/startup-via-cc"

	// startupC2ExecVerb runs a startup script over C2. Experiments stored by
	// older phenix versions hold startupC2LegacyExecVerb commands instead.
	startupC2ExecVerb       = "exec"
	startupC2LegacyExecVerb = "exec-once"

	// DoNotBootReasonAnnotation records why the startup app set a node's
	// do_not_boot, so the flag can be cleared when the reason goes away. A
	// do_not_boot without it was set by the user and is kept.
	DoNotBootReasonAnnotation = v1.DoNotBootReasonAnnotation
	dnbReasonMissingImage     = "missing disk image"

	linuxHostnameInjectDst  = "/etc/phenix/startup/1_hostname-start.sh"
	linuxTimezoneInjectDst  = "/etc/phenix/startup/2_timezone-start.sh"
	linuxIfaceInjectDst     = "/etc/phenix/startup/3_interfaces-start.sh"
	linuxDomainInjectDst    = "/etc/phenix/startup/4_domain-start.sh"
	windowsStartupInjectDst = "/phenix/startup/20-startup.ps1"

	// The Windows startup wrapper runs every script staged in
	// /phenix/startup, and the Start Menu scheduler registers the wrapper as
	// an on-start scheduled task.
	windowsStartupWrapperAsset = "phenix-startup.ps1"
	windowsStartupWrapperDst   = "/phenix/" + windowsStartupWrapperAsset
	windowsSchedulerAsset      = "startup-scheduler.cmd"
	windowsSchedulerDst        = "ProgramData/Microsoft/Windows/Start Menu/Programs/Startup/startup_scheduler.cmd"
)

type Startup struct {
	options Options
}

type startupC2Executor string

const (
	startupC2Linux   startupC2Executor = "linux"
	startupC2Windows startupC2Executor = "windows"
)

type commandSetter interface {
	SetCommands([]string)
}

var startupMMFullPath = mm.GetMMFullPath //nolint:gochecknoglobals // overridden by tests

// statImage reads a node's disk image file; tests replace it to report errors
// root never gets, such as a denied directory.
var statImage = os.Stat //nolint:gochecknoglobals // overridden by tests

func (s *Startup) Init(opts ...Option) error {
	s.options = NewOptions(opts...)

	return nil
}

func (Startup) Name() string {
	return appNameStartup
}

func (s *Startup) Configure(ctx context.Context, exp *types.Experiment) error {
	return nil
}

// startupViaCCEnabled reports whether the startup app should use C2 delivery.
func startupViaCCEnabled(node ifaces.NodeSpec) bool {
	val, ok := node.GetAnnotation(startupViaCCAnnotation)
	if !ok {
		return false
	}

	switch v := val.(type) {
	case bool:
		return v
	case string:
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "false", "0":
			return false
		default:
			return true
		}
	// One case per type: in a case listing several, v is an any, which equals
	// 0 only for an int, so a 0 read from the store, a float64, read as true.
	case int:
		return v != 0
	case int64:
		return v != 0
	case uint64:
		return v != 0
	case float64:
		return v != 0
	default:
		return true
	}
}

// startupC2DeliveryEnabled reports whether generated startup scripts need C2 delivery.
func startupC2DeliveryEnabled(node ifaces.NodeSpec) bool {
	if node.General() != nil && node.General().DoNotBoot() != nil && *node.General().DoNotBoot() {
		return false
	}

	return startupViaCCEnabled(node) || firstDriveInjectPartition(node) == 0
}

// firstDriveInjectPartition returns the first disk inject partition, defaulting to one.
func firstDriveInjectPartition(node ifaces.NodeSpec) int {
	if node.Hardware() == nil {
		return 1
	}

	drives := node.Hardware().Drives()
	if len(drives) == 0 {
		return 1
	}

	part := drives[0].InjectPartition()
	if part == nil {
		return 1
	}

	return *part
}

// addStartupInject adds a startup app injection unless startup injection suppression is active.
func addStartupInject(node ifaces.NodeSpec, src, dst string, suppress bool) {
	if suppress {
		return
	}

	node.AddInject(src, dst, "0644", "")
}

// addWindowsStartupWrapperInjects restores the Windows startup wrapper and the
// Start Menu scheduler that registers it as an on-start scheduled task.
func addWindowsStartupWrapperInjects(node ifaces.NodeSpec, startupDir string, suppress bool) error {
	err := tmpl.RestoreAsset(startupDir, windowsStartupWrapperAsset)
	if err != nil {
		return fmt.Errorf("restoring phenix startup script: %w", err)
	}

	addStartupInject(node, startupDir+"/"+windowsStartupWrapperAsset, windowsStartupWrapperDst, suppress)

	err = tmpl.RestoreAsset(startupDir, windowsSchedulerAsset)
	if err != nil {
		return fmt.Errorf("restoring windows startup scheduler: %w", err)
	}

	addStartupInject(node, startupDir+"/"+windowsSchedulerAsset, windowsSchedulerDst, suppress)

	return nil
}

// checkStartupC2Dir checks that an experiment name can name the directory,
// relative to minimega's files directory, that its startup scripts are staged
// in. An empty name, "." or ".." would stage them in, and point the globs that
// delete them at, the files directory itself or its parent, and a glob
// metacharacter would make the globs match other experiments' directories.
func checkStartupC2Dir(expName string) error {
	switch {
	case expName == "":
		return errors.New("experiment name required")
	case expName == "." || expName == ".." || strings.ContainsAny(expName, `/\*?[`):
		return fmt.Errorf(
			"experiment name %q cannot name the directory its C2 startup scripts are staged in", expName,
		)
	}

	return nil
}

// addStartupC2Script stages a generated startup script and queues its C2 commands.
func addStartupC2Script(exp *types.Experiment, node ifaces.NodeSpec, filename string, executor startupC2Executor) error {
	if err := checkStartupC2Dir(exp.Spec.ExperimentName()); err != nil {
		return fmt.Errorf("staging startup C2 script for node %s: %w", node.General().Hostname(), err)
	}

	send, exec, relPath := startupC2Commands(exp, filename, executor)
	if err := stageStartupC2Script(filename, relPath, executor); err != nil {
		return fmt.Errorf("staging startup C2 script for node %s: %w", node.General().Hostname(), err)
	}

	node.AddCommand(send)
	node.AddCommand(exec)

	return nil
}

// stageStartupC2Script copies a generated startup script into minimega's file
// path. A Linux script gets a guard that makes it run at most once per boot.
func stageStartupC2Script(src, relPath string, executor startupC2Executor) error {
	dst := startupMMFullPath(relPath)

	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return fmt.Errorf("creating startup C2 script directory: %w", err)
	}

	content, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("reading startup script: %w", err)
	}

	if executor != startupC2Windows {
		guard := startupC2RunOnceGuard(startupC2RanDir, linuxBootIDFile, startupC2ScriptKind(relPath))
		content = insertAfterShebang(content, guard)
	}

	if err := os.WriteFile(dst, content, 0o600); err != nil {
		return fmt.Errorf("writing startup C2 script: %w", err)
	}

	return nil
}

const (
	// startupC2RanDir holds, in the guest, the boot ID of the boot each Linux
	// startup script delivered over C2 last ran in.
	startupC2RanDir = "/run/phenix/startup-c2"
	linuxBootIDFile = "/proc/sys/kernel/random/boot_id"
)

// startupC2RunOnceGuard returns shell lines that end a Linux startup script
// delivered over C2 when it already ran in the guest's current boot. minimega
// gives a cc exec to a miniccc client each time it connects, and miniccc
// reconnects after missed heartbeats, a pause or a memory snapshot restore;
// the interfaces script would then take the guest's links down and flush their
// addresses in the middle of the experiment. The guard records the kernel's
// boot ID rather than relying on /run being cleared, so a reboot runs the
// script again either way. Without a boot ID the script runs every time.
func startupC2RunOnceGuard(dir, bootIDFile, kind string) string {
	return fmt.Sprintf(`
# phenix: minimega runs this script again whenever miniccc reconnects; run it
# at most once per boot.
phenix_boot_id=$(cat %[2]s 2>/dev/null)
if [ -n "$phenix_boot_id" ]; then
    phenix_ran=%[1]s/%[3]s.boot
    if [ "$(cat "$phenix_ran" 2>/dev/null)" = "$phenix_boot_id" ]; then
        exit 0
    fi
    mkdir -p %[1]s && echo "$phenix_boot_id" > "$phenix_ran"
fi
`, dir, bootIDFile, kind)
}

// startupC2ScriptKind returns the kind of a staged startup script, such as
// "interfaces" for <hostname>-interfaces.sh.
func startupC2ScriptKind(name string) string {
	base := path.Base(name)

	for _, suffix := range startupC2ScriptSuffixes {
		if strings.HasSuffix(base, suffix) {
			return strings.TrimSuffix(strings.TrimPrefix(suffix, "-"), path.Ext(suffix))
		}
	}

	return strings.TrimSuffix(base, path.Ext(base))
}

// insertAfterShebang inserts lines into a script after its #! line, if it
// has one, or else at its top.
func insertAfterShebang(script []byte, lines string) []byte {
	var shebang []byte

	if bytes.HasPrefix(script, []byte("#!")) {
		end := bytes.IndexByte(script, '\n')
		if end < 0 {
			end = len(script) - 1
		}

		shebang, script = script[:end+1], script[end+1:]
	}

	out := make([]byte, 0, len(shebang)+len(lines)+1+len(script))
	out = append(out, shebang...)

	if len(shebang) > 0 && shebang[len(shebang)-1] != '\n' {
		out = append(out, '\n')
	}

	out = append(out, lines...)

	return append(out, script...)
}

// startupC2Commands builds the minimega send and exec commands for a startup
// script. It uses exec, not exec-once: phenix queues the commands before any
// guest connects, and minimega marks an exec-once command sent when it is
// queued, even with no client connected, so no guest would run it. minimega
// sends an exec to each client when it connects, so the script runs at every
// boot, as an injected one does. A client also connects again within a boot,
// so a staged Linux script ends early when it already ran in that boot.
func startupC2Commands(exp *types.Experiment, filename string, executor startupC2Executor) (string, string, string) {
	var (
		relPath   = path.Join(exp.Spec.ExperimentName(), filepath.Base(filename))
		guestPath = path.Join("/tmp/miniccc/files", relPath)

		send = "send " + relPath
	)

	switch executor {
	case startupC2Windows:
		exec := fmt.Sprintf(
			"%s cmd /c 'powershell.exe -noprofile -executionpolicy bypass -file %s'",
			startupC2ExecVerb, guestPath,
		)

		return send, exec, relPath
	case startupC2Linux:
		fallthrough
	default:
		return send, startupC2ExecVerb + " bash " + guestPath, relPath
	}
}

// removeStartupC2Commands removes previously generated startup C2 commands for
// idempotency, including those generated under a hostname the node no longer
// has, which would otherwise run before the current ones.
func removeStartupC2Commands(exp *types.Experiment, node ifaces.NodeSpec) {
	setter, ok := node.(commandSetter)
	if !ok {
		return
	}

	commands := node.Commands()
	filtered := make([]string, 0, len(commands))

	for _, command := range commands {
		if isStartupC2Command(exp, command) {
			continue
		}

		filtered = append(filtered, command)
	}

	setter.SetCommands(filtered)
}

// isStartupC2Command reports whether command sends or runs a startup script
// the startup app staged for one of the experiment's nodes, under any hostname.
func isStartupC2Command(exp *types.Experiment, command string) bool {
	// No file name contains a NUL, so it marks where the name goes.
	const name = "\x00"

	for _, executor := range []startupC2Executor{startupC2Linux, startupC2Windows} {
		send, exec, _ := startupC2Commands(exp, name, executor)

		// Experiments stored before the scripts ran with exec hold the
		// exec-once form of the command.
		legacy := startupC2LegacyExecVerb + strings.TrimPrefix(exec, startupC2ExecVerb)

		for _, form := range []string{send, exec, legacy} {
			prefix, suffix, _ := strings.Cut(form, name)

			if len(command) < len(prefix)+len(suffix) ||
				!strings.HasPrefix(command, prefix) || !strings.HasSuffix(command, suffix) {
				continue
			}

			if isStartupC2ScriptName(command[len(prefix) : len(command)-len(suffix)]) {
				return true
			}
		}
	}

	return false
}

// isStartupC2ScriptName reports whether name is the file name of a startup
// script the startup app generates for some hostname.
func isStartupC2ScriptName(name string) bool {
	if strings.ContainsAny(name, "/ \t") {
		return false
	}

	for _, suffix := range startupC2ScriptSuffixes {
		if len(name) > len(suffix) && strings.HasSuffix(name, suffix) {
			return true
		}
	}

	return false
}

// startupC2ScriptSuffixes follow a node's hostname in the names of the
// startup scripts generated for it.
var startupC2ScriptSuffixes = []string{ //nolint:gochecknoglobals // constant list
	"-hostname.sh",
	"-timezone.sh",
	"-interfaces.sh",
	"-domain.sh",
	"-startup.ps1",
}

// StagedStartupScripts returns minimega file globs, relative to its files
// directory, that match every startup script the startup app stages there for
// C2 delivery to the given experiment's nodes.
func StagedStartupScripts(expName string) ([]string, error) {
	if err := checkStartupC2Dir(expName); err != nil {
		return nil, err
	}

	globs := make([]string, 0, len(startupC2ScriptSuffixes))

	for _, suffix := range startupC2ScriptSuffixes {
		globs = append(globs, path.Join(expName, "*"+suffix))
	}

	return globs, nil
}

// removeStartupInjections removes only injections owned by the startup app.
func removeStartupInjections(node ifaces.NodeSpec) {
	removeInjectionDestinations(node, map[string]struct{}{
		linuxHostnameInjectDst:    {},
		linuxTimezoneInjectDst:    {},
		linuxIfaceInjectDst:       {},
		linuxDomainInjectDst:      {},
		windowsStartupInjectDst:   {},
		windowsStartupWrapperDst:  {},
		windowsSchedulerDst:       {},
		"/" + windowsSchedulerDst: {},
	})
}

// removeWindowsStartupWrapperInjections removes the startup-owned Windows
// wrapper and Start Menu scheduler injections.
func removeWindowsStartupWrapperInjections(node ifaces.NodeSpec) {
	removeInjectionDestinations(node, map[string]struct{}{
		windowsStartupWrapperDst:  {},
		windowsSchedulerDst:       {},
		"/" + windowsSchedulerDst: {},
	})
}

// removeInjectionDestinations filters node injections whose destinations match dsts.
func removeInjectionDestinations(node ifaces.NodeSpec, dsts map[string]struct{}) {
	injections := node.Injections()
	filtered := make([]ifaces.NodeInjection, 0, len(injections))

	for _, injection := range injections {
		if _, ok := dsts[injection.Dst()]; ok {
			continue
		}

		filtered = append(filtered, injection)
	}

	node.SetInjections(filtered)
}

//nolint:cyclop,funlen,gocyclo,maintidx // complex logic
func (s Startup) PreStart(ctx context.Context, exp *types.Experiment) error {
	var (
		startupDir = exp.Spec.BaseDir() + "/startup"

		// detect duplicate IPs within a VLAN (VLAN|IP --> hostname)
		ips = make(map[string]string)
	)

	err := os.MkdirAll(startupDir, 0o750)
	if err != nil {
		return fmt.Errorf("creating experiment startup directory path: %w", err)
	}

	for _, node := range exp.Spec.Topology().Nodes() {
		// check for duplicate IPs (including any non-minimega topology nodes)
		if node.Network() != nil && node.Network().Interfaces() != nil {
			for _, iface := range node.Network().Interfaces() {
				if iface.Address() == "" {
					continue
				}

				ip := net.ParseIP(iface.Address())
				if ip == nil {
					return fmt.Errorf(
						"invalid IP %s provided for %s",
						iface.Address(),
						node.General().Hostname(),
					)
				}

				key := iface.Address()

				if util.PrivateIP(ip) {
					key = fmt.Sprintf("%s|%s", iface.VLAN(), iface.Address())
					if h, ok := ips[key]; ok {
						return fmt.Errorf(
							"duplicate private IP detected on VLAN %s: %s and %s both have %s configured",
							iface.VLAN(),
							h,
							node.General().Hostname(),
							iface.Address(),
						)
					}
				} else {
					if h, ok := ips[key]; ok {
						return fmt.Errorf(
							"duplicate public IP detected: %s and %s both have %s configured",
							h,
							node.General().Hostname(),
							iface.Address(),
						)
					}
				}

				ips[key] = node.General().Hostname()

				// Warn if a gateway is not on the interfaces subnet
				if !node.External() && iface.Gateway() != "" {
					if _, subnet, err := net.ParseCIDR(
						fmt.Sprintf("%s/%d", iface.Address(), iface.Mask()),
					); err == nil {
						if gw := net.ParseIP(iface.Gateway()); gw != nil && !subnet.Contains(gw) {
							notes.AddWarnings(ctx, false, fmt.Errorf(
								"node %q interface %q gateway %s is outside the interface subnet %s (ok only with an on-link route)",
								node.General().Hostname(), iface.Name(), iface.Gateway(), subnet,
							))
						}
					}
				}
			}
		}

		if node.External() {
			continue
		}

		// Ensure a node has at least one drive
		drives := node.Hardware().Drives()
		if len(drives) == 0 {
			return fmt.Errorf(
				"node %q has no drives defined; cannot determine disk image",
				node.General().Hostname(),
			)
		}

		// A relative image path is relative to minimega's files directory
		// (its -filepath), which is not necessarily <phenix base>/images.
		imagePath := drives[0].Image()

		if !filepath.IsAbs(imagePath) {
			imagePath = startupMMFullPath(imagePath)
		}

		_, statErr := statImage(imagePath)

		// A path through a regular file (ENOTDIR) names no image either.
		missingImage := errors.Is(statErr, fs.ErrNotExist) || errors.Is(statErr, syscall.ENOTDIR)

		if statErr != nil && !missingImage {
			// Any other error, such as a file name that is too long, would leave
			// the node to boot and minimega to fail its launch. minimega, which
			// may run as another user, may still read a path phenix may not, and
			// a node that does not boot needs no image.
			if dnb := node.General().DoNotBoot(); !errors.Is(statErr, fs.ErrPermission) && (dnb == nil || !*dnb) {
				return fmt.Errorf(
					"node %q: checking disk image %q: %w", node.General().Hostname(), imagePath, statErr,
				)
			}

			plog.Warn(
				plog.TypeSystem,
				"cannot check disk image",
				"node", node.General().Hostname(),
				"image", imagePath,
				"err", statErr,
			)

			notes.AddWarnings(ctx, false, fmt.Errorf(
				"node %q: cannot check disk image %q: %w", node.General().Hostname(), imagePath, statErr,
			))
		}

		// A node this app marked do-not-boot only because its image was missing
		// boots again once the image exists; a user's do-not-boot is kept.
		if reason, ok := node.GetAnnotation(DoNotBootReasonAnnotation); ok && reason == dnbReasonMissingImage && statErr == nil {
			node.General().SetDoNotBoot(false)
			delete(node.Annotations(), DoNotBootReasonAnnotation)
		}

		// check if the disk image is present, if not set do not boot to true and warn user
		if missingImage {
			// Record the reason only when this app sets the flag, so a node the
			// user already marked do-not-boot stays down once the image exists.
			if dnb := node.General().DoNotBoot(); dnb == nil || !*dnb {
				node.AddAnnotation(DoNotBootReasonAnnotation, dnbReasonMissingImage)
			}

			node.General().SetDoNotBoot(true)
			plog.Warn(
				plog.TypeSystem,
				"disk image not found; node will not boot",
				"node", node.General().Hostname(),
				"image", imagePath,
			)

			notes.AddWarnings(ctx, false, fmt.Errorf(
				"node %q will not boot: disk image %q not found",
				node.General().Hostname(), imagePath,
			))
		}

		useC2 := startupC2DeliveryEnabled(node)
		suppressStartupInjects := startupViaCCEnabled(node)

		removeStartupC2Commands(exp, node)

		if suppressStartupInjects {
			removeStartupInjections(node)
		}

		// Skip routers other than minirouters; vrouter's config sets the
		// hostname of a Vyatta or VyOS router.
		if strings.EqualFold(node.Type(), "Router") &&
			!strings.EqualFold(node.Hardware().OSType(), osMinirouter) {
			continue
		}

		// Skip generating startup scripts/templates for this node if default
		// apps are disabled, now that the checks/normalization above (which
		// should always run) have been performed.
		if !defaultAppsEnabled(node) {
			continue
		}

		// Check to see if a scenario exists for this experiment and if it
		// contains a "startup" app. If so, store it for later use
		var startupApp ifaces.ScenarioApp

		for _, app := range exp.Apps() {
			if app.Name() == appNameStartup {
				startupApp = app
			}
		}

		switch strings.ToLower(node.Hardware().OSType()) {
		case osMinirouter:
			// minirouter configures the interfaces itself, but nothing else
			// replaces the image's hostname "phenix", which hostname-mode C2
			// (SOH, delay.c2, autotunnel) does not match.
			hostnameFile := startupDir + "/" + node.General().Hostname() + "-hostname.sh"

			addStartupInject(node, hostnameFile, linuxHostnameInjectDst, suppressStartupInjects)

			err := tmpl.CreateFileFromTemplate("linux_hostname.tmpl", node.General().Hostname(), hostnameFile)
			if err != nil {
				return fmt.Errorf("generating minirouter hostname script: %w", err)
			}

			if useC2 {
				if err := addStartupC2Script(exp, node, hostnameFile, startupC2Linux); err != nil {
					return err
				}
			}
		case osLinux, "rhel", "centos":
			var (
				hostnameFile = startupDir + "/" + node.General().Hostname() + "-hostname.sh"
				timezoneFile = startupDir + "/" + node.General().Hostname() + "-timezone.sh"
				ifaceFile    = startupDir + "/" + node.General().Hostname() + "-interfaces.sh"
			)

			addStartupInject(node, hostnameFile, linuxHostnameInjectDst, suppressStartupInjects)
			addStartupInject(node, timezoneFile, linuxTimezoneInjectDst, suppressStartupInjects)
			addStartupInject(node, ifaceFile, linuxIfaceInjectDst, suppressStartupInjects)

			timeZone := "Etc/UTC"

			err := tmpl.CreateFileFromTemplate(
				"linux_hostname.tmpl",
				node.General().Hostname(),
				hostnameFile,
			)
			if err != nil {
				return fmt.Errorf("generating linux hostname script: %w", err)
			}

			err = tmpl.CreateFileFromTemplate("linux_timezone.tmpl", timeZone, timezoneFile)
			if err != nil {
				return fmt.Errorf("generating linux timezone script: %w", err)
			}

			err = tmpl.CreateFileFromTemplate("linux_interfaces.tmpl", node, ifaceFile)
			if err != nil {
				return fmt.Errorf("generating linux interfaces script: %w", err)
			}

			if useC2 {
				if err := addStartupC2Script(exp, node, hostnameFile, startupC2Linux); err != nil {
					return err
				}

				if err := addStartupC2Script(exp, node, timezoneFile, startupC2Linux); err != nil {
					return err
				}

				if err := addStartupC2Script(exp, node, ifaceFile, startupC2Linux); err != nil {
					return err
				}
			}

			if startupApp != nil {
				for _, host := range startupApp.Hosts() {
					if host.Hostname() == node.General().Hostname() {
						domainFile := startupDir + "/" + node.General().Hostname() + "-domain.sh"

						addStartupInject(node, domainFile, linuxDomainInjectDst, suppressStartupInjects)

						err := tmpl.CreateFileFromTemplate(
							"linux_domain.tmpl",
							host.Metadata(),
							domainFile,
						)
						if err != nil {
							return fmt.Errorf("generating linux domain script: %w", err)
						}

						if useC2 {
							if err := addStartupC2Script(exp, node, domainFile, startupC2Linux); err != nil {
								return err
							}
						}
					}
				}
			}

		case osWindows:
			startupFile := startupDir + "/" + node.General().Hostname() + "-startup.ps1"

			addStartupInject(node, startupFile, windowsStartupInjectDst, suppressStartupInjects)

			// The wrapper runs every script staged in /phenix/startup, and the
			// Start Menu scheduler registers the wrapper as an on-start
			// scheduled task. C2 delivery executes the generated script
			// directly, so neither is needed then.
			if useC2 {
				removeWindowsStartupWrapperInjections(node)
			} else if err := addWindowsStartupWrapperInjects(node, startupDir, suppressStartupInjects); err != nil {
				return err
			}

			// Temporary struct to send to the Windows Startup template.
			data := struct {
				Node     ifaces.NodeSpec
				Metadata map[string]any
			}{
				Node:     node,
				Metadata: make(map[string]any),
			}

			// If startup app exists, see if this node has a metadata entry
			// in the scenario app configuration.
			if startupApp != nil {
				for _, host := range startupApp.Hosts() {
					if host.Hostname() == node.General().Hostname() {
						data.Metadata = host.Metadata()
					}
				}
			}

			err := tmpl.CreateFileFromTemplate("windows_startup.tmpl", data, startupFile)
			if err != nil {
				return fmt.Errorf("generating windows startup script: %w", err)
			}

			if useC2 {
				if err := addStartupC2Script(exp, node, startupFile, startupC2Windows); err != nil {
					return err
				}
			}
		}
	}

	return nil
}

func (s Startup) PostStart(ctx context.Context, exp *types.Experiment) error {
	// A dry run launched no VM to tunnel to, and asking minimega about one
	// would create the experiment's namespace.
	if s.options.DryRun || exp.DryRun() {
		return nil
	}

	start := exp.Status.StartTime()

	for _, node := range exp.Spec.Topology().Nodes() {
		if node.External() {
			continue
		}

		// The autotunnel annotation is independent of default apps, so it is
		// honored regardless of whether default apps are enabled for this node.
		if annotation, ok := node.GetAnnotation("phenix/startup-autotunnel"); ok {
			var tunnels []string

			err := mapstructure.Decode(annotation, &tunnels)
			if err != nil {
				plog.Error(
					plog.TypeSystem,
					"parsing phenix/startup-autotunnel annotation",
					"exp",
					exp.Metadata.Name,
					"vm",
					node.General().Hostname(),
					"err",
					err,
				)
			} else {
				for _, config := range tunnels {
					tunnel := CreateTunnel{ //nolint:exhaustruct // partial initialization
						Experiment: exp.Metadata.Name,
						VM:         node.General().Hostname(),
						User:       "bot",
					}

					tokens := strings.Split(config, ":")

					switch len(tokens) {
					case tunnelConfigPartsPortOnly:
						tunnel.Sport = tokens[0]
						tunnel.Dhost = "127.0.0.1"
						tunnel.Dport = tokens[0]
					case tunnelConfigPartsPortHost:
						tunnel.Sport = tokens[0]
						tunnel.Dhost = "127.0.0.1"
						tunnel.Dport = tokens[1]
					case tunnelConfigPartsPortHostDest:
						tunnel.Sport = tokens[0]
						tunnel.Dhost = tokens[1]
						tunnel.Dport = tokens[2]
					default:
						plog.Error(
							plog.TypeSystem,
							"invalid phenix/startup-autotunnel annotation",
							"value",
							config,
						)
					}

					if tunnel.Sport == "" {
						continue
					}

					// Port forwards live in phenix ui, which subscribes only once it
					// serves. Any other process, such as `phenix experiment start`,
					// would drop the request, and exits before the VM's C2 is up.
					if !pubsub.HasSubscribers(CreateTunnelTopic) {
						plog.Warn(
							plog.TypeSystem,
							"startup autotunnel not created: autotunnels are created only for experiments started through phenix ui",
							"exp",
							exp.Metadata.Name,
							"vm",
							node.General().Hostname(),
							"tunnel",
							config,
						)

						continue
					}

					go requestAutotunnel(trackAutotunnelWait(exp.Metadata.Name), start, tunnel)
				}
			}
		}
	}

	return nil
}

// requestAutotunnel asks phenix ui for an autotunnel once the VM's C2 client
// registered, unless the tunnel is to VNC, which needs no miniccc agent.
func requestAutotunnel(wait *autotunnelWait, start string, msg CreateTunnel) {
	defer wait.wg.Done()

	if strings.ToUpper(msg.Dport) != "VNC" {
		err := waitForAutotunnelC2(wait.ctx, msg.Experiment, start, msg.VM)
		if errors.Is(err, errAutotunnelStopped) || wait.ctx.Err() != nil {
			plog.Info(
				plog.TypeSystem,
				"startup autotunnel not created: the experiment stopped before the C2 client registered",
				"exp", msg.Experiment, "vm", msg.VM,
			)

			return
		}

		if err != nil {
			plog.Error(
				plog.TypeSystem,
				"startup autotunnel not created: waiting for the C2 client",
				"exp", msg.Experiment, "vm", msg.VM, "err", err,
			)

			return
		}
	}

	pubsub.Publish(CreateTunnelTopic, msg)
}

func (Startup) Running(ctx context.Context, exp *types.Experiment) error {
	return nil
}

func (Startup) Cleanup(ctx context.Context, exp *types.Experiment) error {
	// Stop runs the cleanup apps before it clears the experiment's namespace.
	EndAutotunnelWaits(exp.Metadata.Name)

	return nil
}

var (
	// autotunnelC2Timeout bounds the wait for a VM's C2 client before its
	// autotunnel is created, and autotunnelC2Interval paces the checks.
	autotunnelC2Timeout  = mm.DefaultC2Timeout //nolint:gochecknoglobals // overridden by tests
	autotunnelC2Interval = 2 * time.Second     //nolint:gochecknoglobals // overridden by tests

	errAutotunnelStopped = errors.New("experiment stopped")

	// autotunnelWaits holds, per experiment, the goroutines that wait for a
	// VM's C2 client before they ask phenix ui for its autotunnel.
	autotunnelWaits   = make(map[string]*autotunnelWait) //nolint:gochecknoglobals // per-process state
	autotunnelWaitsMu sync.Mutex                         //nolint:gochecknoglobals // guards autotunnelWaits
)

// autotunnelWait lets the startup app's cleanup stop an experiment's
// autotunnel waits and wait for them to end. They query minimega in the
// experiment's namespace, and minimega creates a namespace again for any
// command that names it once stop has cleared it.
type autotunnelWait struct {
	ctx    context.Context //nolint:containedctx // canceled by the experiment's cleanup
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// trackAutotunnelWait records one more autotunnel wait of the experiment; the
// waiting goroutine calls wg.Done when it ends.
func trackAutotunnelWait(exp string) *autotunnelWait {
	autotunnelWaitsMu.Lock()
	defer autotunnelWaitsMu.Unlock()

	wait, ok := autotunnelWaits[exp]
	if !ok {
		ctx, cancel := context.WithCancel(context.Background())
		wait = &autotunnelWait{ctx: ctx, cancel: cancel} //nolint:exhaustruct // zero WaitGroup
		autotunnelWaits[exp] = wait
	}

	wait.wg.Add(1)

	return wait
}

// EndAutotunnelWaits cancels the experiment's autotunnel waits in this
// process and waits for them to end. phenix ui's port forward service calls it
// from the experiment's stopping hook, before it deletes the experiment's
// forwards, since the stopping hooks run before the cleanup apps.
func EndAutotunnelWaits(exp string) {
	autotunnelWaitsMu.Lock()
	wait := autotunnelWaits[exp]
	delete(autotunnelWaits, exp)
	autotunnelWaitsMu.Unlock()

	if wait == nil {
		return
	}

	wait.cancel()
	wait.wg.Wait()
}

// waitForAutotunnelC2 waits for vm's C2 client to register, up to
// autotunnelC2Timeout. It gives up with errAutotunnelStopped once the
// experiment is no longer stored with the start time PostStart saw, which a
// stop from another process clears; this process's stop cancels ctx instead.
func waitForAutotunnelC2(ctx context.Context, exp, start, vm string) error {
	deadline := time.Now().Add(autotunnelC2Timeout)

	for {
		if !ExperimentStillStarted(exp, start) {
			return errAutotunnelStopped
		}

		began := time.Now()

		err := mm.IsC2ClientActive(mm.C2Context(ctx), mm.C2NS(exp), mm.C2VM(vm), mm.C2Timeout(time.Second))
		if err == nil || ctx.Err() != nil {
			return ctx.Err()
		}

		if !errors.Is(err, mm.ErrC2ClientNotActive) || !time.Now().Before(deadline) {
			return err //nolint:wrapcheck // logged by the caller
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Until(began.Add(autotunnelC2Interval))):
		}
	}
}

// ExperimentStillStarted reports whether the experiment is still stored with
// the given start time, which a stop clears and a later start replaces. An
// app's wait polls it to notice a stop from another process, which cannot
// cancel this process's context. If the store can't tell, it reports true.
func ExperimentStillStarted(name, start string) bool {
	exp := types.Experiment{Metadata: store.ConfigMetadata{Name: name}} //nolint:exhaustruct // loaded below

	if err := exp.Reload(); err != nil {
		return !errors.Is(err, store.ErrNotExist)
	}

	return exp.Status != nil && exp.Status.StartTime() == start
}
