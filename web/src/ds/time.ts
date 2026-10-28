import { useSyncExternalStore } from 'react';

// Time and size formatting for readouts, plus one shared clock so every
// "2 m ago" on screen ticks from the same interval instead of one each.

const listeners = new Set<() => void>();
let now = Date.now();
let timer: ReturnType<typeof setInterval> | null = null;

function subscribe(fn: () => void): () => void {
  listeners.add(fn);
  if (!timer) {
    now = Date.now();
    timer = setInterval(() => {
      now = Date.now();
      for (const l of listeners) l();
    }, 1000);
  }
  return () => {
    listeners.delete(fn);
    if (listeners.size === 0 && timer) {
      clearInterval(timer);
      timer = null;
    }
  };
}

const snapshot = () => now;

/** useNow returns the shared clock (ms since the epoch), updated once a second. */
export function useNow(): number {
  return useSyncExternalStore(subscribe, snapshot, snapshot);
}

/** parseTime reads an RFC 3339 string; NaN when empty or unreadable. */
export function parseTime(t: string | undefined | null): number {
  if (!t) return NaN;
  return Date.parse(t);
}

/** ago formats the distance from `t` to `nowMs` as "now", "12 s ago", "2 m ago", "3 h ago", "4 d ago". */
export function ago(t: string | number, nowMs: number): string {
  const ms = typeof t === 'number' ? t : parseTime(t);
  if (!Number.isFinite(ms)) return '—';
  const d = Math.max(0, nowMs - ms) / 1000;
  if (d < 5) return 'now';
  if (d < 60) return `${Math.floor(d)} s ago`;
  if (d < 3600) return `${Math.floor(d / 60)} m ago`;
  if (d < 86400) return `${Math.floor(d / 3600)} h ago`;
  return `${Math.floor(d / 86400)} d ago`;
}

const pad = (n: number) => String(n).padStart(2, '0');

/** fmtDuration formats milliseconds compactly: "840 ms", "42 s", "3 m 12 s", "1 h 04 m", "2 d 03 h". */
export function fmtDuration(ms: number): string {
  if (!Number.isFinite(ms) || ms < 0) return '—';
  if (ms < 1000) return `${Math.round(ms)} ms`;
  const s = Math.floor(ms / 1000);
  if (s < 60) return `${s} s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m} m ${pad(s % 60)} s`;
  const h = Math.floor(m / 60);
  if (h < 48) return `${h} h ${pad(m % 60)} m`;
  return `${Math.floor(h / 24)} d ${pad(h % 24)} h`;
}

/** fmtElapsed formats a running clock: "04:12" or "1:04:12". */
export function fmtElapsed(ms: number): string {
  const s = Math.max(0, Math.floor(ms / 1000));
  const h = Math.floor(s / 3600);
  const m = Math.floor((s % 3600) / 60);
  return h > 0 ? `${h}:${pad(m)}:${pad(s % 60)}` : `${pad(m)}:${pad(s % 60)}`;
}

const GO_UNITS: Record<string, number> = { ns: 1e-6, us: 1e-3, µs: 1e-3, ms: 1, s: 1000, m: 60_000, h: 3_600_000 };

/** parseGoDuration reads a Go duration ("1h30m", "45s", "2h0m0s") into milliseconds; NaN when unreadable. */
export function parseGoDuration(d: string | undefined | null): number {
  if (!d) return NaN;
  const re = /(\d+(?:\.\d+)?)(ns|us|µs|ms|s|m|h)/g;
  let total = 0;
  let consumed = 0;
  let m: RegExpExecArray | null;
  while ((m = re.exec(d)) !== null) {
    total += parseFloat(m[1]) * GO_UNITS[m[2]];
    consumed += m[0].length;
  }
  return consumed === d.length && consumed > 0 ? total : NaN;
}

/** goDuration writes milliseconds as a Go duration ("4h", "1h30m", "45s"). */
export function goDuration(ms: number): string {
  if (!Number.isFinite(ms) || ms <= 0) return '0s';
  let s = Math.round(ms / 1000);
  const h = Math.floor(s / 3600);
  s -= h * 3600;
  const m = Math.floor(s / 60);
  s -= m * 60;
  return `${h ? `${h}h` : ''}${m ? `${m}m` : ''}${s ? `${s}s` : ''}` || '0s';
}

/** fmtBytes formats a byte count with binary units ("512 B", "3.2 MiB"). */
export function fmtBytes(n: number | bigint): string {
  const v = Number(n);
  if (!Number.isFinite(v) || v < 0) return '—';
  if (v < 1024) return `${v} B`;
  const units = ['KiB', 'MiB', 'GiB', 'TiB'];
  let x = v / 1024;
  let i = 0;
  while (x >= 1024 && i < units.length - 1) {
    x /= 1024;
    i++;
  }
  return `${x < 10 ? x.toFixed(1) : Math.round(x)} ${units[i]}`;
}

/** fmtClock formats an RFC 3339 time as local HH:MM:SS (with the date when not today). */
export function fmtClock(t: string, nowMs = Date.now()): string {
  const ms = parseTime(t);
  if (!Number.isFinite(ms)) return '—';
  const d = new Date(ms);
  const time = `${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`;
  const today = new Date(nowMs);
  if (d.toDateString() === today.toDateString()) return time;
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${time}`;
}
