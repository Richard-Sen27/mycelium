import { fmtUntil } from '../format';
import { ago, fmtClock, fmtElapsed, parseTime, useNow } from '../time';

/** Ago is a <time> that ticks ("now", "2 m ago") from the shared clock; the title holds the exact time. */
export function Ago({ at, className, title }: { at: string; className?: string; title?: string }) {
  const now = useNow();
  return (
    <time dateTime={at} title={title ?? (at ? fmtClock(at, now) : undefined)} className={className}>
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

/**
 * Countdown ticks the time left until `to` ("4 m 10 s", then "expired");
 * below `warnUnderMs` it turns to the warning hue.
 */
export function Countdown({ to, warnUnderMs, className }: { to: string; warnUnderMs?: number; className?: string }) {
  const now = useNow();
  const t = parseTime(to);
  if (!Number.isFinite(t)) return <span className={className}>—</span>;
  const warn = warnUnderMs != null && t - now < warnUnderMs;
  return (
    <time dateTime={to} title={fmtClock(to, now)} className={className} style={warn ? { color: 'var(--warn)' } : undefined}>
      {fmtUntil(to, now)}
    </time>
  );
}
