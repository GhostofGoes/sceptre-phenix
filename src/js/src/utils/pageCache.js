// The last data each page loaded, so returning to a page shows it straight
// away while a fresh copy loads, rather than a blank table under a spinner.
// Cleared on logout so one user never sees another's data.
const cache = new Map();

export function cachedPage(key) {
  return cache.get(key);
}

export function cachePage(key, data) {
  cache.set(key, data);
}

export function clearPageCache() {
  cache.clear();
}
