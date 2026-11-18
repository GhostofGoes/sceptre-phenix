import { describe, expect, test } from 'vitest';
import { hostOptions } from '@/utils/hosts.js';

describe('hostOptions', () => {
  test("uses minimega's host name as the value", () => {
    // A VM scheduled on compute1-minimega showed no host: the options had
    // only the display name compute1.
    expect(
      hostOptions([
        { name: 'compute2', minimega_name: 'compute2', schedulable: true },
        {
          name: 'compute1',
          minimega_name: 'compute1-minimega',
          schedulable: true,
        },
      ]),
    ).toEqual([
      { label: 'compute1', value: 'compute1-minimega' },
      { label: 'compute2', value: 'compute2' },
    ]);
  });

  test('leaves out hosts that cannot be scheduled on', () => {
    expect(
      hostOptions([
        { name: 'head', minimega_name: 'head', schedulable: false },
      ]),
    ).toEqual([]);
  });

  test('falls back to the display name from an older server', () => {
    expect(hostOptions([{ name: 'compute1', schedulable: true }])).toEqual([
      { label: 'compute1', value: 'compute1' },
    ]);
  });
});
