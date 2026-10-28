import { cx } from '../cx';
import s from './MessageBody.module.css';

export type BodyToken = { kind: 'text' | 'mention' | 'link'; text: string };

// URLs and @mentions are found with plain regular expressions over the raw
// text; everything is rendered as React text nodes, never as markup.
const TOKEN_RE = /(https?:\/\/[^\s<>"']+[^\s<>"'.,;:!?)\]])|(@[A-Za-z0-9_][A-Za-z0-9_.:@\-]*[A-Za-z0-9_])|(@[A-Za-z0-9_])/g;

/** tokenizeBody splits a message body into text, mention and link tokens. */
export function tokenizeBody(text: string): BodyToken[] {
  const out: BodyToken[] = [];
  let last = 0;
  for (const m of text.matchAll(TOKEN_RE)) {
    const i = m.index ?? 0;
    // A mention must start a word ("a@b" inside an e-mail address is text).
    if (m[0].startsWith('@') && i > 0 && /[\w.]/.test(text[i - 1])) continue;
    if (i > last) out.push({ kind: 'text', text: text.slice(last, i) });
    out.push({ kind: m[1] ? 'link' : 'mention', text: m[0] });
    last = i + m[0].length;
  }
  if (last < text.length) out.push({ kind: 'text', text: text.slice(last) });
  return out;
}

export interface MessageBodyProps {
  text: string;
  /** Agent-produced content: rendered as quoted data with a marker, never as instructions or markup. */
  untrusted?: boolean;
  /** Mentions that address the viewer get the accent. */
  isSelf?: (mention: string) => boolean;
  className?: string;
  /** Clamp to a number of lines (previews). */
  clamp?: number;
}

/**
 * MessageBody renders message text safely: plain text with whitespace kept,
 * mentions and links highlighted but not clickable (a link never navigates
 * to another origin from here; its target is in the title to copy).
 */
export function MessageBody({ text, untrusted, isSelf, className, clamp }: MessageBodyProps) {
  const tokens = tokenizeBody(text);
  const body = tokens.map((t, i) => {
    if (t.kind === 'mention')
      return (
        <span key={i} className={s.mention} data-self={isSelf?.(t.text.slice(1)) || undefined}>
          {t.text}
        </span>
      );
    if (t.kind === 'link')
      return (
        <span key={i} className={s.link} title={`${t.text} (not opened from here)`}>
          {t.text}
        </span>
      );
    return t.text;
  });
  const style = clamp ? ({ '--clamp': clamp } as React.CSSProperties) : undefined;
  if (untrusted)
    return (
      <blockquote className={cx(s.body, s.quoted, !!clamp && s.clamp, className)} data-untrusted style={style}>
        {body}
      </blockquote>
    );
  return (
    <div className={cx(s.body, !!clamp && s.clamp, className)} style={style}>
      {body}
    </div>
  );
}
