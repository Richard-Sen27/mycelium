import { useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react';
import { cx } from '../cx';
import s from './LogView.module.css';

/** Row height in px; must match .row in LogView.module.css. */
export const LOG_ROW = 20;
const OVERSCAN = 20;
const FALLBACK_HEIGHT = 480;

export interface LogViewProps {
  lines: string[];
  /** Keep the view pinned to the newest line. */
  follow: boolean;
  /** Called with false when the reader scrolls away from the bottom. */
  onFollowChange?: (follow: boolean) => void;
  /** Show only lines containing this text (case-insensitive), with the match marked. */
  query?: string;
  /** Number of the first line (when older lines were trimmed). Default 1. */
  firstLine?: number;
  className?: string;
  'aria-label'?: string;
}

const ALERT_RE = /\b(error|fatal|panic|fail(ed|ure)?|denied)\b/i;

/**
 * LogView is a virtualised, line-numbered log: only the visible rows (plus
 * an overscan) are in the DOM, so a job's whole output stays cheap to show.
 */
export function LogView({ lines, follow, onFollowChange, query, firstLine = 1, className, 'aria-label': ariaLabel = 'Log' }: LogViewProps) {
  const ref = useRef<HTMLDivElement>(null);
  const [scrollTop, setScrollTop] = useState(0);
  const [height, setHeight] = useState(FALLBACK_HEIGHT);
  const programmatic = useRef(false);

  const q = query?.trim().toLowerCase() ?? '';
  const rows = useMemo(() => {
    const all = lines.map((text, i) => ({ n: i + firstLine, text }));
    return q ? all.filter((r) => r.text.toLowerCase().includes(q)) : all;
  }, [lines, q, firstLine]);

  useEffect(() => {
    const el = ref.current;
    if (!el) return;
    const measure = () => setHeight(el.clientHeight || FALLBACK_HEIGHT);
    measure();
    if (typeof ResizeObserver !== 'function') return;
    const ro = new ResizeObserver(measure);
    ro.observe(el);
    return () => ro.disconnect();
  }, []);

  useLayoutEffect(() => {
    const el = ref.current;
    if (!el || !follow) return;
    const bottom = Math.max(0, rows.length * LOG_ROW - (el.clientHeight || height));
    if (Math.abs(el.scrollTop - bottom) > 1) {
      programmatic.current = true;
      el.scrollTop = bottom;
    }
    setScrollTop(bottom);
  }, [rows.length, follow, height]);

  const onScroll = () => {
    const el = ref.current;
    if (!el) return;
    setScrollTop(el.scrollTop);
    if (programmatic.current) {
      programmatic.current = false;
      return;
    }
    const atBottom = el.scrollTop + el.clientHeight >= el.scrollHeight - LOG_ROW;
    if (follow && !atBottom) onFollowChange?.(false);
  };

  const start = Math.max(0, Math.floor(scrollTop / LOG_ROW) - OVERSCAN);
  const end = Math.min(rows.length, Math.ceil((scrollTop + height) / LOG_ROW) + OVERSCAN);
  const gutter = String(lines.length + firstLine - 1).length;

  return (
    <div
      ref={ref}
      className={cx(s.view, className)}
      onScroll={onScroll}
      role="log"
      aria-label={ariaLabel}
      aria-live="off"
      tabIndex={0}
      style={{ '--gutter': `${Math.max(3, gutter)}ch` } as React.CSSProperties}
    >
      <div className={s.inner} style={{ height: rows.length * LOG_ROW }}>
        {rows.slice(start, end).map((r, i) => (
          <div
            key={r.n}
            className={s.row}
            data-alert={ALERT_RE.test(r.text) || undefined}
            style={{ transform: `translateY(${(start + i) * LOG_ROW}px)` }}
          >
            <span className={s.n} aria-hidden>
              {r.n}
            </span>
            <span className={s.text}>{q ? <Marked text={r.text} q={q} /> : r.text}</span>
          </div>
        ))}
      </div>
    </div>
  );
}

function Marked({ text, q }: { text: string; q: string }) {
  const parts: React.ReactNode[] = [];
  const lower = text.toLowerCase();
  let i = 0;
  let k = 0;
  while (i < text.length) {
    const j = lower.indexOf(q, i);
    if (j < 0) {
      parts.push(text.slice(i));
      break;
    }
    if (j > i) parts.push(text.slice(i, j));
    parts.push(<mark key={k++}>{text.slice(j, j + q.length)}</mark>);
    i = j + q.length;
  }
  return <>{parts}</>;
}

/**
 * splitLines appends decoded text to a line buffer: `carry` is the
 * unterminated tail of the previous chunk. Returns the complete lines and the
 * new tail.
 */
export function splitLines(carry: string, chunk: string): { lines: string[]; carry: string } {
  const parts = (carry + chunk).split(/\r?\n/);
  const tail = parts.pop() ?? '';
  return { lines: parts, carry: tail };
}
