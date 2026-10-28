import { Link } from 'react-router';
import { Ago, cx, MessageBody } from '../../ds';
import type { Message } from '../../gen/mycel/v1/envelope_pb';
import { ArtifactLink } from '../jobs/artifacts';
import { grantIdIn, isUntrusted, MESSAGE_TYPE, parseSender, typeClass } from './model';
import s from './Channels.module.css';

export interface MessageItemProps {
  m: Message;
  /** The viewer's ids, to mark mentions of them and their own messages. */
  selfIds: string[];
  onReply?: (m: Message) => void;
  /** Reply rows are denser and drop the type tag when it is chat. */
  reply?: boolean;
  /** Arrived while the channel was open: animate its arrival. */
  live?: boolean;
}

/** MessageItem: provenance line, body (quoted plain text when untrusted), parts and actions. */
export function MessageItem({ m, selfIds, onReply, reply, live }: MessageItemProps) {
  const from = parseSender(m.from);
  const prov = m.meta?.provenance;
  const node = prov?.node || from.node;
  const tc = typeClass(m.type);
  const untrusted = isUntrusted(m);
  const text = m.payload?.text ?? '';
  const grant = tc === 'approval' ? grantIdIn(text) : null;
  const self = selfIds.includes(m.from);
  const trace = m.meta?.traceId;
  const isSelf = (mention: string) => selfIds.some((id) => id === mention || id.endsWith(`:${mention}`) || id.includes(`:${mention}@`));
  return (
    <article
      className={cx(s.msg, reply && s.reply)}
      data-type={tc}
      data-live={live || undefined}
      data-origin={from.kind}
      data-self={self || undefined}
      id={`m-${m.id}`}
    >
      <header className={s.prov}>
        <span className={s.sender} data-kind={from.kind}>
          <span className={s.senderMark} aria-hidden />
          {from.kind === 'user' && self ? 'you' : from.name}
        </span>
        {node && <span className={s.node}>@{node}</span>}
        {tc !== 'chat' && <span className={s.typeTag}>{MESSAGE_TYPE[m.type]}</span>}
        {untrusted && from.kind === 'agent' && (
          <span className={s.untrusted} title="Agent output: shown as quoted data">
            untrusted
          </span>
        )}
        <span className={s.spacer} />
        {(m.meta?.hop ?? 0) > 0 && (
          <span className={s.hop} title="Reply chain length / channel limit">
            hop {m.meta!.hop}
            {m.meta!.maxHops ? `/${m.meta!.maxHops}` : ''}
          </span>
        )}
        {trace && (
          <Link className={s.trace} to={`/traces?trace=${encodeURIComponent(trace)}`} title={`Replay trace ${trace}`}>
            trace
          </Link>
        )}
        <Ago at={m.ts} className={s.time} />
      </header>
      {tc === 'approval' ? (
        <div className={s.card}>
          <MessageBody text={text} untrusted={untrusted} isSelf={isSelf} />
          {grant && (
            <Link className={s.cardLink} to={`/approvals?grant=${encodeURIComponent(grant)}`}>
              Open {grant} in approvals
            </Link>
          )}
        </div>
      ) : (
        text !== '' && <MessageBody text={text} untrusted={untrusted} isSelf={isSelf} />
      )}
      {(m.payload?.parts.length ?? 0) > 0 && (
        <ul className={s.parts}>
          {m.payload!.parts.map((p, i) => (
            <li key={i} className={s.part}>
              <span className={s.partKind}>{p.kind}</span>
              {p.kind === 'artifact' ? (
                <ArtifactLink id={p.ref} name={p.name || p.ref} />
              ) : (
                <span className={s.partRef} title={p.kind === 'link' ? `${p.ref} (not opened from here)` : undefined}>
                  {p.name || p.ref}
                </span>
              )}
            </li>
          ))}
        </ul>
      )}
      {onReply && (
        <div className={s.actions}>
          <button type="button" className={s.action} onClick={() => onReply(m)}>
            Reply
          </button>
        </div>
      )}
    </article>
  );
}
