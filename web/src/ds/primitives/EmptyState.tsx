import type { ReactNode } from 'react';
import { cx } from '../cx';
import s from './EmptyState.module.css';

export interface EmptyStateProps {
  title: ReactNode;
  children?: ReactNode;
  /** A button or link that resolves the empty state. */
  action?: ReactNode;
  className?: string;
}

/** EmptyState: a reticle and a short sentence; says why nothing is here and what would change that. */
export function EmptyState({ title, children, action, className }: EmptyStateProps) {
  return (
    <div className={cx(s.empty, className)}>
      <svg className={s.reticle} viewBox="0 0 40 40" aria-hidden>
        <circle cx="20" cy="20" r="11" />
        <path d="M20 2v8M20 30v8M2 20h8M30 20h8" />
        <circle className={s.dot} cx="20" cy="20" r="1.5" />
      </svg>
      <p className={s.title}>{title}</p>
      {children != null && <div className={s.body}>{children}</div>}
      {action != null && <div className={s.action}>{action}</div>}
    </div>
  );
}
