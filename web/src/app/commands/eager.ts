// Commands of the lazily loaded views that must be in the palette before
// their route is visited (every CLI-equivalent action, D025). main.tsx
// imports this eagerly, so each views/<id>/commands.ts it pulls in must stay
// light: stores, the API and registerCommands only, never view components.
import { registerChannelCommands } from '../../views/channels/commands';
import { registerJobCommands } from '../../views/jobs/commands';
import { registerAgentCommands } from '../../views/agents/commands';
import { registerGrantCommands } from '../../views/grants/commands';
import { registerNodeCommands } from '../../views/nodes/commands';
import { registerAdminCommands } from '../../views/admin/commands';

let done = false;

export function registerViewCommands(): void {
  if (done) return;
  done = true;
  registerChannelCommands();
  registerJobCommands();
  registerNodeCommands();
  registerAgentCommands();
  registerGrantCommands();
  registerAdminCommands();
}
