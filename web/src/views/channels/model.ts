import { ConnectError } from '@connectrpc/connect';
import { create } from 'zustand';
import { ChannelType, type Channel } from '../../gen/mycel/v1/channel_pb';
import { MessageType, type Message } from '../../gen/mycel/v1/envelope_pb';
import type { FreezeStatusResponse } from '../../gen/mycel/v1/grant_pb';
import { HttpError } from '../../app/api';
import type { Rights } from '../../app/session';

/** errText is what a view shows for a failed call. */
export function errText(err: unknown): string {
  if (err instanceof HttpError) return err.message;
  return ConnectError.from(err).rawMessage;
}

export function channelPath(name: string): string {
  return `/channels/${encodeURIComponent(name)}`;
}

export const CHANNEL_KIND: Record<ChannelType, string> = {
  [ChannelType.UNSPECIFIED]: 'channel',
  [ChannelType.TOPIC]: 'topic',
  [ChannelType.TASK]: 'task',
  [ChannelType.DIRECT]: 'direct',
  [ChannelType.SYSTEM]: 'system',
};

export function isChannelFrozen(name: string, f: FreezeStatusResponse | null | undefined): boolean {
  return !!f && (f.allFrozen || f.frozenChannels.includes(name));
}

/** canManageChannel: hub write access and the channel's owner or an admin. */
export function canManageChannel(rights: Rights, ch: Channel | undefined): boolean {
  if (!ch || rights.mode !== 'hub' || rights.readOnly) return false;
  return rights.isAdmin || (!!rights.userId && ch.ownerId === rights.userId);
}

/** postBlockedReason says why the composer is disabled, or null when the caller may post. */
export function postBlockedReason(rights: Rights, ch: Channel | undefined, f: FreezeStatusResponse | null | undefined): string | null {
  if (rights.mode !== 'hub' || rights.readOnly)
    return 'Read-only here: the node UI shows the network but cannot post. Post from the hub UI.';
  if (!ch) return 'Unknown channel.';
  if (ch.archivedAt) return 'This channel is archived.';
  if (f?.allFrozen) return 'The network is frozen; posts are refused until an admin unfreezes it.';
  if (f?.frozenChannels.includes(ch.name)) return 'This channel is frozen; posts are refused until it is unfrozen.';
  if (ch.type === ChannelType.SYSTEM) return 'System channel: the hub and nodes post here, humans read.';
  return null;
}

export interface Sender {
  kind: 'agent' | 'user' | 'hub' | 'node' | 'other';
  /** Short display name: the agent name, the user id, "hub". */
  name: string;
  /** Node for agents ("coder@mac" → "mac"). */
  node: string;
  raw: string;
}

/** parseSender reads Message.from: "agent:<name>@<node>", "user:<id>", "node:<name>", "hub". */
export function parseSender(from: string): Sender {
  if (from.startsWith('agent:')) {
    const rest = from.slice(6);
    const at = rest.lastIndexOf('@');
    return { kind: 'agent', name: at > 0 ? rest.slice(0, at) : rest, node: at > 0 ? rest.slice(at + 1) : '', raw: from };
  }
  if (from.startsWith('user:')) return { kind: 'user', name: from.slice(5), node: '', raw: from };
  if (from.startsWith('node:')) return { kind: 'node', name: from.slice(5), node: from.slice(5), raw: from };
  if (from === 'hub' || from === '') return { kind: 'hub', name: 'hub', node: '', raw: from };
  return { kind: 'other', name: from, node: '', raw: from };
}

export const MESSAGE_TYPE: Record<MessageType, string> = {
  [MessageType.UNSPECIFIED]: 'chat',
  [MessageType.CHAT]: 'chat',
  [MessageType.TASK]: 'task',
  [MessageType.RESULT]: 'result',
  [MessageType.EVENT]: 'event',
  [MessageType.APPROVAL_REQUEST]: 'approval',
  [MessageType.APPROVAL_DECISION]: 'decision',
  [MessageType.GRANT_REQUEST]: 'grant request',
};

/** typeClass groups message types into the four styles the chat uses. */
export function typeClass(t: MessageType): 'chat' | 'task' | 'result' | 'event' | 'approval' {
  switch (t) {
    case MessageType.TASK:
      return 'task';
    case MessageType.RESULT:
      return 'result';
    case MessageType.EVENT:
      return 'event';
    case MessageType.APPROVAL_REQUEST:
    case MessageType.APPROVAL_DECISION:
    case MessageType.GRANT_REQUEST:
      return 'approval';
    default:
      return 'chat';
  }
}

/** grantIdIn finds a grant id in an approval card's text. */
export function grantIdIn(text: string): string | null {
  return /\bgrt_[0-9A-Za-z]+/.exec(text)?.[0] ?? null;
}

/** isUntrusted: everything an agent produced is untrusted, whatever the flag says. */
export function isUntrusted(m: Message): boolean {
  const p = m.meta?.provenance;
  return !!p?.untrusted || p?.origin === 'agent' || m.from.startsWith('agent:');
}

export const bySeq = (a: Message, b: Message) => (a.seq < b.seq ? -1 : a.seq > b.seq ? 1 : 0);

/** mergeMessages unions two lists by id, ordered by seq. */
export function mergeMessages(a: Message[], b: Message[]): Message[] {
  const m = new Map<string, Message>();
  for (const x of a) m.set(x.id, x);
  for (const x of b) m.set(x.id, x);
  return [...m.values()].sort(bySeq);
}

export interface ThreadGroup {
  key: string;
  /** The first loaded message of the thread (its root, unless the root is older than what is loaded). */
  root: Message;
  /** True when the root is not loaded and `root` is the first loaded reply. */
  partial: boolean;
  /** Thread id (the root's id or an explicit thr_…). */
  thread: string;
  replies: Message[];
}

/**
 * groupThreads folds replies under the message that started their thread.
 * A reply's thread is its root's id (the hub's rule) or an explicit thr_…
 * id. Groups keep the order of their first message, so the channel reads
 * top to bottom with the newest at the bottom.
 */
export function groupThreads(msgs: Message[]): ThreadGroup[] {
  const groups: ThreadGroup[] = [];
  const byThread = new Map<string, ThreadGroup>();
  for (const m of [...msgs].sort(bySeq)) {
    const t = m.thread || m.id;
    const g = byThread.get(t);
    if (g) {
      g.replies.push(m);
      continue;
    }
    const ng: ThreadGroup = { key: m.id, root: m, partial: t !== m.id && !t.startsWith('thr_'), thread: t, replies: [] };
    byThread.set(t, ng);
    groups.push(ng);
  }
  return groups;
}

// ---- Per-browser read cursor (the hub keeps inbox cursors for agents only) ----

const READ_KEY = 'mycel.read.';

export function readCursor(channel: string): bigint {
  try {
    const v = localStorage.getItem(READ_KEY + channel);
    return v ? BigInt(v) : 0n;
  } catch {
    return 0n;
  }
}

/** useReads changes whenever a read cursor moves, so lists re-read the cursors. */
export const useReads = create<{ v: number }>(() => ({ v: 0 }));

export function markRead(channel: string, seq: bigint): void {
  if (seq <= readCursor(channel)) return;
  try {
    localStorage.setItem(READ_KEY + channel, seq.toString());
  } catch {
    /* storage unavailable: unread markers just stay */
  }
  useReads.setState((s) => ({ v: s.v + 1 }));
}
