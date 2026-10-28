import { useId, useMemo, useRef, useState, type KeyboardEvent } from 'react';
import { Button, Kbd } from '../../ds';
import { getApi } from '../../app/api';
import { useEntityList } from '../../app/events/entities';
import { agentsStore } from '../../app/events/stores';
import type { Channel } from '../../gen/mycel/v1/channel_pb';
import type { Message } from '../../gen/mycel/v1/envelope_pb';
import type { Agent } from '../../gen/mycel/v1/hub_pb';
import { errText, parseSender } from './model';
import s from './Channels.module.css';

const byName = (a: Agent, b: Agent) => a.name.localeCompare(b.name);

/** The mention being typed before the caret ("@co" → "co"), or null. */
export function mentionQuery(text: string, caret: number): { query: string; start: number } | null {
  const m = /(^|\s)@([A-Za-z0-9_.\-]*)$/.exec(text.slice(0, caret));
  if (!m) return null;
  return { query: m[2], start: caret - m[2].length - 1 };
}

/** mentionsIn returns the ids of agents mentioned as @name or @name@node in the text. */
export function mentionsIn(text: string, agents: Agent[]): string[] {
  const out = new Set<string>();
  for (const m of text.matchAll(/(^|\s)@([A-Za-z0-9_.\-]+(?:@[A-Za-z0-9_.\-]+)?)/g)) {
    const tok = m[2];
    const hit =
      agents.find((a) => `${a.name}@${a.nodeName}` === tok) ?? agents.find((a) => a.name === tok && a.state !== 'stopped' && !a.exitedAt);
    if (hit) out.add(hit.id);
  }
  return [...out];
}

export interface ComposerProps {
  channel: Channel;
  /** Why posting is not possible here; the composer is disabled and says so. */
  blocked: string | null;
  replyTo: Message | null;
  onCancelReply: () => void;
  onPosted?: (m: Message | undefined) => void;
}

/** Composer: a human's post box with @mention completion, reply-to (which threads) and Enter to send. */
export function Composer({ channel, blocked, replyTo, onCancelReply, onPosted }: ComposerProps) {
  const { items: agents } = useEntityList(agentsStore, byName);
  const [text, setText] = useState('');
  const [caret, setCaret] = useState(0);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [active, setActive] = useState(0);
  const [dismissed, setDismissed] = useState<number | null>(null);
  const ref = useRef<HTMLTextAreaElement>(null);
  const listId = useId();

  const members = useMemo(() => new Set(channel.members.map((m) => m.id)), [channel.members]);
  const mq = mentionQuery(text, caret);
  const suggestions = useMemo(() => {
    if (!mq || dismissed === mq.start) return [];
    const q = mq.query.toLowerCase();
    return agents
      .filter((a) => a.state !== 'stopped' && !a.exitedAt && a.name.toLowerCase().startsWith(q))
      .sort((a, b) => Number(members.has(b.id)) - Number(members.has(a.id)))
      .slice(0, 6);
  }, [agents, mq?.query, mq?.start, dismissed, members]);

  const pick = (a: Agent) => {
    if (!mq) return;
    const dup = agents.filter((x) => x.name === a.name && x.state !== 'stopped').length > 1;
    const token = `@${dup ? `${a.name}@${a.nodeName}` : a.name} `;
    const next = text.slice(0, mq.start) + token + text.slice(caret);
    const pos = mq.start + token.length;
    setText(next);
    setCaret(pos);
    setActive(0);
    setTimeout(() => {
      ref.current?.focus();
      ref.current?.setSelectionRange(pos, pos);
    }, 0);
  };

  const send = async () => {
    const body = text.trim();
    if (!body || blocked || busy) return;
    setBusy(true);
    setError(null);
    try {
      const res = await getApi().rpc.channels.post({
        channel: channel.name,
        text: body,
        mentions: mentionsIn(body, agents),
        replyTo: replyTo?.id ?? '',
      });
      setText('');
      setCaret(0);
      onCancelReply();
      onPosted?.(res.message);
    } catch (err) {
      setError(errText(err));
    } finally {
      setBusy(false);
    }
  };

  const onKeyDown = (e: KeyboardEvent<HTMLTextAreaElement>) => {
    if (suggestions.length > 0) {
      if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
        e.preventDefault();
        const d = e.key === 'ArrowDown' ? 1 : -1;
        setActive((i) => (i + d + suggestions.length) % suggestions.length);
        return;
      }
      if (e.key === 'Enter' || e.key === 'Tab') {
        e.preventDefault();
        pick(suggestions[Math.min(active, suggestions.length - 1)]);
        return;
      }
      if (e.key === 'Escape') {
        e.preventDefault();
        setDismissed(mq?.start ?? null);
        return;
      }
    }
    if (e.key === 'Escape' && replyTo) {
      onCancelReply();
      return;
    }
    if (e.key === 'Enter' && !e.shiftKey && !e.nativeEvent.isComposing) {
      e.preventDefault();
      void send();
    }
  };

  if (blocked)
    return (
      <div className={s.composer} data-blocked>
        <textarea className={s.input} disabled aria-label={`Message ${channel.name}`} placeholder={blocked} rows={1} />
        <p className={s.blocked} role="note">
          {blocked}
        </p>
      </div>
    );

  const replySender = replyTo ? parseSender(replyTo.from) : null;
  return (
    <div className={s.composer}>
      {replyTo && (
        <div className={s.replying}>
          <span>
            Replying to <span className={s.mono}>{replySender!.name}</span>
            <span className={s.replyExcerpt}> {(replyTo.payload?.text ?? '').slice(0, 80)}</span>
          </span>
          <button type="button" className={s.action} onClick={onCancelReply} aria-label="Cancel reply">
            Cancel
          </button>
        </div>
      )}
      {suggestions.length > 0 && (
        <ul className={s.suggest} role="listbox" aria-label="Mention an agent" id={listId}>
          {suggestions.map((a, i) => (
            <li
              key={a.id}
              id={`${listId}-${i}`}
              role="option"
              aria-selected={i === active}
              className={s.suggestItem}
              onMouseDown={(e) => {
                e.preventDefault();
                pick(a);
              }}
            >
              <span className={s.mono}>@{a.name}</span>
              <span className={s.suggestMeta}>
                {a.nodeName} · {a.state}
                {members.has(a.id) ? ' · member' : ''}
              </span>
            </li>
          ))}
        </ul>
      )}
      <div className={s.inputRow}>
        <textarea
          ref={ref}
          className={s.input}
          role="combobox"
          aria-autocomplete="list"
          aria-expanded={suggestions.length > 0}
          aria-controls={suggestions.length > 0 ? listId : undefined}
          aria-activedescendant={suggestions.length > 0 ? `${listId}-${Math.min(active, suggestions.length - 1)}` : undefined}
          value={text}
          rows={Math.min(6, Math.max(1, text.split('\n').length))}
          placeholder={`Message ${channel.name}  ·  @ to mention`}
          aria-label={`Message ${channel.name}`}
          onChange={(e) => {
            setText(e.target.value);
            setCaret(e.target.selectionStart ?? e.target.value.length);
            setActive(0);
            setDismissed(null);
          }}
          onSelect={(e) => setCaret(e.currentTarget.selectionStart ?? 0)}
          onKeyDown={onKeyDown}
        />
        <Button variant="primary" busy={busy} disabled={!text.trim()} onClick={() => void send()}>
          Post
        </Button>
      </div>
      <div className={s.composerFoot}>
        {error ? (
          <span className={s.error} role="alert">
            {error}
          </span>
        ) : (
          <span>
            <Kbd keys={['Enter']} /> post · <Kbd keys={['Shift', 'Enter']} /> new line
          </span>
        )}
      </div>
    </div>
  );
}
