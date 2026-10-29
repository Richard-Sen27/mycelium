import { create } from '@bufbuild/protobuf';
import type { DescService } from '@bufbuild/protobuf';
import type { GenFile } from '@bufbuild/protobuf/codegenv2';
import { afterEach, describe, expect, it } from 'vitest';
import { agentsStore, channelsStore, grantsStore, jobsStore, nodesStore, useFreeze } from '../events/stores';
import { AgentSchema } from '../../gen/mycel/v1/hub_pb';
import { NodeSchema } from '../../gen/mycel/v1/node_pb';
import { ChannelSchema, ChannelType } from '../../gen/mycel/v1/channel_pb';
import { JobSchema, JobState } from '../../gen/mycel/v1/job_pb';
import { FreezeStatusResponseSchema, GrantSchema, GrantState } from '../../gen/mycel/v1/grant_pb';
import { fakeServer, resetAll, seed, signIn } from '../../views/test-utils/fakeServer';
import { registerCoreCommands } from './core';
import { registerViewCommands } from './eager';
import { availableCommands } from './registry';

// Spec phase 6: "every admin action available in the CLI is also available
// in the hub UI". CLI_ACTIONS lists every verb of internal/cli/cli.go that
// changes state on the hub, with where the hub UI offers it: a palette
// command id (per-object commands with the object's id) or a view action
// (the source file that calls the RPC). A verb without a place fails the
// test: add the command (views/<id>/commands.ts or views/admin) rather than
// dropping it from this list. Every mutating Connect method must be named
// by some verb, so a new write RPC needs a CLI verb and a UI place too.

type Place = { palette: string } | { view: string };

interface CliAction {
  verb: string;
  /** The Connect method behind it ("<service local name>.<method local name>"). */
  rpc: string;
  where: Place[];
}

const CLI_ACTIONS: CliAction[] = [
  // users
  { verb: 'user add', rpc: 'UserService.createUser', where: [{ palette: 'user.add' }] },
  { verb: 'user rm', rpc: 'UserService.removeUser', where: [{ palette: 'user.remove' }] },
  // join tokens
  { verb: 'token create', rpc: 'TokenService.createJoinToken', where: [{ palette: 'token.create' }] },
  { verb: 'token revoke', rpc: 'TokenService.revokeJoinToken', where: [{ palette: 'token.revoke' }] },
  // nodes
  { verb: 'nodes caps', rpc: 'NodeService.setNodeCaps', where: [{ palette: 'node.caps.alpha' }, { view: 'views/nodes/NodeForms.tsx' }] },
  {
    verb: 'nodes share',
    rpc: 'ControlService.setNodeSharing',
    where: [{ palette: 'node.sharing.alpha' }, { view: 'views/nodes/NodeForms.tsx' }],
  },
  { verb: 'nodes rm', rpc: 'NodeService.removeNode', where: [{ palette: 'node.remove.alpha' }, { view: 'views/nodes/NodeDetail.tsx' }] },
  // channels
  { verb: 'channels create', rpc: 'ChannelService.createChannel', where: [{ palette: 'channel.create' }] },
  { verb: 'channels policy', rpc: 'ChannelService.updateChannelPolicy', where: [{ view: 'views/channels/Panels.tsx' }] },
  { verb: 'channels archive', rpc: 'ChannelService.archiveChannel', where: [{ palette: 'channel.archive.#backend' }] },
  { verb: 'channels add', rpc: 'ChannelService.addMember', where: [{ palette: 'channel.members.#backend' }] },
  { verb: 'channels rm', rpc: 'ChannelService.removeMember', where: [{ palette: 'channel.members.#backend' }] },
  { verb: 'channels reset-usage', rpc: 'AgentService.resetChannelUsage', where: [{ palette: 'channel.usage.reset.#backend' }] },
  { verb: 'post', rpc: 'ChannelService.post', where: [{ view: 'views/channels/Composer.tsx' }] },
  { verb: 'bb put', rpc: 'ChannelService.blackboardPut', where: [{ view: 'views/channels/Panels.tsx' }] },
  { verb: 'bb rm', rpc: 'ChannelService.blackboardDelete', where: [{ view: 'views/channels/Panels.tsx' }] },
  // agents
  { verb: 'agents pause', rpc: 'AgentService.pauseAgent', where: [{ palette: 'agent.pause.agent:lead@alpha' }] },
  { verb: 'freeze --agent', rpc: 'AgentService.pauseAgent', where: [{ palette: 'agent.pause.agent:lead@alpha' }] },
  { verb: 'agents resume', rpc: 'AgentService.resumeAgent', where: [{ palette: 'agent.resume.agent:napper@alpha' }] },
  { verb: 'unfreeze --agent', rpc: 'AgentService.resumeAgent', where: [{ palette: 'agent.resume.agent:napper@alpha' }] },
  { verb: 'agents stop', rpc: 'AgentService.stopAgent', where: [{ palette: 'agent.stop.agent:lead@alpha' }] },
  { verb: 'agents budget', rpc: 'AgentService.setAgentBudget', where: [{ palette: 'agent.budget.agent:lead@alpha' }] },
  { verb: 'agents revoke', rpc: 'AgentService.revokeAgent', where: [{ palette: 'agent.revoke.agent:lead@alpha' }] },
  // spawn_agent goes to the orchestrator as a task message (`mycel agent spawn`).
  { verb: 'agent spawn', rpc: 'ChannelService.post', where: [{ palette: 'agent.spawn.agent:lead@alpha' }] },
  // jobs and artifacts
  { verb: 'job run', rpc: 'JobService.runJob', where: [{ palette: 'job.run' }, { view: 'views/jobs/NewJob.tsx' }] },
  { verb: 'job cancel', rpc: 'JobService.cancelJob', where: [{ palette: 'job.cancel.job_1' }] },
  { verb: 'artifacts put', rpc: 'PUT /api/artifacts', where: [{ view: 'views/jobs/artifacts.tsx' }] },
  { verb: 'artifacts rm', rpc: 'ArtifactService.deleteArtifact', where: [{ palette: 'artifact.remove' }] },
  // grants and approvals
  { verb: 'grants request', rpc: 'GrantService.requestGrant', where: [{ palette: 'grant.request' }] },
  { verb: 'approve', rpc: 'GrantService.approveGrant', where: [{ palette: 'grant.approve.grt_P' }, { view: 'views/approvals/index.tsx' }] },
  { verb: 'deny', rpc: 'GrantService.denyGrant', where: [{ palette: 'grant.deny.grt_P' }, { view: 'views/approvals/index.tsx' }] },
  { verb: 'grants revoke', rpc: 'GrantService.revokeGrant', where: [{ palette: 'grant.revoke.grt_A' }] },
  // kill switches
  { verb: 'freeze --all', rpc: 'ControlService.freezeAll', where: [{ palette: 'network.freeze' }] },
  { verb: 'unfreeze --all', rpc: 'ControlService.unfreezeAll', where: [{ palette: 'network.unfreeze' }] },
  { verb: 'freeze --channel', rpc: 'ControlService.freezeChannel', where: [{ palette: 'channel.freeze.#backend' }] },
  { verb: 'unfreeze --channel', rpc: 'ControlService.unfreezeChannel', where: [{ palette: 'channel.unfreeze.#ops' }] },
];

// Verbs that act on the hub's host, not through the API: they need the hub's
// files or process (`mycel hub redirect|tunnel|export|import`, `mycel relay`,
// `mycel service`), so the UI cannot offer them. Checked below: none of them
// may appear as a Connect method.
const HOST_ONLY = /redirect|tunnel|export|import|relay|moved/i;

// Read-only methods and the node-only enrolment service need no UI action.
const READ = /^(list|get|watch|whoami|search|freezeStatus|streamLogs|blackboardGet|channelSummary)/;

// Write methods without a CLI verb (so outside the spec's parity rule):
// CreateApiKey issues a further key for a user; no `mycel` command calls it.
const NO_CLI_VERB = new Set(['UserService.createApiKey']);

// Every service of every generated file, except the node-only enrolment
// service (called by `mycel join` with a join token, never by a person).
const NODE_ONLY = new Set(['mycel.v1.EnrolService']);
const GEN_FILES = import.meta.glob<Record<string, unknown>>('../../gen/mycel/v1/*_pb.ts', { eager: true });
const SERVICES: [string, DescService][] = Object.values(GEN_FILES)
  .flatMap((mod) => Object.entries(mod).filter(([k]) => k.startsWith('file_')))
  .flatMap(([, f]) => (f as GenFile).services)
  .filter((svc) => !NODE_ONLY.has(svc.typeName))
  .map((svc) => [svc.name, svc]);

// The views' sources, by path relative to web/src.
const VIEW_SOURCES = import.meta.glob<string>('../../views/**/*.{ts,tsx}', { query: '?raw', import: 'default', eager: true });

function source(rel: string): string {
  const src = VIEW_SOURCES[`../../${rel}`];
  if (src === undefined) throw new Error(`no source ${rel}`);
  return src;
}

/** viewCalls reports whether a view source calls the action's RPC. */
function viewCalls(rel: string, rpc: string): boolean {
  const src = source(rel);
  if (rpc.startsWith('PUT ')) return src.includes('.upload(') && src.includes(rpc.slice(4));
  const method = rpc.split('.')[1];
  return new RegExp(`\\.${method}\\(`).test(src);
}

/** paletteIds are the command ids an admin sees with one of each kind of object, in both freeze states. */
function paletteIds(): Set<string> {
  signIn(fakeServer(), { admin: true });
  seed(nodesStore, [create(NodeSchema, { name: 'alpha', ownerId: 'u1' })]);
  seed(agentsStore, [
    create(AgentSchema, { id: 'agent:lead@alpha', name: 'lead', nodeName: 'alpha', ownerId: 'u1', state: 'running' }),
    create(AgentSchema, { id: 'agent:napper@alpha', name: 'napper', nodeName: 'alpha', ownerId: 'u1', state: 'paused' }),
  ]);
  seed(channelsStore, [
    create(ChannelSchema, { name: '#backend', type: ChannelType.TOPIC, ownerId: 'u1' }),
    create(ChannelSchema, { name: '#ops', type: ChannelType.TOPIC, ownerId: 'u1' }),
  ]);
  seed(jobsStore, [create(JobSchema, { id: 'job_1', state: JobState.RUNNING, spec: { command: ['make'] } })]);
  seed(grantsStore, [
    create(GrantSchema, { id: 'grt_P', node: 'alpha', subject: 'agent:lead@alpha', state: GrantState.PENDING }),
    create(GrantSchema, { id: 'grt_A', node: 'alpha', subject: 'agent:lead@alpha', ownerId: 'u1', state: GrantState.ACTIVE }),
  ]);
  registerCoreCommands();
  registerViewCommands();
  const ctx = {
    rights: { mode: 'hub' as const, isAdmin: true, readOnly: false, userId: 'u1', userName: 'ada' },
    navigate: () => undefined,
  };
  const ids = new Set<string>();
  useFreeze.setState({ status: create(FreezeStatusResponseSchema, {}), loaded: true });
  for (const c of availableCommands(ctx)) ids.add(c.id);
  useFreeze.setState({ status: create(FreezeStatusResponseSchema, { allFrozen: true, frozenChannels: ['#ops'] }), loaded: true });
  for (const c of availableCommands(ctx)) ids.add(c.id);
  return ids;
}

describe('CLI parity (spec phase 6)', () => {
  afterEach(() => resetAll());

  it('offers every CLI admin action in the hub UI', () => {
    const ids = paletteIds();
    const missing: string[] = [];
    for (const a of CLI_ACTIONS) {
      for (const p of a.where) {
        if ('palette' in p) {
          if (!ids.has(p.palette)) missing.push(`mycel ${a.verb}: no palette command ${p.palette}`);
        } else if (!viewCalls(p.view, a.rpc)) {
          missing.push(`mycel ${a.verb}: ${p.view} does not call ${a.rpc}`);
        }
      }
    }
    expect(missing).toEqual([]);
  });

  it('names a CLI verb for every mutating Connect method', () => {
    const named = new Set(CLI_ACTIONS.map((a) => a.rpc));
    const writes: string[] = [];
    for (const [name, svc] of SERVICES) {
      for (const m of svc.methods) {
        expect(m.localName, `${name}.${m.localName} looks like a host-only verb`).not.toMatch(HOST_ONLY);
        if (!READ.test(m.localName)) writes.push(`${name}.${m.localName}`);
      }
    }
    expect(SERVICES.map(([n]) => n)).toEqual(expect.arrayContaining(['TokenService', 'UserService', 'EventService', 'ControlService']));
    expect(writes.length).toBeGreaterThan(30);
    expect(writes.filter((w) => !named.has(w) && !NO_CLI_VERB.has(w))).toEqual([]);
  });

  it('hides every admin action from the node UI', () => {
    signIn(fakeServer(), { mode: 'node' });
    seed(channelsStore, [create(ChannelSchema, { name: '#backend', type: ChannelType.TOPIC, ownerId: 'u1' })]);
    registerViewCommands();
    const ids = availableCommands({
      rights: { mode: 'node', isAdmin: false, readOnly: true, userId: 'u1', userName: 'ada' },
      navigate: () => undefined,
    }).map((c) => c.id);
    const admin = ['user.', 'token.', 'channel.', 'artifact.', 'job.', 'grant.', 'node.', 'agent.', 'network.'];
    expect(ids.filter((i) => admin.some((p) => i.startsWith(p)))).toEqual([]);
  });
});
