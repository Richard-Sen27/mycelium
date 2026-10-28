import { createElement } from 'react';
import { toast } from '../../ds';
import { getApi } from '../../app/api';
import { registerCommands, type Command, type CommandContext } from '../../app/commands/registry';
import { agentsStore } from '../../app/events/stores';
import { inspect, useInspector } from '../../app/shell/inspector';
import { canManageAgent } from '../nodes/shared';
import { agentState, canSpawn } from './agents';

// Palette commands for agents (`mycel agent pause|resume|stop|budget|revoke|spawn`),
// one set per agent the caller may manage (admins and the agent's owner;
// spawning also for the owner of the orchestrator's node). Registered
// eagerly from app/commands/eager.ts: no components here, the forms are
// imported when a command runs.

const FORM_TITLE = { pause: 'Pause', budget: 'Budget', spawn: 'Spawn a worker' } as const;

async function openForm(id: string, name: string, which: 'pause' | 'budget' | 'spawn'): Promise<void> {
  const m = await import('./AgentForms');
  const form = which === 'pause' ? m.PauseForm : which === 'budget' ? m.BudgetForm : m.SpawnForm;
  inspect({ key: `agent:${id}:${which}`, title: `${FORM_TITLE[which]} · ${name}`, content: createElement(form, { id }) });
}

export function agentCommands(ctx: CommandContext): Command[] {
  return agentsStore.list().flatMap((a): Command[] => {
    const manage = canManageAgent(ctx.rights, a);
    const spawn = canSpawn(ctx.rights, a);
    if (!manage && !spawn) return [];
    const id = a.id;
    const who = `${a.name}@${a.nodeName}`;
    const st = agentState(a);
    const out: Command[] = [];
    if (manage && st === 'running')
      out.push({
        id: `agent.pause.${id}`,
        title: `Pause ${who}`,
        group: 'Agents',
        keywords: ['kill switch', 'agent pause', a.harness],
        hint: 'mycel agent pause',
        run: () => openForm(id, a.name, 'pause'),
      });
    if (manage && st === 'paused')
      out.push({
        id: `agent.resume.${id}`,
        title: `Resume ${who}`,
        group: 'Agents',
        keywords: ['continue', 'unpause', 'agent resume'],
        hint: 'mycel agent resume',
        run: async () => {
          await getApi().rpc.agents.resumeAgent({ id });
          toast({ title: `Resumed ${a.name}` });
        },
      });
    if (manage)
      out.push({
        id: `agent.budget.${id}`,
        title: `Set budget of ${who}`,
        group: 'Agents',
        keywords: ['limit', 'tokens', 'usd', 'agent budget'],
        hint: 'mycel agent budget',
        run: () => openForm(id, a.name, 'budget'),
      });
    if (spawn)
      out.push({
        id: `agent.spawn.${id}`,
        title: `Spawn a worker from ${who}`,
        group: 'Agents',
        keywords: ['worker', 'start', 'agent spawn', 'harness'],
        hint: 'mycel agent spawn',
        run: () => openForm(id, a.name, 'spawn'),
      });
    if (manage && st !== 'stopped')
      out.push({
        id: `agent.stop.${id}`,
        title: `Stop ${who}`,
        group: 'Agents',
        keywords: ['kill', 'agent stop'],
        hint: 'mycel agent stop',
        confirm: { title: `Stop ${who}?`, body: "The node stops the agent's process.", action: 'Stop agent', tone: 'alert' },
        run: async () => {
          await getApi().rpc.agents.stopAgent({ id });
          toast({ title: `Stopped ${a.name}` });
        },
      });
    if (manage)
      out.push({
        id: `agent.revoke.${id}`,
        title: `Revoke ${who}`,
        group: 'Agents',
        keywords: ['remove', 'delete', 'agent revoke'],
        hint: 'mycel agent revoke',
        confirm: {
          title: `Revoke ${who}?`,
          body: 'Its token is revoked and it is removed from the hub. Its grants end.',
          action: 'Revoke agent',
          tone: 'alert',
        },
        run: async () => {
          await getApi().rpc.agents.revokeAgent({ id });
          if (useInspector.getState().key?.startsWith(`agent:${id}`)) useInspector.getState().close();
          toast({ title: `Revoked ${a.name}` });
        },
      });
    return out;
  });
}

let registered = false;
export function registerAgentCommands(): void {
  if (registered) return;
  registered = true;
  registerCommands(agentCommands);
}
