import { render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it } from 'vitest';
import { fakeServer, resetAll, signIn } from '../test-utils/fakeServer';
import { ArtifactRemoveForm, ChannelCreateForm, ChannelMembersForm, TokenCreateForm, UserAddForm, UserRemoveForm } from './AdminForms';

describe('admin forms', () => {
  afterEach(() => resetAll());

  it('adds a user and shows the API key once', async () => {
    const server = fakeServer({ 'UserService/CreateUser': () => ({ user: { id: 'usr_c', name: 'carol' }, apiKey: 'mck_secret' }) });
    signIn(server, { admin: true });
    const user = userEvent.setup();
    render(<UserAddForm />);
    await user.type(screen.getByLabelText('Name'), 'carol');
    await user.click(screen.getByRole('button', { name: 'Add user' }));
    expect(await screen.findByDisplayValue('mck_secret')).toBeInTheDocument();
    expect(server.bodies('UserService/CreateUser')).toEqual([{ name: 'carol', role: 'ROLE_MEMBER' }]);
  });

  it('creates a join token with caps and a ttl', async () => {
    const server = fakeServer({ 'TokenService/CreateJoinToken': () => ({ token: 'mcl1_tok' }) });
    signIn(server, { admin: true });
    const user = userEvent.setup();
    render(<TokenCreateForm />);
    await user.type(screen.getByLabelText('Node name'), 'gpu-box');
    await user.click(screen.getByRole('checkbox', { name: 'gpu' }));
    await user.click(screen.getByRole('button', { name: 'Create token' }));
    expect(await screen.findByDisplayValue('mcl1_tok')).toBeInTheDocument();
    expect(server.bodies('TokenService/CreateJoinToken')).toEqual([{ nodeName: 'gpu-box', caps: { names: ['gpu'] }, ttl: '15m' }]);
  });

  it('creates a channel', async () => {
    const server = fakeServer();
    signIn(server, { admin: true });
    const user = userEvent.setup();
    render(<ChannelCreateForm />);
    await user.type(screen.getByLabelText('Name'), 'eval');
    await user.click(screen.getByRole('button', { name: 'Create channel' }));
    expect(server.bodies('ChannelService/CreateChannel')).toEqual([{ name: '#eval', type: 'CHANNEL_TYPE_TOPIC' }]);
  });

  it('adds and removes channel members', async () => {
    const server = fakeServer({
      'ChannelService/GetChannel': () => ({
        channel: {
          name: '#eval',
          members: [
            { id: 'user:u1', role: 'owner' },
            { id: 'agent:a@mac', role: 'agent' },
          ],
        },
      }),
    });
    signIn(server, { admin: true });
    const user = userEvent.setup();
    render(<ChannelMembersForm channel="#eval" />);
    expect(await screen.findByText('agent:a@mac')).toBeInTheDocument();
    await user.type(screen.getByLabelText('Add member'), 'agent:b@mac');
    await user.click(screen.getByRole('button', { name: 'Add member' }));
    expect(server.bodies('ChannelService/AddMember')).toEqual([{ channel: '#eval', member: 'agent:b@mac' }]);
    await user.click(screen.getByRole('button', { name: 'Remove' }));
    await user.click(await screen.findByRole('button', { name: 'Remove member' }));
    expect(server.bodies('ChannelService/RemoveMember')).toEqual([{ channel: '#eval', member: 'agent:a@mac' }]);
  });

  it('deletes an artifact by id', async () => {
    const server = fakeServer();
    signIn(server, { admin: true });
    const user = userEvent.setup();
    render(<ArtifactRemoveForm />);
    await user.type(screen.getByLabelText('Artifact id'), 'art_01');
    await user.click(screen.getByRole('button', { name: 'Delete artifact' }));
    // Nothing is deleted before the confirmation.
    expect(await screen.findByText('Delete artifact art_01?')).toBeInTheDocument();
    expect(screen.getByText('It cannot be recovered.')).toBeInTheDocument();
    expect(server.bodies('ArtifactService/DeleteArtifact')).toEqual([]);
    const dialog = screen.getByRole('dialog');
    await user.click(within(dialog).getByRole('button', { name: 'Delete artifact' }));
    expect(server.bodies('ArtifactService/DeleteArtifact')).toEqual([{ id: 'art_01' }]);
  });

  it('does not offer to remove the signed-in admin', async () => {
    const server = fakeServer({
      'UserService/ListUsers': () => ({
        users: [
          { id: 'u1', name: 'ada', role: 'ROLE_ADMIN' },
          { id: 'u2', name: 'bob', role: 'ROLE_MEMBER' },
        ],
      }),
    });
    signIn(server, { admin: true, userId: 'u1' });
    render(<UserRemoveForm />);
    expect(await screen.findByText('bob')).toBeInTheDocument();
    expect(screen.queryByText('ada')).not.toBeInTheDocument();
  });
});
