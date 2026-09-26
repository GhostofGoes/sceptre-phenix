// Exercises the options of the experiment views directly, with a plain object
// standing in for the component instance.
import { beforeEach, describe, expect, it, vi } from 'vitest';

const allowed = vi.hoisted(() => ({ fn: () => true }));
vi.mock('@/utils/rbac.js', () => ({
  roleAllowed: (...args) => allowed.fn(...args),
}));

const axios = vi.hoisted(() => ({ get: vi.fn(), patch: vi.fn() }));
vi.mock('@/utils/axios.js', () => ({ default: axios }));

vi.mock('@/store', () => ({
  usePhenixStore: () => ({ token: 't', features: [], role: null }),
}));

vi.mock('@/utils/websocket', () => ({
  addWsHandler: () => {},
  removeWsHandler: () => {},
}));

import StoppedExperiment from '@/views/experiment/StoppedExperiment.vue';
import Experiments from '@/views/Experiments.vue';
import VMtilesView from '@/views/experiment/VMtilesView.vue';
import VMLabelsModal from '@/components/VMLabelsModal.vue';
import VMMountBrowserModal from '@/components/VMMountBrowserModal.vue';

const flush = () => new Promise((resolve) => setTimeout(resolve));

beforeEach(() => {
  allowed.fn = () => true;
  axios.get.mockReset();
  axios.patch.mockReset();
});

describe('StoppedExperiment', () => {
  const { methods, computed } = StoppedExperiment;

  function stopped(vms) {
    return {
      $route: { params: { id: 'exp1' } },
      $buefy: { toast: { open: vi.fn() } },
      experiment: { name: 'exp1', vms },
      applySchedule: methods.applySchedule,
    };
  }

  it('applies VM updates only for its own experiment', () => {
    const ctx = stopped([{ name: 'vm1', host: 'a' }]);
    const update = (exp) => ({
      resource: { type: 'experiment/vm', name: `${exp}/vm1`, action: 'update' },
      result: { name: 'vm1', host: 'b' },
    });

    methods.handleWs.call(ctx, update('exp2'));
    expect(ctx.experiment.vms[0].host).toBe('a');

    methods.handleWs.call(ctx, update('exp1'));
    expect(ctx.experiment.vms[0].host).toBe('b');
  });

  it('applies schedules only for its own experiment', () => {
    const ctx = stopped([{ name: 'vm1', host: 'a' }]);
    const schedule = (exp) => ({
      resource: { type: 'experiment', name: exp, action: 'schedule' },
      result: { schedule: [{ vm: 'vm1', host: 'b' }] },
    });

    methods.handleWs.call(ctx, schedule('exp2'));
    expect(ctx.experiment.vms[0].host).toBe('a');

    methods.handleWs.call(ctx, schedule('exp1'));
    expect(ctx.experiment.vms[0].host).toBe('b');
  });

  it('ignores publishes before the experiment has loaded', () => {
    const ctx = stopped(undefined);
    ctx.experiment = [];
    expect(() =>
      methods.handleWs.call(ctx, {
        resource: { type: 'experiment/vm', name: 'exp1/vm1', action: 'update' },
        result: { name: 'vm1' },
      }),
    ).not.toThrow();
    expect(ctx.$buefy.toast.open).not.toHaveBeenCalled();
  });

  function filesCtx() {
    return {
      $route: { params: { id: 'exp1' } },
      canListFiles: true,
      searchName: 'a&b',
      table: { isPaginated: false, currentPage: 5, perPage: 99 },
      filesTable: {
        isPaginated: true,
        currentPage: 2,
        perPage: 10,
        sortColumn: 'date',
        defaultSortDirection: 'desc',
        categories: [],
        category: null,
      },
      searchHistory: [],
      searchHistoryLength: 10,
      files: [],
      getUniqueItems: methods.getUniqueItems,
    };
  }

  it('requests files with encoded params and the files table paging', () => {
    const ctx = filesCtx();
    axios.get.mockResolvedValue({ data: { files: [], total: 0 } });

    methods.updateFiles.call(ctx);

    expect(axios.get).toHaveBeenCalledWith('experiments/exp1/files', {
      params: {
        filter: 'a&b',
        sortCol: 'date',
        sortDir: 'desc',
        pageNum: 2,
        perPage: 10,
      },
    });
  });

  it('treats a null file list as empty', async () => {
    const ctx = filesCtx();
    axios.get.mockResolvedValue({ data: { files: null } });

    methods.updateFiles.call(ctx);
    await flush();

    expect(ctx.files).toEqual([]);
    expect(ctx.filesTable.total).toBe(0);
    expect(ctx.filesLoaded).toBe(true);
  });

  it('does not request files without permission to list them', () => {
    const ctx = { ...filesCtx(), canListFiles: false };
    methods.updateFiles.call(ctx);
    expect(axios.get).not.toHaveBeenCalled();
  });

  it('checks file listing permission for the routed experiment', () => {
    allowed.fn = vi.fn(() => false);
    const ctx = { $route: { params: { id: 'exp1' } } };
    expect(computed.canListFiles.call(ctx)).toBe(false);
    expect(allowed.fn).toHaveBeenCalledWith(
      'experiments/files',
      'list',
      'exp1',
    );
  });

  it('clears individually selected rows after setting boot', () => {
    axios.patch.mockResolvedValue({ data: { vms: [] } });
    const ctx = {
      ...stopped([]),
      searchName: '',
      checkAll: false,
      selectedRows: ['vm1', 'vm2'],
    };

    methods.setBoot.call(ctx, true);

    expect(axios.patch).toHaveBeenCalledWith('experiments/exp1/vms', {
      vms: [
        { name: 'vm1', dnb: true },
        { name: 'vm2', dnb: true },
      ],
      total: 2,
    });
    expect(ctx.selectedRows).toEqual([]);
  });

  it('caps the search history', () => {
    const ctx = {
      ...filesCtx(),
      searchName: 'vm-new',
      searchHistory: Array.from({ length: 10 }, (_, i) => `vm-${i}`),
    };
    axios.get.mockResolvedValue({ data: { files: [{ name: 'f' }] } });

    methods.updateFiles.call(ctx);

    return flush().then(() => {
      expect(ctx.searchHistory).toHaveLength(10);
      expect(ctx.searchHistory).toContain('vm-new');
    });
  });
});

describe('Experiments', () => {
  it('searches names literally', () => {
    const ctx = {
      searchName: 'a(',
      experiments: [{ name: 'a(1', start_time: '' }, { name: 'b' }],
    };
    const found = Experiments.computed.filteredExperiments.call(ctx);
    expect(found.map((e) => e.name)).toEqual(['a(1']);
  });

  it('reports delayed VMs from the published experiment', () => {
    const open = vi.fn();
    const ctx = {
      experiments: [{ name: 'exp1' }],
      liveRows: { touch: () => {} },
      $buefy: { toast: { open } },
    };

    Experiments.methods.handleWs.call(ctx, {
      resource: { type: 'experiment', name: 'exp1', action: 'start' },
      result: { name: 'exp1', delayed_vms: 2 },
    });

    expect(open.mock.calls[0][0].message).toContain('with 2 delayed VMs');
  });

  it('focuses the create input with a Vue 3 directive hook', () => {
    expect(Experiments.directives.focus.mounted).toBeTypeOf('function');
  });
});

describe('VMtilesView', () => {
  it("starts with the routed experiment's VMs", () => {
    const data = VMtilesView.data.call({ $route: { params: { id: 'exp1' } } });
    expect(data.exp).toBe('exp1');

    const all = VMtilesView.data.call({ $route: { params: {} } });
    expect(all.exp).toBeNull();
  });

  it('filters VMs by experiment and a literal search', () => {
    const ctx = {
      exp: 'exp1',
      searchName: '[',
      vms: [
        { experiment: 'exp1', name: 'vm[1]' },
        { experiment: 'exp1', name: 'vm2' },
        { experiment: 'exp2', name: 'vm[3]' },
      ],
    };
    const vms = VMtilesView.computed.getVms.call(ctx);
    expect(vms.map((v) => v.name)).toEqual(['vm[1]']);
  });
});

describe('VMLabelsModal', () => {
  it('saves to the experiment it was given and closes on 200', async () => {
    axios.patch.mockResolvedValue({ status: 200, statusText: '' });
    const ctx = {
      experiment: 'exp1',
      vmName: 'vm1',
      tags: {},
      workingTags: [{ key: 'k', value: 'v' }],
      workingNotes: [],
      $emit: vi.fn(),
    };

    VMLabelsModal.methods.save.call(ctx);
    await flush();

    expect(axios.patch.mock.calls[0][0]).toBe('experiments/exp1/vms/vm1');
    expect(ctx.$emit).toHaveBeenCalledWith('close');
  });
});

describe('VMMountBrowserModal', () => {
  const parts = (currentPath) =>
    VMMountBrowserModal.computed.pathParts.call({ currentPath });

  it('links each crumb to its own position in the path', () => {
    expect(parts('/a/a/b')).toEqual([
      { part: 'mnt', upTo: '/' },
      { part: 'a', upTo: '/a' },
      { part: 'a', upTo: '/a/a' },
      { part: 'b', upTo: '/a/a/b' },
    ]);
  });

  it('shows only the mount at its root', () => {
    expect(parts('/')).toEqual([{ part: 'mnt', upTo: '/' }]);
  });
});
