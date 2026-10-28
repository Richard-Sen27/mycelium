import { create } from '@bufbuild/protobuf';
import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it } from 'vitest';
import { SharingSettingsSchema } from '../../gen/mycel/v1/grant_pb';
import { fakeServer, json, resetAll, signIn } from '../test-utils/fakeServer';
import { SharingForm } from './index';
import { draftOf, problems } from './sharing';

const hub = create(SharingSettingsSchema, {
  offer: { names: ['docker', 'gpu'] },
  when: 'always',
  localFirst: true,
  maxJobs: 4,
  idleAfterMinutes: 10,
});
const data = { hub, local: create(SharingSettingsSchema, {}), effective: hub, idle: 'unknown' };

describe('sharing rules', () => {
  it('flag anything wider than the hub', () => {
    const d = draftOf(hub);
    expect(problems(hub, d)).toEqual([]);
    expect(problems(hub, { ...d, offer: ['docker', 'gpu', 'ollama'] })[0]).toMatch(/does not offer ollama/);
    expect(problems(hub, { ...d, localFirst: false })[0]).toMatch(/local-first/);
    expect(problems(hub, { ...d, maxJobs: 5 })[0]).toMatch(/1 to 4/);
    expect(problems(hub, { ...d, idleAfter: 5 })[0]).toMatch(/at least 10/);
    const sched = create(SharingSettingsSchema, { when: 'schedule', schedule: [{ days: ['mon'], from: '22:00', to: '07:00' }] });
    expect(problems(sched, { ...draftOf(sched), when: 'always' }).join(' ')).toMatch(/widen/);
  });
});

describe('this machine: sharing form', () => {
  afterEach(() => resetAll());

  it('offers only what the hub offers and submits the tightened settings', async () => {
    const user = userEvent.setup();
    const server = fakeServer({ 'PUT /api/local/sharing': () => ({ offer: { names: ['docker'] } }) });
    signIn(server, { mode: 'node' });
    render(<SharingForm data={data} />);
    expect(screen.getByRole('checkbox', { name: 'ollama' })).toBeDisabled();
    expect(screen.getByRole('checkbox', { name: 'Local first' })).toBeDisabled();
    expect(screen.getByRole('radio', { name: 'always' })).toBeEnabled();
    expect(screen.getByText(/idle detector cannot tell/)).toBeInTheDocument();
    await user.click(screen.getByRole('checkbox', { name: 'gpu' }));
    const max = screen.getByLabelText('Max concurrent jobs');
    await user.clear(max);
    await user.type(max, '2');
    await user.click(screen.getByRole('button', { name: 'Restrict' }));
    const put = server.calls.find((c) => c.method === 'PUT');
    expect(put?.url).toBe('/api/local/sharing');
    expect(put?.body).toEqual({ offer: { names: ['docker'] }, when: 'always', local_first: true, max_jobs: 2, idle_after_minutes: 10 });
    expect(await screen.findByText(/Saved/)).toBeInTheDocument();
  });

  it('does not submit a wider setting and shows the daemon’s 400 message', async () => {
    const user = userEvent.setup();
    const msg =
      'sharing: the local ceiling may not widen the hub-side settings (offers, schedule, idle-only, local_first, max_jobs, idle_after_minutes); widen from the hub';
    const server = fakeServer({ 'PUT /api/local/sharing': () => json({ error: msg }, 400) });
    signIn(server, { mode: 'node' });
    render(<SharingForm data={data} />);
    // Too many jobs: the form refuses before sending.
    const max = screen.getByLabelText('Max concurrent jobs');
    await user.clear(max);
    await user.type(max, '9');
    expect(screen.getByRole('button', { name: 'Restrict' })).toBeDisabled();
    expect(screen.getByRole('list', { name: 'Why this cannot be saved' })).toHaveTextContent('1 to 4');
    expect(server.calls.filter((c) => c.method === 'PUT')).toHaveLength(0);
    // A schedule the form cannot judge goes to the daemon, whose refusal is shown.
    await user.clear(max);
    await user.type(max, '3');
    await user.click(screen.getByRole('radio', { name: 'on a schedule' }));
    await user.click(screen.getByRole('button', { name: 'Add window' }));
    await user.click(screen.getByRole('button', { name: 'Restrict' }));
    expect(await screen.findByRole('alert')).toHaveTextContent(msg);
  });
});
