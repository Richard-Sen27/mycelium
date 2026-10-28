import type { ReactNode } from 'react';
import { cx } from '../cx';
import s from './Chips.module.css';

export interface ChipOption<V extends string = string> {
  value: V;
  label: ReactNode;
  /** A small count after the label. */
  count?: number;
}

export interface ChipsProps<V extends string = string> {
  options: ChipOption<V>[];
  /** The pressed values. */
  value: V[];
  onChange: (value: V[]) => void;
  /** Several may be pressed at once (default); otherwise pressing one releases the others. */
  multiple?: boolean;
  'aria-label': string;
  className?: string;
}

/** Chips is a row of toggle buttons for filters (aria-pressed): hairline, no fill until pressed. */
export function Chips<V extends string = string>({
  options,
  value,
  onChange,
  multiple = true,
  'aria-label': ariaLabel,
  className,
}: ChipsProps<V>) {
  const toggle = (v: V) => {
    const on = value.includes(v);
    if (multiple) onChange(on ? value.filter((x) => x !== v) : [...value, v]);
    else onChange(on ? [] : [v]);
  };
  return (
    <div className={cx(s.chips, className)} role="group" aria-label={ariaLabel}>
      {options.map((o) => {
        const on = value.includes(o.value);
        return (
          <button key={o.value} type="button" className={s.chip} aria-pressed={on} onClick={() => toggle(o.value)}>
            {o.label}
            {o.count != null && <span className={s.count}>{o.count}</span>}
          </button>
        );
      })}
    </div>
  );
}
