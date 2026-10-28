import type { ReactNode } from 'react';
import { cx } from '../cx';
import s from './Gauge.module.css';

export interface GaugeProps {
  value: number;
  max?: number;
  /** Fraction (0..1) above which the bar switches to the warning hue. */
  warnAt?: number;
  label?: ReactNode;
  /** Right-hand readout, e.g. "3.2 / 8 GiB". Defaults to a percentage. */
  valueText?: ReactNode;
  /** Draw quarter ticks under the bar. */
  ticks?: boolean;
  className?: string;
}

/** Gauge is a thin horizontal bar for usage against a limit. */
export function Gauge({ value, max = 1, warnAt = 0.85, label, valueText, ticks = true, className }: GaugeProps) {
  const frac = max > 0 ? Math.max(0, Math.min(1, value / max)) : 0;
  const alert = frac >= warnAt;
  const pct = Math.round(frac * 100);
  return (
    <div className={cx(s.gauge, className)} data-alert={alert || undefined}>
      {(label != null || valueText !== null) && (
        <div className={s.head}>
          {label != null && <span className={s.label}>{label}</span>}
          <span className={s.value}>{valueText ?? `${pct}%`}</span>
        </div>
      )}
      <div
        className={s.track}
        role="meter"
        aria-valuemin={0}
        aria-valuemax={max}
        aria-valuenow={value}
        aria-valuetext={typeof valueText === 'string' ? valueText : `${pct}%`}
        aria-label={typeof label === 'string' ? label : undefined}
      >
        <div className={s.fill} style={{ transform: `scaleX(${frac})` }} />
        {ticks && (
          <div className={s.ticks} aria-hidden>
            <span />
            <span />
            <span />
          </div>
        )}
      </div>
    </div>
  );
}
