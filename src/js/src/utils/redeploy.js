// redeployRequestBody returns the body of the REST redeploy of one VM of the
// redeploy dialog. The disk is sent only when the user picked one: without a
// disk the server keeps the VM's own, such as its phenix disk snapshot, which
// the image the VM reports as its disk is only the backing file of. The
// injections are replicated only when inject is true (Yes).
export function redeployRequestBody(vm) {
  const body = {
    cpus: parseInt(vm.cpus),
    ram: parseInt(vm.ram),
  };

  if (vm.disk) {
    body.disk = vm.disk;
  }

  if (vm.inject === true) {
    body.injects = true;
  }

  return body;
}
