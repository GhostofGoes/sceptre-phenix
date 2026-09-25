import { reactive } from 'vue';
import { cachedPage, cachePage } from '@/utils/pageCache.js';
import { useErrorNotification } from '@/utils/errorNotif.js';

// Load state of the page on screen, shown by RefreshStatus in the header.
// refresh is null when the page has nothing to reload.
export const pageStatus = reactive({
  refresh: null,
  loading: false,
  updatedAt: null,
  failed: false, // whether the latest load failed
});

// the loader that owns pageStatus; others must not touch it
let owner = null;

// Loads a page's data without hiding the page behind a spinner: cached data
// is shown at once, progress shows in the header, and the header's refresh
// button reloads it.
//
//   fetch(signal) resolves to the data; it is passed the AbortSignal that
//     cancels the request when the page is left or reloaded.
//   apply(data) puts the data on the page.
//   key is the cache key; pass a function to decide per load, returning
//     null to skip caching (e.g. for non-default filters), or omit it for
//     live pages whose cached copy would mislead.
//   refresh, if given, replaces load() as the header button's action, for
//     pages that also reload secondary data on request.
export function createPageLoader({ key = null, fetch, apply, refresh }) {
  let controller = null;
  const cacheKey = () => (typeof key === 'function' ? key() : key);

  const loader = {
    // whether a load is in flight, so polling can skip a tick
    get loading() {
      return controller !== null;
    },

    // shows cached data (if any) and loads fresh data
    start() {
      owner = loader;
      pageStatus.refresh = refresh ?? (() => loader.load());
      pageStatus.loading = false;
      pageStatus.updatedAt = null;
      pageStatus.failed = false;

      const k = cacheKey();
      const cached = k ? cachedPage(k) : undefined;
      if (cached) {
        apply(cached.data);
        pageStatus.updatedAt = cached.at;
      }

      return loader.load();
    },

    // resolves to true once fresh data is applied, false otherwise
    async load() {
      controller?.abort();
      const current = (controller = new AbortController());
      if (owner === loader) pageStatus.loading = true;

      try {
        const data = await fetch(current.signal);
        if (current.signal.aborted) return false;

        const k = cacheKey();
        if (k) cachePage(k, data);
        apply(data);

        if (owner === loader) {
          pageStatus.updatedAt = Date.now();
          pageStatus.failed = false;
        }
        return true;
      } catch (err) {
        if (current.signal.aborted) return false;

        useErrorNotification(err);
        if (owner === loader) pageStatus.failed = true;
        return false;
      } finally {
        if (controller === current) {
          controller = null;
          if (owner === loader) pageStatus.loading = false;
        }
      }
    },

    // cancels any request in flight and clears the header
    stop() {
      controller?.abort();
      controller = null;
      if (owner !== loader) return;

      owner = null;
      pageStatus.refresh = null;
      pageStatus.loading = false;
      pageStatus.updatedAt = null;
      pageStatus.failed = false;
    },
  };

  return loader;
}
