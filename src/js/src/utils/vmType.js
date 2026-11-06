// The minimega VM type that the VM API returns for each VM (vm_type).
// Firecracker VMs have no display and no CD-ROM, so the UI shows no
// screenshot, VNC console or optical disc action for them.

export const NO_CONSOLE_TEXT = 'No console for Firecracker VMs';

/** Whether a VM from the VM API is a Firecracker microVM. */
export function isFirecracker(vm) {
  return String(vm?.vm_type || '').toLowerCase() === 'firecracker';
}
