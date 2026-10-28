import { cx } from '../cx';
import s from './Skeleton.module.css';

export interface SkeletonProps {
  /** Number of placeholder rows. */
  rows?: number;
  className?: string;
  label?: string;
}

/** Skeleton marks data that is loading: dim rows with a scanning hairline. */
export function Skeleton({ rows = 3, className, label = 'Loading' }: SkeletonProps) {
  return (
    <div className={cx(s.skeleton, className)} role="status" aria-label={label}>
      {Array.from({ length: rows }, (_, i) => (
        <div key={i} className={s.row} style={{ width: `${92 - ((i * 17) % 40)}%` }} />
      ))}
    </div>
  );
}
