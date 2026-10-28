import type { ReactNode } from 'react';
import { cx } from '../cx';
import { useTicker } from '../motion';
import s from './Readout.module.css';

export interface ReadoutProps {
  label: ReactNode;
  /** A number ticks to its new value; a string is shown as is. */
  value: number | string | null | undefined;
  unit?: ReactNode;
  /** Decimal places for numbers (default: 0 for integers, else 1). */
  decimals?: number;
  /** Custom number formatting, applied to every intermediate tick. */
  format?: (n: number) => string;
  size?: 'sm' | 'md' | 'lg';
  tone?: 'default' | 'accent' | 'alert' | 'muted';
  /** A secondary line under the value (delta, limit, since). */
  sub?: ReactNode;
  className?: string;
}

/** Readout is a label above a monospace value; numeric values tick. */
export function Readout({ label, value, unit, decimals, format, size = 'md', tone = 'default', sub, className }: ReadoutProps) {
  return (
    <div className={cx(s.readout, s[size], className)} data-tone={tone}>
      <div className={s.label}>{label}</div>
      <div className={s.value}>
        {typeof value === 'number' ? (
          <TickingNumber value={value} decimals={decimals} format={format} />
        ) : (
          <span>{value == null || value === '' ? '—' : value}</span>
        )}
        {unit != null && <span className={s.unit}>{unit}</span>}
      </div>
      {sub != null && <div className={s.sub}>{sub}</div>}
    </div>
  );
}

const formatters = new Map<number, Intl.NumberFormat>();
function fmt(n: number, decimals: number): string {
  let f = formatters.get(decimals);
  if (!f) {
    f = new Intl.NumberFormat('en-US', { minimumFractionDigits: decimals, maximumFractionDigits: decimals });
    formatters.set(decimals, f);
  }
  return f.format(n);
}

export function TickingNumber({ value, decimals, format }: { value: number; decimals?: number; format?: (n: number) => string }) {
  const shown = useTicker(value);
  const d = decimals ?? (Number.isInteger(value) ? 0 : 1);
  const text = format ? format(shown) : fmt(d === 0 ? Math.round(shown) : shown, d);
  // Screen readers get the final value, not every intermediate tick.
  const final = format ? format(value) : fmt(value, d);
  return (
    <span aria-label={final}>
      <span aria-hidden>{text}</span>
    </span>
  );
}
