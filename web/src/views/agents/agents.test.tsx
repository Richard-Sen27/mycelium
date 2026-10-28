import { render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter } from 'react-router';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { MessageType } from '../../gen/mycel/v1/envelope_pb';
import type { CommandContext } from '../../app/commands/registry';
import { agentsStore, nodesStore } from '../../app/events/stores';
import { useInspector } from '../../app/shell/inspector';
import { ADMIN, MEMBER, NODE_RO, agent, node, resetAll, seed, signIn } from '../nodes/fixtures.test-utils';
import { buildSpawnPost } from './agents';
import { agentCommands } from './commands';
import { Component as AgentsView, filterAgents } from './index';

const ctx = (rights: CommandContext['rights']): CommandContext => ({ rights, navigate: () => undefined });

function seedAgents() {
  seed(nodesStore, [node('alpha'), node('beta', { ownerId: 'u_bob' })]);
  seed(agentsStore, [
    agent('lead', 'alpha', { homeChannel: '#backend' }),
    agent('coder', 'alpha', {
      parent: 'agent:lead@alpha',
      homeChannel: '#backend',
      used: { tokens: 1200n, usd: 0.5 },
      budget: { tokens: 10000n, usd: 0 },
    }),
    agent('helper', 'beta', { ownerId: 'u_bob', state: 'paused', pausedReason: 'budget: agent' }),
    agent('old', 'beta', { ownerId: 'u_bob', state: 'stopped', exitedAt: '2026-10-28T11:00:00Z' }),
  ]);
}

describe('agents view', () => {
  beforeEach(() => {
    signIn(ADMIN);
    seedAgents();
  });
  afterEach(resetAll);

  it('renders rows and filters by state and kind', async () => {
    const user = userEvent.setup();
    render(
      <MemoryRouter>
        <AgentsView />
      </MemoryRouter>,
    );
    const table = () => screen.getByRole('table', { name: 'Agents' });
    expect(within(table()).getAllByRole('row')).toHaveLength(5);
    expect(within(table()).getByText(/budget: agent/)).toBeInTheDocument();
    const chips = screen.getByRole('group', { name: 'Agent filters' });
    await user.click(within(chips).getByRole('button', { name: /^paused/ }));
    expect(within(table()).getAllByRole('row')).toHaveLength(2);
    await user.click(within(chips).getByRole('button', { name: /^paused/ }));
    await user.click(within(chips).getByRole('button', { name: /workers/ }));
    const rows = within(table()).getAllByRole('row');
    expect(rows).toHaveLength(2);
    expect(rows[1]).toHaveTextContent('coder');
  });

  it('opens AgentDetail from the keyboard', async () => {
    const user = userEvent.setup();
    render(
      <MemoryRouter>
        <AgentsView />
      </MemoryRouter>,
    );
    const rows = within(screen.getByRole('table', { name: 'Agents' })).getAllByRole('row');
    rows[1].focus();
    await user.keyboard('{Enter}');
    expect(useInspector.getState().key).toMatch(/^agent:agent:/);
  });

  it('filterAgents: mine', () => {
    expect(
      filterAgents(agentsStore.list(), '', ['mine'], { userId: 'u_bob' })
        .map((a) => a.name)
        .sort(),
    ).toEqual(['helper', 'old']);
  });
});

describe('agent commands', () => {
  beforeEach(seedAgents);
  afterEach(resetAll);

  it('gives admins pause, budget, stop, revoke and spawn on orchestrators', () => {
    const ids = agentCommands(ctx(ADMIN)).map((c) => c.id);
    expect(ids).toEqual(
      expect.arrayContaining([
        'agent.pause.agent:lead@alpha',
        'agent.budget.agent:lead@alpha',
        'agent.stop.agent:lead@alpha',
        'agent.revoke.agent:lead@alpha',
        'agent.spawn.agent:lead@alpha',
        'agent.resume.agent:helper@beta',
      ]),
    );
    // Workers do not spawn; stopped agents cannot be paused or stopped.
    expect(ids).not.toContain('agent.spawn.agent:coder@alpha');
    expect(ids).not.toContain('agent.stop.agent:old@beta');
    const revoke = agentCommands(ctx(ADMIN)).find((c) => c.id === 'agent.revoke.agent:lead@alpha');
    expect(revoke?.confirm?.tone).toBe('alert');
  });

  it('gives a member only their own agents, and nothing in read-only mode', () => {
    const ids = agentCommands(ctx(MEMBER)).map((c) => c.id);
    expect(ids.length).toBeGreaterThan(0);
    expect(ids.every((id) => id.includes('@beta'))).toBe(true);
    expect(agentCommands(ctx(NODE_RO))).toEqual([]);
  });
});

describe('spawn request', () => {
  it('asks the orchestrator with a task message naming the spawn_agent arguments', () => {
    const req = buildSpawnPost(
      { id: 'agent:lead@alpha' },
      { harness: 'codex', task: ' fix the flaky test ', channel: 'backend', budgetTokens: '50000', budgetUsd: '' },
    );
    expect(req.channel).toBe('@agent:lead@alpha');
    expect(req.mentions).toEqual(['agent:lead@alpha']);
    expect(req.type).toBe(MessageType.TASK);
    expect(req.text).toContain('spawn_agent harness=codex channel=#backend budget_tokens=50000');
    expect(req.text.endsWith('fix the flaky test')).toBe(true);
  });
});
