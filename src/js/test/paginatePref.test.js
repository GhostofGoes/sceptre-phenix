import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { nextTick } from 'vue';
import { loadPaginate, savePaginate } from '@/utils/paginatePref.js';
import { useTable } from '@/utils/useTable.js';

function memoryStorage() {
  const items = new Map();
  return {
    getItem: (k) => (items.has(k) ? items.get(k) : null),
    setItem: (k, v) => items.set(k, String(v)),
    removeItem: (k) => items.delete(k),
  };
}

describe('paginate preference', () => {
  beforeEach(() => vi.stubGlobal('localStorage', memoryStorage()));
  afterEach(() => vi.unstubAllGlobals());

  it('is off until someone turns it on', () => {
    expect(loadPaginate()).toBe(false);
    expect(useTable().table.isPaginated).toBe(false);
  });

  it('is remembered for every table, under one key for all users', async () => {
    const { table } = useTable();
    table.isPaginated = true;
    await nextTick();

    expect(localStorage.getItem('phenix.paginate')).toBe('true');
    expect(useTable().table.isPaginated).toBe(true);

    table.isPaginated = false;
    await nextTick();
    expect(useTable().table.isPaginated).toBe(false);
  });

  it('leaves tables without a toggle unpaginated', async () => {
    savePaginate(true);
    const { table } = useTable({ persist: false });
    expect(table.isPaginated).toBe(false);

    table.isPaginated = false;
    await nextTick();
    expect(loadPaginate()).toBe(true);
  });

  it('starts unpaginated when storage is unavailable', () => {
    vi.stubGlobal('localStorage', {
      getItem: () => {
        throw new Error('blocked');
      },
      setItem: () => {
        throw new Error('blocked');
      },
    });

    expect(loadPaginate()).toBe(false);
    expect(() => savePaginate(true)).not.toThrow();
  });
});
