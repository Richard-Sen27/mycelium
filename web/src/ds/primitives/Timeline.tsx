import { useEffect, useRef, type ReactNode } from 'react';
import { cx } from '../cx';
import s from './Timeline.module.css';

export interface TimelineStep {
  key: string;
  /** Left column: the time of the step. */
  time: ReactNode;
  /** The latency since the previous step, shown on the rail between them. */
  gap?: ReactNode;
  /** Who acted. */
  actor: ReactNode;
  /** What happened (one line). */
  title: ReactNode;
  /** Optional detail under the title. */
  body?: ReactNode;
  /** "alert" for denied or failed steps; "muted" for bookkeeping. */
  tone?: 'default' | 'alert' | 'muted';
  /** Small marker shape: a message (filled) or an action (hollow). */
  kind?: 'message' | 'action';
}

export interface TimelineProps {
  steps: TimelineStep[];
  /** Replay position: steps after it are dimmed, the current one is marked. -1 or undefined shows everything. */
  current?: number;
  onSelect?: (index: number) => void;
  className?: string;
  'aria-label'?: string;
}

/** Timeline is a vertical rail of steps with the time on the left and the latency between steps on the rail. */
export function Timeline({ steps, current, onSelect, className, 'aria-label': ariaLabel = 'Timeline' }: TimelineProps) {
  const replay = current != null && current >= 0;
  const currentRef = useRef<HTMLLIElement>(null);
  useEffect(() => {
    if (replay) currentRef.current?.scrollIntoView?.({ block: 'nearest' });
  }, [current, replay]);
  return (
    <ol className={cx(s.timeline, className)} aria-label={ariaLabel}>
      {steps.map((st, i) => {
        const state = !replay ? undefined : i < current! ? 'past' : i === current ? 'current' : 'future';
        return (
          <li
            key={st.key}
            ref={i === current ? currentRef : undefined}
            className={s.step}
            data-tone={st.tone ?? 'default'}
            data-state={state}
            data-kind={st.kind ?? 'action'}
            aria-current={state === 'current' ? 'step' : undefined}
          >
            {st.gap != null && i > 0 && <div className={s.gap}>{st.gap}</div>}
            <div className={s.row}>
              <div className={s.time}>{st.time}</div>
              <div className={s.rail} aria-hidden>
                <span className={s.mark} />
              </div>
              <div className={s.content}>
                {onSelect ? (
                  <button type="button" className={s.head} onClick={() => onSelect(i)}>
                    <span className={s.actor}>{st.actor}</span>
                    <span className={s.title}>{st.title}</span>
                  </button>
                ) : (
                  <div className={s.head}>
                    <span className={s.actor}>{st.actor}</span>
                    <span className={s.title}>{st.title}</span>
                  </div>
                )}
                {st.body != null && <div className={s.body}>{st.body}</div>}
              </div>
            </div>
          </li>
        );
      })}
    </ol>
  );
}
