// The Firecracker checks the Builder makes while editing, as the capacity
// preflight check makes them on the server (firecrackerIssues in
// api/builder/preflight.go): the issues of the document and the warnings of
// the Inspector's fields.
import { describe, expect, test } from 'vitest';

import { deviceFieldWarnings, validateDocument } from '@/builder/validate.js';

import { sampleDocument } from './fixtures.js';

const FIRECRACKER = 'preflight.capacity.firecracker';
const DEVICES = 'preflight.capacity.firecracker-devices';
const IGNORED = 'preflight.capacity.firecracker-ignored';
const SUFFIX = 'use a number of bytes with an optional K, M, G or T suffix';
const SECTORS = 'the size must be a multiple of 512 bytes';

// A firecracker spec of device alpha that minimega can launch, with one drive
// and `nics` interfaces on network EXP.
function firecrackerSpec({ nics = 1, hardware = {}, ...rest } = {}) {
  return {
    type: 'VirtualMachine',
    general: { hostname: 'alpha', vm_type: 'firecracker' },
    hardware: {
      os_type: 'linux',
      kernel: '/phenix/images/vmlinux',
      drives: [{ image: 'root.raw' }],
      ...hardware,
    },
    network: {
      interfaces: Array.from({ length: nics }, (_, index) => ({
        name: `eth${index}`,
        vlan: 'EXP',
      })),
    },
    ...rest,
  };
}

// The Firecracker issues of the sample document with spec as the spec of
// device alpha.
function firecrackerIssues(spec) {
  const { doc, alpha } = sampleDocument();
  const copy = structuredClone(doc);

  copy.nodes.find((node) => node.id === alpha.id).device.spec = spec;

  return validateDocument(copy).filter((issue) =>
    issue.code.startsWith(FIRECRACKER),
  );
}

// The code and the Inspector field of each issue.
function fields(spec) {
  return firecrackerIssues(spec).map(({ code, field }) => [code, field]);
}

// The code, the Inspector field and the message of each issue.
function details(spec) {
  return firecrackerIssues(spec).map(({ code, field, message }) => [
    code,
    field,
    message,
  ]);
}

describe('the Firecracker checks', () => {
  test('find nothing in a valid firecracker device', () => {
    const hardware = { vcpus: 32, scratch: '64M' };

    expect(fields(firecrackerSpec())).toEqual([]);
    expect(fields(firecrackerSpec({ hardware }))).toEqual([]);
  });

  test('are advice, which blocks neither saving nor publishing', () => {
    const issues = firecrackerIssues(
      firecrackerSpec({ hardware: { os_type: 'windows', kernel: '' } }),
    );

    expect(issues).toHaveLength(2);

    for (const issue of issues) {
      expect(issue.level).toBe('warning');
      expect(issue.severity).toBe('warning');
      expect(issue.blocksPublish).toBeUndefined();
    }
  });

  test('refuse an OS type other than linux, no kernel and 33 vCPUs', () => {
    const hardware = { os_type: 'windows', kernel: ' ', vcpus: '33' };

    expect(details(firecrackerSpec({ hardware }))).toEqual([
      [
        FIRECRACKER,
        'spec.hardware.kernel',
        'device "alpha" is a firecracker VM, which boots a kernel file, but it names no kernel',
      ],
      [
        FIRECRACKER,
        'spec.hardware.os_type',
        'device "alpha" is a firecracker VM, which runs only linux, but its OS type is "windows"',
      ],
      [
        FIRECRACKER,
        'spec.hardware.vcpus',
        'device "alpha" takes 33 vCPUs, but a firecracker VM has at most 32',
      ],
    ]);
  });

  test('count the devices, the scratch drive too, against the limit', () => {
    const counted = (nics, hardware = {}, advanced = {}) =>
      details(firecrackerSpec({ nics, hardware, advanced }));

    expect(counted(13, { scratch: '64M' })).toEqual([]);
    expect(counted(14, { scratch: '64M' })).toEqual([
      [
        DEVICES,
        'spec.network.interfaces',
        'device "alpha" needs 18 Firecracker devices (1 disk, scratch, 14 NICs, vsock, entropy); the limit is 17 without firecracker-pci',
      ],
    ]);
    expect(counted(14, { scratch: '0' })).toEqual([]);
    expect(counted(15, {}, { 'firecracker-entropy': 'false' })).toEqual([]);
    expect(counted(28, {}, { 'firecracker-pci': 'true' })).toEqual([]);
    expect(counted(29, {}, { 'firecracker-pci': 'true' })).toEqual([
      [
        DEVICES,
        'spec.network.interfaces',
        'device "alpha" needs 32 Firecracker devices (1 disk, 29 NICs, vsock, entropy); the limit is 31 with firecracker-pci',
      ],
    ]);
  });

  test('refuse a scratch drive size that minimega refuses', () => {
    const refused = (scratch, problem) => [
      [
        FIRECRACKER,
        'spec.hardware.scratch',
        `device "alpha" is a firecracker VM, but minimega refuses its scratch drive size: invalid size "${scratch}": ${problem}`,
      ],
    ];

    for (const scratch of ['12x', ' 64M', '-512', '1.5G', '9999999999T']) {
      const found = details(firecrackerSpec({ hardware: { scratch } }));

      expect(found).toEqual(refused(scratch, SUFFIX));
    }

    const sectors = details(firecrackerSpec({ hardware: { scratch: '1000' } }));

    expect(sectors).toEqual(refused('1000', SECTORS));
  });

  test('take the scratch drive sizes that minimega takes', () => {
    for (const scratch of ['', '0', '512', '64m', '1K', '2T']) {
      expect(fields(firecrackerSpec({ hardware: { scratch } }))).toEqual([]);
    }
  });

  test('need a raw or read-only drive without a snapshot', () => {
    const general = {
      hostname: 'alpha',
      vm_type: 'firecracker',
      snapshot: false,
    };
    const drive = (entry) =>
      fields(firecrackerSpec({ general, hardware: { drives: [entry] } }));
    const qcow2 = { drives: [{ image: 'base.qc2' }] };

    expect(drive({ image: 'base.QCOW2' })).toEqual([
      [FIRECRACKER, 'spec.hardware.drives.0.image'],
    ]);
    expect(drive({ image: 'base.qc2', read_only: true })).toEqual([]);
    expect(drive({ image: 'base.raw' })).toEqual([]);
    expect(fields(firecrackerSpec({ hardware: qcow2 }))).toEqual([]);
  });

  test('need a writable first drive for file injections', () => {
    const injections = [{ src: 'a.sh', dst: '/etc/a.sh' }];
    const deletions = [{ path: '/etc/old' }];
    const shared = { image: 'root.squashfs', read_only: true };
    const unpartitioned = { ...shared, inject_partition: 0 };
    const second = [{ image: 'root.raw' }, shared];
    const spec = (drives, files) =>
      firecrackerSpec({ hardware: { drives }, ...files });

    expect(details(spec([shared], { injections }))).toEqual([
      [
        FIRECRACKER,
        'spec.hardware.drives.0.read_only',
        'device "alpha" is a firecracker VM with file injections or deletions, but its first drive is read only, and injections need a writable first drive',
      ],
    ]);
    expect(fields(spec([shared], { deletions }))).toEqual([
      [FIRECRACKER, 'spec.hardware.drives.0.read_only'],
    ]);
    expect(fields(spec([unpartitioned], { injections }))).toEqual([]);
    expect(fields(spec([shared], {}))).toEqual([]);
    expect(fields(spec(second, { injections }))).toEqual([]);
  });

  test('refuse serial and QinQ interfaces', () => {
    const spec = firecrackerSpec({ nics: 2 });

    spec.network.interfaces[0].type = 'serial';
    spec.network.interfaces[1].qinq = true;

    expect(fields(spec)).toEqual([
      [FIRECRACKER, 'spec.network.interfaces.0.type'],
      [FIRECRACKER, 'spec.network.interfaces.1.qinq'],
    ]);
  });

  test('warn about what Firecracker ignores', () => {
    const spec = firecrackerSpec({
      hardware: { drives: [{ image: 'root.raw', interface: 'ide' }] },
      overrides: { Broadwell: 'host' },
    });

    expect(fields(spec)).toEqual([
      [IGNORED, 'spec.hardware.drives.0.interface'],
      [IGNORED, 'spec.overrides'],
    ]);
  });

  test.each([
    ['kvm', 'a kvm VM'],
    ['container', 'a container'],
    ['', 'a kvm VM'],
  ])('refuse read_only and scratch for VM type %j', (vmType, kind) => {
    const general = { hostname: 'alpha', vm_type: vmType };
    const drives = [
      { image: 'base.qc2' },
      { image: 'tools.img', read_only: true },
    ];
    const hardware = { drives, scratch: '64M' };

    expect(details(firecrackerSpec({ general, hardware }))).toEqual([
      [
        FIRECRACKER,
        'spec.hardware.drives.1.read_only',
        `device "alpha" is ${kind}, but drive 1 is read only, which only firecracker VMs support`,
      ],
      [
        FIRECRACKER,
        'spec.hardware.scratch',
        `device "alpha" is ${kind}, but it sets a scratch drive, which only firecracker VMs have`,
      ],
    ]);
    expect(fields(firecrackerSpec({ general }))).toEqual([]);
  });

  test('give the Inspector a warning at each field', () => {
    const { doc } = sampleDocument();
    const spec = firecrackerSpec({
      hardware: {
        drives: [{ image: 'root.squashfs', read_only: true }],
        scratch: '1000',
      },
      injections: [{ src: 'a.sh', dst: '/etc/a.sh' }],
    });
    const warnings = deviceFieldWarnings(doc, spec, { hostname: 'alpha' });

    expect(warnings['spec.hardware.scratch']).toEqual([
      `minimega refuses this size: invalid size "1000": ${SECTORS}.`,
    ]);
    expect(warnings['spec.hardware.drives.0.read_only']).toEqual([
      'phenix cannot inject files into a read-only first drive. Remove the injections and deletions, or set inject partition 0.',
    ]);
  });
});
