import { create } from '@bufbuild/protobuf';
import { render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { createMemoryRouter, RouterProvider } from 'react-router';
import { afterEach, describe, expect, it } from 'vitest';
import { MessageBody, tokenizeBody } from '../../ds';
import { channelsStore, useFreeze, useMessages, agentsStore } from '../../app/events/stores';
import { ChannelSchema, ChannelType } from '../../gen/mycel/v1/channel_pb';
import { FreezeStatusResponseSchema } from '../../gen/mycel/v1/grant_pb';
import { AgentSchema } from '../../gen/mycel/v1/hub_pb';
import { Component } from './index';
import { fakeServer, resetAll, seed, signIn } from '../test-utils/fakeServer';
import { groupThreads } from './model';
import { MessageSchema, MessageType } from '../../gen/mycel/v1/envelope_pb';

const XSS = '<img src=x onerror="window.__pwned=1"> see https://evil.example/x and @coder';

function msgJson(over: Record<string, unknown>) {
  return {
    channel: '#backend',
    type: 'MESSAGE_TYPE_CHAT',
    ts: new Date(Date.now() - 120_000).toISOString(),
    meta: { provenance: { origin: 'agent', untrusted: true, node: 'mac' }, traceId: 'tr_1' },
    ...over,
  };
}

function renderChannels(path: string) {
  const router = createMemoryRouter([{ path: '/channels/*', Component }], { initialEntries: [path] });
  return render(<RouterProvider router={router} />);
}

function seedChannel(over: Partial<{ archivedAt: string; type: ChannelType }> = {}) {
  seed(channelsStore, [
    create(ChannelSchema, {
      name: '#backend',
      type: ChannelType.TOPIC,
      ownerId: 'u1',
      members: [
        { id: 'agent:coder@mac', role: 'agent' },
        { id: 'user:u1', role: 'owner' },
      ],
      lastSeq: 3n,
      ...over,
    }),
  ]);
  seed(agentsStore, [create(AgentSchema, { id: 'agent:coder@mac', name: 'coder', nodeName: 'mac', state: 'running' })]);
}

describe('message bodies', () => {
  it('render an untrusted body with markup as plain text', () => {
    const { container } = render(<MessageBody text={XSS} untrusted />);
    expect(container.querySelector('img')).toBeNull();
    expect(container.querySelector('a')).toBeNull();
    expect(container.textContent).toContain('<img src=x onerror="window.__pwned=1">');
    expect(container.querySelector('blockquote[data-untrusted]')).not.toBeNull();
    expect((window as unknown as { __pwned?: number }).__pwned).toBeUndefined();
  });

  it('highlight mentions and links without making links clickable', () => {
    expect(tokenizeBody('ping @coder at https://x.example/a, mail a@b.c').map((t) => t.kind)).toEqual([
      'text',
      'mention',
      'text',
      'link',
      'text',
    ]);
  });
});

describe('threads', () => {
  it('fold replies under their root in seq order', () => {
    const m = (id: string, seq: bigint, thread = '') => create(MessageSchema, { id, seq, thread, channel: '#c', type: MessageType.CHAT });
    const groups = groupThreads([m('c', 3n, 'a'), m('a', 1n), m('b', 2n), m('d', 4n, 'thr_x'), m('e', 5n, 'thr_x')]);
    expect(groups.map((g) => [g.root.id, g.replies.map((r) => r.id)])).toEqual([
      ['a', ['c']],
      ['b', []],
      ['d', ['e']],
    ]);
  });
});

describe('channels view', () => {
  afterEach(() => resetAll());

  it('shows history with an untrusted agent body as quoted text and threads it', async () => {
    const server = fakeServer({
      'ChannelService/ListMessages': () => ({
        messages: [
          msgJson({ id: 'msg_1', seq: '1', from: 'agent:coder@mac', payload: { text: XSS } }),
          msgJson({
            id: 'msg_2',
            seq: '2',
            from: 'user:u1',
            thread: 'msg_1',
            payload: { text: 'thanks' },
            meta: { provenance: { origin: 'human' } },
          }),
        ],
      }),
    });
    signIn(server, { admin: false });
    seedChannel();
    useFreeze.setState({ status: create(FreezeStatusResponseSchema, {}), loaded: true });
    const { container } = renderChannels('/channels/%23backend');
    const log = await screen.findByRole('log', { name: 'Messages in #backend' });
    await within(log).findByText(/onerror/);
    expect(container.querySelector('img')).toBeNull();
    expect(within(log).getByText('1 reply', { exact: false })).toBeInTheDocument();
    expect(within(log).getByText('you')).toBeInTheDocument();
    expect(within(log).getByText('untrusted')).toBeInTheDocument();
    // The composer is enabled for a member of a live channel.
    expect(screen.getByRole('combobox', { name: 'Message #backend' })).not.toBeDisabled();
  });

  it('disables the composer with a reason when the channel is frozen', async () => {
    signIn(fakeServer({ 'ChannelService/ListMessages': () => ({ messages: [] }) }));
    seedChannel();
    useFreeze.setState({ status: create(FreezeStatusResponseSchema, { frozenChannels: ['#backend'] }), loaded: true });
    renderChannels('/channels/%23backend');
    const box = await screen.findByRole('textbox', { name: 'Message #backend' });
    expect(box).toBeDisabled();
    expect(screen.getByRole('note')).toHaveTextContent('This channel is frozen');
    expect(screen.queryByRole('button', { name: 'Post' })).toBeNull();
  });

  it('disables the composer for a read-only node UI and hides channel actions', async () => {
    signIn(fakeServer({ 'ChannelService/ListMessages': () => ({ messages: [] }) }), { mode: 'node' });
    seedChannel();
    useFreeze.setState({ status: create(FreezeStatusResponseSchema, {}), loaded: true });
    renderChannels('/channels/%23backend');
    expect(await screen.findByRole('textbox', { name: 'Message #backend' })).toBeDisabled();
    expect(screen.getByRole('note')).toHaveTextContent('Read-only');
    expect(screen.queryByRole('button', { name: 'Freeze' })).toBeNull();
    expect(screen.queryByRole('button', { name: 'Policy' })).toBeNull();
  });

  it('posts with mentions resolved to agent ids and a reply-to', async () => {
    const user = userEvent.setup();
    const server = fakeServer({
      'ChannelService/ListMessages': () => ({
        messages: [msgJson({ id: 'msg_1', seq: '1', from: 'agent:coder@mac', payload: { text: 'done' } })],
      }),
      'ChannelService/Post': () => ({}),
    });
    signIn(server);
    seedChannel();
    useFreeze.setState({ status: create(FreezeStatusResponseSchema, {}), loaded: true });
    renderChannels('/channels/%23backend');
    const log = await screen.findByRole('log', { name: 'Messages in #backend' });
    await within(log).findByText('done');
    await user.click(within(log).getByRole('button', { name: 'Reply' }));
    const box = screen.getByRole('combobox', { name: 'Message #backend' });
    await user.type(box, '@co');
    expect(box).toHaveAttribute('aria-expanded', 'true');
    expect(screen.getByRole('option', { name: /@coder/ })).toBeInTheDocument();
    await user.keyboard('{Enter}looks good{Enter}');
    const sent = server.bodies('ChannelService/Post');
    expect(sent).toHaveLength(1);
    expect(sent[0]).toMatchObject({ channel: '#backend', text: '@coder looks good', mentions: ['agent:coder@mac'], replyTo: 'msg_1' });
    expect(useMessages.getState().byChannel['#backend']).toHaveLength(1);
  });
});
