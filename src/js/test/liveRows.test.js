import { afterEach, describe, expect, it, vi } from 'vitest';
import { createLiveRows } from '@/utils/liveRows.js';

describe('createLiveRows', () => {
  afterEach(() => vi.useRealTimers());

  it('keeps a row updated after the load was requested', () => {
    vi.useFakeTimers({ now: 1000 });
    const rows = createLiveRows();
    const requestedAt = Date.now();

    vi.setSystemTime(2000);
    rows.touch('demo');

    const current = [{ name: 'demo', status: 'stopped' }];
    const loaded = [
      { name: 'demo', status: 'started' },
      { name: 'other', status: 'started' },
    ];

    expect(rows.merge(loaded, current, requestedAt)).toEqual([
      { name: 'demo', status: 'stopped' },
      { name: 'other', status: 'started' },
    ]);
  });

  it('takes the loaded row when the update came before the request', () => {
    vi.useFakeTimers({ now: 1000 });
    const rows = createLiveRows();
    rows.touch('demo');

    vi.setSystemTime(2000);
    const merged = rows.merge(
      [{ name: 'demo', status: 'started' }],
      [{ name: 'demo', status: 'stopped' }],
      Date.now(),
    );

    expect(merged).toEqual([{ name: 'demo', status: 'started' }]);
  });

  it('drops rows that are no longer loaded', () => {
    const rows = createLiveRows();
    rows.touch('gone');

    expect(rows.merge([], [{ name: 'gone' }], 0)).toEqual([]);
  });
});
