import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react';
import { Link } from 'react-router';
import { Ago, Badge, Button, ConfirmButton, EmptyState, Frame, Input, Skeleton, toast } from '../../ds';
import { getApi } from '../../app/api';
import { useEntity } from '../../app/events/entities';
import { channelsStore, useFreeze, useMessages } from '../../app/events/stores';
import { useInspector } from '../../app/shell/inspector';
import { useRights } from '../../app/session';
import type { Message } from '../../gen/mycel/v1/envelope_pb';
import { BudgetGauge, hasBudget } from './Budget';
import { Composer } from './Composer';
import { MessageItem } from './MessageItem';
import {
  CHANNEL_KIND,
  canManageChannel,
  errText,
  groupThreads,
  isChannelFrozen,
  markRead,
  mergeMessages,
  parseSender,
  postBlockedReason,
  type ThreadGroup,
} from './model';
import { BlackboardPanel, MembersPanel, PolicyPanel } from './Panels';
import { useChannelUsage } from './usage';
import { archiveChannel, freezeChannel, unfreezeChannel } from './actions';
import s from './Channels.module.css';

const PAGE = 100;
const EMPTY: Message[] = [];

/** Chat is one channel: history (older on scroll-up), live messages, threads, composer and the channel's actions. */
export function Chat({ name }: { name: string }) {
  const rights = useRights();
  const ch = useEntity(channelsStore, name);
  const chLoaded = channelsStore.use((st) => st.loaded);
  const freeze = useFreeze((st) => st.status);
  const live = useMessages((st) => st.byChannel[name]) ?? EMPTY;
  const [older, setOlder] = useState<Message[]>([]);
  const [loading, setLoading] = useState(true);
  const [loadingOlder, setLoadingOlder] = useState(false);
  const [hasMore, setHasMore] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [replyTo, setReplyTo] = useState<Message | null>(null);
  const [freezeReason, setFreezeReason] = useState('');
  const scroller = useRef<HTMLDivElement>(null);
  const stick = useRef(true);
  const anchor = useRef<{ height: number; top: number } | null>(null);
  const usage = useChannelUsage(name, hasBudget(ch?.policy?.budget));

  useEffect(() => {
    if (!useFreeze.getState().loaded) void useFreeze.getState().load();
  }, []);

  // History: the newest page, merged with whatever arrived live meanwhile.
  useEffect(() => {
    let cancelled = false;
    liveFrom.current = null;
    setOlder([]);
    setLoading(true);
    setError(null);
    setReplyTo(null);
    stick.current = true;
    getApi()
      .rpc.channels.listMessages({ channel: name, limit: PAGE })
      .then((r) => {
        if (cancelled) return;
        const cur = useMessages.getState().byChannel[name] ?? [];
        const merged = mergeMessages(r.messages, cur);
        // Everything up to here is history: no arrival animation, no announcement.
        liveFrom.current = merged.length ? merged[merged.length - 1].seq : 0n;
        useMessages.getState().setChannel(name, merged);
        setHasMore(r.messages.length >= PAGE);
      })
      .catch((err: unknown) => !cancelled && setError(errText(err)))
      .finally(() => !cancelled && setLoading(false));
    return () => {
      cancelled = true;
    };
  }, [name]);

  const all = useMemo(() => mergeMessages(older, live), [older, live]);
  const groups = useMemo(() => groupThreads(all), [all]);
  // Messages after liveFrom arrived while the channel was open.
  const liveFrom = useRef<bigint | null>(null);
  const isLive = useCallback((m: Message) => liveFrom.current !== null && m.seq > liveFrom.current, []);
  const newest = all.length ? all[all.length - 1] : null;
  const announce = newest && isLive(newest) ? `${parseSender(newest.from).name}: ${(newest.payload?.text ?? '').slice(0, 200)}` : '';
  const selfIds = useMemo(() => (rights.userId ? [`user:${rights.userId}`] : []), [rights.userId]);

  const loadOlder = useCallback(async () => {
    if (loadingOlder || !hasMore || all.length === 0) return;
    setLoadingOlder(true);
    const el = scroller.current;
    if (el) anchor.current = { height: el.scrollHeight, top: el.scrollTop };
    try {
      const r = await getApi().rpc.channels.listMessages({ channel: name, beforeSeq: all[0].seq, limit: PAGE });
      setOlder((prev) => mergeMessages(r.messages, prev));
      setHasMore(r.messages.length >= PAGE);
    } catch (err) {
      toast({ title: 'Could not load older messages', body: errText(err), tone: 'alert' });
      anchor.current = null;
    } finally {
      setLoadingOlder(false);
    }
  }, [loadingOlder, hasMore, all, name]);

  // Keep the reader's place: pinned to the bottom for new messages, and
  // steady when older ones are prepended.
  useLayoutEffect(() => {
    const el = scroller.current;
    if (!el) return;
    if (anchor.current) {
      el.scrollTop = anchor.current.top + (el.scrollHeight - anchor.current.height);
      anchor.current = null;
    } else if (stick.current) {
      el.scrollTop = el.scrollHeight;
    }
  }, [groups]);

  const last = all.length ? all[all.length - 1].seq : 0n;
  useEffect(() => {
    if (stick.current && last > 0n) markRead(name, last);
  }, [name, last]);

  const onScroll = () => {
    const el = scroller.current;
    if (!el) return;
    stick.current = el.scrollHeight - el.scrollTop - el.clientHeight < 48;
    if (stick.current && last > 0n) markRead(name, last);
    if (el.scrollTop < 80) void loadOlder();
  };

  if (!ch) {
    return (
      <Frame title={name} className={s.chatFrame}>
        {chLoaded ? (
          <EmptyState title="No such channel">It may have been removed, or you are not a member.</EmptyState>
        ) : (
          <Skeleton rows={6} />
        )}
      </Frame>
    );
  }

  const frozen = isChannelFrozen(name, freeze);
  const blocked = postBlockedReason(rights, ch, freeze);
  const manage = canManageChannel(rights, ch);
  const chFrozen = !!freeze?.frozenChannels.includes(name);

  return (
    <Frame
      className={s.chatFrame}
      bodyClassName={s.chatBody}
      flush
      state={frozen ? 'alert' : 'idle'}
      title={name}
      index={CHANNEL_KIND[ch.type]}
      aria-label={`Channel ${name}`}
      meta={
        <>
          {ch.archivedAt && <Badge tone="off">archived</Badge>}
          {frozen && (
            <Badge tone="alert" pulse>
              frozen
            </Badge>
          )}
          <span>{ch.members.length} members</span>
        </>
      }
    >
      <div className={s.chatTools}>
        <Link to="/channels" className={s.back}>
          ← Channels
        </Link>
        {hasBudget(ch.policy?.budget) && (
          <div className={s.chatBudget}>
            <BudgetGauge label="Budget" budget={ch.policy?.budget} used={usage?.used} compact />
          </div>
        )}
        <span className={s.spacer} />
        <PanelButton keyName={`members:${name}`} title={`${name} · members`} label="Members">
          <MembersPanel channel={name} />
        </PanelButton>
        <PanelButton keyName={`bb:${name}`} title={`${name} · blackboard`} label="Blackboard">
          <BlackboardPanel channel={name} />
        </PanelButton>
        {manage && (
          <PanelButton keyName={`policy:${name}`} title={`${name} · policy`} label="Policy">
            <PolicyPanel channel={name} />
          </PanelButton>
        )}
        {manage && !ch.archivedAt && (
          <>
            {chFrozen ? (
              <ConfirmButton
                size="sm"
                confirmTitle={`Unfreeze ${name}?`}
                confirmBody="Posts are accepted again and agents whose home channel this is resume (an operator pause stays)."
                action="Unfreeze"
                describeError={errText}
                onConfirm={() => unfreezeChannel(name)}
              >
                Unfreeze
              </ConfirmButton>
            ) : (
              <ConfirmButton
                size="sm"
                variant="warn"
                tone="alert"
                confirmTitle={`Freeze ${name}?`}
                confirmBody="Every agent whose home channel this is pauses and posts are refused until the channel is unfrozen."
                action="Freeze channel"
                extra={
                  <Input
                    label="Reason (optional)"
                    value={freezeReason}
                    onChange={(e) => setFreezeReason(e.target.value)}
                    placeholder="what went wrong"
                  />
                }
                describeError={errText}
                onConfirm={async () => {
                  await freezeChannel(name, freezeReason);
                  setFreezeReason('');
                }}
              >
                Freeze
              </ConfirmButton>
            )}
            <ConfirmButton
              size="sm"
              variant="ghost"
              tone="alert"
              confirmTitle={`Archive ${name}?`}
              confirmBody="The channel becomes read-only and leaves the active list. Its history and blackboard stay."
              action="Archive"
              describeError={errText}
              onConfirm={() => archiveChannel(name)}
            >
              Archive
            </ConfirmButton>
          </>
        )}
      </div>
      <div className={s.scroller} ref={scroller} onScroll={onScroll} role="log" aria-label={`Messages in ${name}`} aria-live="off">
        {hasMore && (
          <div className={s.older}>
            <Button size="sm" variant="ghost" busy={loadingOlder} onClick={() => void loadOlder()}>
              Load older
            </Button>
          </div>
        )}
        {loading && all.length === 0 ? (
          <Skeleton rows={6} />
        ) : error && all.length === 0 ? (
          <EmptyState title="Could not load the history">{error}</EmptyState>
        ) : groups.length === 0 ? (
          <EmptyState title="No messages yet">Messages posted here appear at the bottom as they arrive.</EmptyState>
        ) : (
          <ol className={s.messages}>
            {groups.map((g) => (
              <li key={g.key}>
                <ThreadBlock g={g} selfIds={selfIds} isLive={isLive} onReply={blocked ? undefined : setReplyTo} />
              </li>
            ))}
          </ol>
        )}
      </div>
      <div className={s.visuallyHidden} aria-live="polite" role="status">
        {announce}
      </div>
      <Composer channel={ch} blocked={blocked} replyTo={replyTo} onCancelReply={() => setReplyTo(null)} />
    </Frame>
  );
}

function ThreadBlock({
  g,
  selfIds,
  isLive,
  onReply,
}: {
  g: ThreadGroup;
  selfIds: string[];
  isLive: (m: Message) => boolean;
  onReply?: (m: Message) => void;
}) {
  const [open, setOpen] = useState(g.replies.length <= 3);
  const n = g.replies.length;
  const lastReply = n ? g.replies[n - 1] : null;
  return (
    <div className={s.thread}>
      {g.partial && <div className={s.partial}>in a thread that started earlier</div>}
      <MessageItem m={g.root} selfIds={selfIds} onReply={onReply} live={isLive(g.root)} />
      {n > 0 && (
        <div className={s.replies}>
          <button type="button" className={s.threadToggle} onClick={() => setOpen(!open)} aria-expanded={open}>
            <span className={s.chev} data-open={open || undefined} aria-hidden />
            {n} {n === 1 ? 'reply' : 'replies'}
            {lastReply && (
              <span className={s.threadMeta}>
                {' '}
                · last <Ago at={lastReply.ts} />
              </span>
            )}
          </button>
          {open && (
            <ol className={s.replyList}>
              {g.replies.map((r) => (
                <li key={r.id}>
                  <MessageItem m={r} selfIds={selfIds} onReply={onReply} reply live={isLive(r)} />
                </li>
              ))}
            </ol>
          )}
        </div>
      )}
    </div>
  );
}

function PanelButton({ keyName, title, label, children }: { keyName: string; title: string; label: string; children: React.ReactNode }) {
  const open = useInspector((st) => st.open && st.key === keyName);
  return (
    <Button
      size="sm"
      variant="ghost"
      aria-pressed={open}
      onClick={() => useInspector.getState().toggle({ key: keyName, title, content: children })}
    >
      {label}
    </Button>
  );
}
