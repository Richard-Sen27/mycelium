import { create } from '@bufbuild/protobuf';
import { parseTime, type Tone } from '../../ds';
import { PostRequestSchema, type PostRequest } from '../../gen/mycel/v1/channel_pb';
import { MessageType, type Message } from '../../gen/mycel/v1/envelope_pb';
import type { Agent } from '../../gen/mycel/v1/hub_pb';
import { nodesStore } from '../../app/events/stores';
import type { Rights } from '../../app/session';
import { canManageAgent, canManageNode } from '../nodes/shared';

/** canSpawn: a live orchestrator, asked by an admin, its owner or the owner of its node. */
export function canSpawn(r: Rights, a: Agent): boolean {
  if (agentKind(a) !== 'orchestrator' || agentState(a) === 'stopped') return false;
  const node = nodesStore.use.getState().byId[a.nodeName];
  return canManageAgent(r, a) || canManageNode(r, node);
}

// Pure agent helpers shared by the agents table, the inspector, the forms
// and the constellation.

export type AgentKind = 'orchestrator' | 'worker';

export function agentKind(a: Pick<Agent, 'parent'>): AgentKind {
  return a.parent ? 'worker' : 'orchestrator';
}

/** agentState: running, paused or stopped (an exited agent counts as stopped). */
export function agentState(a: Pick<Agent, 'state' | 'exitedAt'>): 'running' | 'paused' | 'stopped' {
  if (a.exitedAt || a.state === 'stopped') return 'stopped';
  return a.state === 'paused' ? 'paused' : 'running';
}

export function agentTone(a: Pick<Agent, 'state' | 'exitedAt'>): Tone {
  const st = agentState(a);
  return st === 'running' ? 'ok' : st === 'paused' ? 'alert' : 'off';
}

const TOKEN_TTL_MS = 3600_000;

/**
 * lastSeen estimates when the agent was last active. The row has no
 * heartbeat; the agent token is refreshed hourly while it runs, so
 * "token expiry − 1 h" is the last refresh. The newest message from the
 * agent (when loaded) and the exit time are used when later.
 */
export function lastSeen(a: Agent, messages: Message[] = []): string {
  let best = parseTime(a.createdAt);
  let at = a.createdAt;
  const consider = (t: number, iso: string) => {
    if (!Number.isNaN(t) && (Number.isNaN(best) || t > best)) {
      best = t;
      at = iso;
    }
  };
  const exp = parseTime(a.tokenExpiresAt);
  if (!Number.isNaN(exp) && !a.exitedAt) {
    const refreshed = Math.min(exp - TOKEN_TTL_MS, Date.now());
    consider(refreshed, new Date(refreshed).toISOString());
  }
  consider(parseTime(a.exitedAt), a.exitedAt);
  for (const m of messages) if (m.from === a.id) consider(parseTime(m.ts), m.ts);
  return at;
}

/** budgetFrac: the larger of tokens and usd used against a budget (0 when unlimited). */
export function budgetFrac(a: Pick<Agent, 'budget' | 'used'>): number {
  const t = a.budget?.tokens ? Number(a.used?.tokens ?? 0n) / Number(a.budget.tokens) : 0;
  const u = a.budget?.usd ? (a.used?.usd ?? 0) / a.budget.usd : 0;
  return Math.max(t, u);
}

export function hasBudget(a: Pick<Agent, 'budget'>): boolean {
  return !!a.budget && (a.budget.tokens > 0n || a.budget.usd > 0);
}

export const byNameNode = (a: Agent, b: Agent) => a.name.localeCompare(b.name) || a.nodeName.localeCompare(b.nodeName);

// ---- Spawn a worker ----

export const HARNESSES = ['claude-code', 'codex', 'gemini-cli', 'fake'] as const;

export interface SpawnForm {
  harness: string;
  task: string;
  channel: string;
  budgetTokens: string;
  budgetUsd: string;
}

/**
 * buildSpawnPost: the hub has no human-facing spawn RPC (workers are
 * started by an orchestrator session on its own node, `spawn_agent` with
 * target self). The UI therefore asks the orchestrator: a TASK message in a
 * direct message that names the spawn_agent arguments.
 */
export function buildSpawnPost(orchestrator: Pick<Agent, 'id'>, f: SpawnForm): PostRequest {
  const ch = f.channel.trim().startsWith('#') ? f.channel.trim() : `#${f.channel.trim()}`;
  const budget: string[] = [];
  if (f.budgetTokens.trim()) budget.push(`budget_tokens=${Number(f.budgetTokens)}`);
  if (f.budgetUsd.trim()) budget.push(`budget_usd=${Number(f.budgetUsd)}`);
  const text = [
    `Please spawn a worker: spawn_agent harness=${f.harness} channel=${ch}${budget.length ? ' ' + budget.join(' ') : ''}`,
    '',
    f.task.trim(),
  ].join('\n');
  return create(PostRequestSchema, {
    channel: `@${orchestrator.id}`,
    text,
    mentions: [orchestrator.id],
    type: MessageType.TASK,
    tags: ['spawn'],
  });
}
