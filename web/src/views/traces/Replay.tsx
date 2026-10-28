import { useEffect, useMemo, useRef, useState, type FormEvent } from 'react';
import {
  Ago,
  Button,
  EmptyState,
  fmtClock,
  fmtDuration,
  Frame,
  Input,
  MessageBody,
  prefersReducedMotion,
  Readout,
  Skeleton,
  Timeline,
} from '../../ds';
import { getApi } from '../../app/api';
import { useMessages } from '../../app/events/stores';
import type { Message } from '../../gen/mycel/v1/envelope_pb';
import type { AuditEntry } from '../../gen/mycel/v1/ui_pb';
import { errText, mergeMessages, parseSender } from '../channels/model';
import { buildTimeline, replaySummary, type ReplayStep } from './model';
import s from './Traces.module.css';

const STEP_MS = 900;

interface Loaded {
  trace: string;
  channel: string;
  thread: string;
  messages: Message[];
  audit: AuditEntry[];
}

/** findMessage looks a message id up in the loaded channels. */
function findMessage(id: string): Message | undefined {
  for (const list of Object.values(useMessages.getState().byChannel)) {
    const m = list.find((x) => x.id === id);
    if (m) return m;
  }
  return undefined;
}

/** loadReplay fetches a trace's audit entries and its thread's messages. */
export async function loadReplay(input: string): Promise<Loaded> {
  const api = getApi();
  const q = input.trim();
  let msg = q.startsWith('msg_') ? findMessage(q) : undefined;
  let trace = msg?.meta?.traceId ?? (q.startsWith('msg_') ? '' : q);
  let channel = msg?.channel ?? '';
  let thread = msg ? msg.thread || msg.id : '';
  if (!msg && !trace) throw new Error('That message is not loaded here; open its channel first or paste its trace id.');
  if (!msg) {
    // A trace id: find a message that carries it among the loaded channels.
    for (const list of Object.values(useMessages.getState().byChannel)) {
      const m = list.find((x) => x.meta?.traceId === trace);
      if (m) {
        msg = m;
        channel = m.channel;
        thread = m.thread || m.id;
        break;
      }
    }
  }
  const auditP = trace ? api.rpc.events.listAudit({ traceId: trace, limit: 1000 }).then((r) => r.entries) : Promise.resolve([]);
  const audit = await auditP;
  if (!channel) {
    // Fall back to the channel named by the trace's audit entries.
    const r = audit.find((e) => e.resource.startsWith('channel:'));
    if (r) channel = r.resource.slice('channel:'.length);
  }
  let messages: Message[] = [];
  if (channel) {
    const r = await api.rpc.channels.listMessages({ channel, thread: thread || '', limit: 500 });
    messages = r.messages;
    if (!thread && trace) messages = messages.filter((m) => m.meta?.traceId === trace);
  }
  const local = Object.values(useMessages.getState().byChannel)
    .flat()
    .filter((m) => trace && m.meta?.traceId === trace);
  return { trace, channel, thread, messages: mergeMessages(local, messages), audit };
}

/** Replay: pick a message or paste a trace id; the thread and its audit entries play back step by step. */
export function Replay({ initial, onTrace }: { initial: string; onTrace: (t: string) => void }) {
  const [input, setInput] = useState(initial);
  const [data, setData] = useState<Loaded | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [current, setCurrent] = useState(-1);
  const [playing, setPlaying] = useState(false);

  const run = async (q: string) => {
    if (!q.trim()) return;
    setLoading(true);
    setError(null);
    setPlaying(false);
    try {
      const d = await loadReplay(q);
      setData(d);
      setCurrent(-1);
      shown.current = d.trace;
      if (d.trace) onTrace(d.trace);
    } catch (err) {
      setError(err instanceof Error && !('code' in err) ? err.message : errText(err));
      setData(null);
    } finally {
      setLoading(false);
    }
  };

  // Load the trace named in the URL (a "trace" link from a message), unless it is the one shown.
  const shown = useRef('');
  useEffect(() => {
    if (!initial || initial === shown.current) return;
    setInput(initial);
    void run(initial);
  }, [initial]); // run is recreated every render; initial is the only input that matters.

  const steps = useMemo(() => (data ? buildTimeline(data.messages, data.audit) : []), [data]);
  const summary = useMemo(() => replaySummary(steps), [steps]);

  // Play: advance one step at a time; the wait mirrors the real latency, compressed.
  useEffect(() => {
    if (!playing) return;
    if (current >= steps.length - 1) {
      setPlaying(false);
      return;
    }
    const next = steps[current + 1];
    const wait = prefersReducedMotion() ? STEP_MS : Math.min(2400, Math.max(300, (next?.latency ?? 0) / 4 || STEP_MS));
    const h = setTimeout(() => setCurrent((c) => Math.min(steps.length - 1, c + 1)), current < 0 ? 200 : wait);
    return () => clearTimeout(h);
  }, [playing, current, steps]);

  const recent = useRecentTraced();

  const submit = (e: FormEvent) => {
    e.preventDefault();
    void run(input);
  };

  return (
    <div className={s.stack}>
      <Frame title="Replay" index="01">
        <form className={s.replayForm} onSubmit={submit}>
          <Input
            label="Trace id or message id"
            mono
            value={input}
            onChange={(e) => setInput(e.target.value)}
            placeholder="trace id, or msg_…"
            className={s.grow}
          />
          <Button type="submit" variant="primary" busy={loading} disabled={!input.trim()}>
            Replay
          </Button>
        </form>
        {recent.length > 0 && (
          <div className={s.recent}>
            <span className={s.label}>Recent traced messages</span>
            <ul className={s.recentList}>
              {recent.map((m) => (
                <li key={m.id}>
                  <button type="button" className={s.recentItem} onClick={() => void run(m.id)}>
                    <span className={s.mono}>{m.channel}</span>
                    <span className={s.mono}>{parseSender(m.from).name}</span>
                    <span className={s.excerpt}>{(m.payload?.text ?? '').slice(0, 70)}</span>
                    <Ago at={m.ts} className={s.dim} />
                  </button>
                </li>
              ))}
            </ul>
          </div>
        )}
        {error && (
          <p className={s.error} role="alert">
            {error}
          </p>
        )}
      </Frame>
      {loading && !data ? (
        <Frame title="Timeline">
          <Skeleton rows={5} />
        </Frame>
      ) : data ? (
        <Frame
          title="Timeline"
          index={String(steps.length).padStart(2, '0')}
          meta={
            <span className={s.mono}>
              {data.channel || 'no channel'}
              {data.trace ? ` · ${data.trace}` : ''}
            </span>
          }
        >
          <div className={s.totals}>
            <Readout size="sm" label="Steps" value={summary.steps} />
            <Readout size="sm" label="Duration" value={fmtDuration(summary.duration)} />
            <Readout size="sm" label="Slowest hop" value={fmtDuration(summary.slowest)} />
            <Readout size="sm" label="Actors" value={summary.actors} />
            <Readout size="sm" label="Denied" value={summary.denied} tone={summary.denied ? 'alert' : 'default'} />
          </div>
          <div className={s.player} role="group" aria-label="Replay controls">
            <Button size="sm" variant="ghost" onClick={() => setCurrent(0)} disabled={!steps.length}>
              First
            </Button>
            <Button size="sm" variant="ghost" onClick={() => setCurrent((c) => Math.max(0, c - 1))} disabled={current <= 0}>
              Back
            </Button>
            <Button
              size="sm"
              variant="primary"
              onClick={() => {
                if (!playing && current >= steps.length - 1) setCurrent(-1);
                setPlaying(!playing);
              }}
              disabled={!steps.length}
            >
              {playing ? 'Pause' : 'Play'}
            </Button>
            <Button
              size="sm"
              variant="ghost"
              onClick={() => setCurrent((c) => Math.min(steps.length - 1, c + 1))}
              disabled={current >= steps.length - 1}
            >
              Step
            </Button>
            <Button size="sm" variant="ghost" onClick={() => setCurrent(-1)} disabled={current < 0}>
              Show all
            </Button>
            <span className={s.position}>
              {current >= 0 ? `step ${current + 1} / ${steps.length} · +${fmtDuration(steps[current].offset)}` : `${steps.length} steps`}
            </span>
          </div>
          {steps.length === 0 ? (
            <EmptyState title="Nothing recorded for this trace">
              The audit log has no entries for it, and its thread has no messages you can read.
            </EmptyState>
          ) : (
            <Timeline steps={steps.map(toStep)} current={current} onSelect={setCurrent} aria-label="Trace replay" />
          )}
        </Frame>
      ) : (
        <Frame title="Timeline">
          <EmptyState title="Pick a message or paste a trace id">
            Every message, grant, broker call and approval is audited with its trace id; the replay lines them up with the thread, the time
            between steps and anything that was denied.
          </EmptyState>
        </Frame>
      )}
    </div>
  );
}

function toStep(st: ReplayStep) {
  return {
    key: st.key,
    kind: st.kind,
    time: fmtClock(st.at),
    gap: st.latency != null ? `+${fmtDuration(st.latency)}` : undefined,
    actor: st.actor,
    title: st.title,
    tone: st.denied ? ('alert' as const) : ('default' as const),
    body: st.text ? (
      <MessageBody text={st.text} untrusted={st.untrusted} clamp={4} />
    ) : st.audit?.detailsJson ? (
      <code className={s.details}>{st.audit.detailsJson}</code>
    ) : undefined,
  };
}

const EMPTY: Message[] = [];

/** useRecentTraced lists the newest loaded messages that carry a trace id. */
function useRecentTraced(): Message[] {
  const byChannel = useMessages((st) => st.byChannel);
  return useMemo(() => {
    const all = Object.values(byChannel).flat();
    return (all.length ? all : EMPTY)
      .filter((m) => m.meta?.traceId)
      .sort((a, b) => b.ts.localeCompare(a.ts))
      .slice(0, 6);
  }, [byChannel]);
}
