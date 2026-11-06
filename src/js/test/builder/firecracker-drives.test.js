// A Firecracker node's read-only drives and scratch drive in the Inspector's
// device form. The fields show for a Firecracker node, and for another node
// only while they hold a value, which that node refuses.
import { describe, expect, test } from 'vitest';
import { createSSRApp, h } from 'vue';
import { renderToString } from 'vue/server-renderer';
import { JsonForms } from '@jsonforms/vue';

import {
  inspectorRenderers,
  uiSchemaForKind,
} from '@/builder/adapters/forms.js';
import { createFormValidator } from '@/builder/form-validator.js';
import { builderSchemaV1, schemaForKind } from '@/builder/schema.js';

const formValidator = createFormValidator();

const READ_ONLY = 'spec.hardware.drives.0.read_only';
const SCRATCH = 'spec.hardware.scratch';

// Renders the device form of a node of a VM type, as BuilderInspector does.
async function renderDeviceForm(vmType, { drive = {}, hardware = {} } = {}) {
  const data = {
    hostname: 'node',
    iconKey: 'linux',
    spec: {
      type: 'VirtualMachine',
      general: { hostname: 'node', vm_type: vmType },
      hardware: {
        os_type: 'linux',
        drives: [{ image: 'root.squashfs', ...drive }],
        ...hardware,
      },
      network: { interfaces: [] },
    },
  };
  const app = createSSRApp({
    render: () =>
      h(JsonForms, {
        data,
        schema: schemaForKind(builderSchemaV1, 'device', { spec: data.spec }),
        uischema: uiSchemaForKind(builderSchemaV1, 'device', {
          spec: data.spec,
        }),
        renderers: inspectorRenderers,
        ajv: formValidator,
      }),
  });

  return renderToString(app);
}

describe('Firecracker drive fields', () => {
  test('show for a Firecracker node with their labels and help', async () => {
    const html = await renderDeviceForm('firecracker');

    expect(html).not.toContain('No applicable renderer');
    expect(html).toContain(READ_ONLY);
    expect(html).toContain(SCRATCH);
    expect(html).toContain('Read only');
    expect(html).toContain('Scratch drive size');
    expect(html).toContain('Size of a writable scratch drive');
  });

  test.each(['kvm', 'container', ''])(
    'do not show for a node of VM type "%s"',
    async (vmType) => {
      const html = await renderDeviceForm(vmType);

      expect(html).not.toContain('No applicable renderer');
      expect(html).toContain('spec.hardware.os_type');
      expect(html).not.toContain(READ_ONLY);
      expect(html).not.toContain(SCRATCH);
    },
  );

  test('show for a kvm node while they hold a value', async () => {
    const html = await renderDeviceForm('kvm', {
      drive: { read_only: true },
      hardware: { scratch: '64M' },
    });

    expect(html).toContain(READ_ONLY);
    expect(html).toContain(SCRATCH);
  });

  // The rules read the working copy, so a field shows as soon as the VM type
  // changes, before Apply.
  test('follow the VM type of the working copy', () => {
    const spec = uiSchemaForKind(builderSchemaV1, 'device').elements.find(
      (element) => element.scope === '#/properties/spec',
    ).options.detail;
    const hardware = spec.elements.find(
      (element) => element.scope === '#/properties/hardware',
    ).options.detail;
    const scratch = hardware.elements.find(
      (element) => element.scope === '#/properties/scratch',
    );
    const drives = hardware.elements.find(
      (element) => element.scope === '#/properties/drives',
    );
    const readOnly = drives.options.detail.elements.find(
      (element) => element.scope === '#/properties/read_only',
    );
    const shows = (rule, vmType, data) => {
      const fullData = { spec: { general: { vm_type: vmType } } };

      return rule.condition.validate({ data, fullData });
    };

    for (const { rule } of [scratch, readOnly]) {
      expect(rule.effect).toBe('SHOW');
      expect(shows(rule, 'firecracker', undefined)).toBe(true);
      expect(shows(rule, 'kvm', undefined)).toBe(false);
      expect(shows(rule, 'kvm', '')).toBe(false);
      expect(shows(rule, 'kvm', false)).toBe(false);
      expect(shows(rule, 'kvm', true)).toBe(true);
      expect(shows(rule, 'container', '64M')).toBe(true);
    }
  });
});
