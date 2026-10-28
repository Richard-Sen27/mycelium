import type { ReactNode } from 'react';
import { cx } from '../cx';
import s from './Badge.module.css';

/** Tone maps object states onto the theme's two hues and neutrals. */
export type Tone = 'ok' | 'busy' | 'idle' | 'off' | 'alert';

export interface BadgeProps {
  tone?: Tone;
  /** Pulse the marker: the state is live and needs attention (e.g. frozen). */
  pulse?: boolean;
  children: ReactNode;
  className?: string;
  title?: string;
}

/** Badge is a state marker plus a short monospace label. */
export function Badge({ tone = 'idle', pulse, children, className, title }: BadgeProps) {
  return (
    <span className={cx(s.badge, className)} data-tone={tone} data-pulse={pulse || undefined} title={title}>
      <span className={s.mark} aria-hidden />
      <span className={s.text}>{children}</span>
    </span>
  );
}
