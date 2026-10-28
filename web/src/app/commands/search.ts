import { ConnectError } from '@connectrpc/connect';
import { useEffect, useState } from 'react';
import { EventKind, type SearchHit } from '../../gen/mycel/v1/ui_pb';
import { maybeApi } from '../api';
import { agentsStore } from '../events/stores';

// Server search for the command bar: free text also queries
// EventService.Search (after a short debounce) and the hits are listed
// below the commands, grouped by kind. A hit opens the object's inspector
// (nodes, agents, grants) or its route (channels, messages, jobs).

export const SEARCH_DEBOUNCE_MS = 150;
export const SEARCH_MIN_CHARS = 2;
const SEARCH_LIMIT = 30;

const KIND_ORDER: EventKind[] = [EventKind.NODE, EventKind.AGENT, EventKind.CHANNEL, EventKind.JOB, EventKind.GRANT, EventKind.MESSAGE];
const KIND_LABEL: Partial<Record<EventKind, string>> = {
  [EventKind.NODE]: 'Nodes',
  [EventKind.AGENT]: 'Agents',
  [EventKind.CHANNEL]: 'Channels',
  [EventKind.JOB]: 'Jobs',
  [EventKind.GRANT]: 'Grants',
  [EventKind.MESSAGE]: 'Messages',
};

export function kindLabel(k: EventKind): string {
  return KIND_LABEL[k] ?? 'Other';
}

/** groupHits orders hits by kind (nodes, agents, channels, jobs, grants, messages), keeping the server's order inside a kind. */
export function groupHits(hits: SearchHit[]): Array<{ kind: EventKind; label: string; hits: SearchHit[] }> {
  const by = new Map<EventKind, SearchHit[]>();
  for (const h of hits) {
    const arr = by.get(h.kind) ?? [];
    arr.push(h);
    by.set(h.kind, arr);
  }
  const kinds = [...KIND_ORDER.filter((k) => by.has(k)), ...[...by.keys()].filter((k) => !KIND_ORDER.includes(k))];
  return kinds.map((kind) => ({ kind, label: kindLabel(kind), hits: by.get(kind)! }));
}

export interface SearchState {
  /** The query the hits belong to. */
  query: string;
  hits: SearchHit[];
  loading: boolean;
  error: string | null;
}

const idle: SearchState = { query: '', hits: [], loading: false, error: null };

/** useServerSearch debounces `query` and runs EventService.Search; stale answers are dropped. */
export function useServerSearch(query: string): SearchState {
  const [state, setState] = useState<SearchState>(idle);
  const q = query.trim();
  useEffect(() => {
    if (q.length < SEARCH_MIN_CHARS) {
      setState(idle);
      return;
    }
    const api = maybeApi();
    if (!api) return;
    let cancelled = false;
    const ctrl = new AbortController();
    setState((st) => ({ ...st, loading: true }));
    const h = setTimeout(() => {
      api.rpc.events
        .search({ query: q.slice(0, 200), limit: SEARCH_LIMIT }, { signal: ctrl.signal })
        .then((r) => {
          if (!cancelled) setState({ query: q, hits: r.hits, loading: false, error: null });
        })
        .catch((err: unknown) => {
          if (!cancelled) setState({ query: q, hits: [], loading: false, error: ConnectError.from(err).rawMessage });
        });
    }, SEARCH_DEBOUNCE_MS);
    return () => {
      cancelled = true;
      clearTimeout(h);
      ctrl.abort();
    };
  }, [q]);
  return state;
}

/** resolveAgentId maps a hit id (agent id, "name@node" or bare name) to the agents store key. */
export function resolveAgentId(id: string): string {
  const rows = agentsStore.list();
  const hit = rows.find((a) => a.id === id || `agent:${id}` === a.id || `${a.name}@${a.nodeName}` === id || a.name === id);
  return hit?.id ?? (id.startsWith('agent:') ? id : `agent:${id}`);
}

/** openHit shows a search hit: the inspector for nodes, agents and grants; the route otherwise. */
export async function openHit(hit: SearchHit, navigate: (to: string) => void): Promise<void> {
  switch (hit.kind) {
    case EventKind.NODE: {
      const m = await import('../../views/nodes/NodeDetail');
      m.inspectNode(hit.id);
      return;
    }
    case EventKind.AGENT: {
      const m = await import('../../views/agents/AgentDetail');
      m.inspectAgent(resolveAgentId(hit.id));
      return;
    }
    case EventKind.GRANT: {
      const m = await import('../../views/grants/GrantDetail');
      m.inspectGrant(hit.id);
      return;
    }
    case EventKind.CHANNEL:
      navigate(`/channels/${encodeURIComponent(hit.id)}`);
      return;
    case EventKind.MESSAGE:
      navigate(`/channels/${encodeURIComponent(hit.channel || hit.id)}`);
      return;
    case EventKind.JOB:
      navigate(`/jobs/${encodeURIComponent(hit.id)}`);
      return;
    default:
      return;
  }
}
