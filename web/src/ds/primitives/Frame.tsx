import { useId, type ReactNode } from 'react';
import { cx } from '../cx';
import s from './Frame.module.css';

export interface FrameProps {
  /** Title readout in the header (rendered uppercase, monospace). */
  title?: ReactNode;
  /** A short index shown before the title, e.g. "01" or a count. */
  index?: ReactNode;
  /** Right-aligned header content: counters, actions. */
  meta?: ReactNode;
  /** "active" brightens the border and ticks; "alert" uses the warning hue. */
  state?: 'idle' | 'active' | 'alert';
  /** No body padding (for tables and lists that run edge to edge). */
  flush?: boolean;
  className?: string;
  bodyClassName?: string;
  children?: ReactNode;
  as?: 'section' | 'div' | 'aside';
  'aria-label'?: string;
}

/** Frame is the hairline-bordered panel every view is built from. */
export function Frame({
  title,
  index,
  meta,
  state = 'idle',
  flush,
  className,
  bodyClassName,
  children,
  as: Tag = 'section',
  'aria-label': ariaLabel,
}: FrameProps) {
  const titleId = useId();
  const hasHeader = title != null || meta != null || index != null;
  return (
    <Tag
      className={cx(s.frame, className)}
      data-state={state}
      aria-labelledby={title != null && !ariaLabel ? titleId : undefined}
      aria-label={ariaLabel}
    >
      <span aria-hidden className={cx(s.tick, s.tl)} />
      <span aria-hidden className={cx(s.tick, s.tr)} />
      <span aria-hidden className={cx(s.tick, s.bl)} />
      <span aria-hidden className={cx(s.tick, s.br)} />
      {hasHeader && (
        <header className={s.header}>
          {index != null && <span className={s.index}>{index}</span>}
          {title != null && (
            <h2 id={titleId} className={s.title}>
              {title}
            </h2>
          )}
          {meta != null && <div className={s.meta}>{meta}</div>}
        </header>
      )}
      <div className={cx(s.body, flush && s.flush, bodyClassName)}>{children}</div>
    </Tag>
  );
}
