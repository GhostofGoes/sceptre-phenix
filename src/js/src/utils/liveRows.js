// Remembers which rows a page changed from websocket updates, so a list
// requested before an update arrived does not put the old state back: an
// experiment stopped while the list was loading would otherwise show as
// started again until the next reload.
export function createLiveRows(key = 'name') {
  const updatedAt = new Map();

  return {
    // records that the row named `name` was just updated
    touch(name) {
      updatedAt.set(name, Date.now());
    },

    // the loaded rows, keeping the current version of each row updated since
    // the load was requested
    merge(loaded, current, requestedAt) {
      const live = new Map((current ?? []).map((row) => [row[key], row]));
      return loaded.map((row) => {
        const at = updatedAt.get(row[key]);
        const kept = live.get(row[key]);
        return kept && at !== undefined && at >= requestedAt ? kept : row;
      });
    },
  };
}
