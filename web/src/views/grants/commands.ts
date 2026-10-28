import { toast } from '../../ds';
import { GrantState } from '../../gen/mycel/v1/grant_pb';
import { getApi } from '../../app/api';
import { grantLabel } from '../../app/commands/core';
import { registerCommands, type Command, type CommandContext } from '../../app/commands/registry';
import { grantsStore } from '../../app/events/stores';
import { canRevokeGrant, hubWrite } from '../nodes/shared';

// Palette commands for grants: request (`mycel grants request`) and revoke
// (`mycel grants revoke`). Approve and deny are registered by the shell.

export function grantCommands(ctx: CommandContext): Command[] {
  if (!hubWrite(ctx.rights)) return [];
  const out: Command[] = [
    {
      id: 'grant.request',
      title: 'Request a grant',
      group: 'Grants',
      keywords: ['ollama', 'gpu', 'docker', 'mount', 'egress', 'grants request', 'lease'],
      hint: 'mycel grants request',
      run: async () => (await import('./RequestGrantForm')).openRequestGrant(),
    },
  ];
  for (const g of grantsStore.list()) {
    if (g.state !== GrantState.ACTIVE || !canRevokeGrant(ctx.rights, g)) continue;
    out.push({
      id: `grant.revoke.${g.id}`,
      title: `Revoke ${g.id}`,
      group: 'Grants',
      keywords: ['revoke', 'end', grantLabel(g)],
      hint: grantLabel(g),
      confirm: {
        title: `Revoke ${g.id}?`,
        body: `${grantLabel(g)}. The broker detaches it at once.`,
        action: 'Revoke grant',
        tone: 'alert',
      },
      run: async () => {
        await getApi().rpc.grants.revokeGrant({ id: g.id });
        toast({ title: `Revoked ${g.id}` });
      },
    });
  }
  return out;
}

let registered = false;
export function registerGrantCommands(): void {
  if (registered) return;
  registered = true;
  registerCommands(grantCommands);
}
