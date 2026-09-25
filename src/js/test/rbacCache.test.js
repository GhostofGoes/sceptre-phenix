import { roleAllowed } from '@/utils/rbac.js';
import { usePhenixStore } from '@/store.js';
import { test, expect, vi } from 'vitest';

vi.mock('@/store.js', () => ({ usePhenixStore: vi.fn() }));

const role = (resourceNames) => ({
  name: 'Same Name',
  policies: [{ resources: ['experiments'], resourceNames, verbs: ['get'] }],
});

test('cached results do not leak across logins with a same-named role', () => {
  usePhenixStore.mockReturnValue({ role: role(['exp1']) });
  expect(roleAllowed('experiments', 'get', 'exp1')).toBe(true);
  expect(roleAllowed('experiments', 'get', 'exp2')).toBe(false);

  // a new login replaces the role object; the cache must not be reused
  usePhenixStore.mockReturnValue({ role: role(['exp2']) });
  expect(roleAllowed('experiments', 'get', 'exp1')).toBe(false);
  expect(roleAllowed('experiments', 'get', 'exp2')).toBe(true);
});
