import { describe, expect, test, vi } from 'vitest';

import {
  lazyRouteLoaders,
  prefetchSequentially,
  schedulePrefetch,
} from '@/utils/prefetch.js';

describe('lazyRouteLoaders', () => {
  test('returns only lazy (function) components', () => {
    const lazy = () => Promise.resolve({});
    const eager = { render() {} };
    const routes = [
      { components: { default: lazy } },
      { components: { default: eager } },
      { redirect: '/' },
      { components: { default: undefined } },
    ];
    expect(lazyRouteLoaders(routes)).toEqual([lazy]);
  });
});

describe('prefetchSequentially', () => {
  test('loads one chunk at a time and survives failures', async () => {
    const events = [];
    const loader = (name, fail) => async () => {
      events.push('start ' + name);
      await Promise.resolve();
      events.push('end ' + name);
      if (fail) throw new Error('offline');
    };
    await prefetchSequentially([loader('a', true), loader('b')]);
    expect(events).toEqual(['start a', 'end a', 'start b', 'end b']);
  });
});

describe('schedulePrefetch', () => {
  test('waits for the browser to be idle', () => {
    const load = vi.fn(() => Promise.resolve());
    const win = { requestIdleCallback: vi.fn() };
    schedulePrefetch([load], win);
    expect(load).not.toHaveBeenCalled();
    win.requestIdleCallback.mock.calls[0][0]();
    expect(load).toHaveBeenCalledOnce();
  });

  test('skips prefetching when the user asked to save data', () => {
    const win = {
      navigator: { connection: { saveData: true } },
      requestIdleCallback: vi.fn(),
    };
    schedulePrefetch([vi.fn()], win);
    expect(win.requestIdleCallback).not.toHaveBeenCalled();
  });
});
