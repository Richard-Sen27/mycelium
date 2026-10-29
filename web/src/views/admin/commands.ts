import { toast } from '../../ds';
import { getApi } from '../../app/api';
import { registerCommands, type Command, type CommandContext } from '../../app/commands/registry';
import { channelsStore } from '../../app/events/stores';
import { canManageChannel } from '../channels/model';
import { hubWrite } from '../nodes/shared';
import type { AdminForm } from './AdminForms';

// Palette commands for the CLI's admin verbs that no view owns: users
// (`mycel user add|rm`), join tokens (`mycel token create|revoke`),
// channels (`mycel channels create|add|rm|reset-usage`) and artifacts
// (`mycel artifacts rm`). Registered eagerly from app/commands/eager.ts,
// so this module stays light: the forms load when a command runs.

const open =
  (which: AdminForm, arg = '') =>
  async () =>
    (await import('./AdminForms')).openAdminForm(which, arg);

export function adminCommands(ctx: CommandContext): Command[] {
  if (!hubWrite(ctx.rights)) return [];
  const out: Command[] = [];
  if (ctx.rights.isAdmin) {
    out.push(
      {
        id: 'user.add',
        title: 'Add a user…',
        group: 'Users',
        keywords: ['user add', 'member', 'admin', 'api key', 'invite'],
        hint: 'mycel user add',
        run: open('user.add'),
      },
      {
        id: 'user.remove',
        title: 'Remove a user…',
        group: 'Users',
        keywords: ['user rm', 'delete', 'offboard'],
        hint: 'mycel user rm',
        run: open('user.remove'),
      },
    );
  }
  out.push(
    {
      id: 'token.create',
      title: 'Create a join token…',
      group: 'Nodes',
      keywords: ['token create', 'enrol', 'join', 'add node', 'new machine'],
      hint: 'mycel token create',
      run: open('token.create'),
    },
    {
      id: 'token.revoke',
      title: 'Revoke a join token…',
      group: 'Nodes',
      keywords: ['token revoke', 'join token', 'cancel'],
      hint: 'mycel token revoke',
      run: open('token.revoke'),
    },
    {
      id: 'channel.create',
      title: 'Create a channel…',
      group: 'Channels',
      keywords: ['channels create', 'new channel', 'topic', 'task'],
      hint: 'mycel channels create',
      run: open('channel.create'),
    },
    {
      id: 'artifact.remove',
      title: 'Delete an artifact…',
      group: 'Jobs',
      keywords: ['artifacts rm', 'delete', 'remove', 'art_'],
      hint: 'mycel artifacts rm',
      run: open('artifact.remove'),
    },
  );
  for (const c of channelsStore.list()) {
    if (c.archivedAt || !canManageChannel(ctx.rights, c)) continue;
    const name = c.name;
    out.push(
      {
        id: `channel.members.${name}`,
        title: `Members of ${name}…`,
        group: 'Channels',
        keywords: ['channels add', 'channels rm', 'member', 'invite', 'remove', name],
        hint: `mycel channels add|rm '${name}'`,
        run: open('channel.members', name),
      },
      {
        id: `channel.usage.reset.${name}`,
        title: `Reset usage of ${name}`,
        group: 'Channels',
        keywords: ['channels reset-usage', 'budget', 'usage', name],
        hint: `mycel channels reset-usage '${name}'`,
        confirm: {
          title: `Reset the usage of ${name}?`,
          body: 'Its spent tokens and dollars start again from zero; agents paused by its budget may resume.',
          action: 'Reset usage',
          tone: 'alert',
        },
        run: async () => {
          await getApi().rpc.agents.resetChannelUsage({ channel: name });
          toast({ title: `Usage of ${name} reset` });
        },
      },
    );
  }
  return out;
}

let registered = false;

export function registerAdminCommands(): void {
  if (registered) return;
  registered = true;
  registerCommands(adminCommands);
}
