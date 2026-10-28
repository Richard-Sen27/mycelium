import { createElement } from 'react';
import { toast } from '../../ds';
import { getApi } from '../../app/api';
import { nodesStore } from '../../app/events/stores';
import { registerCommands, type Command, type CommandContext } from '../../app/commands/registry';
import { inspect, useInspector } from '../../app/shell/inspector';
import { canManageNode } from './shared';

// Palette commands for nodes (`mycel nodes caps|share|remove`). Registered
// eagerly from app/commands/eager.ts, so this module stays light: the
// forms are imported only when a command runs. One set per visible node,
// hidden unless the caller is an admin or the node's owner (and never in
// node mode, where the network is read-only).

async function openForm(name: string, which: 'caps' | 'sharing'): Promise<void> {
  const m = await import('./NodeForms');
  inspect({
    key: `node:${name}:${which}`,
    title: `${which === 'caps' ? 'Caps' : 'Sharing'} · ${name}`,
    content: createElement(which === 'caps' ? m.CapsForm : m.SharingForm, { name }),
  });
}

export function nodeCommands(ctx: CommandContext): Command[] {
  return nodesStore.list().flatMap((n): Command[] => {
    if (!canManageNode(ctx.rights, n)) return [];
    const name = n.name;
    return [
      {
        id: `node.caps.${name}`,
        title: `Set caps on ${name}`,
        group: 'Nodes',
        keywords: ['capabilities', 'docker', 'ollama', 'gpu', 'sandbox', 'nodes caps'],
        hint: 'mycel nodes caps',
        run: () => openForm(name, 'caps'),
      },
      {
        id: `node.sharing.${name}`,
        title: `Set sharing on ${name}`,
        group: 'Nodes',
        keywords: ['share', 'offer', 'idle', 'schedule', 'nodes share'],
        hint: 'mycel nodes share',
        run: () => openForm(name, 'sharing'),
      },
      {
        id: `node.remove.${name}`,
        title: `Remove node ${name}`,
        group: 'Nodes',
        keywords: ['delete', 'revoke', 'nodes remove'],
        hint: 'mycel nodes remove',
        confirm: {
          title: `Remove ${name}?`,
          body: 'Its certificate is revoked, it is disconnected and its sandboxes are torn down. It must join again with a new token.',
          action: 'Remove node',
          tone: 'alert',
        },
        run: async () => {
          await getApi().rpc.nodes.removeNode({ name });
          if (useInspector.getState().key?.startsWith(`node:${name}`)) useInspector.getState().close();
          toast({ title: `Removed ${name}` });
        },
      },
    ];
  });
}

let registered = false;
export function registerNodeCommands(): void {
  if (registered) return;
  registered = true;
  registerCommands(nodeCommands);
}
