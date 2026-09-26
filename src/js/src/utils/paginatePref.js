// Each table's last Paginate setting, by table name, shared by every user of
// this browser. Tables start unpaginated until someone turns it on. Kept in
// localStorage only, so it survives logout and reloads but never reaches the
// server. Storage can be unavailable (private windows, blocked site data), in
// which case tables simply start unpaginated.
const key = (table) => `phenix.paginate.${table}`;

export function loadPaginate(table) {
  try {
    return localStorage.getItem(key(table)) === 'true';
  } catch {
    return false;
  }
}

export function savePaginate(table, on) {
  try {
    localStorage.setItem(key(table), String(Boolean(on)));
  } catch {
    // see above
  }
}
