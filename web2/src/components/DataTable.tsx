import { useState, type ReactNode } from "react";
import clsx from "clsx";
import {
  flexRender,
  getCoreRowModel,
  getSortedRowModel,
  type ColumnDef,
  type SortingState,
  useReactTable,
} from "@tanstack/react-table";

// MIRROR of web/src/components/DataTable.tsx — the sortable/paginated table the
// org list pages share. The org rollups return the full scoped set, so client-
// side sort/paginate is honest here (no server round-trip). The controlled
// (manualSorting) mode is carried over verbatim so a future server-paginated
// surface (6c Sessions) can opt in without re-porting.

export type DataTableProps<T> = {
  data: T[];
  columns: ColumnDef<T, any>[];
  onRowClick?: (row: T) => void;
  emptyMessage?: ReactNode;
  // Minimum table width before horizontal scroll kicks in.
  minWidth?: number;
  initialSort?: SortingState;
  // Controlled server-side sorting. When BOTH `sorting` and `onSortingChange`
  // are provided, the table delegates ordering to the server (manualSorting)
  // and does NOT reorder rows client-side. When omitted, the table keeps its
  // built-in client-side sort over `data` (initialSort seed).
  sorting?: SortingState;
  onSortingChange?: (s: SortingState) => void;
  rowKey: (row: T) => string;
  // Render alternating row backgrounds.
  zebra?: boolean;
  // Apply sticky header positioning. Requires the parent wrapper to be a
  // scroll container with a fixed height; otherwise stickyness is a no-op.
  stickyHeader?: boolean;
  // Loading state — when true, an indeterminate stripe sweeps across the
  // header row and empty-state copy swaps to "Loading…" so a refetch never
  // reads as "no data".
  loading?: boolean;
};

export function DataTable<T>({
  data,
  columns,
  onRowClick,
  emptyMessage = "No data.",
  minWidth = 760,
  initialSort = [],
  sorting: controlledSorting,
  onSortingChange,
  rowKey,
  zebra,
  stickyHeader,
  loading,
}: DataTableProps<T>) {
  const [internalSorting, setInternalSorting] = useState<SortingState>(initialSort);
  // Controlled (server-side) when the parent supplies both the state and the
  // change handler; otherwise fall back to internal client-side sorting.
  const manual = controlledSorting !== undefined && onSortingChange !== undefined;
  const sorting = manual ? controlledSorting : internalSorting;

  const table = useReactTable({
    data,
    columns,
    state: { sorting },
    onSortingChange: manual
      ? (updater) =>
          onSortingChange(
            typeof updater === "function" ? updater(sorting) : updater,
          )
      : setInternalSorting,
    manualSorting: manual,
    getCoreRowModel: getCoreRowModel(),
    ...(manual ? {} : { getSortedRowModel: getSortedRowModel() }),
  });

  return (
    <div className="relative overflow-x-auto">
      {loading && (
        <span
          aria-hidden
          className="pointer-events-none absolute left-0 right-0 top-0 z-20 h-[2px] overflow-hidden"
        >
          <span className="block h-full w-1/3 animate-[datatable-stripe_1.1s_linear_infinite] bg-accent/70" />
        </span>
      )}
      <table className="w-full text-left text-[11.5px]" style={{ minWidth }}>
        <thead className="text-[10px] uppercase tracking-[0.06em] text-fg-3">
          {table.getHeaderGroups().map((hg) => (
            <tr key={hg.id} className="border-b border-line-2">
              {hg.headers.map((h) => {
                const canSort = h.column.getCanSort();
                const sorted = h.column.getIsSorted();
                return (
                  <th
                    key={h.id}
                    className={clsx(
                      "whitespace-nowrap bg-bg-2 px-2 py-2 font-medium",
                      stickyHeader && "sticky top-0 z-10",
                      canSort && "cursor-pointer select-none hover:text-fg-1",
                      h.column.columnDef.meta?.align === "right" && "text-right",
                    )}
                    onClick={canSort ? h.column.getToggleSortingHandler() : undefined}
                  >
                    <span className="inline-flex items-center gap-1">
                      {flexRender(h.column.columnDef.header, h.getContext())}
                      {canSort && (
                        <span className="text-fg-4">
                          {sorted === "asc" ? "↑" : sorted === "desc" ? "↓" : "·"}
                        </span>
                      )}
                    </span>
                  </th>
                );
              })}
            </tr>
          ))}
        </thead>
        <tbody>
          {table.getRowModel().rows.length === 0 ? (
            <tr>
              <td
                colSpan={columns.length}
                className="px-4 py-8 text-center text-[12px] text-fg-3"
              >
                {loading ? (
                  <span className="inline-flex items-center gap-2 text-fg-2">
                    <span
                      aria-hidden
                      className="inline-block h-3 w-3 animate-spin rounded-full border border-line-3 border-t-accent"
                    />
                    Loading…
                  </span>
                ) : (
                  emptyMessage
                )}
              </td>
            </tr>
          ) : (
            table.getRowModel().rows.map((r, i) => (
              <tr
                key={rowKey(r.original)}
                className={clsx(
                  "border-b border-line-1 transition-colors last:border-b-0",
                  zebra && i % 2 === 1 && "bg-bg-3/50",
                  onRowClick && "cursor-pointer hover:bg-bg-3",
                )}
                onClick={onRowClick ? () => onRowClick(r.original) : undefined}
              >
                {r.getVisibleCells().map((c) => (
                  <td
                    key={c.id}
                    className={clsx(
                      "px-2 py-1.5",
                      c.column.columnDef.meta?.align === "right" &&
                        "whitespace-nowrap text-right tabular-nums",
                      c.column.columnDef.meta?.mono && "font-mono text-fg-2",
                    )}
                  >
                    {flexRender(c.column.columnDef.cell, c.getContext())}
                  </td>
                ))}
              </tr>
            ))
          )}
        </tbody>
      </table>
    </div>
  );
}

// Extend TanStack ColumnMeta so column defs can declare align/mono.
declare module "@tanstack/react-table" {
  // eslint-disable-next-line @typescript-eslint/no-unused-vars
  interface ColumnMeta<TData extends unknown, TValue> {
    align?: "left" | "right";
    mono?: boolean;
  }
}

export function Pagination({
  page,
  limit,
  total,
  onPage,
  loading,
}: {
  page: number;
  limit: number;
  total: number;
  onPage: (p: number) => void;
  loading?: boolean;
}) {
  const maxPage = Math.max(1, Math.ceil(total / limit));
  const start = total === 0 ? 0 : (page - 1) * limit + 1;
  const end = Math.min(total, page * limit);
  return (
    <div className="flex items-center justify-between gap-3 pt-3 text-[11px] text-fg-3">
      <span>
        {start.toLocaleString()}–{end.toLocaleString()} of{" "}
        {total.toLocaleString()}
        {loading && <span className="ml-2 text-fg-4">loading…</span>}
      </span>
      <div className="flex items-center gap-1">
        <PagerBtn onClick={() => onPage(1)} disabled={page <= 1}>
          «
        </PagerBtn>
        <PagerBtn onClick={() => onPage(page - 1)} disabled={page <= 1}>
          ‹
        </PagerBtn>
        <span className="px-1 tabular-nums text-fg-2">
          page {page} / {maxPage}
        </span>
        <PagerBtn onClick={() => onPage(page + 1)} disabled={page >= maxPage}>
          ›
        </PagerBtn>
        <PagerBtn onClick={() => onPage(maxPage)} disabled={page >= maxPage}>
          »
        </PagerBtn>
      </div>
    </div>
  );
}

function PagerBtn({
  children,
  onClick,
  disabled,
}: {
  children: ReactNode;
  onClick: () => void;
  disabled?: boolean;
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      disabled={disabled}
      className="grid h-6 w-6 place-items-center rounded-1 border border-line-2 bg-bg-2 text-fg-2 transition-colors hover:bg-bg-3 hover:text-fg-0 disabled:cursor-not-allowed disabled:opacity-30"
    >
      {children}
    </button>
  );
}
