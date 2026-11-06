// A Firecracker node's VM type, kernel, initrd and kernel arguments in the
// Inspector's device form, and in the saved document.
import { describe, expect, test } from 'vitest';
import { createSSRApp, h } from 'vue';
import { renderToString } from 'vue/server-renderer';
import { JsonForms } from '@jsonforms/vue';
import YAML from 'js-yaml';

import {
  applyFormData,
  inspectorRenderers,
  inspectorTarget,
  uiSchemaForKind,
} from '@/builder/adapters/forms.js';
import { parseDocument } from '@/builder/decode.js';
import { toYAMLString } from '@/builder/exporters.js';
import { createFormValidator } from '@/builder/form-validator.js';
import { findNode } from '@/builder/model.js';
import { builderSchemaV1, schemaForKind } from '@/builder/schema.js';

import { sampleDocument } from './fixtures.js';

const formValidator = createFormValidator();

const BOOT = {
  kernel: '/phenix/images/vmlinux-6.1',
  initrd: '/phenix/images/initrd-6.1.img',
  kernel_args: 'console=ttyS0 quiet',
};

// Renders the device form of a node of a VM type, as BuilderInspector does.
async function renderDeviceForm(vmType, hardware = {}) {
  const data = {
    hostname: 'node',
    iconKey: 'linux',
    spec: {
      type: 'VirtualMachine',
      general: { hostname: 'node', vm_type: vmType },
      hardware: {
        os_type: 'linux',
        drives: [{ image: 'debian.raw' }],
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

describe('Firecracker boot fields', () => {
  test('show with their labels and help for a Firecracker node', async () => {
    const html = await renderDeviceForm('firecracker', BOOT);

    expect(html).not.toContain('No applicable renderer');
    for (const key of Object.keys(BOOT)) {
      expect(html).toContain(`spec.hardware.${key}`);
    }
    expect(html).toContain('Kernel arguments');
    expect(html).toContain('Path of the guest kernel on the minimega host');
    expect(html).toContain(`value="${BOOT.kernel}"`);
    expect(html).toContain(`value="${BOOT.kernel_args}"`);
    expect(html).toMatch(/<option[^>]*>\s*firecracker\s*<\/option>/);
  });

  test.each(['kvm', 'container', ''])(
    'do not show for a node of VM type "%s"',
    async (vmType) => {
      const html = await renderDeviceForm(vmType);

      expect(html).not.toContain('No applicable renderer');
      expect(html).toContain('spec.hardware.os_type');
      for (const key of Object.keys(BOOT)) {
        expect(html).not.toContain(`spec.hardware.${key}"`);
      }
      expect(html).not.toContain('Kernel arguments');
    },
  );

  test('apply to the document and survive a YAML round trip', () => {
    const { doc, alpha } = sampleDocument();
    const selection = { type: 'node', id: alpha.id };
    const { data } = inspectorTarget(doc, selection);
    const spec = {
      ...data.spec,
      general: { ...data.spec.general, vm_type: 'firecracker' },
      hardware: { ...data.spec.hardware, ...BOOT },
    };
    const next = applyFormData(doc, selection, { ...data, spec });
    const read = parseDocument(
      YAML.load(toYAMLString(next), { schema: YAML.JSON_SCHEMA }),
    );
    const device = findNode(read, alpha.id).device;

    expect(device.spec.general.vm_type).toBe('firecracker');
    expect(device.spec.hardware).toMatchObject(BOOT);
  });
});
