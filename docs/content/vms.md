# VMs

## VM Info

### From the Web-UI

The experiment must be started; click on the experiment name to enter the
Running Experiment component. Within that component, click on the VM name and
you will be presented with a VM information modal.

![screenshot](images/vms_vm_info.png)

Available commands

* `restore` a snapshot by clicking the play button next to the desired snapshot name.

Buttons from left to right:

* `pause` a running VM
* Create a `memory snapshot` of a running VM
* Create a `backing image` of a running VM
* Create a `snapshot` of a running VM
* `record screenshot` of a running VM
* `modify state` opens another toolbar of buttons

#### Modify State Toolbar

![screenshot](images/vms_modify_state_tlbar.png)

Buttons from left to right:

* `redeploy` a running VM
* `reset disk state` of a running VM
* `restart` a running VM
* `shutdown` a running VM
* `kill` a running VM
* `close` modify state toolbar

### From the Command Line Binary

There are two options for displaying the information for VMs in an experiment. First run
the following command to see information for all VMs in a given experiment.

```shell
phenix vm info <experiment name>
```

Or, run the following to see the information for a specific VM in an experiment.

```shell
phenix vm info <experiment name> [vm name] [--label <label>]
```

## Create a Backing Image

### From the Web-UI

Click on the name of a running VM in a started experiment to access the VM information
modal. Click the `create backing image` button as shown in the screenshot below.

![screenshot](images/vms_backing_image.png)

### From the Command Line Binary

There is currently no CLI subcommand to create backing images from running VMs. This operation is available through the Web-UI and the REST API (`POST /api/v1/experiments/{exp}/vms/{name}/commit`).

The new image is written to phēnix's images directory under the given file
name, which may hold only letters, digits, `.`, `_` and `-`, must start with a
letter or digit, may not contain `..`, and is at most 128 characters long.
phēnix refuses any other name, including a path, and a name of an existing
file, with a 400.

## Create a Memory Snapshot

### From the Web-UI

Click on the name of a running VM in a started experiment to access the VM information
modal. Click the `memory snapshot` button as shown in the screenshot below.

![screenshot](images/vms_memory_snapshot.png)

### From the Command Line Binary

To create an ELF memory dump, run the following command.

```shell
phenix vm memory-snapshot <experiment name> <vm name> <snapshot file name>
```

The dump is written to the experiment's files directory as
`<snapshot file name>.elf`, or as the name itself when it ends in `.elf`; the
other dots of the name are kept, so `run.1` is written as `run.1.elf`. The name may hold only letters, digits, `.`, `_`
and `-`, must start with a letter or digit, may not contain `..`, and is at
most 128 characters long; phēnix refuses any other name, including a path.

## Create a VM Snapshot

A VM snapshot saves the memory and the disk of a running VM as
`<vm>__<name>.state` and `<vm>__<name>.hdd`, without a `.state`, `.hdd`,
`.snap`, `.qc2` or `.qcow2` extension of the name, in the experiment's files
directory, `<experiment>/files/` under minimega's files directory. minimega
pauses the VM while it saves it, and phēnix resumes the VM afterwards, also
when the save fails. phēnix reads the progress of the save from the VM's own
QEMU, and gives up on a save that has not completed after 30 minutes, such as
one of a guest that changes its memory faster than QEMU copies it: it cancels
the save, resumes the VM and removes the files. Each command of the save that
QEMU answers, the polls of its progress and the cancel and resume of a failed
save, fails after one minute without an answer. A snapshot whose VM cannot be
resumed is kept, and the error names it. Only a VM that has a disk and runs with
`general.snapshot: true` can be snapshotted, since minimega saves only the
disk snapshot it made for the VM; phēnix refuses any other VM with a 400 that
names it. A VM with more than one disk is refused the same way, since a
restore gives the VM only the first disk of a snapshot. The snapshot name may hold only letters, digits, `.`, `_` and `-`,
must start with a letter or digit, may not contain `..`, and is at most 128
characters long; phēnix refuses any other name with a 400, since minimega and
QEMU pass it through a shell.
The same rule applies to the `<vm>__<name>` pairs found in the experiment's
files: phēnix does not list a snapshot whose name breaks it, and refuses to
restore it with a 400.

Before a restore, phēnix inspects the `.hdd` disk with `qemu-img` on the VM's
host and refuses it with a 400 unless it is a qcow2 image without a data file
whose backing chain runs through snapshot disks of the experiment and the VM's
disk snapshot to an image a VM may be given (see
[Redeploy a VM](#redeploy-a-vm)): QEMU, running as root, opens the whole chain.
phēnix cannot check the `.state` memory of a snapshot, which QEMU loads
without guarding against a hostile one, so after upgrading from a release
whose file server stored uploaded `.hdd` and `.state` files, remove the
`<vm>__<name>.hdd` and `.state` files in `<experiment>/files/` that phēnix
did not write.

### From the Web-UI

Click on a running VM in a started experiment to access the VM information
modal. Click the `vm snapshot` button as shown in the screenshot below.

![screenshot](images/vms_snapshot.png)

### From the Command Line Binary

While ELF memory dumps can be created via `phenix vm memory-snapshot`, there is currently no CLI subcommand to take standard VM disk snapshots. VM disk snapshots are managed through the Web-UI and the REST API (`POST /api/v1/experiments/{exp}/vms/{name}/snapshots`).

## VM VNC Access

### From the Web-UI

The experiment must be started; click on the VM screenshot to open a new browser
tab that provides VNC access to the VM.

### From the Command Line Binary

Not applicable.

## Mount a VM

The VM Mount feature lets you transfer files to and from a running VM directly
through the phēnix Web-UI. It is an optional feature that must be enabled when
starting the phēnix UI with the `vm-mount` feature flag (see
[Enabling VM Mount](#enabling-vm-mount) below).

!!! note

    Mounting a VM requires the minimega command-and-control agent (`miniccc`)
    to be installed and actively running in the VM. If `cc` is not active for a
    VM, the mount button is disabled.

### Enabling VM Mount

VM Mount is disabled by default. Enable it by passing the `vm-mount` feature to
the `--features` flag when starting the UI:

```shell
phenix ui --features vm-mount
```

The feature can also be enabled via the `ui.features` configuration key or the
`PHENIX_UI_FEATURES` environment variable. See
[Settings & Configuration](settings.md#settings-reference) for details.

### From the Web-UI

The experiment must be started. Click on the name of a running VM to access the
VM information modal, then click the `mount vm` button (the hard-drive icon).

![screenshot](images/vms_mount_vm.png)

The mount browser modal presents two panes:

* **Local file system** — browse to a file on your local machine and upload it
  into the VM, or download a file from the VM to your local machine.
* **Experiment files** — copy files that already exist on the phēnix server
  into the VM. This option is only available when the
  [experiment file server](settings.md#settings-reference) is enabled (see
  below).

### Uploading Experiment Files from the phēnix Server { #uploading-experiment-files-from-the-phenix-server }

phēnix can optionally run a lightweight file server on a port separate from the
main UI. This was added to support environments where the phēnix UI is only
reachable on a private network but files (for example, application installers or
configuration files) need to be uploaded to an experiment from a public network.

When the file server is enabled, the VM Mount modal gains the **Experiment
files** option described above, allowing files that have been uploaded to an
experiment to be copied directly into a VM without going through the user's
local machine.

Enable the file server by passing the `--file-server-endpoint` flag when
starting the UI:

```shell
# Bind to 127.0.0.1 on port 8080 (port-only value binds to localhost)
phenix ui --features vm-mount --file-server-endpoint 8080

# Bind to a specific interface and port
phenix ui --features vm-mount --file-server-endpoint 0.0.0.0:8080
```

The file server presents a simple interface for selecting an experiment and
uploading a file to it. When authentication/authorization is enabled for the
main phēnix UI, it is automatically enforced on the file server as well, so
users must already have valid phēnix credentials to upload files. File upload
permissions are included in the default **Experiment Admin** and **Experiment
User** roles.

The file server refuses files ending in `.hdd` or `.state` with a 400 and
stores none of the files of such an upload: those are the disk and memory of
VM snapshots, which a restore gives QEMU, running as root, and only phēnix
writes them.

See [Settings & Configuration](settings.md#settings-reference) for the
`ui.file-server-endpoint` configuration key.

### From the Command Line Binary

Use `phenix vm mount` to mount a running VM's filesystem to a directory on the
phēnix headnode. Unlike the Web-UI mount browser above, this does not require
the `vm-mount` UI feature flag to be enabled, and the mounted filesystem is
accessed directly on the headnode's filesystem rather than through the
browser.

```shell
phenix vm mount <experiment name> <vm name> [host path]
```

If `host path` is omitted, the VM is mounted to a default path of
`<mount-dir>/<experiment name>/<vm name>`, where `<mount-dir>` defaults to
`<base-dir.phenix>/mounts` (see [`mount-dir`](settings.md#settings-reference)).
The command prints the resolved host path once the mount succeeds. A VM mounted
at its default path can also be browsed through the file routes of a phēnix UI
running with the `vm-mount` feature.

```shell
phenix vm mount my-exp my-vm
# VM my-vm in experiment my-exp mounted at: /phenix/mounts/my-exp/my-vm

phenix vm mount my-exp my-vm /tmp/my-vm-files
# VM my-vm in experiment my-exp mounted at: /tmp/my-vm-files
```

To unmount the VM's filesystem, run:

```shell
phenix vm unmount <experiment name> <vm name>
```

!!! note

    Mounting a VM requires the minimega command-and-control agent (`miniccc`)
    to be installed and actively running in the VM; the command will fail if
    the agent is not reachable.

    Mounts are automatically unmounted, and their mount directories removed,
    when the owning experiment is stopped or deleted, so mounts do not
    outlive their experiment.

## Packet Capture

### From the Web-UI

Click on the name of the network tap on a running VM in a started experiment to
start a packet capture. The name of the network tap will turn green once a packet
capture has started. It is possible to start captures on multiple network taps.

To stop a capture, click the name of a green network tap. If the VM has more
than one capture running, phēnix asks whether to stop only that interface's
capture or all packet captures for the VM.

### From the Command Line Binary

To start a packet capture, run the following command, specifying the target
network interface by its name (as declared in the experiment topology, e.g.
`IF0`) or its zero-based index.

```shell
phenix vm capture start <experiment name> <vm name> <iface name/index> <out file>
```

The capture is written to the experiment's files directory under the base name
of `<out file>`, with a `.pcap` extension. That name, extension included, may
hold only letters, digits, `.`, `_` and `-`, must start with a letter or digit,
may not contain `..`, and is at most 128 characters long.

To stop all packet captures on a running VM, use the following command.

```shell
phenix vm capture stop <experiment name> <vm name>
```

To stop the packet capture running on a single interface, without affecting
any other captures running on the same VM, provide the interface name or
index.

```shell
phenix vm capture stop <experiment name> <vm name> <iface name/index>
```

!!! note
    Stopping the capture on a single interface, from the Web-UI or the command
    line, requires minimega 3.3.0 or later.

## Change an Optical Disc

From the Web-UI, the optical disc button of a running VM's modal inserts an
ISO image into the VM's optical drive or ejects it. Through REST,
`POST /experiments/{exp}/vms/{name}/cdrom?isoPath=<path>` inserts a disc and
`DELETE` on the same route ejects it.

The disc must be an image a redeploy disk may name (see
[Redeploy a VM](#redeploy-a-vm)): a disk of a node in the experiment's
topology, or an image directly in minimega's files directory that is not a
disk snapshot. A relative path is in minimega's files directory. phēnix refuses
any other path with a 400, since QEMU, running as root, attaches whatever file
it names.

## Kill a VM

### From the Web-UI

Click on the name of a running VM in a started experiment to access the VM information
modal. Click the `modify state` button on the far right to open the modify state toolbar.
Click the `kill` button as shown in the screenshot below.

![screenshot](images/vms_kill.png)

_Note_: if you stop and then start the experiment again, that VM will run again
per the experiment configuration.

### From the Command Line Binary

To kill a VM, run the following command.

```shell
phenix vm kill <experiment name> [vm name] [--label <label>]
```

## Modify the Network Connectivity

### From the Web-UI

Click on the network for the desired VM in the Running Component to modify the
settings. Select from a pull down what network you want to switch the VM interface
you clicked on to. To revert back to previous setting, simply repeat selecting the
network interface you wish to change, and select the previous network setting.

### From the Command Line Binary

To connect a VM network interface to a different network, run the following command.

```shell
phenix vm net connect <experiment name> <vm name> <iface index> <vlan id>
```

To disconnect a VM network interface, run the following command.

```shell
phenix vm net disconnect <experiment name> <vm name> <iface index>
```

## Pause a VM

### From the Web-UI

Click on the name of a running VM in a started experiment to access the VM information
modal. To pause a VM, click on the `pause` button as shown in the screenshot below.
To start a paused VM, that same button will become a green play button; simply
click it to start.

![screenshot](images/vms_pause.png)

### From the Command Line Binary

To pause a VM, run the following command.

```shell
phenix vm pause <experiment name> [vm name] [--label <label>]
```

To resume a paused VM, run the following command.

```shell
phenix vm resume <experiment name> [vm name] [--label <label>]
```

## Redeploy a VM

### From the Web-UI

Click on the name of a running VM in a started experiment to access the VM information
modal. Click the `modify state` button on the far right to open the modify state toolbar.
Click the `redeploy` button as shown in the screenshot below.

![screenshot](images/vms_redeploy.png)

### From the Command Line Binary

To redploy a VM, run the following command.

```shell
phenix vm redeploy <experiment name> [vm name] [--label <label>]
```

A new disk, given with `-d` or as `disk` in the REST body, is a minimega
diskspec, `<image>[,<interface>][,<cache mode>]`. The image must be a disk of
a node in the experiment's topology, or be directly in minimega's files
directory, not in a subdirectory, and not be a disk snapshot phēnix made at
start (`<headnode>_<experiment>_<vm>_snapshot`). It may hold only letters,
digits, `.`, `_`, `+`, `-` and `/`, without `..`. phēnix refuses any other
disk (with a 400 through REST), since QEMU, running as root, would attach
whatever file it names to the guest: the subdirectories hold each
experiment's snapshots and the files users upload, and a qcow2 file names its
own backing file.

A VM that runs with `snapshot: false` keeps that flag when it is redeployed,
and QEMU then writes to the new disk itself, so its image must be a disk the
topology gave a node; an image that is only in minimega's files directory is
refused.

## Reset Disk State

### From the Web-UI

Click on the name of a running VM in a started experiment to access the VM information
modal. Click the `modify state` button on the far right to open the modify state toolbar.
Click the `reset disk state` button as shown in the screenshot below.

![screenshot](images/vms_reset_disk_state.png)

### From the Command Line Binary

To reset the first disk to the initial pre-boot state, run the following command.

```shell
phenix vm reset-disk <experiment name> [vm name] [--label <label>]
```

## Restart a VM

### From the Web-UI

Click on the name of a running VM in a started experiment to access the VM information
modal. Click the `modify state` button on the far right to open the modify state toolbar.
Click the `restart` button as shown in the screenshot below.

![screenshot](images/vms_restart.png)

### From the Command Line Binary

To restart a VM, run the following command.

```shell
phenix vm restart <experiment name> [vm name] [--label <label>]
```

## Resume a VM

### From the Web-UI

Click on the name of the paused VM in a started experiment to access the VM information
modal. Click the green play button (previously the pause button, furthest button to the
left).

### From the Command Line Binary

To resume a paused VM, run the following command.

```shell
phenix vm resume <experiment name> [vm name] [--label <label>]
```

## Shutdown a VM

### From the Web-UI

Click on the name of a running VM in a started experiment to access the VM information
modal. Click the `modify state` button on the far right to open the modify state toolbar.
Click the `shutdown` button as shown in the screenshot below.

![screenshot](images/vms_shutdown.png)

### From the Command Line Binary

To shutdown a VM, run the following command.

```shell
phenix vm shutdown <experiment name> [vm name] [--label <label>]
```

## Modify VM Settings

### From the Web-UI

There are two ways to modify VM settings:

1. Click on a stopped experiment to access the Stopped Component. You are able to edit the
following:
    * Host name
    * CPUs
    * Memory
    * Disk
    * Do not boot flag
1. From a running experiment, click on the VM name and then the redeploy button (yellow
power button, second from the right on the modal footer). You are able to edit the following:
    * CPU
    * Memory
    * Disk
    * Replication of original injections

The disk lists of both, and of the optical disc of a running VM, offer only the
images a VM of the experiment may be given: the disks of its topology and the
images directly in minimega's files directory that are not disk snapshots, not
the backing images of those, which may be in subdirectories (see
[Redeploy a VM](#redeploy-a-vm)). A VM with `snapshot: false` may still be
refused an image of the list that is not a disk of the topology.

### From the Command Line Binary

To modify VM settings, run the following command, specifying one or more of the
configuration flags described below.

```shell
phenix vm set <experiment name> [<vm name>] [flags]
```

Only the flags that are explicitly provided will be applied; any setting whose
flag is omitted is left unchanged. The following flags are supported:

| Flag                  | Description                                                                                                       |
| --------------------- | ----------------------------------------------------------------------------------------------------------------- |
| `-c, --cpu`           | Number of VM CPUs (1-8 is valid).                                                                                 |
| `-m, --mem`           | Amount of memory in megabytes (512, 1024, 2048, 3072, 4096, 8192, 12288, 16384 are valid).                        |
| `-d, --disk`          | VM backing disk image: a disk of the topology, or an image directly in minimega's files directory (see the redeploy disk rules). |
| `-p, --partition`     | Partition of disk to inject files into.                                                                           |
| `--do-not-boot`       | Set the do-not-boot flag for the VM.                                                                              |
| `--snapshot`          | Set the snapshot (non-persistent) flag for the VM.                                                                |
| `-L, --label-changes key=value` | VM label to set, in `key=value` form. May be repeated to set multiple labels.                                         |
| `--append-label`       | Append the provided labels to the VM's existing labels instead of replacing them.                                     |
| `-l, --label`         | Apply the change to every VM whose label matches the provided label (supports glob patterns). Use `all` to select every VM in the experiment. May be repeated. |

A VM with `snapshot: false` boots its disk writable, so whichever change
leaves a VM with `snapshot: false` must leave it on a disk the topology gave a
node, not on an image that only a disk change picked. phēnix records the
topology's disk of a VM whose disk a change replaced in the node annotation
`phenix/topology-disk`.

For example, to set the CPU count to 4 and memory to 2048 MB for a VM named
`my-vm` in an experiment named `my-exp`:

```shell
phenix vm set my-exp my-vm --cpu 4 --mem 2048
```

To set the `role=server` labels on VMs whose existing labels matches
`tier=web*` in the same experiment, without clearing the existing labels:

```shell
phenix vm set my-exp --label "tier=web*" -L role=server --append-labels
```

To set two labels on the VM `my-vm` and replace the existing labels on the VM:

```shell
phenix vm set my-exp my-vm -L val1=true -L val2=roger
```

!!! note
    Only label updates and interface VLAN connections can be modified while an
    experiment is running. To change CPU, memory, disk, partition, do-not-boot,
    or snapshot settings, the experiment must be stopped. Use
    [`phenix vm net`](#modify-the-network-connectivity) to modify interface
    VLAN connections on a running experiment.

!!! note
    minimega cannot store some labels as tags of a running VM, so phēnix
    refuses a label of a running VM whose key or value is empty, holds a
    non-printable character other than a tab or line break, or holds a double
    quote, single quote, backslash or `#` without also holding whitespace, such
    as `a"b` or `it's`. The error names the VM and the label, and
    `PATCH /api/v1/experiments/{exp}/vms/{name}` answers it with 400. Such
    labels can still be set while the experiment is stopped. With
    `--append-labels` (or the `ADD` update mode) the VM's existing labels are
    sent to minimega again and checked too, so while the experiment runs an
    existing label of that kind blocks adding labels; replace the labels
    without it instead.

## Applying Actions to Multiple VMs

!!! note
    See [VM Multi Action](vm-multi-action.md) for documentation on applying
    actions to multiple VMs at once.
