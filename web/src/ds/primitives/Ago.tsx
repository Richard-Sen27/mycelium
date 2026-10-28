import { ago, fmtClock, fmtElapsed, useNow } from '../time';

/** Ago is a <time> that ticks ("now", "2 m ago") from the shared clock; the title holds the exact time. */
export function Ago({ at, className }: { at: string; className?: string }) {
  const now = useNow();
  return (
    <time dateTime={at} title={at ? fmtClock(at, now) : undefined} className={className}>
      {ago(at, now)}
    </time>
  );
}

/** Elapsed ticks the time since `from` (until `to`, when set), e.g. a running job's duration. */
export function Elapsed({ from, to, className }: { from: string; to?: string; className?: string }) {
  const now = useNow();
  const a = Date.parse(from);
  const b = to ? Date.parse(to) : now;
  if (!Number.isFinite(a) || !Number.isFinite(b)) return <span className={className}>—</span>;
  return <span className={className}>{fmtElapsed(b - a)}</span>;
}
