import { cachedPage, cachePage } from '@/utils/pageCache.js';
import { usePhenixStore } from '@/store.js';
import { useErrorNotification } from '@/utils/errorNotif.js';
import { createPinia, setActivePinia } from 'pinia';
import { test, expect, vi } from 'vitest';

vi.mock('@/router', () => ({ default: { replace: vi.fn() } }));
vi.mock('buefy', () => ({ NotificationProgrammatic: vi.fn() }));

// the store reads the saved login from web storage when it is created
const storage = () => ({ getItem: () => null, removeItem: () => {} });
vi.stubGlobal('localStorage', storage());
vi.stubGlobal('sessionStorage', storage());

test('logging out drops cached page data', () => {
  setActivePinia(createPinia());
  cachePage('hosts', [{ name: 'host1' }]);
  expect(cachedPage('hosts').data).toEqual([{ name: 'host1' }]);

  usePhenixStore().logout();
  expect(cachedPage('hosts')).toBeUndefined();
});

test('requests cancelled on leaving a page raise no error', async () => {
  const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
  await useErrorNotification({ code: 'ERR_CANCELED', message: 'canceled' });
  expect(warn).not.toHaveBeenCalled();
});
