import { useEffect, useMemo, useRef, useState } from 'react';
import { useSearchParams } from 'react-router';
import {
  radioGroupKeyDown,
  Ago,
  Badge,
  Button,
  Dialog,
  EmptyState,
  Frame,
  MessageBody,
  Skeleton,
  Table,
  TextArea,
  toast,
  type Column,
} from '../../ds';
import { getApi } from '../../app/api';
import { canDecide } from '../../app/commands/core';
import { useEntityList } from '../../app/events/entities';
import { grantsStore, nodesStore } from '../../app/events/stores';
import { useRights, type Rights } from '../../app/session';
import { GrantState, type Grant } from '../../gen/mycel/v1/grant_pb';
import { errText, parseSender } from '../channels/model';
import { durationOptions, grantKind, grantSpecLine, STATE_LABEL, STATE_TONE } from './model';
import s from './Approvals.module.css';

const oldestFirst = (a: Grant, b: Grant) => a.createdAt.localeCompare(b.createdAt);
const HISTORY = 40;

/** Approvals: the pending queue as cards (one-tap approve, deny with a note) and the decided history. The phone's start route. */
export function Component() {
  const rights = useRights();
  const { items, loaded, error, stale } = useEntityList(grantsStore, oldestFirst);
  // canDecide reads node owners: hydrate the nodes and re-render when they change.
  useEntityList(nodesStore);
  const [params] = useSearchParams();
  const focus = params.get('grant');

  const pending = useMemo(() => items.filter((g) => g.state === GrantState.PENDING), [items]);
  const history = useMemo(
    () =>
      items
        .filter((g) => g.state !== GrantState.PENDING && (g.requiresApproval || g.decidedBy !== '' || g.id === focus))
        .reverse()
        .slice(0, HISTORY),
    [items, focus],
  );

  // Cards that arrive after the first render pulse once.
  const seen = useRef<Set<string> | null>(null);
  if (seen.current === null && loaded) seen.current = new Set(pending.map((g) => g.id));

  // Deep link from a push notification: select the card and bring it into view.
  // The card may arrive after the first load (the push can beat the event),
  // so this re-runs as the queue changes and acts once per deep link.
  const focused = useRef<string | null>(null);
  const focusPending = pending.some((g) => g.id === focus);
  const focusShown = focusPending || history.some((g) => g.id === focus);
  useEffect(() => {
    if (!focus || !loaded || !focusShown || focused.current === focus) return;
    const el = document.getElementById(`grant-${focus}`);
    if (!el) return;
    focused.current = focus;
    el.scrollIntoView?.({ block: 'center', behavior: 'smooth' });
    el.focus({ preventScroll: true });
  }, [focus, loaded, focusShown, focusPending, pending]);

  const decidable = (g: Grant) => canDecide(rights, g);

  return (
    <section className={s.page} aria-labelledby="approvals-heading">
      <header className={s.head}>
        <h1 className={s.heading} id="approvals-heading">
          Approvals
        </h1>
        <span className={s.count} aria-label={`${pending.length} pending`}>
          {String(pending.length).padStart(2, '0')} pending
        </span>
        {stale && <Badge tone="off">stale</Badge>}
      </header>
      {rights.mode === 'node' && <p className={s.note}>Read-only on this node. Approve or deny from the hub UI or with `mycel approve`.</p>}
      <section aria-label="Pending approvals" className={s.queue}>
        {!loaded ? (
          error ? (
            <Frame title="Pending" state="alert">
              <EmptyState title="Could not load grants">{error}</EmptyState>
            </Frame>
          ) : (
            <Frame title="Pending">
              <Skeleton rows={4} />
            </Frame>
          )
        ) : pending.length === 0 ? (
          <Frame title="Pending" index="00">
            <EmptyState title="Nothing waits for a decision">
              Grant requests that no auto-approval rule covers appear here and as a push notification.
            </EmptyState>
          </Frame>
        ) : (
          pending.map((g) => (
            <GrantCard
              key={g.id}
              g={g}
              rights={rights}
              canAct={decidable(g)}
              selected={g.id === focus}
              fresh={!!seen.current && !seen.current.has(g.id)}
            />
          ))
        )}
      </section>
      <Frame title="Decided" index={String(history.length).padStart(2, '0')} flush className={s.history}>
        <History rows={history} focus={focus} />
      </Frame>
    </section>
  );
}

function GrantCard({
  g,
  rights,
  canAct,
  selected,
  fresh,
}: {
  g: Grant;
  rights: Rights;
  canAct: boolean;
  selected: boolean;
  fresh: boolean;
}) {
  const options = useMemo(() => durationOptions(g.duration), [g.duration]);
  const [duration, setDuration] = useState(options[options.length - 1].value);
  const [busy, setBusy] = useState<'approve' | 'deny' | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [denyOpen, setDenyOpen] = useState(false);
  const [note, setNote] = useState('');
  const requester = parseSender(g.requestedBy.includes(':') ? g.requestedBy : `user:${g.requestedBy}`);
  const spec = grantSpecLine(g);

  const approve = async () => {
    setBusy('approve');
    setError(null);
    try {
      await getApi().rpc.grants.approveGrant({ id: g.id, duration });
      toast({ title: `Approved ${g.id}`, body: `${grantKind(g)} for ${g.subject} on ${g.node}` });
    } catch (err) {
      setError(errText(err));
    } finally {
      setBusy(null);
    }
  };
  const deny = async () => {
    setBusy('deny');
    setError(null);
    try {
      await getApi().rpc.grants.denyGrant({ id: g.id, note: note.trim() });
      setDenyOpen(false);
      toast({ title: `Denied ${g.id}` });
    } catch (err) {
      setError(errText(err));
      setDenyOpen(false);
    } finally {
      setBusy(null);
    }
  };

  return (
    <article
      id={`grant-${g.id}`}
      tabIndex={-1}
      className={s.card}
      data-selected={selected || undefined}
      data-new={fresh || undefined}
      aria-label={`${grantKind(g)} for ${g.subject} on ${g.node}`}
    >
      <span aria-hidden className={s.tickTl} />
      <span aria-hidden className={s.tickBr} />
      <header className={s.cardHead}>
        <span className={s.kind}>{grantKind(g)}</span>
        <span className={s.id}>{g.id}</span>
        <Ago at={g.createdAt} className={s.age} />
      </header>
      <dl className={s.facts}>
        <div>
          <dt>Subject</dt>
          <dd>{g.subject}</dd>
        </div>
        <div>
          <dt>Node</dt>
          <dd>{g.node}</dd>
        </div>
        <div>
          <dt>Duration</dt>
          <dd>{g.duration || 'default'}</dd>
        </div>
        <div>
          <dt>Requester</dt>
          <dd>{requester.kind === 'user' && requester.name === rights.userId ? 'you' : requester.name}</dd>
        </div>
        {g.channel && (
          <div>
            <dt>Channel</dt>
            <dd>{g.channel}</dd>
          </div>
        )}
      </dl>
      {spec && <p className={s.spec}>{spec}</p>}
      {g.reason && <MessageBody text={g.reason} untrusted={requester.kind === 'agent'} className={s.reason} />}
      {error && (
        <p className={s.error} role="alert">
          {error}
        </p>
      )}
      {canAct && (
        <div className={s.decide}>
          <div className={s.durations} role="radiogroup" aria-label="Approve for" onKeyDown={radioGroupKeyDown}>
            {options.map((o) => (
              <button
                key={o.value || 'default'}
                type="button"
                role="radio"
                aria-checked={duration === o.value}
                tabIndex={duration === o.value ? 0 : -1}
                className={s.chip}
                onClick={() => setDuration(o.value)}
                disabled={!!busy}
              >
                {o.label}
                {o.requested && <span className={s.req}> req</span>}
              </button>
            ))}
          </div>
          <div className={s.buttons}>
            <Button variant="warn" className={s.big} onClick={() => setDenyOpen(true)} disabled={!!busy}>
              Deny
            </Button>
            <Button variant="primary" className={s.big} busy={busy === 'approve'} disabled={!!busy} onClick={() => void approve()}>
              Approve {options.find((o) => o.value === duration)?.label}
            </Button>
          </div>
        </div>
      )}
      <Dialog
        open={denyOpen}
        onOpenChange={(o) => busy !== 'deny' && setDenyOpen(o)}
        title={`Deny ${g.id}?`}
        description={`${grantKind(g)} for ${g.subject} on ${g.node}. The requester sees the note.`}
        tone="alert"
        footer={
          <>
            <Button variant="ghost" onClick={() => setDenyOpen(false)} disabled={busy === 'deny'}>
              Back
            </Button>
            <Button variant="warn" busy={busy === 'deny'} onClick={() => void deny()}>
              Deny
            </Button>
          </>
        }
      >
        <TextArea
          label="Note (optional)"
          value={note}
          onChange={(e) => setNote(e.target.value)}
          rows={2}
          placeholder="Why, or what to ask for instead"
        />
      </Dialog>
    </article>
  );
}

const historyColumns: Column<Grant>[] = [
  { key: 'state', header: 'State', width: '116px', cell: (g) => <Badge tone={STATE_TONE[g.state]}>{STATE_LABEL[g.state]}</Badge> },
  { key: 'kind', header: 'Kind', width: '100px', mono: true, cell: (g) => grantKind(g) },
  { key: 'subject', header: 'Subject', mono: true, cell: (g) => g.subject },
  { key: 'node', header: 'Node', mono: true, hideOnNarrow: true, cell: (g) => g.node },
  { key: 'by', header: 'Decided by', mono: true, hideOnNarrow: true, cell: (g) => g.decidedBy || '—' },
  { key: 'note', header: 'Note', hideOnNarrow: true, cell: (g) => g.decisionNote || '—' },
  { key: 'at', header: 'Requested', width: '96px', align: 'right', mono: true, cell: (g) => <Ago at={g.createdAt} /> },
];

function History({ rows, focus }: { rows: Grant[]; focus: string | null }) {
  return (
    <Table
      aria-label="Decided grants"
      columns={historyColumns}
      rows={rows}
      rowKey={(g) => g.id}
      selectedKey={focus}
      rowTone={(g) => (g.state === GrantState.DENIED || g.state === GrantState.REVOKED ? 'alert' : undefined)}
      empty={<EmptyState title="No decisions yet" />}
    />
  );
}
