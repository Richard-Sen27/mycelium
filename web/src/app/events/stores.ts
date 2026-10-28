import { create } from 'zustand';
import { ChannelSchema } from '../../gen/mycel/v1/channel_pb';
import type { Message } from '../../gen/mycel/v1/envelope_pb';
import { GrantSchema, type FreezeStatusResponse } from '../../gen/mycel/v1/grant_pb';
import { AgentSchema } from '../../gen/mycel/v1/hub_pb';
import { JobSchema } from '../../gen/mycel/v1/job_pb';
import { NodeSchema } from '../../gen/mycel/v1/node_pb';
import { EventKind, EventOp, type AuditEntry, type Event } from '../../gen/mycel/v1/ui_pb';
import { getApi } from '../api';
import { createEntityStore } from './entities';

// The per-kind stores the WatchEvents stream fans into. Every view reads
// from these; none opens its own stream for these kinds.

export const nodesStore = createEntityStore({
  kind: 'nodes',
  schema: NodeSchema,
  key: (n) => n.name,
  load: async () => (await getApi().rpc.nodes.listNodes({})).nodes,
});

export const agentsStore = createEntityStore({
  kind: 'agents',
  schema: AgentSchema,
  key: (a) => a.id,
  // Event ids for agents may be the bare name or "<name>@<node>".
  matches: (a, id) => a.name === id || `${a.name}@${a.nodeName}` === id || `agent:${id}` === a.id,
  load: async () => (await getApi().rpc.agents.listAgents({})).agents,
});

export const channelsStore = createEntityStore({
  kind: 'channels',
  schema: ChannelSchema,
  key: (c) => c.name,
  load: async () => (await getApi().rpc.channels.listChannels({ includeArchived: true })).channels,
});

export const jobsStore = createEntityStore({
  kind: 'jobs',
  schema: JobSchema,
  key: (j) => j.id,
  load: async () => (await getApi().rpc.jobs.listJobs({ includeFinished: true, limit: 500 })).jobs,
});

export const grantsStore = createEntityStore({
  kind: 'grants',
  schema: GrantSchema,
  key: (g) => g.id,
  load: async () => (await getApi().rpc.grants.listGrants({ includeFinished: true })).grants,
});

// ---- Freeze state ----

export interface FreezeState {
  status: FreezeStatusResponse | null;
  loaded: boolean;
  load(): Promise<void>;
}

export const useFreeze = create<FreezeState>((set) => ({
  status: null,
  loaded: false,
  async load() {
    try {
      const st = await getApi().rpc.control.freezeStatus({});
      set({ status: st, loaded: true });
    } catch {
      set({ loaded: true });
    }
  },
}));

// ---- Messages (appended by the stream; views-b loads history with ListMessages) ----

const MAX_PER_CHANNEL = 500;

export interface MessagesState {
  byChannel: Record<string, Message[]>;
  /** Append one live message (ignored when its seq is already present). */
  append(m: Message): void;
  /** Replace a channel's history (sorted by seq), e.g. after ListMessages. */
  setChannel(channel: string, msgs: Message[]): void;
  reset(): void;
}

export const useMessages = create<MessagesState>((set) => ({
  byChannel: {},
  append(m) {
    set((st) => {
      const cur = st.byChannel[m.channel] ?? [];
      if (cur.some((x) => x.seq === m.seq && x.id === m.id)) return st;
      const next = [...cur, m].sort((a, b) => (a.seq < b.seq ? -1 : a.seq > b.seq ? 1 : 0));
      return { byChannel: { ...st.byChannel, [m.channel]: next.slice(-MAX_PER_CHANNEL) } };
    });
  },
  setChannel(channel, msgs) {
    const sorted = [...msgs].sort((a, b) => (a.seq < b.seq ? -1 : a.seq > b.seq ? 1 : 0));
    set((st) => ({ byChannel: { ...st.byChannel, [channel]: sorted.slice(-MAX_PER_CHANNEL) } }));
  },
  reset() {
    set({ byChannel: {} });
  },
}));

// ---- Audit (live tail; history comes from EventService.ListAudit) ----

const MAX_AUDIT = 500;

export interface AuditState {
  entries: AuditEntry[]; // newest first
  push(e: AuditEntry): void;
  reset(): void;
}

export const useAudit = create<AuditState>((set) => ({
  entries: [],
  push(e) {
    set((st) => ({ entries: [e, ...st.entries.filter((x) => x.id !== e.id)].slice(0, MAX_AUDIT) }));
  },
  reset() {
    set({ entries: [] });
  },
}));

// ---- Fan-out ----

type Listener = (e: Event) => void;
const listeners = new Set<Listener>();

/**
 * onEvent lets a view react to raw events (e.g. animate a message along an
 * edge in the constellation) without owning a stream. Returns an unsubscribe.
 */
export function onEvent(fn: Listener): () => void {
  listeners.add(fn);
  return () => listeners.delete(fn);
}

/** dispatchEvent routes one stream event into its store. */
export function dispatchEvent(e: Event): void {
  const del = e.op === EventOp.DELETED;
  const b = e.body;
  switch (e.kind) {
    case EventKind.NODE:
      if (del) nodesStore.remove(b.case === 'node' ? b.value.name : e.id);
      else if (b.case === 'node') nodesStore.upsert(b.value);
      break;
    case EventKind.AGENT:
      if (del) agentsStore.remove(b.case === 'agent' ? b.value.id : e.id);
      else if (b.case === 'agent') agentsStore.upsert(b.value);
      break;
    case EventKind.CHANNEL:
      if (del) channelsStore.remove(b.case === 'channel' ? b.value.name : e.id);
      else if (b.case === 'channel') channelsStore.upsert(b.value);
      break;
    case EventKind.JOB:
      if (del) jobsStore.remove(b.case === 'job' ? b.value.id : e.id);
      else if (b.case === 'job') jobsStore.upsert(b.value);
      break;
    case EventKind.GRANT:
      if (del) grantsStore.remove(b.case === 'grant' ? b.value.id : e.id);
      else if (b.case === 'grant') grantsStore.upsert(b.value);
      break;
    case EventKind.MESSAGE:
      if (b.case === 'message') useMessages.getState().append(b.value);
      break;
    case EventKind.FREEZE:
      if (b.case === 'freeze') useFreeze.setState({ status: b.value, loaded: true });
      else void useFreeze.getState().load();
      break;
    case EventKind.AUDIT:
      if (b.case === 'audit') useAudit.getState().push(b.value);
      break;
  }
  for (const fn of listeners) {
    try {
      fn(e);
    } catch (err) {
      console.error('event listener failed', err);
    }
  }
}

/** resetStores clears every store (sign-out). */
export function resetStores(): void {
  for (const s of [nodesStore, agentsStore, channelsStore, jobsStore, grantsStore]) s.reset();
  useMessages.getState().reset();
  useAudit.getState().reset();
  useFreeze.setState({ status: null, loaded: false });
}
