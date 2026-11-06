// The VM type helper (utils/vmType.js).

import { describe, expect, test } from 'vitest';

import { isFirecracker } from '@/utils/vmType.js';

describe('isFirecracker', () => {
  test('is true only for a VM whose vm_type is firecracker', () => {
    expect(isFirecracker({ vm_type: 'firecracker' })).toBe(true);
    expect(isFirecracker({ vm_type: 'FIRECRACKER' })).toBe(true);
    expect(isFirecracker({ vm_type: 'kvm' })).toBe(false);
    expect(isFirecracker({ vm_type: 'container' })).toBe(false);
    expect(isFirecracker({ vm_type: '' })).toBe(false);
    expect(isFirecracker({})).toBe(false);
    expect(isFirecracker(null)).toBe(false);
    expect(isFirecracker(undefined)).toBe(false);
  });
});
