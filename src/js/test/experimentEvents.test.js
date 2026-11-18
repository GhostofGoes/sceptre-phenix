import { describe, expect, test } from 'vitest';
import { reloadsExperiments } from '@/utils/experimentEvents.js';

describe('reloadsExperiments', () => {
  test('reloads after a failed start or stop', () => {
    // The row stayed "starting" or "stopping" until a page reload.
    expect(reloadsExperiments('errorStarting')).toBe(true);
    expect(reloadsExperiments('errorStopping')).toBe(true);
  });

  test('handles the other events in place', () => {
    for (const action of [
      'start',
      'stop',
      'starting',
      'stopping',
      'progress',
      'create',
      'delete',
    ]) {
      expect(reloadsExperiments(action)).toBe(false);
    }
  });
});
