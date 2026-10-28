import { GrantState, type Grant } from '../../gen/mycel/v1/grant_pb';
import type { Agent } from '../../gen/mycel/v1/hub_pb';
import { toast } from '../../ds';
import { getApi } from '../api';
import { signOut } from '../boot';
import { agentsStore, grantsStore, nodesStore, useFreeze } from '../events/stores';
import { navFor } from '../nav';
import { THEMES, usePrefs } from '../prefs';
import type { Rights } from '../session';
import { registerCommands, type Command, type CommandContext } from './registry';

// The commands this package owns: navigation, sign-out, theme and motion,
// freeze/unfreeze (admins, with a confirm step) and approve/deny for each
// pending grant (admins and the owner of the grant's node). Views register
// their own commands with registerCommands from their modules.

const hubWrite = (ctx: CommandContext) => ctx.rights.mode === 'hub' && !ctx.rights.readOnly;
const isAdmin = (ctx: CommandContext) => hubWrite(ctx) && ctx.rights.isAdmin;

export function grantLabel(g: Grant): string {
  const kind = g.spec?.kind.case ?? 'grant';
  return `${kind === 'dockerHost' ? 'docker' : kind} for ${g.subject} on ${g.node}`;
}

/** grantDetail is the confirm text: what, for whom, where, and the requester's reason. */
function grantDetail(g: Grant): string {
  const dur = g.duration ? ` for ${g.duration}` : '';
  return `${grantLabel(g)}${dur}.${g.reason ? ` Reason: ${g.reason}` : ''}`;
}

/** canDecide: admins, or the owner of the node the grant attaches to. */
export function canDecide(rights: Rights, g: Grant): boolean {
  if (rights.mode !== 'hub' || rights.readOnly) return false;
  if (rights.isAdmin) return true;
  const node = nodesStore.use.getState().byId[g.node];
  return !!node && !!rights.userId && node.ownerId === rights.userId;
}

/** findOrchestrator: the caller's live top-level agent (no parent), if any. */
export function findOrchestrator(agents: Agent[], userId: string | null): Agent | undefined {
  if (!userId) return undefined;
  return agents
    .filter((a) => a.ownerId === userId && a.parent === '' && a.state !== 'stopped' && a.exitedAt === '')
    .sort((a, b) => (a.state === 'running' ? -1 : 0) - (b.state === 'running' ? -1 : 0) || b.createdAt.localeCompare(a.createdAt))[0];
}

/** sendToOrchestrator posts free text as a DM to the caller's orchestrator. */
export async function sendToOrchestrator(agent: Agent, text: string): Promise<void> {
  await getApi().rpc.channels.post({ channel: `@${agent.id}`, text, mentions: [agent.id] });
  toast({ title: `Sent to ${agent.name}`, body: text.length > 80 ? `${text.slice(0, 80)}…` : text });
}

function navCommands(ctx: CommandContext): Command[] {
  return navFor(ctx.rights.mode).map((n, i) => ({
    id: `nav.${n.id}`,
    title: `Go to ${n.label}`,
    group: 'Go to',
    keywords: [n.label, ...n.keywords],
    shortcut: i < 9 ? ['g', String(i + 1)] : undefined,
    run: (c) => c.navigate(n.path),
  }));
}

const staticCommands: Command[] = [
  {
    id: 'session.signout',
    title: 'Sign out',
    group: 'Session',
    keywords: ['logout', 'log out', 'exit'],
    run: async () => {
      await signOut();
    },
  },
  {
    id: 'prefs.motion.reduced',
    title: 'Reduce motion',
    group: 'Display',
    keywords: ['animation', 'accessibility', 'calm'],
    allowed: () => usePrefs.getState().motion !== 'reduced',
    run: () => usePrefs.getState().setMotion('reduced'),
  },
  {
    id: 'prefs.motion.system',
    title: 'Motion: follow the system setting',
    group: 'Display',
    keywords: ['animation', 'accessibility'],
    allowed: () => usePrefs.getState().motion !== 'system',
    run: () => usePrefs.getState().setMotion('system'),
  },
  {
    id: 'network.freeze',
    title: 'Freeze the network',
    group: 'Network',
    keywords: ['kill switch', 'stop everything', 'emergency', 'freeze --all'],
    hint: 'mycel freeze --all',
    allowed: (ctx) => isAdmin(ctx) && !useFreeze.getState().status?.allFrozen,
    confirm: {
      title: 'Freeze the whole network?',
      body: 'Every sandbox stops and every agent is paused on every node until an admin unfreezes.',
      action: 'Freeze network',
      tone: 'alert',
    },
    run: async () => {
      await getApi().rpc.control.freezeAll({ reason: 'from the UI command bar' });
      await useFreeze.getState().load();
      toast({ title: 'Network frozen', tone: 'alert' });
    },
  },
  {
    id: 'network.unfreeze',
    title: 'Unfreeze the network',
    group: 'Network',
    keywords: ['resume', 'thaw', 'unfreeze --all'],
    hint: 'mycel unfreeze --all',
    allowed: (ctx) => isAdmin(ctx) && !!useFreeze.getState().status?.allFrozen,
    confirm: {
      title: 'Unfreeze the network?',
      body: 'Nodes accept work again. Stopped sandboxes stay stopped; paused agents resume.',
      action: 'Unfreeze',
    },
    run: async () => {
      await getApi().rpc.control.unfreezeAll({});
      await useFreeze.getState().load();
      toast({ title: 'Network unfrozen' });
    },
  },
];

function themeCommands(): Command[] {
  return THEMES.map((t) => ({
    id: `prefs.theme.${t.id}`,
    title: `Theme: ${t.label}`,
    group: 'Display',
    keywords: ['theme', 'skin', 'look'],
    run: () => usePrefs.getState().setTheme(t.id),
  }));
}

function grantCommands(ctx: CommandContext): Command[] {
  if (!hubWrite(ctx)) return [];
  const pending = grantsStore.list().filter((g) => g.state === GrantState.PENDING && canDecide(ctx.rights, g));
  return pending.flatMap((g) => [
    {
      id: `grant.approve.${g.id}`,
      title: `Approve ${g.id}`,
      group: 'Approvals',
      keywords: ['approve', grantLabel(g), g.reason],
      hint: grantLabel(g),
      confirm: { title: `Approve ${g.id}?`, body: grantDetail(g), action: 'Approve' },
      run: async () => {
        await getApi().rpc.grants.approveGrant({ id: g.id });
        toast({ title: `Approved ${g.id}`, body: grantLabel(g) });
      },
    },
    {
      id: `grant.deny.${g.id}`,
      title: `Deny ${g.id}`,
      group: 'Approvals',
      keywords: ['deny', 'reject', grantLabel(g), g.reason],
      hint: grantLabel(g),
      confirm: { title: `Deny ${g.id}?`, body: grantDetail(g), action: 'Deny', tone: 'alert' },
      run: async () => {
        await getApi().rpc.grants.denyGrant({ id: g.id });
        toast({ title: `Denied ${g.id}`, body: grantLabel(g) });
      },
    },
  ]);
}

let registered = false;

/** registerCoreCommands is called once at startup. */
export function registerCoreCommands(): void {
  if (registered) return;
  registered = true;
  registerCommands(navCommands);
  registerCommands(staticCommands);
  registerCommands(themeCommands);
  registerCommands(grantCommands);
}

export function orchestratorFor(ctx: CommandContext): Agent | undefined {
  if (!hubWrite(ctx)) return undefined;
  return findOrchestrator(agentsStore.list(), ctx.rights.userId);
}
