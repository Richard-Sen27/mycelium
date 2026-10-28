import { create } from '@bufbuild/protobuf';
import { render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { createMemoryRouter, RouterProvider } from 'react-router';
import { afterEach, describe, expect, it } from 'vitest';
import { useMessages } from '../../app/events/stores';
import { MessageSchema, MessageType } from '../../gen/mycel/v1/envelope_pb';
import { AuditEntrySchema } from '../../gen/mycel/v1/ui_pb';
import { fakeServer, resetAll, signIn } from '../test-utils/fakeServer';
import { Component } from './index';
import { buildTimeline, replaySummary } from './model';

const T = (s: number) => new Date(Date.UTC(2026, 9, 28, 14, 0, s)).toISOString();

describe('buildTimeline', () => {
  it('orders messages and audit entries by time and computes the latency between steps', () => {
    const msgs = [
      create(MessageSchema, { id: 'msg_2', seq: 2n, from: 'agent:reviewer@mac', ts: T(9), type: MessageType.RESULT }),
      create(MessageSchema, { id: 'msg_1', seq: 1n, from: 'user:u1', ts: T(0), type: MessageType.TASK, to: ['agent:reviewer@mac'] }),
    ];
    const audit = [
      create(AuditEntrySchema, { id: '12', at: T(4), actor: 'agent:reviewer@mac', action: 'grant.request', outcome: 'denied' }),
      create(AuditEntrySchema, { id: '10', at: T(0), actor: 'user:u1', action: 'post', resource: 'channel:#backend', outcome: 'allowed' }),
      create(AuditEntrySchema, { id: '11', at: T(1), actor: 'hub', action: 'agent.inbox', outcome: 'allowed' }),
    ];
    const steps = buildTimeline(msgs, audit);
    expect(steps.map((s) => s.key)).toEqual(['m:msg_1', 'a:10', 'a:11', 'a:12', 'm:msg_2']);
    expect(steps.map((s) => s.latency)).toEqual([null, 0, 1000, 3000, 5000]);
    expect(steps.map((s) => s.offset)).toEqual([0, 0, 1000, 4000, 9000]);
    expect(steps[3].denied).toBe(true);
    expect(steps[0].title).toBe('task → reviewer');
    expect(replaySummary(steps)).toMatchObject({ steps: 5, duration: 9000, denied: 1, slowest: 5000 });
  });

  it('keeps sequence order for steps at the same instant', () => {
    const audit = [
      create(AuditEntrySchema, { id: '7', at: T(0), actor: 'hub', action: 'b' }),
      create(AuditEntrySchema, { id: '6', at: T(0), actor: 'hub', action: 'a' }),
    ];
    expect(buildTimeline([], audit).map((s) => s.key)).toEqual(['a:6', 'a:7']);
  });
});

describe('traces view', () => {
  afterEach(() => resetAll());

  it('replays a trace from the URL with denied steps marked', async () => {
    const server = fakeServer({
      'EventService/ListAudit': () => ({
        entries: [
          {
            id: '21',
            at: T(3),
            actor: 'agent:coder@mac',
            action: 'egress.connect',
            resource: 'job:job_1',
            outcome: 'denied',
            traceId: 'tr_9',
          },
          { id: '20', at: T(1), actor: 'agent:coder@mac', action: 'job.run', resource: 'job:job_1', outcome: 'allowed', traceId: 'tr_9' },
        ],
      }),
      'ChannelService/ListMessages': () => ({
        messages: [
          {
            id: 'msg_5',
            seq: '5',
            channel: '#backend',
            from: 'user:u1',
            ts: T(0),
            type: 'MESSAGE_TYPE_TASK',
            payload: { text: 'run the tests' },
            meta: { traceId: 'tr_9' },
          },
        ],
      }),
    });
    signIn(server, { admin: true });
    useMessages
      .getState()
      .setChannel('#backend', [
        create(MessageSchema, { id: 'msg_5', seq: 5n, channel: '#backend', from: 'user:u1', ts: T(0), meta: { traceId: 'tr_9' } }),
      ]);
    const router = createMemoryRouter([{ path: '/traces/*', Component }], { initialEntries: ['/traces?trace=tr_9'] });
    render(<RouterProvider router={router} />);
    const tl = await screen.findByRole('list', { name: 'Trace replay' });
    const items = within(tl).getAllByRole('listitem');
    expect(items).toHaveLength(3);
    expect(items[0]).toHaveTextContent('run the tests');
    expect(items[2]).toHaveAttribute('data-tone', 'alert');
    expect(items[2]).toHaveTextContent('+2 s');
    expect(server.bodies('EventService/ListAudit')[0]).toMatchObject({ traceId: 'tr_9' });
    expect(server.bodies('ChannelService/ListMessages')[0]).toMatchObject({ channel: '#backend', thread: 'msg_5' });

    // Step control walks the timeline.
    const user = userEvent.setup();
    await user.click(screen.getByRole('button', { name: 'Step' }));
    expect(items[0]).toHaveAttribute('aria-current', 'step');
    await user.click(screen.getByRole('button', { name: 'Step' }));
    expect(items[1]).toHaveAttribute('aria-current', 'step');
    expect(items[2]).toHaveAttribute('data-state', 'future');
  });
});
