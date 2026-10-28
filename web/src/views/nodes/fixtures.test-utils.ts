import { create, type MessageInitShape } from '@bufbuild/protobuf';
import { vi } from 'vitest';
import { ChannelSchema, ChannelType, type Channel } from '../../gen/mycel/v1/channel_pb';
import { GrantSchema, GrantState, type Grant } from '../../gen/mycel/v1/grant_pb';
import { AgentSchema, type Agent } from '../../gen/mycel/v1/hub_pb';
import { JobSchema, JobState, type Job } from '../../gen/mycel/v1/job_pb';
import { NodeSchema, type Node } from '../../gen/mycel/v1/node_pb';
import { makeHubApi, setApi } from '../../app/api';
import { agentsStore, channelsStore, grantsStore, jobsStore, nodesStore } from '../../app/events/stores';
import type { EntityStore } from '../../app/events/entities';
import { anonymousRights, useSession, type Rights } from '../../app/session';
import { useInspector } from '../../app/shell/inspector';

// Test fixtures for the nodes, agents, grants and constellation tests
// (imported by *.test.tsx only).

export const ADMIN: Rights = { mode: 'hub', isAdmin: true, readOnly: false, userId: 'u_admin', userName: 'ada' };
export const MEMBER: Rights = { mode: 'hub', isAdmin: false, readOnly: false, userId: 'u_bob', userName: 'bob' };
export const NODE_RO: Rights = { mode: 'node', isAdmin: false, readOnly: true, userId: 'u_bob', userName: 'bob' };

export function node(
  name: string,
  o: Omit<MessageInitShape<typeof NodeSchema>, '$typeName'> & { cpu?: number; memGiB?: number } = {},
): Node {
  const { cpu = 8, memGiB = 32, ...rest } = o;
  return create(NodeSchema, {
    id: `node:${name}`,
    name,
    ownerId: 'u_admin',
    online: true,
    lastSeenAt: new Date().toISOString(),
    createdAt: '2026-10-01T00:00:00Z',
    capsCeiling: { names: ['docker', 'ollama', 'gpu', 'sandbox'] },
    capsEnabled: { names: ['sandbox'] },
    inventory: {
      os: 'linux',
      arch: 'amd64',
      hostname: name,
      cpuCount: cpu,
      memBytes: BigInt(memGiB) * 1024n ** 3n,
      sandboxBackends: ['gvisor'],
    },
    ...rest,
  });
}

export function agent(name: string, nodeName: string, o: Omit<MessageInitShape<typeof AgentSchema>, '$typeName'> = {}): Agent {
  return create(AgentSchema, {
    id: `agent:${name}@${nodeName}`,
    name,
    nodeName,
    ownerId: 'u_admin',
    harness: 'claude-code',
    state: 'running',
    createdAt: '2026-10-28T10:00:00Z',
    ...o,
  });
}

export function job(id: string, nodeName: string, o: Omit<MessageInitShape<typeof JobSchema>, '$typeName'> = {}): Job {
  return create(JobSchema, {
    id,
    node: nodeName,
    state: JobState.RUNNING,
    ownerId: 'u_admin',
    submittedBy: 'user:u_admin',
    createdAt: '2026-10-28T10:00:00Z',
    spec: { profile: 'default', command: ['make', 'test'], name: id },
    ...o,
  });
}

export function grant(id: string, o: Omit<MessageInitShape<typeof GrantSchema>, '$typeName'> = {}): Grant {
  return create(GrantSchema, {
    id,
    subject: 'agent:coder@alpha',
    node: 'alpha',
    ownerId: 'u_admin',
    requestedBy: 'agent:coder@alpha',
    state: GrantState.ACTIVE,
    duration: '1h',
    createdAt: '2026-10-28T10:00:00Z',
    spec: { kind: { case: 'ollama', value: { models: ['llama3.1:8b'] } } },
    ...o,
  });
}

export function channel(name: string, members: string[] = [], o: Omit<MessageInitShape<typeof ChannelSchema>, '$typeName'> = {}): Channel {
  return create(ChannelSchema, {
    name,
    type: ChannelType.TOPIC,
    ownerId: 'u_admin',
    members: members.map((id) => ({ id, role: 'agent' })),
    ...o,
  });
}

/** seed fills a store as if the List RPC had answered. */
export function seed<T>(store: EntityStore<T>, rows: T[]): void {
  const byId: Record<string, T> = {};
  for (const r of rows) byId[store.key(r)] = r;
  store.use.setState({ byId, loaded: true, loading: false, error: null, stale: false, snapshotAt: null });
}

/** Every Connect call answers an empty message (lists stay as seeded). */
export const fakeFetch = vi.fn(async (input: RequestInfo | URL) => {
  const url = String(input);
  if (url.includes('WatchEvents'))
    return new Response(new ReadableStream(), { status: 200, headers: { 'Content-Type': 'application/connect+json' } });
  return new Response('{}', { status: 200, headers: { 'Content-Type': 'application/json' } });
}) as unknown as typeof fetch;

export function signIn(rights: Rights): void {
  setApi(makeHubApi({ getCsrf: () => 'c', fetch: fakeFetch }));
  useSession.setState({
    status: 'signed-in',
    config: { mode: rights.mode, hub: 'https://hub.example:8443' },
    me: null,
    rights,
  });
}

export function resetAll(): void {
  for (const st of [nodesStore, agentsStore, jobsStore, grantsStore, channelsStore] as EntityStore<unknown>[]) seed(st, []);
  useInspector.getState().close();
  useSession.setState({ status: 'booting', me: null, config: null, rights: anonymousRights });
}
