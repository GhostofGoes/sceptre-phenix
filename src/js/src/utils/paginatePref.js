// The Paginate toggle's last setting, shared by every table and every user of
// this browser. Tables start unpaginated until someone turns it on. Kept in
// localStorage only, so it survives logout and reloads but never reaches the
// server. Storage can be unavailable (private windows, blocked site data), in
// which case tables simply start unpaginated.
const KEY = 'phenix.paginate';

export function loadPaginate() {
  try {
    return localStorage.getItem(KEY) === 'true';
  } catch {
    return false;
  }
}

export function savePaginate(on) {
  try {
    localStorage.setItem(KEY, String(Boolean(on)));
  } catch {
    // see above
  }
}
