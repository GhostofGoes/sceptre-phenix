import { describe, expect, test } from 'vitest';
import { redeployRequestBody } from '@/utils/redeploy.js';

describe('redeployRequestBody', () => {
  test('sends no disk unless the user picked one', () => {
    expect(
      redeployRequestBody({ cpus: '2', ram: '1024', disk: '', inject: false }),
    ).toEqual({ cpus: 2, ram: 1024 });
  });

  test('sends the disk the user picked', () => {
    expect(
      redeployRequestBody({
        cpus: 1,
        ram: 512,
        disk: '/phenix/images/other.qc2',
        inject: false,
      }),
    ).toEqual({ cpus: 1, ram: 512, disk: '/phenix/images/other.qc2' });
  });

  test('asks to replicate the injections', () => {
    expect(
      redeployRequestBody({ cpus: 1, ram: 512, disk: '', inject: true }),
    ).toEqual({ cpus: 1, ram: 512, injects: true });
  });

  test('replicates no injections for No', () => {
    expect(
      redeployRequestBody({ cpus: 1, ram: 512, disk: '', inject: false }),
    ).toEqual({ cpus: 1, ram: 512 });
  });
});
