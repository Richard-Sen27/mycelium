import { registerCommands, type Command, type CommandContext } from '../../app/commands/registry';
import { channelsStore, useFreeze } from '../../app/events/stores';
import { archiveChannel, freezeChannel, unfreezeChannel } from './actions';
import { canManageChannel } from './model';

// Channel commands for the palette (CLI: mycel freeze --channel, mycel
// unfreeze --channel, mycel channel archive). Registered at startup from
// app/commands/eager.ts, so keep this file free of view components.

function channelCommands(ctx: CommandContext): Command[] {
  const frozen = new Set(useFreeze.getState().status?.frozenChannels ?? []);
  return channelsStore
    .list()
    .filter((c) => !c.archivedAt && canManageChannel(ctx.rights, c))
    .flatMap((c): Command[] => [
      frozen.has(c.name)
        ? {
            id: `channel.unfreeze.${c.name}`,
            title: `Unfreeze ${c.name}`,
            group: 'Channels',
            keywords: ['unfreeze', 'thaw', 'resume', c.name],
            hint: `mycel unfreeze --channel '${c.name}'`,
            confirm: { title: `Unfreeze ${c.name}?`, body: 'Posts are accepted again and its agents resume.', action: 'Unfreeze' },
            run: () => unfreezeChannel(c.name),
          }
        : {
            id: `channel.freeze.${c.name}`,
            title: `Freeze ${c.name}`,
            group: 'Channels',
            keywords: ['freeze', 'kill switch', 'stop', c.name],
            hint: `mycel freeze --channel '${c.name}'`,
            confirm: {
              title: `Freeze ${c.name}?`,
              body: 'Every agent whose home channel this is pauses and posts are refused.',
              action: 'Freeze channel',
              tone: 'alert',
            },
            run: () => freezeChannel(c.name, 'from the command bar'),
          },
      {
        id: `channel.archive.${c.name}`,
        title: `Archive ${c.name}`,
        group: 'Channels',
        keywords: ['archive', 'close', c.name],
        hint: `mycel channel archive '${c.name}'`,
        confirm: { title: `Archive ${c.name}?`, body: 'The channel becomes read-only; history stays.', action: 'Archive', tone: 'alert' },
        run: () => archiveChannel(c.name),
      },
    ]);
}

let registered = false;

export function registerChannelCommands(): void {
  if (registered) return;
  registered = true;
  registerCommands(channelCommands);
}
