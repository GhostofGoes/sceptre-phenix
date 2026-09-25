import { createPageLoader, pageStatus } from '@/utils/pageLoader.js';
import { cachePage, clearPageCache } from '@/utils/pageCache.js';
import { beforeEach, test, expect, vi } from 'vitest';

vi.mock('@/utils/errorNotif.js', () => ({ useErrorNotification: vi.fn() }));

beforeEach(() => clearPageCache());

// a fetch the test resolves by hand, recording the signal it was given
function manualFetch() {
  const calls = [];
  const fetch = (signal) =>
    new Promise((resolve) => calls.push({ signal, resolve }));
  return { fetch, calls };
}

test('shows cached data at once and replaces it when fresh data arrives', async () => {
  cachePage('hosts', ['cached']);
  const { fetch, calls } = manualFetch();
  const apply = vi.fn();
  const loader = createPageLoader({ key: 'hosts', fetch, apply });

  const done = loader.start();
  expect(apply).toHaveBeenCalledWith(['cached']);
  expect(pageStatus.loading).toBe(true);
  expect(pageStatus.updatedAt).not.toBeNull();

  calls[0].resolve(['fresh']);
  await done;
  expect(apply).toHaveBeenLastCalledWith(['fresh']);
  expect(pageStatus.loading).toBe(false);
  loader.stop();
});

test('leaving the page cancels the request and clears the header', async () => {
  const { fetch, calls } = manualFetch();
  const apply = vi.fn();
  const loader = createPageLoader({ key: 'disks', fetch, apply });

  const done = loader.start();
  expect(pageStatus.refresh).not.toBeNull();
  loader.stop();
  expect(calls[0].signal.aborted).toBe(true);
  expect(pageStatus.refresh).toBeNull();

  calls[0].resolve(['late']);
  expect(await done).toBe(false);
  expect(apply).not.toHaveBeenCalled();
});

test('a newer load supersedes an older one', async () => {
  const { fetch, calls } = manualFetch();
  const apply = vi.fn();
  const loader = createPageLoader({ fetch, apply });

  const first = loader.start();
  const second = loader.load();
  expect(calls[0].signal.aborted).toBe(true);

  calls[1].resolve('new');
  calls[0].resolve('old');
  await Promise.all([first, second]);
  expect(apply).toHaveBeenCalledTimes(1);
  expect(apply).toHaveBeenCalledWith('new');
  loader.stop();
});

test("a page left behind does not clear the next page's header", () => {
  const { fetch } = manualFetch();
  const previous = createPageLoader({ fetch, apply: () => {} });
  const next = createPageLoader({ fetch, apply: () => {} });

  previous.start();
  next.start();
  previous.stop();
  expect(pageStatus.refresh).not.toBeNull();
  next.stop();
});
