import { render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter } from 'react-router';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import type { CommandContext } from '../../app/commands/registry';
import { nodesStore } from '../../app/events/stores';
import { useInspector } from '../../app/shell/inspector';
import { nodeCommands } from './commands';
import { ADMIN, MEMBER, NODE_RO, node, resetAll, seed, signIn } from './fixtures.test-utils';
import { Component as NodesView, filterNodes } from './index';
import { buildSharing } from './NodeForms';

const ctx = (rights: CommandContext['rights']): CommandContext => ({ rights, navigate: () => undefined });

describe('nodes view', () => {
  beforeEach(() => {
    signIn(ADMIN);
    seed(nodesStore, [
      node('alpha', { cpu: 16, memGiB: 64 }),
      node('beta', { online: false, ownerId: 'u_bob' }),
      node('gamma', { capsEnabled: { names: ['gpu', 'sandbox'] } }),
    ]);
  });
  afterEach(resetAll);

  it('renders a row per node and filters by text and chips', async () => {
    const user = userEvent.setup();
    render(
      <MemoryRouter>
        <NodesView />
      </MemoryRouter>,
    );
    const table = screen.getByRole('table', { name: 'Nodes' });
    expect(within(table).getAllByRole('row')).toHaveLength(4); // header + 3
    expect(within(table).getByText('alpha')).toBeInTheDocument();

    await user.type(screen.getByRole('textbox', { name: 'Filter nodes' }), 'gam');
    expect(within(table).getAllByRole('row')).toHaveLength(2);
    await user.clear(screen.getByRole('textbox', { name: 'Filter nodes' }));

    await user.click(within(screen.getByRole('group', { name: 'Node filters' })).getByRole('button', { name: /offline/ }));
    const rows = within(screen.getByRole('table', { name: 'Nodes' })).getAllByRole('row');
    expect(rows).toHaveLength(2);
    expect(rows[1]).toHaveTextContent('beta');
  });

  it('opens the inspector from the keyboard', async () => {
    const user = userEvent.setup();
    render(
      <MemoryRouter>
        <NodesView />
      </MemoryRouter>,
    );
    const rows = within(screen.getByRole('table', { name: 'Nodes' })).getAllByRole('row');
    rows[1].focus();
    await user.keyboard('{ArrowDown}{Enter}');
    expect(useInspector.getState().open).toBe(true);
    expect(useInspector.getState().key).toBe('node:beta');
  });

  it('filterNodes: mine and capability chips', () => {
    const rows = nodesStore.list();
    expect(filterNodes(rows, '', ['mine'], { userId: 'u_bob' }).map((n) => n.name)).toEqual(['beta']);
    expect(filterNodes(rows, '', ['gpu'], { userId: null }).map((n) => n.name)).toEqual(['gamma']);
    expect(filterNodes(rows, 'ALP', [], { userId: null }).map((n) => n.name)).toEqual(['alpha']);
  });
});

describe('node commands', () => {
  beforeEach(() => {
    seed(nodesStore, [node('alpha'), node('beta', { ownerId: 'u_bob' })]);
  });
  afterEach(resetAll);

  it('offers every node to an admin', () => {
    const ids = nodeCommands(ctx(ADMIN)).map((c) => c.id);
    expect(ids).toEqual(expect.arrayContaining(['node.caps.alpha', 'node.sharing.alpha', 'node.remove.alpha', 'node.remove.beta']));
    expect(nodeCommands(ctx(ADMIN)).find((c) => c.id === 'node.remove.alpha')?.confirm).toBeTruthy();
  });

  it('offers a member only their own nodes', () => {
    const ids = nodeCommands(ctx(MEMBER)).map((c) => c.id);
    expect(ids).toContain('node.caps.beta');
    expect(ids.some((id) => id.endsWith('.alpha'))).toBe(false);
  });

  it('hides everything in read-only node mode', () => {
    expect(nodeCommands(ctx(NODE_RO))).toEqual([]);
    expect(nodeCommands(ctx({ ...ADMIN, readOnly: true }))).toEqual([]);
  });
});

describe('node detail', () => {
  afterEach(resetAll);

  it('hides the admin buttons in read-only mode and shows them to the owner', async () => {
    seed(nodesStore, [node('beta', { ownerId: 'u_bob' })]);
    const { NodeDetail } = await import('./NodeDetail');
    signIn(NODE_RO);
    const { unmount } = render(
      <MemoryRouter>
        <NodeDetail name="beta" />
      </MemoryRouter>,
    );
    expect(screen.queryByRole('button', { name: 'Remove' })).toBeNull();
    unmount();
    signIn(MEMBER);
    render(
      <MemoryRouter>
        <NodeDetail name="beta" />
      </MemoryRouter>,
    );
    expect(screen.getByRole('button', { name: 'Remove' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Set caps' })).toBeInTheDocument();
  });
});

describe('sharing form', () => {
  it('builds SharingSettings with a schedule only when scheduled', () => {
    const s = buildSharing({
      offer: ['docker'],
      when: 'schedule',
      schedule: [{ days: ['sat', 'sun'], from: '00:00', to: '23:59' }],
      localFirst: true,
      maxJobs: '2',
      idleAfter: '10',
    });
    expect(s.offer?.names).toEqual(['docker']);
    expect(s.schedule).toHaveLength(1);
    expect(s.maxJobs).toBe(2);
    expect(s.idleAfterMinutes).toBe(0);
    const idle = buildSharing({
      offer: [],
      when: 'idle',
      schedule: [{ days: ['mon'], from: '1', to: '2' }],
      localFirst: false,
      maxJobs: '',
      idleAfter: '15',
    });
    expect(idle.schedule).toHaveLength(1); // idle AND inside the window
    expect(idle.idleAfterMinutes).toBe(15);
  });
});
