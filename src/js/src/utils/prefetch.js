// Every page is a lazy-loaded chunk, so without this the first visit to a page
// blocks navigation on fetching its chunk(s) from the server; on a slow or
// high-latency link that reads as the UI hanging for seconds after a click.
// Warming the chunks once the app is idle makes the first click instant.

// Lazy route components are the functions (not objects) in each record's
// `components`; calling one starts the dynamic import.
export function lazyRouteLoaders(routes) {
  const loaders = [];
  for (const route of routes) {
    for (const component of Object.values(route.components ?? {})) {
      if (typeof component === 'function') {
        loaders.push(component);
      }
    }
  }
  return loaders;
}

// One chunk at a time so prefetching never competes with requests the user
// triggers; a failed prefetch is harmless, navigation retries the import.
export async function prefetchSequentially(loaders) {
  for (const load of loaders) {
    try {
      await load();
    } catch {
      // ignored, see above
    }
  }
}

export function schedulePrefetch(loaders, win = globalThis) {
  if (win.navigator?.connection?.saveData) {
    return;
  }

  const start = () => prefetchSequentially(loaders);
  if (typeof win.requestIdleCallback === 'function') {
    win.requestIdleCallback(start, { timeout: 2000 });
  } else {
    win.setTimeout(start, 1);
  }
}
