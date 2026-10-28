import type { Message } from '../../gen/mycel/v1/envelope_pb';
import type { AuditEntry } from '../../gen/mycel/v1/ui_pb';
import { isUntrusted, MESSAGE_TYPE, parseSender } from '../channels/model';
import { auditSeq, isDenied } from './audit';

export interface ReplayStep {
  key: string;
  kind: 'message' | 'action';
  at: string;
  /** ms since the epoch (NaN when the time is unreadable). */
  t: number;
  /** ms since the previous step (null for the first or when unknown). */
  latency: number | null;
  /** ms since the first step. */
  offset: number;
  actor: string;
  title: string;
  text?: string;
  untrusted?: boolean;
  denied: boolean;
  message?: Message;
  audit?: AuditEntry;
}

/**
 * buildTimeline merges a thread's messages and its trace's audit entries
 * into one ordered list of steps: by time; at the same instant messages
 * before actions, then by sequence. Latency is the time since the previous
 * step, so a slow hop stands out.
 */
export function buildTimeline(messages: Message[], audit: AuditEntry[]): ReplayStep[] {
  const steps: Omit<ReplayStep, 'latency' | 'offset'>[] = [];
  const seenMsg = new Set<string>();
  for (const m of messages) {
    if (seenMsg.has(m.id)) continue;
    seenMsg.add(m.id);
    const who = parseSender(m.from);
    const text = m.payload?.text ?? '';
    steps.push({
      key: `m:${m.id}`,
      kind: 'message',
      at: m.ts,
      t: Date.parse(m.ts),
      actor: who.node && who.kind === 'agent' ? `${who.name}@${who.node}` : who.name,
      title: `${MESSAGE_TYPE[m.type]}${m.to.length ? ` → ${m.to.map((x) => parseSender(x).name).join(', ')}` : ''}`,
      text,
      untrusted: isUntrusted(m),
      denied: false,
      message: m,
    });
  }
  const seenAudit = new Set<string>();
  for (const e of audit) {
    if (seenAudit.has(e.id)) continue;
    seenAudit.add(e.id);
    steps.push({
      key: `a:${e.id}`,
      kind: 'action',
      at: e.at,
      t: Date.parse(e.at),
      actor: parseSender(e.actor).name,
      title: `${e.action}${e.resource ? ` ${e.resource}` : ''}${e.outcome && e.outcome !== 'allowed' ? ` · ${e.outcome}` : ''}`,
      denied: isDenied(e),
      audit: e,
    });
  }
  const seqOf = (s: Omit<ReplayStep, 'latency' | 'offset'>): bigint => (s.message ? s.message.seq : s.audit ? auditSeq(s.audit) : 0n);
  steps.sort((a, b) => {
    const ta = Number.isFinite(a.t) ? a.t : Infinity;
    const tb = Number.isFinite(b.t) ? b.t : Infinity;
    if (ta !== tb) return ta - tb;
    if (a.kind !== b.kind) return a.kind === 'message' ? -1 : 1;
    const sa = seqOf(a);
    const sb = seqOf(b);
    return sa === sb ? 0 : sa < sb ? -1 : 1;
  });
  const t0 = steps.find((s) => Number.isFinite(s.t))?.t ?? NaN;
  let prev = NaN;
  return steps.map((s) => {
    const latency = Number.isFinite(prev) && Number.isFinite(s.t) ? s.t - prev : null;
    if (Number.isFinite(s.t)) prev = s.t;
    return { ...s, latency, offset: Number.isFinite(t0) && Number.isFinite(s.t) ? s.t - t0 : 0 };
  });
}

/** Summary readouts for a replay. */
export function replaySummary(steps: ReplayStep[]): { steps: number; duration: number; denied: number; actors: number; slowest: number } {
  const times = steps.map((s) => s.t).filter(Number.isFinite);
  return {
    steps: steps.length,
    duration: times.length ? Math.max(...times) - Math.min(...times) : 0,
    denied: steps.filter((s) => s.denied).length,
    actors: new Set(steps.map((s) => s.actor)).size,
    slowest: Math.max(0, ...steps.map((s) => s.latency ?? 0)),
  };
}
