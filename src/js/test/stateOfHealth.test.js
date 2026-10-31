// Calls the StateOfHealth methods with a stand-in for the component, so no
// DOM is needed.
import { afterEach, describe, expect, test, vi } from 'vitest';
import StateOfHealth from '@/views/StateOfHealth.vue';

vi.mock('@/utils/websocket', () => ({
  addWsHandler: vi.fn(),
  removeWsHandler: vi.fn(),
}));
vi.mock('@/utils/errorNotif', () => ({ useErrorNotification: vi.fn() }));

const { methods } = StateOfHealth;

describe('StateOfHealth', () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  test('a VM node is named with its status', () => {
    expect(
      methods.nodeLabel({ label: 'server-00', status: 'notrunning' }),
    ).toBe('server-00, status not running');
    expect(methods.nodeLabel({ label: 'plc', status: 'external' })).toBe(
      'plc, status external',
    );
  });

  test('only VM nodes, not VLAN switches, take keyboard focus', () => {
    expect(methods.isVmNode({ image: 'Linux' })).toBe(true);
    expect(methods.isVmNode({ image: 'Switch' })).toBe(false);
    expect(methods.isVmNode({})).toBe(false);
  });

  test('a black "not booted" node gets a light ring', () => {
    const vm = { running: true, updateNodeColor: methods.updateNodeColor };

    expect(methods.updateNodeBorder.call(vm, { status: 'notboot' })).toBe(
      'whitesmoke',
    );
    expect(methods.updateNodeBorder.call(vm, { status: 'running' })).toBe(
      '#4F8F00',
    );
    expect(
      methods.updateNodeBorder.call(vm, {
        status: 'notboot',
        soh: { errors: ['x'] },
      }),
    ).toBe('#FF9900');
  });

  test('reduced motion skips the hover transition', () => {
    const selection = { transition: vi.fn(() => 'animated') };

    vi.stubGlobal('window', { matchMedia: () => ({ matches: true }) });
    expect(methods.transitionFor(selection)).toBe(selection);

    vi.stubGlobal('window', { matchMedia: () => ({ matches: false }) });
    expect(methods.transitionFor(selection)).toBe('animated');
  });
});
