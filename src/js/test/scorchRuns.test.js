import { beforeEach, describe, expect, test, vi } from 'vitest';
import axiosInstance from '@/utils/axios.js';
import { useErrorNotification } from '@/utils/errorNotif';
import ScorchRuns from '@/views/ScorchRuns.vue';

// The real modules import the Pinia store, the websocket or xterm, which need
// a browser. Vitest hoists vi.mock above the imports.
vi.mock('@/utils/axios.js', () => ({
  default: { get: vi.fn(), post: vi.fn(), delete: vi.fn() },
}));
vi.mock('@/utils/errorNotif', () => ({ useErrorNotification: vi.fn() }));
vi.mock('@/utils/websocket', () => ({
  addWsHandler: vi.fn(),
  removeWsHandler: vi.fn(),
}));
vi.mock('@/store.js', () => ({ usePhenixStore: vi.fn() }));
vi.mock('@/components/scorch/ScorchKey.vue', () => ({ default: {} }));
vi.mock('@/components/scorch/ScorchRun.vue', () => ({ default: {} }));
vi.mock('@/components/MiniTerminal.vue', () => ({ default: {} }));

// The repo has no DOM test environment or @vue/test-utils, so run the
// component's methods against a plain object.
function newScorchRuns(running) {
  const vm = { runs: [{ name: 'run0', running, nodes: [], loop: 0 }] };
  for (const [name, method] of Object.entries(ScorchRuns.methods)) {
    vm[name] = method.bind(vm);
  }
  return vm;
}

const conflict = {
  response: {
    status: 409,
    data: {
      message: 'unable to start Scorch run 0 for experiment exp',
      cause: 'the Scorch app is currently already running',
    },
  },
};

describe('ScorchRuns run control', () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  // Regression test: the start and cancel requests' errors were ignored, so
  // a refused start, such as one while a run executed, showed nothing.
  test.each([
    ['start', false, 'post'],
    ['cancel', true, 'delete'],
  ])('shows a refused %s', async (_, running, method) => {
    axiosInstance[method].mockRejectedValueOnce(conflict);

    await newScorchRuns(running).scorchControl('exp', 0);

    expect(axiosInstance[method]).toHaveBeenCalledWith(
      'experiments/exp/scorch/pipelines/0',
    );
    expect(useErrorNotification).toHaveBeenCalledWith(conflict);
  });

  test('shows nothing for an accepted start', async () => {
    axiosInstance.post.mockResolvedValueOnce({ status: 204 });

    await newScorchRuns(false).scorchControl('exp', 0);

    expect(useErrorNotification).not.toHaveBeenCalled();
  });
});
