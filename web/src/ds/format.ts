import { fmtDuration, parseTime } from './time';

// Readout formatting the time module does not cover: counts, money, GiB
// and countdowns. Pure functions, no API knowledge.

const GiB = 1024 ** 3;

/** gib converts bytes to GiB (for gauges and capacity maths). */
export function gib(n: number | bigint | null | undefined): number {
  return Number(n ?? 0) / GiB;
}

/** fmtCount: 950 → "950", 12300 → "12.3k", 4.2e6 → "4.2M". */
export function fmtCount(n: number | bigint | null | undefined): string {
  const v = Number(n ?? 0);
  if (!Number.isFinite(v)) return '—';
  const a = Math.abs(v);
  const trim = (s: string) => (s.includes('.') ? s.replace(/\.?0+$/, '') : s);
  if (a < 1000) return String(Math.round(v));
  if (a < 1e6) return `${trim((v / 1e3).toFixed(a < 1e4 ? 2 : 1))}k`;
  if (a < 1e9) return `${trim((v / 1e6).toFixed(a < 1e7 ? 2 : 1))}M`;
  return `${trim((v / 1e9).toFixed(1))}G`;
}

/** fmtUsd: 0.0042 → "$0.0042", 1.5 → "$1.50". */
export function fmtUsd(n: number | null | undefined): string {
  const v = n ?? 0;
  if (!Number.isFinite(v)) return '—';
  if (v !== 0 && Math.abs(v) < 0.01) return `$${v.toFixed(4)}`;
  return `$${v.toFixed(2)}`;
}

/** fmtUntil: time left until `at` ("4 m 10 s"), "expired" once passed, "—" when unset. */
export function fmtUntil(at: string | null | undefined, now: number): string {
  const t = parseTime(at);
  if (Number.isNaN(t)) return '—';
  const d = t - now;
  return d <= 0 ? 'expired' : fmtDuration(Math.max(1000, Math.floor(d / 1000) * 1000));
}
