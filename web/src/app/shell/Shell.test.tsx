import { render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeAll, describe, expect, it, vi } from 'vitest';
import { App } from '../App';
import { makeHubApi, setApi } from '../api';
import { registerCoreCommands } from '../commands/core';
import { registerCommands } from '../commands/registry';
import { anonymousRights, useSession } from '../session';

// Every Connect call answers an empty message; streams answer nothing.
const fakeFetch = vi.fn(async (input: RequestInfo | URL) => {
  const url = String(input);
  if (url.includes('WatchEvents'))
    return new Response(new ReadableStream(), { status: 200, headers: { 'Content-Type': 'application/connect+json' } });
  return new Response('{}', { status: 200, headers: { 'Content-Type': 'application/json' } });
}) as unknown as typeof fetch;

function signInAs(role: 'admin' | 'member') {
  setApi(makeHubApi({ getCsrf: () => 'c', fetch: fakeFetch }));
  useSession.setState({
    status: 'signed-in',
    config: { mode: 'hub', hub: 'https://hub.example:8443', passkeys: true, push: false },
    me: { user_id: 'u1', name: 'ada', role, csrf: 'c', passkeys: [] },
    rights: { mode: 'hub', isAdmin: role === 'admin', readOnly: false, userId: 'u1', userName: 'ada' },
  });
}

describe('app shell', () => {
  beforeAll(() => registerCoreCommands());
  afterEach(() => {
    useSession.setState({ status: 'booting', me: null, config: null, rights: anonymousRights });
    window.history.replaceState(null, '', '/');
  });

  it('renders the rail, top bar and a lazily loaded route', async () => {
    window.history.replaceState(null, '', '/nodes');
    signInAs('admin');
    render(<App />);
    const rail = await screen.findByRole('navigation', { name: 'Views' });
    const links = within(rail)
      .getAllByRole('link')
      .map((a) => a.textContent);
    expect(links.join(' ')).toContain('Constellation');
    expect(links.join(' ')).not.toContain('This machine');
    expect(await screen.findByRole('region', { name: 'Nodes' })).toBeInTheDocument();
    expect(screen.getByText('hub.example')).toBeInTheDocument();
    expect(screen.getByText('admin')).toBeInTheDocument();
  });

  it('opens the command bar with Ctrl+K and navigates', async () => {
    const user = userEvent.setup();
    window.history.replaceState(null, '', '/settings');
    signInAs('member');
    render(<App />);
    await screen.findByRole('heading', { name: 'Settings' });
    await user.keyboard('{Control>}k{/Control}');
    const input = await screen.findByRole('combobox');
    await user.type(input, 'go appr');
    const opts = screen.getAllByRole('option');
    expect(opts[0]).toHaveTextContent('Go to Approvals');
    // Members do not see the freeze command.
    await user.clear(input);
    await user.type(input, 'freeze');
    expect(screen.queryByText('Freeze the network')).toBeNull();
    await user.clear(input);
    await user.type(input, 'go appr{Enter}');
    expect(await screen.findByRole('region', { name: 'Approvals' })).toBeInTheDocument();
    expect(window.location.pathname).toBe('/approvals');
  });

  it('asks before running a command that has a confirm step', async () => {
    const user = userEvent.setup();
    const run = vi.fn();
    const off = registerCommands([
      { id: 'test.approve', title: 'Approve grt_test', confirm: { title: 'Approve grt_test?', action: 'Approve' }, run },
    ]);
    try {
      window.history.replaceState(null, '', '/settings');
      signInAs('admin');
      render(<App />);
      await screen.findByRole('heading', { name: 'Settings' });
      await user.keyboard('{Control>}k{/Control}');
      await user.type(await screen.findByRole('combobox'), 'approve grt_test{Enter}');
      expect(await screen.findByText('Approve grt_test?')).toBeInTheDocument();
      expect(run).not.toHaveBeenCalled();
      await user.click(screen.getByRole('button', { name: 'Back' }));

      // Freeze (admin, network not frozen) confirms too and sends nothing yet.
      const input = await screen.findByRole('combobox');
      await user.clear(input);
      await user.type(input, 'freeze the network{Enter}');
      expect(await screen.findByText('Freeze the whole network?')).toBeInTheDocument();
      expect(document.querySelector('.confirm[data-tone="alert"]')).not.toBeNull();
      const calls = (fakeFetch as unknown as ReturnType<typeof vi.fn>).mock.calls.map((c) => String(c[0]));
      expect(calls.some((u) => u.includes('FreezeAll'))).toBe(false);

      // Confirming runs it.
      await user.click(screen.getByRole('button', { name: 'Back' }));
      await user.type(await screen.findByRole('combobox'), '{Control>}a{/Control}approve grt_test{Enter}');
      await user.click(await screen.findByRole('button', { name: 'Approve' }));
      expect(run).toHaveBeenCalledTimes(1);
    } finally {
      off();
    }
  });

  it('shows the sign-in form when signed out', async () => {
    useSession.setState({ status: 'signed-out', config: { mode: 'hub', passkeys: false }, rights: anonymousRights });
    render(<App />);
    expect(screen.getByLabelText('API key')).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /passkey/ })).toBeNull();
  });
});
