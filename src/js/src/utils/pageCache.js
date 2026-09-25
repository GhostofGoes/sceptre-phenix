// The last data each page loaded, so returning to a page shows it straight
// away while a fresh copy loads, rather than a blank table under a spinner.
// Cleared on logout so one user never sees another's data.
const cache = new Map();

// Returns { data, at } (at: when it was loaded, in ms) or undefined.
export function cachedPage(key) {
  return cache.get(key);
}

export function cachePage(key, data) {
  cache.set(key, { data, at: Date.now() });
}

export function clearPageCache() {
  cache.clear();
}
