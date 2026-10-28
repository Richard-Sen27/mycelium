import { render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter } from 'react-router';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { GrantState } from '../../gen/mycel/v1/grant_pb';
import type { CommandContext } from '../../app/commands/registry';
import { grantsStore, nodesStore } from '../../app/events/stores';
import { useInspector } from '../../app/shell/inspector';
import { ADMIN, MEMBER, NODE_RO, grant, node, resetAll, seed, signIn } from '../nodes/fixtures.test-utils';
import { grantCommands } from './commands';
import { GrantDetail } from './GrantDetail';
import { buildRequestGrant, emptyGrantForm, validateGrantForm, type GrantForm } from './grants';
import { Component as GrantsView, filterGrants } from './index';

const ctx = (rights: CommandContext['rights']): CommandContext => ({ rights, navigate: () => undefined });

function seedGrants() {
  seed(nodesStore, [node('alpha'), node('beta', { ownerId: 'u_bob' })]);
  seed(grantsStore, [
    grant('grt_A', { expiresAt: new Date(Date.now() + 3600_000).toISOString() }),
    grant('grt_P', { state: GrantState.PENDING, node: 'beta', spec: { kind: { case: 'gpu', value: { device: 0, exclusive: true } } } }),
    grant('grt_D', { state: GrantState.DENIED, ownerId: 'u_bob', requestedBy: 'u_bob' }),
  ]);
}

describe('grants view', () => {
  beforeEach(() => {
    signIn(ADMIN);
    seedGrants();
  });
  afterEach(resetAll);

  it('renders rows, filters pending and opens a grant from the keyboard', async () => {
    const user = userEvent.setup();
    render(
      <MemoryRouter>
        <GrantsView />
      </MemoryRouter>,
    );
    const table = () => screen.getByRole('table', { name: 'Grants' });
    expect(within(table()).getAllByRole('row')).toHaveLength(4);
    await user.click(within(screen.getByRole('group', { name: 'Grant filters' })).getByRole('button', { name: /^pending/ }));
    const rows = within(table()).getAllByRole('row');
    expect(rows).toHaveLength(2);
    expect(rows[1]).toHaveTextContent('gpu');
    rows[1].focus();
    await user.keyboard(' ');
    expect(useInspector.getState().key).toBe('grant:grt_P');
  });

  it('filterGrants: mine and node', () => {
    const all = grantsStore.list();
    expect(filterGrants(all, '', ['mine'], '', { userId: 'u_bob' }).map((g) => g.id)).toEqual(['grt_D']);
    expect(filterGrants(all, '', [], 'beta', { userId: null }).map((g) => g.id)).toEqual(['grt_P']);
    expect(filterGrants(all, '', ['finished'], '', { userId: null }).map((g) => g.id)).toEqual(['grt_D']);
  });

  it('shows the decision buttons only where the caller can decide', () => {
    const { unmount } = render(
      <MemoryRouter>
        <GrantDetail id="grt_P" />
      </MemoryRouter>,
    );
    expect(screen.getByRole('button', { name: /Approve/ })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Deny' })).toBeInTheDocument();
    unmount();
    // A member who does not own node alpha cannot revoke grt_A there.
    signIn(MEMBER);
    render(
      <MemoryRouter>
        <GrantDetail id="grt_A" />
      </MemoryRouter>,
    );
    expect(screen.queryByRole('button', { name: 'Revoke' })).toBeNull();
    expect(screen.getByText('llama3.1:8b')).toBeInTheDocument();
  });
});

describe('grant commands', () => {
  beforeEach(seedGrants);
  afterEach(resetAll);

  it('offers request and revoke to writers, nothing read-only', () => {
    const ids = grantCommands(ctx(ADMIN)).map((c) => c.id);
    expect(ids).toEqual(['grant.request', 'grant.revoke.grt_A']);
    expect(grantCommands(ctx(MEMBER)).map((c) => c.id)).toEqual(['grant.request']);
    expect(grantCommands(ctx(NODE_RO))).toEqual([]);
  });
});

describe('request grant form', () => {
  const base = (kind: GrantForm['kind'], o: Partial<GrantForm> = {}): GrantForm => ({ ...emptyGrantForm(kind, 'agent:coder@alpha'), ...o });

  it('builds an ollama request', () => {
    const r = buildRequestGrant(base('ollama', { models: 'llama3.1:8b, qwen2.5', allowPull: true, channel: 'backend', reason: 'eval' }));
    expect(r.subject).toBe('agent:coder@alpha');
    expect(r.duration).toBe('1h');
    expect(r.channel).toBe('#backend');
    expect(r.reason).toBe('eval');
    expect(r.spec?.kind.case).toBe('ollama');
    if (r.spec?.kind.case === 'ollama') {
      expect(r.spec.kind.value.models).toEqual(['llama3.1:8b', 'qwen2.5']);
      expect(r.spec.kind.value.allowPull).toBe(true);
    }
  });

  it('builds gpu, docker-host, mount and egress requests', () => {
    const gpu = buildRequestGrant(base('gpu', { device: '1', exclusive: true })).spec?.kind;
    expect(gpu).toEqual(expect.objectContaining({ case: 'gpu' }));
    if (gpu?.case === 'gpu') expect([gpu.value.device, gpu.value.exclusive]).toEqual([1, true]);

    const dh = buildRequestGrant(base('docker-host', { images: 'python:3.12\nghcr.io/acme/' })).spec?.kind;
    if (dh?.case !== 'dockerHost') throw new Error('want dockerHost');
    expect(dh.value.images).toEqual(['python:3.12', 'ghcr.io/acme/']);

    const mnt = buildRequestGrant(base('mount', { hostPath: '/data', guestPath: '/mnt/data', readWrite: false })).spec?.kind;
    if (mnt?.case !== 'mount') throw new Error('want mount');
    expect(mnt.value).toEqual(expect.objectContaining({ hostPath: '/data', guestPath: '/mnt/data', readOnly: true }));

    const eg = buildRequestGrant(base('egress', { allow: 'pypi.org, *.github.com:443' })).spec?.kind;
    if (eg?.case !== 'egress') throw new Error('want egress');
    expect(eg.value.allow).toEqual(['pypi.org', '*.github.com:443']);
  });

  it('validates like the CLI', () => {
    expect(validateGrantForm(base('ollama'))).toEqual({});
    expect(validateGrantForm(base('ollama', { subject: '' })).subject).toBeTruthy();
    expect(validateGrantForm(base('ollama', { duration: '25h' })).duration).toMatch(/24h/);
    expect(validateGrantForm(base('ollama', { duration: 'soon' })).duration).toBeTruthy();
    expect(validateGrantForm(base('docker-host')).images).toBeTruthy();
    expect(validateGrantForm(base('mount', { hostPath: '/x' })).guestPath).toBeTruthy();
    expect(validateGrantForm(base('egress')).allow).toBeTruthy();
    expect(validateGrantForm(base('gpu', { device: '-1' })).device).toBeTruthy();
  });
});
