package vm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"phenix/api/experiment"
	"phenix/app"
	ifaces "phenix/types/interfaces"
	v1 "phenix/types/version/v1"
	"phenix/util/common"
	"phenix/util/file"
	"phenix/util/mm"
	"phenix/util/mm/mmcli"
	"phenix/util/plog"
)

const (
	vmStateQuit          = "QUIT"
	vmStatePaused        = "PAUSED"
	vmStateRunning       = "RUNNING"
	vmInfoCmd            = "vm info"
	statusCompleted      = "completed"
	statusFailed         = "failed"
	statusCancelled      = "cancelled"
	shutdownTimeout      = 30 * time.Second
	copyProgressFactor   = 0.8
	commitWait           = 2 * time.Second
	commitProgressBase   = 0.8
	commitProgressFactor = 0.2
)

var vlanAliasRegex = regexp.MustCompile(`(.*) \(\d*\)`)

// vmStatesKillable are the states of a VM that `vm kill` applies to
// (minimega VM_KILLABLE).
var vmStatesKillable = []string{"BUILDING", vmStateRunning, vmStatePaused} //nolint:gochecknoglobals // constant list

var (
	// ErrNoDiskSnapshot is returned by Snapshot for a VM whose disk minimega
	// cannot save: one that runs with snapshot: false, or one without a disk.
	ErrNoDiskSnapshot = errors.New("minimega has no disk snapshot to save")

	// ErrMultiDiskSnapshot is returned by Snapshot for a VM with several disks:
	// minimega saves each of them, but a restore gives the VM only the first.
	ErrMultiDiskSnapshot = errors.New("phenix restores only the first disk of a snapshot")

	// ErrImageExists is wrapped by the error CommitToDisk returns for a new
	// disk image whose name an existing file has.
	ErrImageExists = errors.New("disk image already exists")

	// ErrPartialList is wrapped by the error List returns together with the
	// VMs of the hosts that listed theirs when other hosts did not, so that
	// callers that show VMs can show those.
	ErrPartialList = errors.New("some hosts did not list their VMs")
)

// mmFullPath resolves a path relative to minimega's files directory.
var mmFullPath = mm.GetMMFullPath //nolint:gochecknoglobals // overridden by tests

func Count(expName string) (int, error) {
	if expName == "" {
		return 0, errors.New("no experiment name provided")
	}

	exp, err := experiment.Get(expName)
	if err != nil {
		return 0, fmt.Errorf("getting experiment %s: %w", expName, err)
	}

	return len(exp.Spec.Topology().Nodes()), nil
}

// List collects VMs, combining topology settings with running VM details if the
// experiment is running. It returns a slice of VM structs and any errors
// encountered while gathering them. When minimega fails for some hosts of a
// running experiment, the VMs the other hosts listed are returned together
// with an error wrapping ErrPartialList.
func List(expName string) ([]mm.VM, error) { //nolint:funlen // complex logic
	if expName == "" {
		return nil, errors.New("no experiment name provided")
	}

	exp, err := experiment.Get(expName)
	if err != nil {
		return nil, fmt.Errorf("getting experiment %s: %w", expName, err)
	}

	var (
		running = make(map[string]mm.VM)
		vms     []mm.VM
		listErr error
	)

	// A dry run never reached minimega; asking it would only create an empty
	// namespace for the experiment.
	if exp.Running() && !exp.DryRun() {
		// The loop below leaves out every VM minimega does not list, so a failed
		// listing would otherwise read as an experiment without VMs. minimega
		// answers once per namespace host, so the VMs of the hosts that answered
		// are kept for callers that show what they can.
		details, err := mm.GetVMInfoErr(mm.NS(expName))

		switch {
		case err != nil && len(details) > 0:
			listErr = fmt.Errorf("getting VMs of %s from minimega: %w: %w", expName, ErrPartialList, err)
		case err != nil:
			listErr = fmt.Errorf("getting VMs of %s from minimega: %w", expName, err)
		}

		for _, vm := range details {
			running[vm.Name] = vm
		}
	}

	for idx, node := range exp.Spec.Topology().Nodes() {
		var (
			disk            string
			dnb             bool
			snapshot        bool
			injectPartition int
		)

		if drives := node.Hardware().Drives(); len(drives) > 0 {
			disk = mm.GetMMFullPath(drives[0].Image())
			injectPartition = *drives[0].InjectPartition()
		}

		if node.General().DoNotBoot() != nil {
			dnb = *node.General().DoNotBoot()
		}

		if node.General().Snapshot() != nil {
			snapshot = *node.General().Snapshot()
		}

		vm := mm.VM{ //nolint:exhaustruct // partial initialization
			ID:              idx,
			Name:            node.General().Hostname(),
			Description:     node.General().Description(),
			Experiment:      exp.Spec.ExperimentName(),
			CPUs:            node.Hardware().VCPU(),
			RAM:             node.Hardware().Memory(),
			Disk:            disk,
			InjectPartition: injectPartition,
			Interfaces:      make(map[string]string),
			DoNotBoot:       dnb,
			Type:            node.Type(),
			OSType:          node.Hardware().OSType(),
			Snapshot:        snapshot,
			Tags:            node.Labels(),
		}

		ifaceNameByVLAN := make(map[string]string)

		for _, iface := range node.Network().Interfaces() {
			vm.IPv4 = append(vm.IPv4, iface.Address()) // empty for DHCP

			if iface.VLAN() != "" { // might be empty for external nodes
				vm.Networks = append(vm.Networks, iface.VLAN())
				vm.IfaceNames = append(vm.IfaceNames, iface.Name())
				vm.Interfaces[iface.VLAN()] = iface.Address() // empty for DHCP
				ifaceNameByVLAN[iface.VLAN()] = iface.Name()
			}
		}

		details, exists := running[vm.Name]
		if exp.Running() && !exp.DryRun() && !node.External() && !dnb && !exists {
			continue
		}

		switch {
		case node.External():
			vm.State = "EXTERNAL"
		case exists:
			vm.Host = details.Host
			vm.State = details.State
			vm.Running = details.Running
			vm.Networks = details.Networks
			vm.Taps = details.Taps
			vm.IPv4 = details.IPv4
			vm.Captures = details.Captures
			vm.CdRom = details.CdRom
			vm.Tags = details.Tags
			vm.Uptime = details.Uptime
			vm.CPUs = details.CPUs
			vm.RAM = details.RAM
			vm.Disk = details.Disk
			vm.CCActive = details.CCActive

			// `vm.IPv4` could be nil/empty if minimega isn't reporting any IPs for it
			if len(vm.IPv4) == 0 {
				vm.IPv4 = make([]string, len(details.Networks))
			}

			// minimega is the source of truth for interface ordering in a running
			// experiment, so rebuild `vm.IfaceNames` to match `details.Networks`
			// instead of leaving it in the (possibly different) order the
			// interfaces were declared in the topology.
			vm.IfaceNames = make([]string, len(details.Networks))

			// Since we get the IP from the experiment config, but the network name
			// from minimega (to preserve iface to network ordering), make sure the
			// ordering of IPs matches the ordering of networks. We could just use a
			// map here, but then the iface to network ordering that minimega ensures
			// would be lost.
			for i, nw := range details.Networks {
				// At this point, `nw` will look something like `EXP_1 (101)`. In the
				// experiment config, we just have `EXP_1` so we need to use that
				// portion from minimega as the `Interfaces`/`ifaceNameByVLAN` map key.
				match := vlanAliasRegex.FindStringSubmatch(nw)

				// If it's set here, we got it from minimega, which is the source of truth
				// for running experiments.
				if vm.IPv4[i] == "" {
					if match != nil {
						vm.IPv4[i] = vm.Interfaces[match[1]]
					} else {
						vm.IPv4[i] = "n/a"
					}
				}

				if match != nil {
					vm.IfaceNames[i] = ifaceNameByVLAN[match[1]]
				}
			}
		default:
			vm.Host = exp.Spec.Schedules()[vm.Name]
		}

		vms = append(vms, vm)
	}

	return vms, listErr
}

// Get retrieves the VM with the given name from the experiment with the given
// name. If the experiment is running, topology VM settings are combined with
// running VM details. It returns a pointer to a VM struct, and any errors
// encountered while retrieving the VM.
func Get(expName, vmName string) (*mm.VM, error) { //nolint:funlen // complex logic
	if expName == "" {
		return nil, errors.New("no experiment name provided")
	}

	if vmName == "" {
		return nil, errors.New("no VM name provided")
	}

	exp, err := experiment.Get(expName)
	if err != nil {
		return nil, fmt.Errorf("getting experiment %s: %w", expName, err)
	}

	var (
		vm              *mm.VM
		external        bool
		ifaceNameByVLAN = make(map[string]string)
	)

	for idx, node := range exp.Spec.Topology().Nodes() {
		if node.General().Hostname() != vmName {
			continue
		}

		// External nodes may have no drives and unset flags; read them the same
		// guarded way List does.
		var (
			disk            string
			dnb             bool
			snapshot        bool
			injectPartition int
		)

		if drives := node.Hardware().Drives(); len(drives) > 0 {
			disk = mm.GetMMFullPath(drives[0].Image())
			injectPartition = *drives[0].InjectPartition()
		}

		if node.General().DoNotBoot() != nil {
			dnb = *node.General().DoNotBoot()
		}

		if node.General().Snapshot() != nil {
			snapshot = *node.General().Snapshot()
		}

		external = node.External()

		vm = &mm.VM{ //nolint:exhaustruct // partial initialization
			ID:              idx,
			Name:            node.General().Hostname(),
			Description:     node.General().Description(),
			Experiment:      exp.Spec.ExperimentName(),
			CPUs:            node.Hardware().VCPU(),
			RAM:             node.Hardware().Memory(),
			Disk:            disk,
			InjectPartition: injectPartition,
			Interfaces:      make(map[string]string),
			DoNotBoot:       dnb,
			Type:            node.Type(),
			OSType:          node.Hardware().OSType(),
			Snapshot:        snapshot,
			Metadata:        make(map[string]any),
			Labels:          node.Labels(),
			Tags:            node.Labels(),
			Annotations:     node.Annotations(),
		}

		for _, iface := range node.Network().Interfaces() {
			vm.IPv4 = append(vm.IPv4, iface.Address()) // empty for DHCP
			vm.Networks = append(vm.Networks, iface.VLAN())
			vm.IfaceNames = append(vm.IfaceNames, iface.Name())
			vm.Interfaces[iface.VLAN()] = iface.Address() // empty for DHCP
			ifaceNameByVLAN[iface.VLAN()] = iface.Name()
		}

		for _, app := range exp.Apps() {
			for _, h := range app.Hosts() {
				if h.Hostname() == vm.Name {
					vm.Metadata[app.Name()] = h.Metadata()
				}
			}
		}
	}

	if vm == nil {
		return nil, fmt.Errorf("%w: %s is not a node of experiment %s", mm.ErrVMNotFound, vmName, expName)
	}

	if !exp.Running() || exp.DryRun() {
		vm.Host = exp.Spec.Schedules()[vm.Name]

		return vm, nil
	}

	// minimega knows nothing about external nodes.
	if external {
		vm.State = "EXTERNAL"

		return vm, nil
	}

	// A failed listing must not read as a VM that is not running; another
	// host of the namespace failing does not matter once the VM's own host
	// listed it.
	details, err := mm.GetVMInfoErr(mm.NS(expName), mm.VMName(vmName))
	if err != nil && len(details) != 1 {
		return nil, fmt.Errorf("getting VM %s of %s from minimega: %w", vmName, expName, err)
	}

	if len(details) != 1 {
		return vm, nil
	}

	vm.Host = details[0].Host
	vm.State = details[0].State
	vm.Running = details[0].Running
	vm.Networks = details[0].Networks
	vm.Taps = details[0].Taps
	vm.IPv4 = details[0].IPv4
	vm.Captures = details[0].Captures
	vm.CdRom = details[0].CdRom
	vm.Tags = details[0].Tags
	vm.Uptime = details[0].Uptime
	vm.CPUs = details[0].CPUs
	vm.RAM = details[0].RAM
	vm.Disk = details[0].Disk
	vm.CCActive = details[0].CCActive

	// `vm.IPv4` could be nil/empty if minimega isn't reporting any IPs for it
	if len(vm.IPv4) == 0 {
		vm.IPv4 = make([]string, len(details[0].Networks))
	}

	// minimega is the source of truth for interface ordering in a running
	// experiment, so rebuild `vm.IfaceNames` to match `details[0].Networks`
	// instead of leaving it in the (possibly different) order the interfaces
	// were declared in the topology.
	vm.IfaceNames = make([]string, len(details[0].Networks))

	// Since we get the IP from the experiment config, but the network name from
	// minimega (to preserve iface to network ordering), make sure the ordering of
	// IPs matches the ordering of networks. We could just use a map here, but then
	// the iface to network ordering that minimega ensures would be lost.
	for idx, nw := range details[0].Networks {
		// At this point, `nw` will look something like `EXP_1 (101)`. In the exp,
		// we just have `EXP_1` so we need to use that portion from minimega as the
		// `Interfaces`/`ifaceNameByVLAN` map key.
		match := vlanAliasRegex.FindStringSubmatch(nw)

		// If it's set here, we got it from minimega, which is the source of truth
		// for running experiments.
		if vm.IPv4[idx] == "" {
			if match != nil {
				vm.IPv4[idx] = vm.Interfaces[match[1]]
			} else {
				vm.IPv4[idx] = "n/a"
			}
		}

		if match != nil {
			vm.IfaceNames[idx] = ifaceNameByVLAN[match[1]]
		}
	}

	return vm, nil
}

func Update(opts ...UpdateOption) error { //nolint:funlen,gocyclo,cyclop  // complex logic
	o := newUpdateOptions(opts...)

	if o.exp == "" || o.vm == "" {
		return errors.New("experiment or VM name not provided")
	}

	exp, err := experiment.Get(o.exp)
	if err != nil {
		return fmt.Errorf("unable to get experiment %s: %w", o.exp, err)
	}

	vm := exp.Spec.Topology().FindNodeByName(o.vm)
	if vm == nil {
		return fmt.Errorf("unable to find VM %s in experiment %s", o.vm, o.exp)
	}

	// if appending, copy over old labels (keep newer version if present)
	if o.tags != nil && o.appendTags {
		for k, v := range vm.Labels() {
			if _, ok := (*o.tags)[k]; !ok {
				(*o.tags)[k] = v
			}
		}
	}

	running := experiment.Running(o.exp)

	// The only settings that can be updated while an experiment is running is the
	// VLAN an interface is connected to and the vm's tags
	if running {
		unsupported := o.cpu != 0 ||
			o.mem != 0 ||
			o.disk != "" ||
			o.partition != 0 ||
			o.dnb != nil ||
			o.host != nil ||
			o.snapshot != nil

		if unsupported {
			return errors.New("only interface connections and tags can be updated while experiment is running")
		}

		if o.iface == nil && o.tags == nil {
			return errors.New(
				"only interface connections and tags can be updated while experiment is running",
			)
		}

		// Refuse a label minimega cannot store before anything changes.
		if o.tags != nil {
			if err := mm.CheckVMTags(o.vm, *o.tags); err != nil {
				return err //nolint:wrapcheck // names the VM and the label
			}
		}

		if o.iface != nil {
			if o.iface.vlan == "" {
				err := Disconnect(o.exp, o.vm, o.iface.index)
				if err != nil {
					return err
				}
			} else {
				err := Connect(o.exp, o.vm, o.iface.index, o.iface.vlan)
				if err != nil {
					return err
				}
			}
		}

		if o.tags != nil {
			// update both the live minimega tags and the experiment spec labels;
			// a dry run has no VMs in minimega
			if !exp.DryRun() {
				err := mm.SetVMTags(mm.NS(o.exp), mm.VMName(o.vm), mm.Tags(*o.tags))
				if err != nil {
					return err
				}
			}

			vm.SetLabels(*o.tags)

			err = experiment.Save(
				experiment.SaveWithName(o.exp),
				experiment.SaveWithSpec(exp.Spec),
			)
			if err != nil {
				return fmt.Errorf("unable to save experiment with updated VM: %w", err)
			}
		}

		return nil
	}

	// An external node has no drives to change.
	if drives := vm.Hardware().Drives(); len(drives) == 0 && (o.disk != "" || o.partition != 0) {
		return fmt.Errorf("unable to change the disk of VM %s in experiment %s: it has no disk", o.vm, o.exp)
	}

	// The start gives the image to QEMU, which runs as root, so a user who
	// may change VMs must not pick any host file.
	if o.disk != "" {
		if err := checkDiskImage(exp.Spec.Topology(), "disk", o.vm, o.disk); err != nil {
			return err
		}
	}

	if err := checkWritableDisk(exp.Spec.Topology(), vm, o); err != nil {
		return err
	}

	if o.cpu != 0 {
		vm.Hardware().SetVCPU(o.cpu)
	}

	if o.mem != 0 {
		vm.Hardware().SetMemory(o.mem)
	}

	if o.disk != "" {
		setDisk(vm, o.disk)
	}

	if o.partition != 0 {
		vm.Hardware().Drives()[0].SetInjectPartition(&o.partition)
	}

	if o.dnb != nil {
		vm.General().SetDoNotBoot(*o.dnb)

		// The user's choice now, so the startup app must not clear it later.
		delete(vm.Annotations(), app.DoNotBootReasonAnnotation)
	}

	if o.tags != nil {
		vm.SetLabels(*o.tags)
	}

	if o.host != nil {
		if *o.host == "" {
			delete(exp.Spec.Schedules(), o.vm)
		} else {
			_ = exp.Spec.ScheduleNode(o.vm, *o.host)
		}
	}

	if o.snapshot != nil {
		vm.General().SetSnapshot(*o.snapshot)
	}

	err = experiment.Save(experiment.SaveWithName(o.exp), experiment.SaveWithSpec(exp.Spec))
	if err != nil {
		return fmt.Errorf("unable to save experiment with updated VM: %w", err)
	}

	return nil
}

// checkWritableDisk returns an error wrapping ErrInvalidDisk when the change
// o of a stopped experiment's VM node would leave it with snapshot: false on
// a disk the topology did not give a node, whichever request changes the
// disk or the flag: QEMU then writes the disk itself.
func checkWritableDisk(topo ifaces.TopologySpec, node ifaces.NodeSpec, o updateOptions) error {
	if o.disk == "" && o.snapshot == nil {
		return nil
	}

	image := o.disk
	if drives := node.Hardware().Drives(); image == "" && len(drives) > 0 {
		image = drives[0].Image()
	}

	snapshot := node.General().Snapshot() != nil && *node.General().Snapshot()
	if o.snapshot != nil {
		snapshot = *o.snapshot
	}

	if snapshot || image == "" {
		return nil
	}

	if problem := writableDiskProblem(topo, image); problem != "" {
		return invalidDisk("disk", image, o.vm, problem)
	}

	return nil
}

// setDisk gives the first drive of node image, and records in
// TopologyDiskAnnotation the image the topology gave it while the drive has
// another.
func setDisk(node ifaces.NodeSpec, image string) {
	original := topologyDisk(node)

	node.Hardware().Drives()[0].SetImage(image)

	if filepath.Clean(mmFullPath(image)) == filepath.Clean(mmFullPath(original)) {
		delete(node.Annotations(), TopologyDiskAnnotation)
	} else {
		node.AddAnnotation(TopologyDiskAnnotation, original)
	}
}

func Screenshot(expName, vmName, size string) ([]byte, error) {
	if err := requireRunning(expName); err != nil {
		return nil, err
	}

	screenshot, err := mm.GetVMScreenshot(
		mm.NS(expName),
		mm.VMName(vmName),
		mm.ScreenshotSize(size),
	)
	if err != nil {
		return nil, fmt.Errorf("getting VM screenshot: %w", err)
	}

	return screenshot, nil
}

// Pause stops a running VM with the given name in the experiment with the given
// name. It returns any errors encountered while pausing the VM.
func Pause(expName, vmName string) error {
	if expName == "" {
		return errors.New("no experiment name provided")
	}

	if vmName == "" {
		return errors.New("no VM name provided")
	}

	if err := requireRunning(expName); err != nil {
		return err
	}

	err := StopCaptures(expName, vmName)
	if err != nil && !errors.Is(err, ErrNoCaptures) {
		return fmt.Errorf("stopping captures for VM %s in experiment %s: %w", vmName, expName, err)
	}

	if err := mm.StopVM(mm.NS(expName), mm.VMName(vmName)); err != nil {
		return fmt.Errorf("pausing VM: %w", err)
	}

	return nil
}

// Restart restarts a running VM with the given name in the experiment with the given
// name. It returns any errors encountered while restarting the VM.
func Restart(expName, vmName string) error {
	if expName == "" {
		return errors.New("no experiment name provided")
	}

	if vmName == "" {
		return errors.New("no VM name provided")
	}

	if err := requireRunning(expName); err != nil {
		return err
	}

	state, err := mm.GetVMState(mm.NS(expName), mm.VMName(vmName))
	if err != nil {
		return fmt.Errorf("retrieving state for VM %s in experiment %s: %w", vmName, expName, err)
	}

	// Using "system_reset" on a VM that is in the "QUIT" state fails
	if state == vmStateQuit {
		return mm.StartVM(mm.NS(expName), mm.VMName(vmName))
	}

	cmd := mmcli.NewNamespacedCommand(expName)
	qmp := `{ "execute": "system_reset" }`
	cmd.Command = fmt.Sprintf("vm qmp %s '%s'", vmName, qmp)

	_, err = mmcli.SingleResponse(mmcli.Run(cmd))
	if err != nil {
		return fmt.Errorf("restarting VM %s: %w", vmName, err)
	}

	return nil
}

// Shutdown powers off a running VM with the given name in the experiment with the given
// name. It returns any errors encountered while shutting down the VM.
func Shutdown(expName, vmName string) error {
	if expName == "" {
		return errors.New("no experiment name provided")
	}

	if vmName == "" {
		return errors.New("no VM name provided")
	}

	if err := requireRunning(expName); err != nil {
		return err
	}

	state, err := mm.GetVMState(mm.NS(expName), mm.VMName(vmName))
	if err != nil {
		return fmt.Errorf("retrieving state for VM %s in experiment %s: %w", vmName, expName, err)
	}

	// No need to power off a VM that has already been powered down
	if state == vmStateQuit {
		return nil
	}

	// Stop all packet captures for a vm that will be powered down
	err = StopCaptures(expName, vmName)
	if err != nil && !errors.Is(err, ErrNoCaptures) {
		return fmt.Errorf("stopping captures for VM %s in experiment %s: %w", vmName, expName, err)
	}

	// Send a powerdown signal to the VM using QEMU QMP.
	cmd := mmcli.NewNamespacedCommand(expName)
	qmp := `{ "execute": "system_powerdown" }`
	cmd.Command = fmt.Sprintf("vm qmp %s '%s'", vmName, qmp)

	if err := mmcli.ErrorResponse(mmcli.Run(cmd)); err != nil {
		cmd.Command = "vm kill " + vmName

		err := mmcli.ErrorResponse(mmcli.Run(cmd))
		if err != nil {
			return fmt.Errorf("shutting down VM %s in experiment %s: %w", vmName, expName, err)
		}
	}

	waitForShutdown := func() bool {
		// Give the VM a maximum of 30s to shutdown.
		after := time.After(shutdownTimeout)

		for {
			select {
			case <-after:
				return false
			default:
				time.Sleep(1 * time.Second)

				state, _ := mm.GetVMState(mm.NS(expName), mm.VMName(vmName))
				if state == vmStateQuit {
					return true
				}
			}
		}
	}

	if !waitForShutdown() {
		// Forced shutdown implementation is equivalent to killing the vm without a
		// flush to preserve the state.
		cmd.Command = "vm kill " + vmName

		err := mmcli.ErrorResponse(mmcli.Run(cmd))
		if err != nil {
			return fmt.Errorf("shutting down VM %s in experiment %s: %w", vmName, expName, err)
		}
	}

	return nil
}

// ResetDiskState restores the disk state of a vm to the initial disk state
// It returns any errors encountered while restarting the VM.
func ResetDiskState(expName, vmName string) error {
	if expName == "" {
		return errors.New("no experiment name provided")
	}

	if vmName == "" {
		return errors.New("no VM name provided")
	}

	if err := requireRunning(expName); err != nil {
		return err
	}

	// Overwrite the snapshot in the vm instance
	// directory with a new snapshot.
	cmd := mmcli.NewNamespacedCommand(expName)
	cmd.Command = vmInfoCmd
	cmd.Columns = []string{"host", "name", "id", "state", "disks", "snapshot"}
	cmd.Filters = []string{"name=" + vmName}

	status := mmcli.RunTabular(cmd)

	if len(status) == 0 {
		return errors.New("vm not found")
	}

	var (
		origSnap  = strings.Split(status[0]["disks"], ",")[0]
		finalDst  = fmt.Sprintf("%s/%s/%s", common.MinimegaBase, status[0]["id"], "disk-0.qcow2")
		node      = status[0]["host"]
		cmdPrefix = ""
	)

	// Make sure the snapshot flag is set
	if status[0]["snapshot"] == "false" {
		return fmt.Errorf("snapshot flag for %s was not set", vmName)
	}

	// QEMU holds the instance disk open, with a write lock, while the VM is
	// killable (BUILDING, RUNNING or PAUSED), so such a VM is killed before
	// its disk is replaced. minimega refuses to kill a QUIT or ERROR VM, whose
	// QEMU is gone, and `vm start` relaunches it. Stop all packet captures for
	// a vm that will be reset to its original state; a vm that has been shut
	// down has none.
	killed := slices.Contains(vmStatesKillable, status[0]["state"])

	if killed {
		err := StopCaptures(expName, vmName)
		if err != nil && !errors.Is(err, ErrNoCaptures) {
			return fmt.Errorf(
				"stopping captures for VM %s in experiment %s: %w",
				vmName,
				expName,
				err,
			)
		}

		// Kill the vm without a flush to preserve state
		cmd := mmcli.NewNamespacedCommand(expName)
		cmd.Command = "vm kill " + vmName

		if err := mmcli.ErrorResponse(mmcli.Run(cmd)); err != nil {
			return fmt.Errorf("killing VM %s in experiment %s: %w", vmName, expName, err)
		}
	}

	if !mm.IsHeadnode(node) {
		cmdPrefix = "mesh send " + node
	}

	// Replace the VM's instance disk with a new snapshot of its original disk,
	// which drops every change made since the VM started. minimega records the
	// backing file relative to the destination unless it runs with
	// -abssnapshot, so the snapshot must be created where the VM uses it.
	cmd.Command = fmt.Sprintf("%s disk snapshot %s %s", cmdPrefix, origSnap, finalDst)

	err := mmcli.ErrorResponse(mmcli.Run(cmd))
	if err != nil {
		err = fmt.Errorf(
			"taking disk snapshot remotely for VM %s in experiment %s: %w",
			vmName,
			expName,
			err,
		)

		// The VM's old instance disk is still in place, so a VM killed above
		// runs again on it instead of being left powered off.
		if killed {
			if startErr := mm.StartVM(mm.NS(expName), mm.VMName(vmName)); startErr != nil {
				return fmt.Errorf("%w; starting VM %s again on its old disk: %w", err, vmName, startErr)
			}
		}

		return err
	}

	// restart the vm
	err = mm.StartVM(mm.NS(expName), mm.VMName(vmName))
	if err != nil {
		return fmt.Errorf("starting VM %s in experiment %s: %w", vmName, expName, err)
	}

	return nil
}

// Resume starts a paused VM with the given name in the experiment with the
// given name. It returns any errors encountered while resuming the VM.
func Resume(expName, vmName string) error {
	if expName == "" {
		return errors.New("no experiment name provided")
	}

	if vmName == "" {
		return errors.New("no VM name provided")
	}

	if err := requireRunning(expName); err != nil {
		return err
	}

	err := mm.StartVM(mm.NS(expName), mm.VMName(vmName))
	if err != nil {
		return fmt.Errorf("resuming VM: %w", err)
	}

	return nil
}

// Redeploy redeploys a VM with the given name in the experiment with the given
// name. Multiple redeploy options can be passed to alter the resulting
// redeployed VM, such as CPU, memory, and disk options. It returns any errors
// encountered while redeploying the VM.
func Redeploy(expName, vmName string, opts ...RedeployOption) error {
	if expName == "" {
		return errors.New("no experiment name provided")
	}

	if vmName == "" {
		return errors.New("no VM name provided")
	}

	if err := requireRunning(expName); err != nil {
		return err
	}

	o := newRedeployOptions(opts...)

	exp, err := experiment.Get(expName)
	if err != nil {
		return fmt.Errorf("getting experiment %s: %w", expName, err)
	}

	node := exp.Spec.Topology().FindNodeByName(vmName)

	switch {
	case node == nil:
		return fmt.Errorf("VM %s not found in experiment %s: %w", vmName, expName, ErrNotRedeployable)
	case node.External():
		return fmt.Errorf("VM %s is an external node, which minimega does not run: %w", vmName, ErrNotRedeployable)
	case o.inject && len(node.Hardware().Drives()) == 0:
		return fmt.Errorf("VM %s has no disk to replicate injections on: %w", vmName, ErrNotRedeployable)
	case o.inject && (node.General().Snapshot() == nil || !*node.General().Snapshot()):
		// The start injects only into the disk snapshot it makes for a node
		// with snapshot: true; the disk of any other node is its own image,
		// which recreating a snapshot would delete.
		return fmt.Errorf(
			"VM %s runs with snapshot: false, so it has no disk snapshot to recreate and inject: %w",
			vmName, ErrNotRedeployable,
		)
	}

	if o.disk != "" {
		if err := checkRedeployDisk(exp.Spec.Topology(), vmName, o.disk); err != nil {
			return err
		}

		// The redeployed VM keeps its snapshot flag, and with snapshot: false
		// QEMU writes the new disk itself.
		if snapshot := node.General().Snapshot(); snapshot == nil || !*snapshot {
			image, _, _ := strings.Cut(o.disk, ",")

			if problem := writableDiskProblem(exp.Spec.Topology(), image); problem != "" {
				return invalidDisk("disk", o.disk, vmName, problem)
			}
		}
	}

	var injects, deletions []string

	if o.inject {
		if o.disk == "" {
			o.disk = node.Hardware().Drives()[0].Image()
			o.part = *node.Hardware().Drives()[0].InjectPartition()
		}

		injects, deletions, err = redeployFiles(node, exp.Spec.BaseDir())
		if err != nil {
			return fmt.Errorf("replicating injections: %w", err)
		}
	}

	mmOpts := []mm.Option{
		mm.NS(expName),
		mm.VMName(vmName),
		mm.CPU(o.cpu),
		mm.Mem(o.mem),
		mm.Disk(o.disk),
		mm.Injects(injects...),
		mm.Deletions(deletions...),
		mm.InjectPartition(o.part),
		mm.RecreateSnapshot(o.inject),
	}

	err = mm.RedeployVM(mmOpts...)
	if err != nil {
		return fmt.Errorf("redeploying VM: %w", err)
	}

	return nil
}

// redeployFiles returns the injections of node, as the src:dst pairs the
// start-time script injects with sources relative to baseDir, and the paths of
// its deletions, which a redeploy that replicates injections replays in that
// order on the recreated disk snapshot. It refuses the injections and
// deletions the start-time script refuses.
func redeployFiles(node ifaces.NodeSpec, baseDir string) ([]string, []string, error) {
	injects, err := v1.InjectPairs(baseDir, node.Injections())
	if err != nil {
		return nil, nil, fmt.Errorf("node %s: %w", node.General().Hostname(), err)
	}

	if err := v1.CheckDeletions(node.Deletions()); err != nil {
		return nil, nil, fmt.Errorf("node %s: %w", node.General().Hostname(), err)
	}

	deletions := make([]string, 0, len(node.Deletions()))

	for _, d := range node.Deletions() {
		deletions = append(deletions, d.Path())
	}

	return injects, deletions, nil
}

// Kill deletes a VM with the given name in the experiment with the given name.
// It returns any errors encountered while killing the VM.
func Kill(expName, vmName string) error {
	if expName == "" {
		return errors.New("no experiment name provided")
	}

	if vmName == "" {
		return errors.New("no VM name provided")
	}

	if err := requireRunning(expName); err != nil {
		return err
	}

	err := mm.KillVM(mm.NS(expName), mm.VMName(vmName))
	if err != nil {
		return fmt.Errorf("killing VM: %w", err)
	}

	return nil
}

func Snapshots(expName, vmName string) ([]string, error) {
	snapshots, err := file.GetExperimentSnapshots(expName)
	if err != nil {
		return nil, fmt.Errorf("getting list of experiment snapshots: %w", err)
	}

	var (
		prefix = vmName + "__"
		names  []string
	)

	for _, ss := range snapshots {
		// A restore refuses a name Snapshot would not have made.
		if name, ok := strings.CutPrefix(ss, prefix); ok && mm.CheckFileName("snapshot name", name) == nil {
			names = append(names, ss)
		}
	}

	return names, nil
}

// Snapshot takes a snapshot of the current state of the VM (disk and memory)
// snapshots can later be restored. out names the snapshot; it must pass
// mm.CheckFileName, and trimSnapshotExt drops a snapshot file extension.
func Snapshot(expName, vmName, out string, cb func(string)) error { //nolint:funlen // complex logic
	// The name ends up in `shell` commands and in the `exec:` URI QEMU runs
	// through /bin/sh, so it is checked before anything is sent.
	if err := mm.CheckFileName("snapshot name", out); err != nil {
		return fmt.Errorf("snapshotting VM %s: %w", vmName, err)
	}

	vm, err := Get(expName, vmName)
	if err != nil {
		return fmt.Errorf("getting VM details: %w", err)
	}

	if !vm.Running {
		return fmt.Errorf("snapshotting VM %s: %w", vmName, mm.ErrVMNotRunning)
	}

	out = fmt.Sprintf("%s_%s__%s", expName, vmName, trimSnapshotExt(out))

	// ***** BEGIN: SNAPSHOT VM *****

	cmd := mmcli.NewNamespacedCommand(expName)
	cmd.Command = vmInfoCmd
	cmd.Columns = []string{"host", "snapshot", "disks"}
	cmd.Filters = []string{"name=" + vmName}

	status := mmcli.RunTabular(cmd)

	if len(status) == 0 {
		return fmt.Errorf("vm %s not found", vmName)
	}

	// minimega saves only the disk snapshot it made in the VM's instance
	// directory; for any other VM it saves the memory and no disk.
	switch {
	case status[0]["snapshot"] != "true":
		return fmt.Errorf("cannot snapshot VM %s: it runs with snapshot: false, so %w", vmName, ErrNoDiskSnapshot)
	case status[0]["disks"] == "":
		return fmt.Errorf("cannot snapshot VM %s: it has no disk, so %w", vmName, ErrNoDiskSnapshot)
	case len(strings.Fields(status[0]["disks"])) > 1:
		// minimega would also write <name>.hdd.1 and so on, which nothing moves,
		// removes or restores.
		return fmt.Errorf(
			"cannot snapshot VM %s: it has %d disks, and %w",
			vmName, len(strings.Fields(status[0]["disks"])), ErrMultiDiskSnapshot,
		)
	}

	cmd.Columns = nil
	cmd.Filters = nil

	var (
		host = status[0]["host"]

		// `vm save` writes both files under minimega's files directory.
		src       = mmFullPath(out)
		dst       = mmFullPath(expName + "/files")
		final     = strings.TrimPrefix(out, expName+"_")
		cmdPrefix string
	)

	if !mm.IsHeadnode(host) {
		cmdPrefix = "mesh send " + host
	}

	saved := []string{src + ".state", src + ".hdd"}

	// ***** BEGIN: SAVE VM *****

	cmd.Command = fmt.Sprintf("vm save %s %s", vmName, out)

	if err := mmcli.ErrorResponse(mmcli.Run(cmd)); err != nil {
		err = resumeAfterFailedSave(
			expName,
			vmName,
			fmt.Errorf("starting memory snapshot for VM %s: %w", vmName, err),
		)

		return removeFailedSnapshot(cmdPrefix, err, saved...)
	}

	poll := func() (migration, error) { return queryMigration(expName, vmName) }

	if err := waitForVMSave(vmName, poll, cb, time.Second, vmSaveStatusTimeout, vmSaveTimeout); err != nil {
		return removeFailedSnapshot(cmdPrefix, resumeAfterFailedSave(expName, vmName, err), saved...)
	}

	// ***** END: SAVE VM *****

	// The snapshot is complete, so a VM that cannot be resumed keeps it: the
	// failed resume is reported once the files are in place.
	var resumeErr error

	cmd.Command = "vm start " + vmName

	if err := mmcli.ErrorResponse(mmcli.Run(cmd)); err != nil {
		resumeErr = fmt.Errorf("resuming VM %s after its snapshot %s was saved: %w", vmName, final, err)
	}

	// failed adds resumeErr to err, the failure of a step after the save.
	failed := func(err error) error {
		if resumeErr != nil {
			return fmt.Errorf("%w; %w", err, resumeErr)
		}

		return err
	}

	cmd = mmcli.NewCommand()
	cmd.Command = fmt.Sprintf("%s shell mkdir -p %s", cmdPrefix, dst)

	if err := mmcli.ErrorResponse(mmcli.Run(cmd)); err != nil {
		return removeFailedSnapshot(
			cmdPrefix,
			failed(fmt.Errorf("ensuring experiment files directory exists: %w", err)),
			saved...,
		)
	}

	cmd.Command = fmt.Sprintf("%s shell mv %s.state %s/%s.state", cmdPrefix, src, dst, final)

	if err := mmcli.ErrorResponse(mmcli.Run(cmd)); err != nil {
		return removeFailedSnapshot(
			cmdPrefix,
			failed(fmt.Errorf("moving memory snapshot to experiment files directory: %w", err)),
			saved...,
		)
	}

	cmd.Command = fmt.Sprintf("%s shell mv %s.hdd %s/%s.hdd", cmdPrefix, src, dst, final)

	if err := mmcli.ErrorResponse(mmcli.Run(cmd)); err != nil {
		return removeFailedSnapshot(
			cmdPrefix,
			failed(fmt.Errorf("moving disk snapshot to experiment files directory: %w", err)),
			fmt.Sprintf("%s/%s.state", dst, final),
			src+".hdd",
		)
	}

	return resumeErr
}

// removeFailedSnapshot deletes paths, the files a failed snapshot wrote, on
// the VM's host, which cmdPrefix sends commands to, so that the failure leaves
// no partial snapshot in minimega's files directory or in the experiment's.
// It returns cause, with the error of the removal when that fails too.
func removeFailedSnapshot(cmdPrefix string, cause error, paths ...string) error {
	cmd := mmcli.NewCommand()
	cmd.Command = fmt.Sprintf("%s shell rm -f %s", cmdPrefix, strings.Join(paths, " "))

	if err := mmcli.ErrorResponse(mmcli.Run(cmd)); err != nil {
		return fmt.Errorf("%w; removing the files of the failed snapshot: %w", cause, err)
	}

	return cause
}

// resumeAfterFailedSave starts vmName again after a failed `vm save`, which
// pauses the VM before it saves its memory and leaves it paused when the save
// fails. A save QEMU still runs, such as one that timed out, is canceled
// first, since QEMU would pause the VM again when it completes. A VM that
// minimega does not report PAUSED, because the save failed before pausing it,
// is left alone. It returns cause, with the error of the resume when that
// fails too.
func resumeAfterFailedSave(expName, vmName string, cause error) error {
	// Only a running migration is canceled: QEMU reports a canceled one as
	// cancelled until the VM's next migration, and minimega's `vm save` status
	// table panics on that status (cmd/minimega/kvm.go QueryMigrate).
	if status, err := queryMigration(expName, vmName); err == nil && migrationRunning(status) {
		cmd := mmcli.NewNamespacedCommand(expName)

		// The JSON keeps its spaces: minimega rebuilds a namespaced command
		// quoting only the arguments that hold whitespace and parses it again,
		// which strips the double quotes of JSON written without any.
		cmd.Command = fmt.Sprintf(`vm qmp %s '{ "execute": "migrate_cancel" }'`, vmName)
		cmd.Timeout = saveCommandTimeout

		if err := mmcli.ErrorResponse(mmcli.Run(cmd)); err != nil {
			return fmt.Errorf("%w; canceling the memory snapshot of VM %s: %w", cause, vmName, err)
		}
	}

	cmd := mmcli.NewNamespacedCommand(expName)
	cmd.Command = vmInfoCmd
	cmd.Columns = []string{"state"}
	cmd.Filters = []string{"name=" + vmName}

	// minimega answers once per namespace host, so the VM's own host may list
	// it although another host failed.
	rows, err := mmcli.RunTabularErr(cmd)

	paused := len(rows) > 0 && rows[0]["state"] == vmStatePaused

	switch {
	case err != nil && !paused:
		return fmt.Errorf("%w; getting the state of VM %s after it: %w", cause, vmName, err)
	case !paused:
		return cause
	}

	// minimega resumes a KVM VM over QMP too.
	cmd = mmcli.NewNamespacedCommand(expName)
	cmd.Command = "vm start " + vmName
	cmd.Timeout = saveCommandTimeout

	if err := mmcli.ErrorResponse(mmcli.Run(cmd)); err != nil {
		return fmt.Errorf("%w; resuming VM %s after it: %w", cause, vmName, err)
	}

	return cause
}

// snapshotExts are the extensions trimSnapshotExt drops from a snapshot name.
var snapshotExts = []string{".state", ".hdd", ".snap", ".qc2", ".qcow2"} //nolint:gochecknoglobals // constant list

// trimSnapshotExt returns name without the extension of a snapshot file, if
// it has one. Snapshot names may hold dots, so no other extension is dropped:
// a.b.c names the snapshot a.b.c.
func trimSnapshotExt(name string) string {
	if slices.Contains(snapshotExts, filepath.Ext(name)) {
		return strings.TrimSuffix(name, filepath.Ext(name))
	}

	return name
}

// Restore restores VM vmName from snap, one of the snapshots Snapshots lists
// for it. The snapshot files could come from anyone who may add files to the
// experiment, and their name reaches the `exec:` URI QEMU runs through
// /bin/sh as root, so the name after <vm>__ must pass mm.CheckFileName, and
// the disk must pass checkSnapshotDisk.
func Restore(expName, vmName, snap string) error {
	snap = trimSnapshotExt(snap)

	if err := mm.CheckFileName("snapshot name", strings.TrimPrefix(snap, vmName+"__")); err != nil {
		return fmt.Errorf("restoring VM %s: %w", vmName, err)
	}

	if err := requireRunning(expName); err != nil {
		return err
	}

	snapshots, err := Snapshots(expName, vmName)
	if err != nil {
		return fmt.Errorf("getting list of snapshots for VM: %w", err)
	}

	var found bool

	if slices.Contains(snapshots, snap) {
		found = true
	}

	if !found {
		return errors.New("snapshot does not exist on cluster")
	}

	snap = fmt.Sprintf("%s/files/%s", expName, snap)

	details := mm.GetVMInfo(mm.NS(expName), mm.VMName(vmName))
	if len(details) == 0 {
		return errors.New("error getting vm details")
	}

	// minimega's name filter folds case, so the rows may hold another VM too.
	target := details.Named(vmName)

	if err := checkSnapshotDisk(expName, vmName, target.Host, mmFullPath(snap+".hdd")); err != nil {
		return err
	}

	cmd := mmcli.NewNamespacedCommand(expName)

	cmd.Command = "vm config clone " + vmName
	if err := mmcli.ErrorResponse(mmcli.Run(cmd)); err != nil {
		return fmt.Errorf("cloning config for VM %s: %w", vmName, err)
	}

	// Have to copy over UUID separate from clone.
	// Needs to stay the same for miniccc agent to connect
	cmd.Command = "vm config uuid " + target.UUID
	if err := mmcli.ErrorResponse(mmcli.Run(cmd)); err != nil {
		return fmt.Errorf("setting uuid for VM %s: %w", vmName, err)
	}

	cmd.Command = fmt.Sprintf("vm config state %s.state", snap)
	if err := mmcli.ErrorResponse(mmcli.Run(cmd)); err != nil {
		return fmt.Errorf("configuring state file for VM %s: %w", vmName, err)
	}

	cmd.Command = fmt.Sprintf("vm config disk %s.hdd,writeback", snap)
	if err := mmcli.ErrorResponse(mmcli.Run(cmd)); err != nil {
		return fmt.Errorf("configuring disk file for VM %s: %w", vmName, err)
	}

	// KillVM only flushes a powered-off or errored VM, which minimega refuses
	// to kill, and its flush ignores the `vm not found` of the namespace's
	// hosts that do not run the VM.
	if err := mm.KillVM(mm.NS(expName), mm.VMName(vmName)); err != nil {
		return fmt.Errorf("killing VM %s: %w", vmName, err)
	}

	cmd.Command = "vm launch kvm " + vmName
	if err := mmcli.ErrorResponse(mmcli.Run(cmd)); err != nil {
		return fmt.Errorf("relaunching VM %s: %w", vmName, err)
	}

	cmd.Command = "vm launch"
	if err := mmcli.ErrorResponse(mmcli.Run(cmd)); err != nil {
		return fmt.Errorf("scheduling VM %s: %w", vmName, err)
	}

	cmd.Command = "vm start " + vmName
	if err := mmcli.ErrorResponse(mmcli.Run(cmd)); err != nil {
		return fmt.Errorf("starting VM %s: %w", vmName, err)
	}

	return nil
}

// CommitToDisk creates a new disk with the current state of a running vm
// 1. creates a copy of the base image (e.g., the image used in the topology) named "out"
// 2. stops the vm
// 3. next rebases the snapshot (the running image phenix uses <vm>/disk-0.qcow2) to utilize the copy
// 4. commits the snapshot so that it writes the current disk to "out"
// 5. starts the vm.
//
// A given out must pass mm.CheckFileName and name a new file in phenix's
// images directory.
//
//nolint:cyclop,funlen,gocyclo,maintidx // complex logic
func CommitToDisk(expName, vmName, out string, cb func(float64)) (string, error) {
	// out comes from the request: phenix creates it, as root, under its images
	// directory, and it ends up in minimega commands.
	if out != "" {
		if err := mm.CheckFileName("commit file name", out); err != nil {
			return "", fmt.Errorf("committing VM %s: %w", vmName, err)
		}
	}

	if err := requireRunning(expName); err != nil {
		return "", err
	}

	// Determine name of new disk image, if not provided.
	if out == "" {
		var err error

		out, err = GetNewDiskName(expName, vmName)
		if err != nil {
			return "", fmt.Errorf(
				"getting new disk name for VM %s in experiment %s: %w",
				vmName,
				expName,
				err,
			)
		}
	}

	disk, err := getVMImage(expName, vmName)
	if err != nil {
		return "", fmt.Errorf(
			"getting disk image for VM %s in experiment %s: %w",
			vmName,
			expName,
			err,
		)
	}

	if !filepath.IsAbs(disk) {
		disk = common.PhenixBase + "/images/" + disk
	}

	if !filepath.IsAbs(out) {
		out = common.PhenixBase + "/images/" + out
	}

	base, err := getBackingImage(disk)
	if err != nil {
		return "", fmt.Errorf(
			"getting backing image for VM %s in experiment %s: %w",
			vmName,
			expName,
			err,
		)
	}

	snapshots, err := getImageSnapshots(disk)
	if err != nil {
		return "", fmt.Errorf(
			"getting disk snapshots for VM %s in experiment %s: %w",
			vmName,
			expName,
			err,
		)
	}

	// Get status of VM (scheduled host, VM state).

	cmd := mmcli.NewNamespacedCommand(expName)
	cmd.Command = vmInfoCmd
	cmd.Columns = []string{"host", "name", "id", "state"}
	cmd.Filters = []string{"name=" + vmName}

	status := mmcli.RunTabular(cmd)

	if len(status) == 0 {
		return "", errors.New("vm not found")
	}

	var (
		// Get current disk snapshot on the compute node (based on VM ID).
		snap = fmt.Sprintf("%s/%s/disk-0.qcow2", common.MinimegaBase, status[0]["id"])
		node = status[0]["host"]
	)

	// The copy below truncates out, which may be the base image itself or an
	// image other experiments boot from, so it must be a new file.
	reserved, err := os.OpenFile(out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644) //nolint:gosec // images are world-readable
	if errors.Is(err, fs.ErrExist) {
		return "", fmt.Errorf("committing VM %s in experiment %s to %s: %w", vmName, expName, out, ErrImageExists)
	} else if err != nil {
		return "", fmt.Errorf("creating new disk image for VM %s in experiment %s: %w", vmName, expName, err)
	}

	_ = reserved.Close()

	parent, cancel := context.WithCancel(context.Background())
	wait, ctx := errgroup.WithContext(parent)

	// Until the commit wrote the image, a failure removes the file reserved
	// above, once the copies stopped, so that the name can be used again.
	var (
		committed  bool
		reservedAt = out
	)

	defer func() {
		cancel()

		if !committed {
			_ = wait.Wait()
			_ = os.Remove(reservedAt)
		}
	}()

	// Make copy of base image locally on headnode. Using a context here will help
	// cancel the potentially long running copy of a large base image if the other
	// Goroutine below fails.

	wait.Go(func() error {
		copier := newCopier()
		s := copier.subscribe()

		go func() {
			for p := range s {
				// If the callback is set, intercept it to reflect the copy stage as the
				// initial 80% of the effort.
				if cb != nil {
					cb(p * copyProgressFactor)
				}
			}
		}()

		err := copier.copy(ctx, base, out)
		if err != nil {
			return fmt.Errorf("making copy of backing image: %w", err)
		}

		return nil
	})

	// VM can't be running or we won't be able to copy snapshot remotely.
	if status[0]["state"] != vmStateQuit {
		err := Shutdown(expName, vmName)
		if err != nil {
			return "", fmt.Errorf("stopping VM: %w", err)
		}
	}

	// Make a copy of any snapshots pointed at by the VM disk image in the
	// experiment topology so they can be rebased onto the base backing image for
	// the VM.

	for idx, snapshot := range snapshots {
		var (
			id   = idx
			file = snapshot
		)

		wait.Go(func() error {
			copier := newCopier()

			tmp := fmt.Sprintf("%s/images/%s/tmp/snapshot-%d.qc2", common.PhenixBase, expName, id)

			err := os.MkdirAll(filepath.Dir(tmp), 0o750)
			if err != nil {
				return fmt.Errorf("creating experiment tmp directory: %w", err)
			}

			err = copier.copy(ctx, file, tmp)
			if err != nil {
				_ = os.Remove(tmp)

				return fmt.Errorf("making copy of snapshot %s: %w", file, err)
			}

			// update snapshots entry to point at new copy of snapshot
			snapshots[id] = tmp

			return nil
		})
	}

	// Copy minimega snapshot disk on remote machine to a location (still on
	// remote machine) that can be seen by minimega files. Then use minimega `file
	// get` to copy it to the headnode.

	wait.Go(func() error {
		var cmdPrefix string

		if !mm.IsHeadnode(node) {
			cmdPrefix = "mesh send " + node
		}

		tmp := fmt.Sprintf("%s/images/%s/tmp", common.PhenixBase, expName)

		cmd := mmcli.NewCommand()
		cmd.Command = fmt.Sprintf("%s shell mkdir -p %s", cmdPrefix, tmp)

		err := mmcli.ErrorResponse(mmcli.Run(cmd))
		if err != nil {
			return fmt.Errorf("ensuring experiment tmp directory exists: %w", err)
		}

		tmp = fmt.Sprintf("%s/images/%s/tmp/%s.qc2", common.PhenixBase, expName, vmName)
		cmd.Command = fmt.Sprintf("%s shell cp %s %s", cmdPrefix, snap, tmp)

		err = mmcli.ErrorResponse(mmcli.Run(cmd))
		if err != nil {
			return fmt.Errorf("copying snapshot remotely: %w", err)
		}

		headnode, _ := os.Hostname()
		tmp = fmt.Sprintf("%s/tmp/%s.qc2", expName, vmName)

		err = file.CopyFile(tmp, headnode, nil)
		if err != nil {
			return fmt.Errorf("pulling snapshot to headnode: %w", err)
		}

		return nil
	})

	if err := wait.Wait(); err != nil {
		return "", fmt.Errorf("preparing images for rebase/commit: %w", err)
	}

	// Add the final boss (the minimega snapshot disk) to the end of the list of
	// snapshots to rebase onto the copy of the original backing image. If the
	// image pointed at by the VM config in the topology was not a snapshot, then
	// this will be the only snapshot in the list.
	snapshots = append(
		snapshots,
		fmt.Sprintf("%s/images/%s/tmp/%s.qc2", common.PhenixBase, expName, vmName),
	)

	for idx, snapshot := range snapshots {
		// first parent should be copy of original backing image
		parent := out

		if idx != 0 {
			parent = snapshots[idx-1]
		}

		shell := exec.CommandContext(
			context.Background(),
			"qemu-img",
			"rebase",
			"-f",
			"qcow2",
			"-b",
			parent,
			"-F",
			"qcow2",
			snapshot,
		)

		res, err := shell.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("rebasing snapshot (%s): %w", string(res), err)
		}
	}

	done := make(chan struct{})
	defer close(done)

	if cb != nil {
		stat, _ := os.Stat(out)
		targetSize := float64(stat.Size())

		stat, _ = os.Stat(snap)
		targetSize += float64(stat.Size())

		go func() {
			for {
				select {
				case <-done:
					return
				default:
					// We sleep at the beginning instead of the end to ensure the command
					// we shell out to below has time to run before we try to stat the
					// destination file.
					time.Sleep(commitWait)

					stat, err := os.Stat(out)
					if err != nil {
						continue
					}

					p := float64(stat.Size()) / targetSize

					cb(commitProgressBase + (p * commitProgressFactor))
				}
			}
		}()
	}

	shell := exec.CommandContext(context.Background(), "qemu-img", "commit", "-b", out, snapshots[len(snapshots)-1])

	res, err := shell.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("committing snapshot (%s): %w", string(res), err)
	}

	committed = true

	out, _ = filepath.Rel(common.PhenixBase+"/images/", out)

	if err := file.SyncFile(out, nil); err != nil {
		return "", fmt.Errorf("syncing new backing image across cluster: %w", err)
	}

	// restart the vm
	if err := mm.StartVM(mm.NS(expName), mm.VMName(vmName)); err != nil {
		return "", fmt.Errorf("starting VM: %w", err)
	}

	return out, nil
}

// MemorySnapshot creates an ELF memory snapshot for a running virtual machine
//
//	that is compatible with memory forensic toolkits Volatility and Google's Rekall.
func MemorySnapshot(expName, vmName, out string, cb func(string)) (string, error) { //nolint:funlen // complex logic
	// QEMU, which runs as root, writes the dump to the name, which also ends up
	// in a `shell` command, so only a plain file name is accepted.
	if out != "" {
		if err := mm.CheckFileName("memory snapshot name", out); err != nil {
			return "", fmt.Errorf("creating memory snapshot of VM %s: %w", vmName, err)
		}
	}

	if err := requireRunning(expName); err != nil {
		return "", err
	}

	_, err := Get(expName, vmName)
	if err != nil {
		return "", fmt.Errorf("getting VM details: %w", err)
	}

	if out == "" {
		out = fmt.Sprintf("%s_%s.elf", vmName, fileTimestamp(time.Now()))
	}

	// Make all output files have a .elf extension. Names may hold dots, so
	// no other extension is replaced: run.1 and run.2 are two dumps.
	if filepath.Ext(out) != ".elf" {
		out += ".elf"
	}

	// Get compute node VM is running on.

	cmd := mmcli.NewNamespacedCommand(expName)
	cmd.Command = vmInfoCmd
	cmd.Columns = []string{"host", "name", "id", "state"}
	cmd.Filters = []string{"name=" + vmName}

	status := mmcli.RunTabular(cmd)

	if len(status) == 0 {
		return "", errors.New("vm not found")
	}

	// Save in the experiment files directory so that the file appears in the
	// Web GUI's files tab.
	out = fmt.Sprintf("%s/images/%s/files/%s", common.PhenixBase, expName, out)

	// Make sure that the memory snapshot directory exists
	var cmdPrefix string
	if !mm.IsHeadnode(status[0]["host"]) {
		cmdPrefix = "mesh send " + status[0]["host"]
	}

	cmd.Columns = nil
	cmd.Filters = nil
	cmd.Command = fmt.Sprintf("%s shell mkdir -p %s", cmdPrefix, filepath.Dir(out))

	if err := mmcli.ErrorResponse(mmcli.Run(cmd)); err != nil {
		return "", fmt.Errorf("ensuring experiment files directory exists: %w", err)
	}
	// ***** BEGIN: MEMORY SNAPSHOT VM *****

	qmp := fmt.Sprintf(
		`{ "execute": "dump-guest-memory", "arguments": { "protocol": "file:%s", "paging": false, "format": "elf" , "detach": true} }`,
		out,
	)
	cmd.Command = fmt.Sprintf("vm qmp %s '%s'", vmName, qmp)

	if err := mmcli.ErrorResponse(mmcli.Run(cmd)); err != nil {
		return "", fmt.Errorf("starting memory snapshot for VM %s: ERROR: %w", vmName, err)
	}

	qmp = `{ "execute": "query-dump" }`
	cmd.Command = fmt.Sprintf("vm qmp %s '%s'", vmName, qmp)

	// fail reports a dump that did not complete and removes what QEMU wrote of
	// it, so no partial ELF is left in the experiment's files.
	fail := func(result string, err error) (string, error) {
		if cb != nil {
			cb(statusFailed)
		}

		rm := mmcli.NewCommand()
		rm.Command = fmt.Sprintf("%s shell rm -f %s", cmdPrefix, out)

		if rmErr := mmcli.ErrorResponse(mmcli.Run(rm)); rmErr != nil {
			err = fmt.Errorf("%w; removing the partial memory snapshot %s: %w", err, out, rmErr)
		}

		return result, err
	}

	for done := false; !done; {
		// sleep before querying the vm to prevent errors from start delays
		time.Sleep(1 * time.Second)

		res, err := mmcli.SingleResponse(mmcli.Run(cmd))
		if err != nil {
			return fail("", fmt.Errorf("getting memory snapshot status for VM %s: %w", vmName, err))
		}

		// A fresh value each time: an answer without a status, such as a QMP
		// error, must not leave the previous status in place.
		var v mm.BlockDumpResponse

		if err := json.Unmarshal([]byte(res), &v); err != nil {
			return fail("", fmt.Errorf("getting memory snapshot status for VM %s: QEMU answered %q", vmName, res))
		}

		switch v.Return.Status {
		case "active", statusCompleted:
			if cb != nil && v.Return.Total > 0 {
				cb(fmt.Sprintf("%v", float64(v.Return.Completed)/float64(v.Return.Total)))
			}

			if v.Return.Status == statusCompleted {
				if cb != nil {
					cb(statusCompleted)
				}

				done = true
			}
		case statusFailed:
			return fail(statusFailed, fmt.Errorf("failed to create memory snapshot for %s: %v", vmName, v))
		default:
			return fail("", fmt.Errorf("no status available for %s: QEMU answered %q", vmName, res))
		}
	}

	// Copy the ELF memory dump to the headnode if the VM was not
	// hosted on the headnode
	if !mm.IsHeadnode(status[0]["host"]) {
		// File path should be relative to the minimega files directory
		memoryDumpPath := fmt.Sprintf("%s/files/%s", expName, filepath.Base(out))

		cmd.Command = "file get " + memoryDumpPath

		err := mmcli.ErrorResponse(mmcli.Run(cmd))
		if err != nil {
			return "", fmt.Errorf("pulling ELF memory snapshot to headnode: %w", err)
		}
	}

	return out, nil
}

// CaptureSubnet starts packet captures for all the VMs that
// have an interface in the specified subnet.  The vmList argument
// is optional and defines the list of VMs to search.
func CaptureSubnet(expName, subnet string, vmList []string) ([]mm.Capture, error) {
	// Make sure the experiment is running
	exp, err := experiment.Get(expName)
	if err != nil {
		return nil, fmt.Errorf("getting experiment %s: %w", expName, err)
	}

	if !exp.Running() || exp.DryRun() {
		return nil, errors.New("packet captures can only be started for a running experiment")
	}

	vms, err := List(expName)
	if err != nil {
		return nil, fmt.Errorf("getting VM list for %s: %w", expName, err)
	}

	_, refNet, err := net.ParseCIDR(subnet)
	if err != nil {
		return nil, fmt.Errorf("unable to parse %s", subnet)
	}

	// Use empty struct for code consistency and
	// slight memory savings
	var (
		vmTable    map[string]struct{}
		matchedVMs []string
		failures   []error

		// The files of the experiment's files directory, listed before the
		// first capture starts: minimega truncates an existing capture file.
		taken map[string]bool
	)

	// An optional list of VMs can be provided
	// to restrict the search scope

	if len(vmList) > 0 {
		// Put vms in a table for quick lookup
		vmTable = make(map[string]struct{})

		for _, vmName := range vmList {
			if _, ok := vmTable[vmName]; !ok {
				vmTable[vmName] = struct{}{}
			}
		}
	}

	// Find the interfaces that are in the
	// specified subnet
	for _, vm := range vms {
		// Make sure the VM is running
		state, err := mm.GetVMState(mm.NS(expName), mm.VMName(vm.Name))
		if err != nil {
			continue
		}

		if state != vmStateRunning {
			continue
		}

		// Skip vms not in the list
		if vmTable != nil {
			if _, ok := vmTable[vm.Name]; !ok {
				continue
			}
		}

		for iface, network := range vm.IPv4 {
			address := net.ParseIP(network)

			if address == nil {
				continue
			}

			if refNet.Contains(address) {
				if taken == nil {
					if taken, err = experimentFileNames(expName); err != nil {
						return nil, err
					}
				}

				filename := subnetCaptureName(taken, vm.Name, iface, fileTimestamp(time.Now()))
				taken[filename] = true

				// A disconnected interface keeps the address it had.
				switch err := StartCapture(expName, vm.Name, iface, filename); {
				case err == nil:
					matchedVMs = append(matchedVMs, vm.Name)
				case !errors.Is(err, errDisconnectedInterface):
					plog.Warn(plog.TypeSystem, "skipping subnet capture", "exp", expName, "vm", vm.Name, "iface", iface, "err", err)

					failures = append(failures, fmt.Errorf("VM %s interface %d: %w", vm.Name, iface, err))
				}
			}
		}
	}

	// Each failure was logged; only one that left nothing captured fails.
	if len(matchedVMs) == 0 && len(failures) > 0 {
		return nil, fmt.Errorf("starting captures in %s: %w", subnet, errors.Join(failures...))
	}

	// Get all the captures for all the VMs
	var allVMCaptures []mm.Capture

	for _, vmName := range matchedVMs {
		vmCaptures := mm.GetVMCaptures(mm.NS(expName), mm.VMName(vmName))

		allVMCaptures = append(allVMCaptures, vmCaptures...)
	}

	return allVMCaptures, nil
}

// experimentFileNames returns the paths, relative to the experiment's files
// directory, of the files the cluster's hosts hold there. It fails when a
// host's listing fails, since a name it missed would be truncated.
func experimentFileNames(expName string) (map[string]bool, error) {
	paths, err := file.GetExperimentFilePaths(expName)
	if err != nil {
		return nil, fmt.Errorf("listing the files of experiment %s: %w", expName, err)
	}

	names := make(map[string]bool, len(paths))

	for _, p := range paths {
		names[p] = true
	}

	return names, nil
}

// subnetCaptureName returns the file name of a subnet capture of interface
// iface of vmName, stamped with stamp. A name in taken gets the first numeric
// suffix that is not, since minimega truncates an existing capture file: a
// capture of the interface stopped and started again within the second of the
// stamp would otherwise lose the earlier capture's packets.
func subnetCaptureName(taken map[string]bool, vmName string, iface int, stamp string) string {
	base := fmt.Sprintf("%s_%d_%s", vmName, iface, stamp)
	name := base + ".pcap"

	for n := 1; taken[name]; n++ {
		name = fmt.Sprintf("%s_%d.pcap", base, n)
	}

	return name
}

// StopCaptureSubnet will stop all captures for any VM
// that has an interface in the specified subnet. Unfortunately
// due to a limitation in the minimega capture cli, a capture for
// just the interface that is found in the specified subnet can not
// be stopped.  The subnet argument is optional.  If the subnet
// argument is not specified, then all captures for all VMs will be stopped.
func StopCaptureSubnet(expName, subnet string, vmList []string) ([]string, error) {
	// Make sure the experiment is running
	exp, err := experiment.Get(expName)
	if err != nil {
		return nil, fmt.Errorf("getting experiment %s: %w", expName, err)
	}

	if !exp.Running() || exp.DryRun() {
		return nil, errors.New("packet captures can only be stopped for a running experiment")
	}

	vms, err := List(expName)
	if err != nil {
		return nil, fmt.Errorf("getting VM list for %s: %w", expName, err)
	}

	_, refNet, err := net.ParseCIDR(subnet)
	if err != nil {
		refNet = nil
	}

	// Use empty struct for code consistency and
	// slight memory savings
	var (
		vmTable    map[string]struct{}
		matchedVMs []string
	)

	// An optional list of VMs can be provided
	// to restrict the search scope

	if len(vmList) > 0 {
		// Put vms in a table for quick lookup
		vmTable = make(map[string]struct{})

		for _, vmName := range vmList {
			if _, ok := vmTable[vmName]; !ok {
				vmTable[vmName] = struct{}{}
			}
		}
	}

	// Find the interfaces that are in the
	// specified subnet
	for _, vm := range vms {
		// Skip vms with no current captures
		if len(vm.Captures) == 0 {
			continue
		}

		// Make sure the VM is running
		state, err := mm.GetVMState(mm.NS(expName), mm.VMName(vm.Name))
		if err != nil {
			continue
		}

		if state != vmStateRunning {
			continue
		}

		// Skip vms not in the list
		if vmTable != nil {
			if _, ok := vmTable[vm.Name]; !ok {
				continue
			}
		}

		// if no subnet was specified, then stop
		// all the captures for this VM
		if len(subnet) == 0 {
			if StopCaptures(expName, vm.Name) == nil {
				matchedVMs = append(matchedVMs, vm.Name)
			}

			continue
		}

		if refNet == nil {
			continue
		}

		for _, network := range vm.IPv4 {
			address := net.ParseIP(network)

			if address == nil {
				continue
			}

			if refNet.Contains(address) {
				if StopCaptures(expName, vm.Name) == nil {
					matchedVMs = append(matchedVMs, vm.Name)

					// Avoid trying to stop captures for
					// the same vm since all the captures
					// for a VM should be stopped
					break
				}
			}
		}
	}

	return matchedVMs, nil
}

// ChangeOpticalDisc changes the optical disc in the first drive to isoPath,
// which must be a disk of a node of the experiment's topology or a file
// directly in minimega's files directory; a relative path is in the files
// directory.
func ChangeOpticalDisc(expName, vmName, isoPath string) error {
	if expName == "" {
		return errors.New("no experiment name provided")
	}

	if vmName == "" {
		return errors.New("no VM name provided")
	}

	if err := requireRunning(expName); err != nil {
		return err
	}

	if isoPath == "" {
		return errors.New("no optical disc path provided")
	}

	exp, err := experiment.Get(expName)
	if err != nil {
		return fmt.Errorf("getting experiment %s: %w", expName, err)
	}

	// QEMU, which runs as root, opens the file, and the path ends up in a
	// namespaced command minimega parses twice.
	if err := checkDiskImage(exp.Spec.Topology(), "optical disc", vmName, isoPath); err != nil {
		return err
	}

	// minimega looks a relative path up in its working directory, not in its
	// files directory, where the check above looked.
	cmd := mmcli.NewNamespacedCommand(expName)
	cmd.Command = fmt.Sprintf("vm cdrom change %s %s", vmName, filepath.Clean(mmFullPath(isoPath)))

	err = mmcli.ErrorResponse(mmcli.Run(cmd))
	if err != nil {
		return fmt.Errorf("changing optical disc for VM %s: %w", vmName, err)
	}

	return nil
}

// EjectOpticalDisc ejects the optical disc in the first drive.
func EjectOpticalDisc(expName, vmName string) error {
	if expName == "" {
		return errors.New("no experiment name provided")
	}

	if vmName == "" {
		return errors.New("no VM name provided")
	}

	if err := requireRunning(expName); err != nil {
		return err
	}

	cmd := mmcli.NewNamespacedCommand(expName)
	cmd.Command = "vm cdrom eject " + vmName

	err := mmcli.ErrorResponse(mmcli.Run(cmd))
	if err != nil {
		return fmt.Errorf("ejecting optical disc for VM %s: %w", vmName, err)
	}

	return nil
}
