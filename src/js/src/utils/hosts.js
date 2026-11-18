// hostOptions returns the schedulable hosts of a GET /hosts answer as select
// options, sorted by label: each shows the host's display name and has the
// name minimega gives the host as its value, the name a VM's schedule holds,
// so the select of a scheduled VM shows its host.
export function hostOptions(hosts) {
  return (hosts || [])
    .filter((h) => h.schedulable)
    .map((h) => ({ label: h.name, value: h.minimega_name || h.name }))
    .sort((a, b) => a.label.localeCompare(b.label));
}
