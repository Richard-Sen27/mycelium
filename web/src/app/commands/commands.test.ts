import { create } from '@bufbuild/protobuf';
import { beforeAll, beforeEach, describe, expect, it } from 'vitest';
import { FreezeStatusResponseSchema, GrantSchema, GrantState } from '../../gen/mycel/v1/grant_pb';
import { AgentSchema } from '../../gen/mycel/v1/hub_pb';
import { NodeSchema } from '../../gen/mycel/v1/node_pb';
import { grantsStore, nodesStore, useFreeze } from '../events/stores';
import type { Rights } from '../session';
import { findOrchestrator, registerCoreCommands } from './core';
import { fuzzyMatch, rank } from './fuzzy';
import { availableCommands, registerCommands, searchCommands, type CommandContext } from './registry';

const ctx = (r: Partial<Rights>): CommandContext => ({
  rights: { mode: 'hub', isAdmin: false, readOnly: false, userId: 'u-member', userName: 'm', ...r },
  navigate: () => undefined,
});
const admin = ctx({ isAdmin: true, userId: 'u-admin' });
const member = ctx({});
const nodeUI = ctx({ mode: 'node', readOnly: true });
const ids = (c: CommandContext) => availableCommands(c).map((x) => x.id);

describe('fuzzy matching', () => {
  it('prefers substrings, word starts and early matches', () => {
    expect(fuzzyMatch('appr', 'Go to Approvals')).not.toBeNull();
    expect(fuzzyMatch('gta', 'Go to Approvals')).not.toBeNull();
    expect(fuzzyMatch('xyz', 'Go to Approvals')).toBeNull();
    const r = rank('fr', [{ t: 'Go to Traces' }, { t: 'Freeze the network' }], (x) => [x.t]);
    expect(r[0].item.t).toBe('Freeze the network');
  });

  it('returns highlight positions for the title', () => {
    expect(fuzzyMatch('net', 'Freeze the network')?.positions).toEqual([11, 12, 13]);
  });

  it('scores keyword matches below title matches', () => {
    const r = rank('chat', [{ t: 'Go to Channels', k: ['chat'] }, { t: 'chat bridge' }], (x) => [x.t, ...(x.k ?? [])]);
    expect(r[0].item.t).toBe('chat bridge');
  });
});

describe('command registry', () => {
  beforeAll(() => registerCoreCommands());
  beforeEach(() => {
    nodesStore.reset();
    grantsStore.reset();
    useFreeze.setState({ status: create(FreezeStatusResponseSchema, { allFrozen: false }), loaded: true });
  });

  it('offers navigation in both modes, "This machine" only on a node', () => {
    expect(ids(member)).toContain('nav.approvals');
    expect(ids(member)).not.toContain('nav.machine');
    expect(ids(nodeUI)).toContain('nav.machine');
  });

  it('shows freeze to admins only, and unfreeze only while frozen', () => {
    expect(ids(admin)).toContain('network.freeze');
    expect(ids(admin)).not.toContain('network.unfreeze');
    expect(ids(member)).not.toContain('network.freeze');
    expect(ids(nodeUI)).not.toContain('network.freeze');
    useFreeze.setState({ status: create(FreezeStatusResponseSchema, { allFrozen: true }) });
    expect(ids(admin)).toContain('network.unfreeze');
    expect(ids(admin)).not.toContain('network.freeze');
    const freeze = availableCommands(admin).find((c) => c.id === 'network.unfreeze');
    expect(freeze?.confirm).toBeDefined();
  });

  it('offers approve/deny to admins and the node owner only', () => {
    nodesStore.upsert(create(NodeSchema, { name: 'gpu-box', ownerId: 'u-member' }));
    nodesStore.upsert(create(NodeSchema, { name: 'other', ownerId: 'u-someone' }));
    grantsStore.upsert(create(GrantSchema, { id: 'grt_1', node: 'gpu-box', subject: 'agent:a@x', state: GrantState.PENDING }));
    grantsStore.upsert(create(GrantSchema, { id: 'grt_2', node: 'other', subject: 'agent:b@x', state: GrantState.PENDING }));
    grantsStore.upsert(create(GrantSchema, { id: 'grt_3', node: 'gpu-box', state: GrantState.ACTIVE }));
    expect(ids(admin)).toEqual(expect.arrayContaining(['grant.approve.grt_1', 'grant.deny.grt_1', 'grant.approve.grt_2']));
    expect(ids(admin)).not.toContain('grant.approve.grt_3');
    expect(ids(member)).toContain('grant.approve.grt_1');
    expect(ids(member)).not.toContain('grant.approve.grt_2');
    expect(ids(nodeUI).some((i) => i.startsWith('grant.'))).toBe(false);
    // The deny command confirms first.
    expect(availableCommands(admin).find((c) => c.id === 'grant.deny.grt_1')?.confirm?.tone).toBe('alert');
  });

  it('finds approve commands by grant id', () => {
    nodesStore.upsert(create(NodeSchema, { name: 'gpu-box', ownerId: 'u-member' }));
    grantsStore.upsert(create(GrantSchema, { id: 'grt_01J9ZZ', node: 'gpu-box', state: GrantState.PENDING }));
    const top = searchCommands('approve grt_01j9', admin)[0];
    expect(top.item.id).toBe('grant.approve.grt_01J9ZZ');
  });

  it('lets views register and unregister commands with predicates', () => {
    const off = registerCommands([
      { id: 'test.admin', title: 'Admin thing', allowed: (c) => c.rights.isAdmin, run: () => undefined },
      { id: 'test.all', title: 'Anyone thing', run: () => undefined },
    ]);
    expect(ids(admin)).toEqual(expect.arrayContaining(['test.admin', 'test.all']));
    expect(ids(member)).toContain('test.all');
    expect(ids(member)).not.toContain('test.admin');
    off();
    expect(ids(admin)).not.toContain('test.all');
  });
});

describe('orchestrator lookup', () => {
  it('picks the caller’s live top-level agent', () => {
    const agents = [
      create(AgentSchema, { id: 'agent:w1@mac', name: 'w1', ownerId: 'u1', parent: 'agent:orch@mac', state: 'running' }),
      create(AgentSchema, { id: 'agent:old@mac', name: 'old', ownerId: 'u1', state: 'stopped', exitedAt: '2026-01-01T00:00:00Z' }),
      create(AgentSchema, { id: 'agent:orch@mac', name: 'orch', ownerId: 'u1', state: 'running' }),
      create(AgentSchema, { id: 'agent:theirs@pc', name: 'theirs', ownerId: 'u2', state: 'running' }),
    ];
    expect(findOrchestrator(agents, 'u1')?.id).toBe('agent:orch@mac');
    expect(findOrchestrator(agents, 'u3')).toBeUndefined();
    expect(findOrchestrator(agents, null)).toBeUndefined();
  });
});
