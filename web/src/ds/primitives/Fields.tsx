import type { ReactNode } from 'react';
import { cx } from '../cx';
import s from './Fields.module.css';

export interface FieldItem {
  label: ReactNode;
  value: ReactNode;
  /** Monospace value (ids, numbers, paths). */
  mono?: boolean;
  /** Skip the row (keeps call sites free of conditionals). */
  hidden?: boolean;
}

export interface FieldsProps {
  items: FieldItem[];
  className?: string;
  'aria-label'?: string;
}

/** Fields is a dense label/value list (a definition list) for inspectors. */
export function Fields({ items, className, 'aria-label': ariaLabel }: FieldsProps) {
  return (
    <dl className={cx(s.fields, className)} aria-label={ariaLabel}>
      {items
        .filter((it) => !it.hidden)
        .map((it, i) => (
          <div key={i} className={s.row}>
            <dt className={s.label}>{it.label}</dt>
            <dd className={cx(s.value, it.mono && s.mono)}>
              {it.value == null || it.value === '' ? <span className={s.none}>—</span> : it.value}
            </dd>
          </div>
        ))}
    </dl>
  );
}
