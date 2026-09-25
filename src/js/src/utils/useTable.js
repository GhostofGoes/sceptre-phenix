import { reactive, watch } from 'vue';
import { loadPaginate, savePaginate } from '@/utils/paginatePref.js';

// useTable centralizes the Buefy <b-table> pagination state that every table
// view in phenix duplicated.
//
// It is a Composition API composable, but integrates with the Options API
// components in this project: values returned from setup() are exposed on the
// component instance, so `this.table` is available in methods, computed and
// template.
//
// Each call returns an independent table, so views with more than one table
// (e.g. a VMs table and a files table) can call useTable() once per table.
//
// The Paginate toggle is remembered per browser (see paginatePref.js); pass
// { persist: false } for a table without a toggle, which stays unpaginated.
export function useTable(options = {}) {
  const persist = options.persist ?? true;
  const table = reactive({
    isPaginated: persist ? loadPaginate() : false,
    perPage: options.perPage ?? 10,
    currentPage: 1,
    isPaginationSimple: true,
    paginationSize: 'is-small',
    defaultSortDirection: options.defaultSortDirection ?? 'asc',
  });

  if (persist) {
    watch(() => table.isPaginated, savePaginate);
  }

  return { table };
}
