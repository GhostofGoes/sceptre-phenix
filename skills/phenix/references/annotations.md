# Node Annotations

`annotations` is a free-form `map[string]any` on a Topology node
(`spec.nodes[].annotations`), used to pass out-of-band hints to apps without
adding first-class schema fields. Apps read them via `node.GetAnnotation("key")`
and interpret the value however they define it. The [skill body](../SKILL.md)
covers the Topology and Scenario shapes these annotations attach to.

Annotations used by phenix's own default apps:

## `phenix/default-apps` (all default apps)

`phenix/default-apps: false` skips the `ntp`, `serial`, `startup`, and
`vrouter` default apps for that specific node on every lifecycle stage, while
still letting phenix/minimega manage the VM normally. User apps from the
scenario are unaffected. Omit the annotation (or set it `true`) to keep the
default behavior.

The value may be a bool, a string Go's `strconv.ParseBool` accepts in any case
(`"false"`, `"0"`, `"f"`, `"true"`, `"1"`, ...), or a number (`0` is false).
Any other value, such as `"no"` or an empty value, makes experiment create,
reconfigure and start fail with `node <hostname>: annotation
phenix/default-apps is "no", want true or false`. Unlike
`phenix/startup-via-cc` below, an unrecognized string is an error, not true.

## `phenix/startup-autotunnel` (`startup` app, `post-start` stage)

A list of strings, each describing a port forward to auto-create for the node
once it boots, e.g. `["8080", "8080:9090", "8080:10.0.0.5:9090"]`. Each entry
is `sport[:dhost]:dport`:

| Form | Forwards to |
|---|---|
| `sport` | `127.0.0.1:sport` |
| `sport:dport` | `127.0.0.1:dport` |
| `sport:dhost:dport` | an arbitrary destination host/port |

Malformed entries are logged and skipped.

The forwards are created by `phenix ui`, owned by the user `bot`, and only for
experiments `phenix ui` starts itself (from the web UI or the REST API). Each
non-VNC forward waits up to five minutes for the node's C2 client to register
under the node's hostname, and is logged and skipped if it does not; a stop of
the experiment ends the wait. Dry runs get no autotunnels. An
experiment started any other way, such as with `phenix experiment start`, gets
no autotunnels: the startup app logs the warning `startup autotunnel not
created: autotunnels are created only for experiments started through phenix
ui`, naming the experiment, the VM and the entry. Create those forwards from
the UI or with `POST /api/v1/experiments/{exp}/vms/{name}/forwards` instead.

## `phenix/startup-via-cc` (`startup` app, `pre-start` stage)

When truthy, the startup app drops its disk injections for that node and
instead delivers and runs the generated startup scripts over minimega's C2
(miniccc) channel. The value may be a bool, a number (`0` is false), or a
string (`false`/`0`, case-insensitively, are false; any other string is true);
an absent annotation means false.

Each script is queued as `cc send <exp>/<file>` plus
`cc exec bash /tmp/miniccc/files/<exp>/<file>` (Windows:
`cc exec cmd /c 'powershell.exe ... -file ...'`), so minimega gives it to the
guest's miniccc client every time it connects: at every boot and on every
reconnect. A staged Linux script ends early when it already ran in the current
boot, even if that run failed (it records the boot ID under
`/run/phenix/startup-c2` before it runs); the Windows script
keeps a `done` flag in the registry. phenix does not use `exec-once`, which
minimega marks sent when it is queued, before the guest connects; the next
start also drops the `exec-once` commands older versions stored.

C2 delivery is also used automatically when the node's first drive sets
`inject_partition: 0`, and is skipped entirely for `do_not_boot` nodes.

## `phenix/do-not-boot-reason` (`startup` app, `pre-start` stage)

Set by phenix, not by users. When the startup app finds no file at a node's
first disk image (a relative path is looked up in minimega's files directory,
its `-filepath`), it sets the node's `do_not_boot` and warns
`node "<hostname>" will not boot: disk image "<path>" not found`. Unless the
node already had `do_not_boot` set, it also records the reason here, as
`missing disk image`, and a later start that finds the image boots the node
again and removes the annotation. A `do_not_boot` without the annotation is
the user's and is kept; `phenix vm set --do-not-boot` makes the flag the user's
and removes the annotation. The `delay.c2` checks accept a wait on a node that
is `do_not_boot` only for this reason at experiment create, and name the
reason when they refuse it at start.

## `vrouter/vyos-password` (`vrouter` app)

Overrides the default `vyos` login password used when templating the boot
config for a VyOS router node.

## `vrouter/enable-ssh` (`vrouter` app)

An interface name or IP address. When set, SSH access is enabled on the router,
templated to listen on that interface's address (or the literal IP given).

## Example

```yaml
spec:
  nodes:
    - type: VirtualMachine
      general:
        hostname: web-01
      annotations:
        phenix/startup-via-cc: true
        phenix/startup-autotunnel: ["8080:80"]
      # ...
    - type: Router
      general:
        hostname: rtr-01
      annotations:
        vrouter/enable-ssh: IF0
      # ...
```
