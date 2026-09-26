import { preloadPages, pageFetchers } from '@/utils/pageData.js';
import { cachedPage, cachePage, clearPageCache } from '@/utils/pageCache.js';
import { roleAllowed } from '@/utils/rbac.js';
import { beforeEach, test, expect, vi } from 'vitest';

vi.mock('@/utils/axios.js', () => ({ default: { get: vi.fn() } }));
vi.mock('@/utils/errorNotif.js', () => ({ useErrorNotification: vi.fn() }));
vi.mock('@/utils/rbac.js', () => ({ roleAllowed: vi.fn(() => true) }));
vi.mock('@/store.js', () => ({
  usePhenixStore: () => ({ role: { name: 'Global Admin' } }),
}));

beforeEach(() => {
  clearPageCache();
  for (const key of Object.keys(pageFetchers)) {
    vi.spyOn(pageFetchers, key).mockResolvedValue([key]);
  }
});

test('preloads every permitted tab except VM tiles', async () => {
  roleAllowed.mockImplementation((resource) => resource !== 'disks');
  await preloadPages();

  for (const key of ['experiments', 'configs', 'users', 'logs', 'hosts']) {
    expect(cachedPage(key).data).toEqual([key]);
  }
  expect(cachedPage('disks')).toBeUndefined();
  expect(pageFetchers.vmtiles).not.toHaveBeenCalled();
});

test('skips tabs that are already cached', async () => {
  roleAllowed.mockReturnValue(true);
  cachePage('hosts', ['cached']);
  await preloadPages();

  expect(pageFetchers.hosts).not.toHaveBeenCalled();
  expect(cachedPage('hosts').data).toEqual(['cached']);
});

test('runs only a few requests at a time', async () => {
  roleAllowed.mockReturnValue(true);
  let running = 0;
  let peak = 0;
  for (const key of Object.keys(pageFetchers)) {
    pageFetchers[key].mockImplementation(async () => {
      peak = Math.max(peak, ++running);
      await new Promise((r) => setTimeout(r, 1));
      running--;
      return [];
    });
  }

  await preloadPages({ concurrency: 2 });
  expect(peak).toBe(2);
});
