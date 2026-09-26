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
    expect(loadPaginate('disks')).toBe(false);
    expect(useTable({ name: 'disks' }).table.isPaginated).toBe(false);
  });

  it('is remembered per table, under one key for all users', async () => {
    const { table } = useTable({ name: 'disks' });
    table.isPaginated = true;
    await nextTick();

    expect(localStorage.getItem('phenix.paginate.disks')).toBe('true');
    expect(useTable({ name: 'disks' }).table.isPaginated).toBe(true);
    expect(useTable({ name: 'hosts' }).table.isPaginated).toBe(false);

    table.isPaginated = false;
    await nextTick();
    expect(useTable({ name: 'disks' }).table.isPaginated).toBe(false);
  });

  it('leaves unnamed tables unpaginated and unsaved', async () => {
    const { table } = useTable();
    expect(table.isPaginated).toBe(false);

    table.isPaginated = true;
    await nextTick();
    expect(localStorage.getItem('phenix.paginate.undefined')).toBeNull();
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

    expect(loadPaginate('disks')).toBe(false);
    expect(() => savePaginate('disks', true)).not.toThrow();
  });
});
