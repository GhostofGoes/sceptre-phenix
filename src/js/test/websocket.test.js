import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

vi.mock('@/utils/debug.js', () => ({ debug: () => {} }));
vi.mock('@/store.js', () => ({ usePhenixStore: () => ({ token: null }) }));
vi.mock('buefy', () => ({
  ToastProgrammatic: class {
    open() {
      return { close() {} };
    }
  },
}));

class FakeWebSocket {
  static OPEN = 1;
  static instances = [];

  constructor(url) {
    this.url = url;
    this.readyState = 0;
    this.sent = [];
    FakeWebSocket.instances.push(this);
  }

  send(data) {
    this.sent.push(data);
  }

  close() {}

  open() {
    this.readyState = FakeWebSocket.OPEN;
    this.onopen();
  }
}

describe('onWsReconnect', () => {
  let ws;

  beforeEach(async () => {
    vi.resetModules();
    vi.useFakeTimers();
    FakeWebSocket.instances = [];
    vi.stubGlobal('WebSocket', FakeWebSocket);
    vi.stubGlobal('location', { protocol: 'http:', host: 'localhost' });
    ws = await import('@/utils/websocket');
  });

  afterEach(() => {
    ws.disconnectWebsocket();
    vi.useRealTimers();
    vi.unstubAllGlobals();
  });

  it('fires after a reconnect but not on the first connect', () => {
    const listener = vi.fn();
    ws.onWsReconnect(listener);

    ws.connectWebsocket();
    FakeWebSocket.instances[0].open();
    expect(listener).not.toHaveBeenCalled();

    FakeWebSocket.instances[0].onclose();
    vi.runAllTimers();
    FakeWebSocket.instances[1].open();
    expect(listener).toHaveBeenCalledTimes(1);
  });

  it('stops firing once unsubscribed', () => {
    const listener = vi.fn();
    const off = ws.onWsReconnect(listener);

    ws.connectWebsocket();
    FakeWebSocket.instances[0].open();
    off();

    FakeWebSocket.instances[0].onclose();
    vi.runAllTimers();
    FakeWebSocket.instances[1].open();
    expect(listener).not.toHaveBeenCalled();
  });

  it('keeps calling other listeners when one throws', () => {
    const errors = vi.spyOn(console, 'error').mockImplementation(() => {});
    const listener = vi.fn();
    ws.onWsReconnect(() => {
      throw new Error('boom');
    });
    ws.onWsReconnect(listener);

    ws.connectWebsocket();
    FakeWebSocket.instances[0].open();
    FakeWebSocket.instances[0].onclose();
    vi.runAllTimers();
    FakeWebSocket.instances[1].open();

    expect(listener).toHaveBeenCalledTimes(1);
    errors.mockRestore();
  });
});
