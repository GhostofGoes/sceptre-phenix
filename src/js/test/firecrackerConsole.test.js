// Firecracker VMs have no display and no CD-ROM: the experiment views show
// no screenshot, VNC console or optical disc action for them.
import { flushPromises } from '@vue/test-utils';
import { afterEach, beforeEach, describe, expect, test, vi } from 'vitest';

const { axios, capturing, loaderSpy, modules, resetMocks } = await vi.hoisted(
  () => import('./helpers/mocks.js'),
);
vi.mock('@/utils/rbac.js', modules.rbac);
vi.mock('@/utils/axios.js', modules.axios);
vi.mock('@/utils/errorNotif.js', modules.errorNotif);
vi.mock('@/utils/notify.js', modules.notify);
vi.mock('@/store.js', modules.store);
vi.mock('@/utils/websocket', modules.websocket);
vi.mock('@/utils/pageLoader.js', capturing);

import VmDetailsCard from '@/components/experiment/VmDetailsCard.vue';
import VmTable from '@/components/experiment/VmTable.vue';
import VncTiles from '@/components/experiment/VncTiles.vue';
import { vmActionState } from '@/components/experiment/vmActions.js';
import { applyVmEvent } from '@/components/experiment/vmRows.js';
import { useTable } from '@/utils/useTable.js';
import RunningExperiment from '@/views/experiment/RunningExperiment.vue';
import VMtilesView from '@/views/experiment/VMtilesView.vue';
import StateOfHealth from '@/views/StateOfHealth.vue';
import { vm, wsMsg } from './helpers/fixtures.js';
import { mountComponent, tableRows, textOf } from './helpers/mount.js';

const NO_CONSOLE = 'No console for Firecracker VMs';

const kvm = vm({ vm_type: 'kvm', screenshot: 'data:image/png;base64,AAAA' });
const firecracker = vm({ name: 'micro', vm_type: 'firecracker' });

beforeEach(() => resetMocks());

describe('the running experiment', () => {
  test('the VNC tab leaves out Firecracker VMs', async () => {
    const wrapper = await mountComponent(VncTiles, {
      route: '/experiment/exp',
      props: {
        experiment: 'exp',
        zoom: 4,
        width: 200,
        vms: [kvm, firecracker],
      },
    });

    expect(wrapper.findAll('.vnc-tile-name').map(textOf)).toEqual(['web']);
  });

  test('a screenshot update does not reach a Firecracker VM', () => {
    const experiment = { vms: [{ ...kvm }, { ...firecracker }] };
    const shot = { screenshot: 'data:image/png;base64,BBBB' };

    for (const name of ['web', 'micro']) {
      const msg = wsMsg(
        'experiment/vm/screenshot',
        `exp/${name}`,
        'update',
        shot,
      );
      applyVmEvent(experiment, msg, name);
    }

    expect(experiment.vms[0].screenshot).toBe(shot.screenshot);
    expect(experiment.vms[1].screenshot).toBeUndefined();
  });

  test('the CD-ROM action is not offered for a Firecracker VM', () => {
    const opts = { can: () => true, features: [] };

    expect(vmActionState('cdrom', firecracker, opts).permitted).toBe(false);
    expect(vmActionState('cdrom', kvm, opts).permitted).toBe(true);
  });

  test('the optical disc dialog does not open for a Firecracker VM', async () => {
    const page = { opticalDiscModal: { active: false, vmName: null } };

    await RunningExperiment.methods.showChangeDisc.call(page, firecracker);

    expect(page.opticalDiscModal).toEqual({ active: false, vmName: null });
  });

  test('no optical disc request is sent for a Firecracker VM', () => {
    const page = {
      $route: { params: { id: 'exp' } },
      experiment: { vms: [firecracker] },
      expModal: { vm: firecracker },
      opticalDiscModal: { active: true, disc: '/a.iso', vmName: 'micro' },
    };

    RunningExperiment.methods.insertOpticalDisc.call(page);
    RunningExperiment.methods.ejectOpticalDisc.call(page);

    expect(page.opticalDiscModal.active).toBe(false);
    expect(axios.post).not.toHaveBeenCalled();
    expect(axios.put).not.toHaveBeenCalled();
    expect(axios.delete).not.toHaveBeenCalled();
  });

  test('the Screenshot column says a Firecracker VM has no console', async () => {
    const wrapper = await mountComponent(VmTable, {
      route: '/experiment/exp',
      props: {
        vms: [kvm, firecracker],
        experiment: 'exp',
        table: useTable().table,
        columns: { screenshot: true },
      },
    });

    const rows = tableRows(wrapper);
    const row = (name) =>
      rows.find((cells) => textOf(cells.Node.find('.tag')) === name);

    expect(textOf(row('micro').Screenshot)).toContain(NO_CONSOLE);
    expect(row('micro').Screenshot.find('a').exists()).toBe(false);
    expect(textOf(row('web').Screenshot)).not.toContain(NO_CONSOLE);
    expect(row('web').Screenshot.find('a[href]').exists()).toBe(true);
  });

  test('the details card says a Firecracker VM has no console, and its type', async () => {
    const wrapper = await mountComponent(VmDetailsCard, {
      props: {
        vm: firecracker,
        fullName: 'exp/micro',
        experiment: 'exp',
        screenshot: 'data:image/png;base64,AAAA',
      },
    });

    expect(textOf(wrapper)).toContain(NO_CONSOLE);
    expect(textOf(wrapper.get('.vm-card-meta'))).toContain('firecracker');
    expect(wrapper.find('.vm-screenshot').exists()).toBe(false);
    expect(wrapper.find('.vm-action-cdrom').exists()).toBe(false);
  });
});

describe('StateOfHealth', () => {
  test('the details dialog reads the VM type of the VM it shows', async () => {
    axios.get.mockResolvedValueOnce({ data: { vm_type: 'firecracker' } });
    const page = {
      $route: { params: { id: 'exp' } },
      detailsModal: { vm: 'micro', vmType: '' },
    };

    await StateOfHealth.methods.loadDetailsVmType.call(page, 'micro');

    expect(axios.get).toHaveBeenCalledWith('experiments/exp/vms/micro');
    expect(page.detailsModal.vmType).toBe('firecracker');
  });

  test('a failed read leaves the VM type unknown', async () => {
    axios.get.mockRejectedValueOnce(new Error('no backend'));
    const page = {
      $route: { params: { id: 'exp' } },
      detailsModal: { vm: 'micro', vmType: '' },
    };

    await StateOfHealth.methods.loadDetailsVmType.call(page, 'micro');

    expect(page.detailsModal.vmType).toBe('');
  });
});

describe('VMtilesView', () => {
  beforeEach(() => {
    vi.stubGlobal(
      'IntersectionObserver',
      class {
        observe() {}
        unobserve() {}
        disconnect() {}
      },
    );
  });

  afterEach(() => vi.unstubAllGlobals());

  test('a Firecracker tile says it has no console, with no VNC link', async () => {
    const page = await mountComponent(VMtilesView, { route: '/vms' });
    loaderSpy.options.apply([
      { ...kvm, experiment: 'exp' },
      { ...firecracker, experiment: 'exp' },
    ]);
    await flushPromises();

    const tile = (name) =>
      page.findAll('.tile.is-child').find((t) => t.text().includes(name));

    expect(textOf(tile('exp/micro'))).toContain(NO_CONSOLE);
    expect(tile('exp/micro').find('a').exists()).toBe(false);
    expect(tile('exp/web').find('a[href]').exists()).toBe(true);
  });
});
