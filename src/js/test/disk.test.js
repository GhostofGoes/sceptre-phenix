import { describe, expect, test } from 'vitest';
import { getDiskLabel } from '@/utils/disk.js';

describe('getDiskLabel', () => {
  test.each([
    ['root image', { displayName: 'base.qcow2' }, 'base.qcow2'],
    [
      'nested image',
      { displayName: 'linux/releases/base.qcow2' },
      'linux/releases/base.qcow2',
    ],
    [
      'external image',
      { displayName: '/external/base.qcow2' },
      '/external/base.qcow2',
    ],
    ['legacy response', { name: 'base.qcow2' }, 'base.qcow2'],
  ])('labels a %s', (_, disk, expected) => {
    expect(getDiskLabel(disk)).toBe(expected);
  });
});
