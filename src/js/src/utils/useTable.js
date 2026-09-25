import { reactive } from 'vue';

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
// Pagination starts off on every visit; the toggle is not remembered.
export function useTable(options = {}) {
  const table = reactive({
    isPaginated: false,
    perPage: options.perPage ?? 10,
    currentPage: 1,
    isPaginationSimple: true,
    paginationSize: 'is-small',
    defaultSortDirection: options.defaultSortDirection ?? 'asc',
  });

  return { table };
}
