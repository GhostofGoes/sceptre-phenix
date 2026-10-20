// Experiment events after which the experiments list no longer knows the
// experiment's state: a start or stop failed after the server announced it,
// so the row would stay "starting" or "stopping" until a reload.
const reloadingActions = new Set(['errorStarting', 'errorStopping']);

// reloadsExperiments reports whether an experiment websocket event of action
// means the experiments list must be read again.
export function reloadsExperiments(action) {
  return reloadingActions.has(action);
}
