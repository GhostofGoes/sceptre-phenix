// How each cached page fetches its data. They live here rather than in the
// pages so the data can be preloaded before a page is ever opened.
import axiosInstance from '@/utils/axios.js';
import { useErrorNotification } from '@/utils/errorNotif.js';
import {
  cachedPage,
  fetchIntoCache,
  isLoadingPage,
} from '@/utils/pageCache.js';
import { roleAllowed } from '@/utils/rbac.js';
import { usePhenixStore } from '@/store.js';

// the Logs page's default range, in seconds
export const DEFAULT_LOG_WINDOW = 10 * 60;

const get = async (url, signal, config = {}) =>
  (await axiosInstance.get(url, { signal, ...config })).data;

export const pageFetchers = {
  experiments: async (signal) =>
    (await get('experiments', signal)).experiments ?? [],

  configs: async (signal) => (await get('configs', signal)).configs ?? [],

  // rescan: have the server inspect every image again, not only changed ones
  disks: async (signal, { rescan = false } = {}) =>
    (await get(rescan ? 'disks?refresh=true' : 'disks', signal)).disks ?? [],

  hosts: async (signal) => (await get('hosts', signal)).hosts ?? [],

  users: async (signal) => {
    // roles are only used for the role dropdown when creating/editing
    const [users, roles] = await Promise.all([
      get('users', signal),
      roleAllowed('roles', 'list')
        ? get('roles', signal).catch((err) => {
            // the user list is still worth showing without roles
            useErrorNotification(err);
            return null;
          })
        : null,
    ]);
    users.users.forEach((u) => (u.role_name = u.role.name));
    return {
      users: users.users,
      roleNames: roles ? roles.roles.map((r) => r.name) : [],
    };
  },

  logs: async (signal) => {
    const start = new Date(Date.now() - DEFAULT_LOG_WINDOW * 1000);
    return (await get(`logs?start=${start.toISOString()}`, signal)) ?? [];
  },

  // experiments that have SCORCH configured, with their run state
  scorch: async (signal) => {
    const json = { headers: { Accept: 'application/json' } };
    const { experiments } = await get('experiments', signal);

    // fetch every experiment's apps concurrently rather than one request
    // after another
    const scorchExps = await Promise.all(
      (experiments ?? []).map(async (exp) => {
        const apps = await get(`experiments/${exp.name}/apps`, signal);

        // only do stuff with this exp if it has scorch configured
        if (!('scorch' in apps)) {
          return null;
        }

        exp.scorch = { running: apps['scorch'] };

        if (exp.scorch.running) {
          const pipelines = await get(
            `experiments/${exp.name}/scorch/pipelines`,
            signal,
            json,
          );
          exp.scorch.run = pipelines.running;
        }

        return exp;
      }),
    );

    return scorchExps.filter((exp) => exp !== null);
  },

  vmtiles: async (signal) =>
    (await get('vms?screenshot=500', signal)).vms ?? [],
};

// Tabs worth loading ahead of a visit, cheapest first, with who may see them
// (mirrors the navbar). VM tiles are left out: they carry a screenshot per VM.
const PRELOADS = [
  ['experiments', () => roleAllowed('experiments', 'list')],
  ['configs', () => roleAllowed('configs', 'list')],
  ['users', () => usePhenixStore().role?.name !== 'Disabled'],
  ['logs', () => roleAllowed('logs', 'list')],
  ['hosts', () => roleAllowed('hosts', 'list')],
  ['scorch', () => roleAllowed('experiments', 'list')],
  // last: the server inspects every disk image, one at a time
  ['disks', () => roleAllowed('disks', 'list')],
];

// Loads every tab's data into the page cache in the background, a couple of
// requests at a time so the page on screen keeps its share of the browser's
// connections. Tabs already cached or loading are skipped. A failed preload
// is not reported here; the page reports its own failure when opened.
export async function preloadPages({ concurrency = 2 } = {}) {
  const queue = PRELOADS.filter(
    ([key, allowed]) => allowed() && !cachedPage(key) && !isLoadingPage(key),
  ).map(([key]) => key);

  const worker = async () => {
    for (let key = queue.shift(); key; key = queue.shift()) {
      const request = fetchIntoCache(key, pageFetchers[key]);
      request.release(); // nothing waits on it; the background limit applies
      await request.promise.catch(() => {});
    }
  };

  await Promise.all(Array.from({ length: concurrency }, worker));
}

// Preloads once the browser is idle, so the page being opened loads first.
export function schedulePagePreload(win = globalThis) {
  if (win.navigator?.connection?.saveData) {
    return;
  }

  const start = () => preloadPages();
  if (typeof win.requestIdleCallback === 'function') {
    win.requestIdleCallback(start, { timeout: 3000 });
  } else {
    win.setTimeout(start, 1000);
  }
}
