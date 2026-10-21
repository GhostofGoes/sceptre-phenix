# phēnix end-to-end tests

These tests run a real `phenix` binary against a real minimega daemon that
runs real QEMU/KVM guests. They cover what unit tests cannot: the minimega
commands phenix renders and sends, the minimega output phenix parses, what the
apps phenix runs change on the host and in guests, and whether the images built
from [sceptre-phenix-images](https://github.com/sandialabs/sceptre-phenix-images)
boot and behave under phenix.

The `E2E` workflow (`.github/workflows/e2e.yml`) runs them on GitHub-hosted
`ubuntu-24.04` runners, which expose `/dev/kvm`.

## How the workflow runs them

1. **Plan** picks the shards from `.github/e2e/shards.json` and resolves the
   phenix ref, the apps ref when a shard installs the apps, and the images
   ref when the run has image jobs.
2. **Image** jobs build each image a shard needs with the images repository's
   own `Makefile` targets, using this commit's `phenix image` natively on the
   runner. Images are cached across runs, keyed by a hash of the recipes, the
   phenix image builder and the pinned minimega and vmdb2, and handed to the
   test jobs as artifacts.
3. **Test** jobs run one shard each, on their own runner, with minimega built
   from the commit in `.github/e2e/minimega.ref` (see
   `.github/actions/minimega`).

Every image job also boots its image in a phenix experiment and checks it from
inside the guest (`TestImagesSmoke`, with each recipe's profile in
`images_test.go`).

Other repositories call the workflow with `workflow_call`. The apps repository
passes `apps-ref`, and the images repository passes `images-ref` and the recipes
it changed as `images`, so only those recipes are built and smoke-tested.

## Running locally

You need root, Open vSwitch, QEMU with KVM, and a minimega daemon listening on
`/tmp/minimega/minimega` with `-filepath /phenix/images`, as
`.github/actions/minimega` sets up. The tests also need:

- the `phenix` binary under test at `/usr/local/bin/phenix` (`E2E_PHENIX`);
- the host tools the tests run other than coreutils, bash and `hostname`,
  which `.github/actions/minimega` installs or the runner has: `qemu-utils`
  (`qemu-img`, `qemu-io`, `qemu-nbd`), `ntfs-3g` (with `mkfs.ntfs`),
  `openssh-client` (`ssh-keygen`), `iproute2` (`ip`, `tc`), `iptables`,
  `iputils-ping` (`ping`), `procps` (`pgrep`, `pkill`), `fdisk` (`sfdisk`),
  `mount` (`mount`, `umount`, `losetup`), `util-linux` (`blockdev`,
  `setsid`, `unshare`) and `e2fsprogs` (`mkfs.ext4`);
- the kernel modules `nbd` (`max_part=16`, for `disk inject`), `9p` (for
  `cc mount`), `sch_netem` and `sch_tbf` (SCORCH qos) and `ip_gre` (mirrors);
- `tshark`, with `capinfos`, `editcap` and `mergecap`, for the SCORCH shards;
- the images the shard boots in `/phenix/images`, built from the
  sceptre-phenix-images recipes listed for it in `.github/e2e/shards.json`;
- for the shards with `apps: true`, every sceptre-phenix-apps console script
  in `/usr/local/bin`, with `E2E_APPS=true`.

The tests start their own `phenix ui`.

```bash
cd src/go
go test -c -tags e2e -o /tmp/e2e.test ./e2e
sudo /tmp/e2e.test -test.v -test.run '^TestCore'
sudo E2E_SMOKE_IMAGES='ntp minirouter' /tmp/e2e.test -test.v -test.run '^TestImagesSmoke$'
```

Use a dedicated host. The tests use phenix's default store and base directory,
clear the minimega namespaces of their experiments, and take every QEMU process
under minimega's base for their own.

| Variable | Default | Meaning |
|---|---|---|
| `E2E_PHENIX` | `/usr/local/bin/phenix` | phenix binary under test |
| `E2E_STORE` | `bolt:///etc/phenix/store.bdb` | store shared by the CLI and the UI |
| `E2E_MM_BASE` | `/tmp/minimega` | minimega `-base`, also given to phenix as `--base-dir.minimega`; the apps read theirs from `MM_SOCKET_PATH` |
| `E2E_IMAGES_DIR` | `/phenix/images` | minimega `-filepath` |
| `E2E_WORK_DIR` | `$TMPDIR/phenix-e2e` | configs and logs the tests write |
| `E2E_UI_ADDR` | `127.0.0.1:3080` | where the tests start `phenix ui` |
| `E2E_GUEST_IMAGE` | `ntp.qc2` | general-purpose real guest |
| `E2E_ROUTER_IMAGE` | `minirouter.qc2` | router guest |
| `E2E_TOOLS_IMAGE` | `e2e.qc2` | headless guest with the tools apps and SCORCH components run |
| `E2E_SMOKE_IMAGES` | (none) | recipes `TestImagesSmoke` boots |
| `E2E_APPS` | `false` | whether sceptre-phenix-apps is installed |

## Writing tests

- Name each test after the shard that runs it. `.github/e2e/shards.json` gives
  each shard the test name prefix it runs (`run`), the images it boots and
  whether it installs the apps: `TestCore`, `TestDisk`, `TestConfig`,
  `TestResilience`, `TestBuiltin`, `TestC2`, `TestDelay`, `TestOps`,
  `TestNet`, `TestApps`, `TestScorchHost`, `TestScorchGuest` and
  `TestDomainRender` on pushes and pull requests, and `TestScorchNight`,
  `TestDomainRuntimeOtsim` and `TestDomainRuntimeSceptre` nightly. The image
  jobs run `TestImagesSmoke`. The workflow's Plan job fails the run for a test
  that matches no shard's `run` pattern or more than one, and for a pattern
  that matches no test.
- Create experiments with `newExperiment`; it registers cleanup that stops and
  deletes the experiment and its configs. Give each experiment a unique name.
- Never trust the exit code of `phenix experiment start` alone. Check what
  minimega reports (`vmInfo`, `mmTable`), what Open vSwitch reports
  (`ovs-vsctl`), what the guest reports (`ccExec`, `guestShell`), and what is
  on disk (`inspectDisk`).
- Talk to minimega with the harness client (`mmRun`, `mm`, `mmTable`), not
  with phenix's own `util/mm` package: the tests check phenix's parsing. A
  test of those helpers themselves, such as the C2 helpers in
  `c2_flows_test.go` or the transport in `core_api_test.go`, calls them as the
  subject under test, never as the source of what it expects.
- minimega creates any namespace a command names, so check that a namespace is
  gone with `mmNamespaces`, not with a namespaced command.
- Wait with `eventually`, never with a fixed sleep, unless a product constant is
  under test.
- Hollow VMs (`hollowNode`, on the `e2e-ext4.qc2` fixture from
  `hollowImages`) boot QEMU with no operating system; use them wherever no guest
  OS is needed. Use the real guest image (`node` with `cfg.guest`) for anything
  seen from inside a guest. Real guests take about 90 s to register with C2.
- A missing image is a harness failure (`requireImage`), never a skip.
