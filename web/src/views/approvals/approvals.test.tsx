import { create } from '@bufbuild/protobuf';
import { render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { createMemoryRouter, RouterProvider } from 'react-router';
import { afterEach, describe, expect, it } from 'vitest';
import { parseGoDuration } from '../../ds';
import { grantsStore, nodesStore } from '../../app/events/stores';
import { GrantSchema, GrantState } from '../../gen/mycel/v1/grant_pb';
import { NodeSchema } from '../../gen/mycel/v1/node_pb';
import { fakeServer, resetAll, seed, signIn } from '../test-utils/fakeServer';
import { Component } from './index';
import { durationOptions } from './model';

function seedGrants() {
  seed(nodesStore, [create(NodeSchema, { name: 'gpu-box', ownerId: 'u2' }), create(NodeSchema, { name: 'mac', ownerId: 'u1' })]);
  seed(grantsStore, [
    create(GrantSchema, {
      id: 'grt_A',
      subject: 'agent:trainer@gpu-box',
      node: 'gpu-box',
      requestedBy: 'agent:trainer@gpu-box',
      reason: '<b>please</b>',
      duration: '2h',
      state: GrantState.PENDING,
      createdAt: new Date(Date.now() - 90_000).toISOString(),
      requiresApproval: true,
      spec: { kind: { case: 'gpu', value: { device: 0, exclusive: true } } },
    }),
    create(GrantSchema, {
      id: 'grt_B',
      subject: 'job:job_1',
      node: 'mac',
      requestedBy: 'u1',
      duration: '30m',
      state: GrantState.DENIED,
      decidedBy: 'u9',
      decisionNote: 'not now',
      createdAt: new Date(Date.now() - 900_000).toISOString(),
      requiresApproval: true,
      spec: { kind: { case: 'ollama', value: { models: ['llama3'] } } },
    }),
  ]);
}

function renderAt(path: string) {
  const router = createMemoryRouter([{ path: '/approvals/*', Component }], { initialEntries: [path] });
  return render(<RouterProvider router={router} />);
}

describe('duration presets', () => {
  it('never offer more than was requested', () => {
    for (const req of ['45m', '2h', '1h30m', '24h', '10m']) {
      const opts = durationOptions(req);
      expect(opts[opts.length - 1]).toMatchObject({ value: req, requested: true });
      for (const o of opts) expect(parseGoDuration(o.value)).toBeLessThanOrEqual(parseGoDuration(req));
    }
    expect(durationOptions('2h').map((o) => o.value)).toEqual(['15m', '30m', '1h', '2h']);
  });
});

describe('approvals view', () => {
  afterEach(() => resetAll());

  it('approves with a shortened duration', async () => {
    const user = userEvent.setup();
    const server = fakeServer({ 'GrantService/ApproveGrant': () => ({}) });
    signIn(server, { admin: true });
    seedGrants();
    renderAt('/approvals');
    const card = await screen.findByRole('article', { name: /gpu for agent:trainer@gpu-box/ });
    // The reason is shown as text, never as markup.
    expect(within(card).getByText('<b>please</b>')).toBeInTheDocument();
    // Arrow keys move the selection like native radios.
    within(card).getByRole('radio', { name: /2h/ }).focus();
    await user.keyboard('{ArrowLeft}');
    expect(within(card).getByRole('radio', { name: '1h' })).toHaveAttribute('aria-checked', 'true');
    await user.click(within(card).getByRole('radio', { name: '30m' }));
    await user.click(within(card).getByRole('button', { name: 'Approve 30m' }));
    const sent = server.bodies('GrantService/ApproveGrant') as { id: string; duration: string }[];
    expect(sent).toEqual([{ id: 'grt_A', duration: '30m' }]);
    expect(parseGoDuration(sent[0].duration)).toBeLessThanOrEqual(parseGoDuration('2h'));
  });

  it('approves for the requested duration by default', async () => {
    const user = userEvent.setup();
    const server = fakeServer({ 'GrantService/ApproveGrant': () => ({}) });
    signIn(server, { admin: true });
    seedGrants();
    renderAt('/approvals');
    await user.click(await screen.findByRole('button', { name: 'Approve 2h' }));
    expect(server.bodies('GrantService/ApproveGrant')).toEqual([{ id: 'grt_A', duration: '2h' }]);
  });

  it('denies with the note', async () => {
    const user = userEvent.setup();
    const server = fakeServer({ 'GrantService/DenyGrant': () => ({}) });
    signIn(server, { admin: true });
    seedGrants();
    renderAt('/approvals');
    const card = await screen.findByRole('article', { name: /gpu for/ });
    await user.click(within(card).getByRole('button', { name: 'Deny' }));
    await user.type(await screen.findByLabelText('Note (optional)'), 'use the cpu profile');
    await user.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Deny' }));
    expect(server.bodies('GrantService/DenyGrant')).toEqual([{ id: 'grt_A', note: 'use the cpu profile' }]);
  });

  it('hides the decision buttons from members who do not own the node', async () => {
    signIn(fakeServer(), { admin: false, userId: 'u1' });
    seedGrants();
    renderAt('/approvals');
    const card = await screen.findByRole('article', { name: /gpu for/ });
    expect(within(card).queryByRole('button', { name: /Approve/ })).toBeNull();
    expect(within(card).queryByRole('button', { name: 'Deny' })).toBeNull();
    // The decided history lists the denied grant with its note.
    expect(screen.getByRole('table', { name: 'Decided grants' })).toHaveTextContent('not now');
  });

  it('lets the node owner decide, and selects the card from a push deep link', async () => {
    signIn(fakeServer(), { admin: false, userId: 'u2' });
    seedGrants();
    renderAt('/approvals?grant=grt_A');
    const card = await screen.findByRole('article', { name: /gpu for/ });
    expect(card).toHaveAttribute('data-selected');
    expect(within(card).getByRole('button', { name: 'Approve 2h' })).toBeInTheDocument();
  });
});
