import { errText } from '../channels/model';
import { useEffect } from 'react';
import { create } from 'zustand';
import { toast } from '../../ds';
import type { Grant } from '../../gen/mycel/v1/grant_pb';
import type { Agent } from '../../gen/mycel/v1/hub_pb';
import type { Node } from '../../gen/mycel/v1/node_pb';
import { getApi } from '../../app/api';
import { canDecide } from '../../app/commands/core';
import type { Rights } from '../../app/session';

export { errText };

// Helpers shared by the nodes, agents, grants and constellation views:
// write rights (mirroring internal/policy), action wrappers and user names.

/** hubWrite: the caller can write at all (hub mode, not the node's read-only proxy). */
export function hubWrite(r: Rights): boolean {
  return r.mode === 'hub' && !r.readOnly;
}

/** canManageNode: set caps, set sharing, remove (admins and the node's owner). */
export function canManageNode(r: Rights, n: Pick<Node, 'ownerId'> | undefined): boolean {
  if (!hubWrite(r) || !n) return false;
  return r.isAdmin || (!!r.userId && n.ownerId === r.userId);
}

/** canManageAgent: pause, resume, stop, budget, revoke (admins and the agent's owner). */
export function canManageAgent(r: Rights, a: Pick<Agent, 'ownerId'> | undefined): boolean {
  if (!hubWrite(r) || !a) return false;
  return r.isAdmin || (!!r.userId && a.ownerId === r.userId);
}

/** canRevokeGrant: whoever may decide it, and the owner of its subject. */
export function canRevokeGrant(r: Rights, g: Grant): boolean {
  if (!hubWrite(r)) return false;
  return canDecide(r, g) || (!!r.userId && g.ownerId === r.userId);
}

/**
 * act runs a write RPC, toasts the outcome and rethrows failures so a
 * form or confirm dialog can stay open. The stores update from the event
 * stream; nothing is patched here.
 */
export async function act(fn: () => Promise<unknown>, ok: string, body?: string): Promise<void> {
  try {
    await fn();
    toast({ title: ok, body });
  } catch (err) {
    toast({ title: 'Failed', body: errText(err), tone: 'alert' });
    throw err;
  }
}

// ---- User names (owner columns) ----

interface UsersState {
  names: Record<string, string>;
  state: 'idle' | 'loading' | 'done';
  load(): void;
}

export const useUsers = create<UsersState>((set, get) => ({
  names: {},
  state: 'idle',
  load() {
    if (get().state !== 'idle') return;
    set({ state: 'loading' });
    getApi()
      .rpc.users.listUsers({})
      .then((r) => set({ names: Object.fromEntries(r.users.map((u) => [u.id, u.name])), state: 'done' }))
      .catch(() => set({ state: 'done' }));
  },
}));

/** useUserName maps a user id to a display name ("you" for the caller); falls back to the id. */
export function useUserNames(rights: Rights): (id: string) => string {
  const names = useUsers((s) => s.names);
  useEffect(() => {
    if (rights.mode === 'hub') useUsers.getState().load();
  }, [rights.mode]);
  return (id: string) => {
    if (!id) return '—';
    if (rights.userId && id === rights.userId) return `${rights.userName ?? names[id] ?? id} (you)`;
    return names[id] ?? id;
  };
}

/** agentShort: "agent:coder@gpu-box" → "coder@gpu-box". */
export function agentShort(id: string): string {
  return id.startsWith('agent:') ? id.slice(6) : id;
}
