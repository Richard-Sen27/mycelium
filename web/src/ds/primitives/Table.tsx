import type { ReactNode } from 'react';
import { cx } from '../cx';
import type { Tone } from './Badge';
import { useRoving } from './roving';
import s from './Table.module.css';

export interface Column<T> {
  key: string;
  header: ReactNode;
  cell: (row: T) => ReactNode;
  /** CSS width, e.g. "120px" or "20%". */
  width?: string;
  align?: 'left' | 'right';
  /** Monospace cell (ids, numbers, times). */
  mono?: boolean;
  /** Hide this column on narrow screens. */
  hideOnNarrow?: boolean;
}

export interface TableProps<T> {
  columns: Column<T>[];
  rows: T[];
  rowKey: (row: T) => string;
  /** Enter, Space or click on a row. */
  onActivate?: (row: T) => void;
  selectedKey?: string | null;
  /** Marks a row with a state colour on its left edge. */
  rowTone?: (row: T) => Tone | undefined;
  empty?: ReactNode;
  'aria-label': string;
  className?: string;
}

/** Table renders dense data rows (table semantics) with roving keyboard focus on the rows. */
export function Table<T>({
  columns,
  rows,
  rowKey,
  onActivate,
  selectedKey,
  rowTone,
  empty,
  'aria-label': ariaLabel,
  className,
}: TableProps<T>) {
  const { itemProps } = useRoving(rows.length, onActivate ? (i) => onActivate(rows[i]) : undefined);
  if (rows.length === 0 && empty != null) return <>{empty}</>;
  return (
    <div className={cx(s.wrap, className)}>
      <table className={s.table} aria-label={ariaLabel}>
        <thead>
          <tr>
            {columns.map((c) => (
              <th
                key={c.key}
                scope="col"
                className={cx(c.hideOnNarrow && s.narrowHide)}
                data-align={c.align}
                style={c.width ? { width: c.width } : undefined}
              >
                {c.header}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {rows.map((r, i) => {
            const key = rowKey(r);
            const selected = selectedKey != null && key === selectedKey;
            return (
              <tr
                key={key}
                {...itemProps(i)}
                className={s.row}
                aria-selected={selectedKey != null ? selected : undefined}
                data-tone={rowTone?.(r)}
                onClick={onActivate ? () => onActivate(r) : undefined}
                style={i < 16 ? ({ '--i': i } as React.CSSProperties) : undefined}
              >
                {columns.map((c) => (
                  <td key={c.key} className={cx(c.mono && s.mono, c.hideOnNarrow && s.narrowHide)} data-align={c.align}>
                    {c.cell(r)}
                  </td>
                ))}
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}

export interface ListProps<T> {
  items: T[];
  itemKey: (item: T) => string;
  renderItem: (item: T, selected: boolean) => ReactNode;
  onActivate?: (item: T) => void;
  selectedKey?: string | null;
  empty?: ReactNode;
  'aria-label': string;
  className?: string;
}

/** List is a single-column variant of Table for stacked rows (role listbox). */
export function List<T>({ items, itemKey, renderItem, onActivate, selectedKey, empty, 'aria-label': ariaLabel, className }: ListProps<T>) {
  const { itemProps } = useRoving(items.length, onActivate ? (i) => onActivate(items[i]) : undefined);
  if (items.length === 0 && empty != null) return <>{empty}</>;
  return (
    <ul className={cx(s.list, className)} role="listbox" aria-label={ariaLabel}>
      {items.map((it, i) => {
        const key = itemKey(it);
        const selected = selectedKey != null && key === selectedKey;
        return (
          <li
            key={key}
            role="option"
            aria-selected={selected}
            {...itemProps(i)}
            className={s.item}
            onClick={onActivate ? () => onActivate(it) : undefined}
            style={i < 16 ? ({ '--i': i } as React.CSSProperties) : undefined}
          >
            {renderItem(it, selected)}
          </li>
        );
      })}
    </ul>
  );
}
