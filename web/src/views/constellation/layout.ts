import { ChannelType, type Channel } from '../../gen/mycel/v1/channel_pb';
import type { Message } from '../../gen/mycel/v1/envelope_pb';
import type { Agent } from '../../gen/mycel/v1/hub_pb';
import { JobState, type Job } from '../../gen/mycel/v1/job_pb';
import type { Node } from '../../gen/mycel/v1/node_pb';
import { agentKind, agentState } from '../agents/agents';

// The constellation's data model: store rows → scene objects with
// deterministic positions and orbits. Pure (no three.js, no DOM), so it is
// unit-tested without WebGL and the 3D module only draws what this returns.
//
// Geometry: the hub sits at the origin. Nodes are bodies on a golden-angle
// spiral in a shallow disc, in join order (a new node appears at the rim,
// the others stay put), sized by capacity. Agents are satellites on tilted
// orbits around their home node; running sandboxes (jobs) are small moons
// on tighter orbits. Channels are anchors on an outer ring above the disc.

export type Vec3 = [number, number, number];

export interface Orbit {
  /** Key of the body this orbits. */
  center: string;
  radius: number;
  /** Angle at t = 0 (radians). */
  phase: number;
  /** Angular speed (radians per second). */
  speed: number;
  /** Inclination of the orbit plane (radians) about an axis in the disc at angle `node`. */
  tilt: number;
  node: number;
}

export type BodyKind = 'hub' | 'node' | 'agent' | 'moon' | 'channel';

export interface SceneNode {
  key: string; // "node:<name>"
  name: string;
  pos: Vec3;
  radius: number;
  online: boolean;
  cpu: number;
  memGiB: number;
  gpus: number;
}

export interface SceneAgent {
  key: string; // "agent:<id>" (the id already starts with "agent:")
  id: string;
  name: string;
  nodeName: string;
  state: 'running' | 'paused' | 'stopped';
  kind: 'orchestrator' | 'worker';
  orbit: Orbit;
}

export interface SceneMoon {
  key: string; // "job:<id>"
  jobId: string;
  nodeName: string;
  label: string;
  orbit: Orbit;
}

export interface SceneChannel {
  key: string; // "channel:<name>"
  name: string;
  pos: Vec3;
  radius: number;
}

export type LinkKind = 'hub' | 'home' | 'parent' | 'channel';

export interface SceneLink {
  a: string;
  b: string;
  kind: LinkKind;
}

export interface SceneModel {
  nodes: SceneNode[];
  agents: SceneAgent[];
  moons: SceneMoon[];
  channels: SceneChannel[];
  links: SceneLink[];
  /** Distance from the hub to the outermost object (for the camera and range rings). */
  extent: number;
}

export const HUB_KEY = 'hub';
const GOLDEN = Math.PI * (3 - Math.sqrt(5));
const SPIRAL_START = 7;
const SPIRAL_STEP = 5.6;
const STOPPED_VISIBLE_MS = 24 * 3600_000;
const MAX_CHANNEL_LINKS = 3;

/** hash01 maps a string (and a salt) to [0, 1) deterministically (FNV-1a). */
export function hash01(s: string, salt = 0): number {
  let h = 0x811c9dc5 ^ salt;
  for (let i = 0; i < s.length; i++) {
    h ^= s.charCodeAt(i);
    h = Math.imul(h, 0x01000193);
  }
  h ^= h >>> 15;
  h = Math.imul(h, 0x2c1b3c6d);
  h ^= h >>> 12;
  return (h >>> 0) / 4294967296;
}

/** bodyRadius: a node's size from its cores and memory (log scale, clamped). */
export function bodyRadius(cpu: number, memGiB: number): number {
  if (cpu <= 0 && memGiB <= 0) return 0.6;
  const r = 0.4 + 0.32 * Math.log2(1 + Math.max(0, cpu) / 2) + 0.22 * Math.log2(1 + Math.max(0, memGiB) / 8);
  return Math.min(2.4, Math.max(0.5, r));
}

export function nodeKey(name: string): string {
  return `node:${name}`;
}
export function agentKey(id: string): string {
  return id.startsWith('agent:') ? id : `agent:${id}`;
}
export function channelKey(name: string): string {
  return `channel:${name}`;
}
export function jobKey(id: string): string {
  return `job:${id}`;
}

function isActiveJob(j: Job): boolean {
  return (j.state === JobState.STARTING || j.state === JobState.RUNNING) && !!j.node;
}

export interface SceneInput {
  nodes: Node[];
  agents: Agent[];
  jobs: Job[];
  channels: Channel[];
  /** For hiding agents that stopped long ago. */
  now: number;
}

/** buildScene maps store rows to scene objects. Deterministic for the same input. */
export function buildScene(inp: SceneInput): SceneModel {
  const nodes: SceneNode[] = [];
  const byNode = new Map<string, SceneNode>();
  const ordered = [...inp.nodes].sort((a, b) => a.createdAt.localeCompare(b.createdAt) || a.name.localeCompare(b.name));
  let extent = 4;
  ordered.forEach((n, i) => {
    const cpu = n.inventory?.cpuCount ?? 0;
    const memGiB = Number(n.inventory?.memBytes ?? 0n) / 1024 ** 3;
    const r = SPIRAL_START + SPIRAL_STEP * Math.sqrt(i);
    const a = i * GOLDEN;
    const y = (hash01(n.name, 7) - 0.5) * 3;
    const node: SceneNode = {
      key: nodeKey(n.name),
      name: n.name,
      pos: [r * Math.cos(a), y, r * Math.sin(a)],
      radius: bodyRadius(cpu, memGiB),
      online: n.online,
      cpu,
      memGiB,
      gpus: n.inventory?.gpus.length ?? 0,
    };
    nodes.push(node);
    byNode.set(n.name, node);
    extent = Math.max(extent, r + node.radius);
  });

  // Agents: satellites of their node (or of the hub when the node is not visible).
  const visible = inp.agents.filter((a) => {
    if (agentState(a) !== 'stopped') return true;
    const t = Date.parse(a.exitedAt);
    return Number.isNaN(t) || inp.now - t < STOPPED_VISIBLE_MS;
  });
  const agents: SceneAgent[] = [];
  const slotOf = new Map<string, number>();
  for (const a of [...visible].sort((x, y) => (x.parent ? 1 : 0) - (y.parent ? 1 : 0) || x.id.localeCompare(y.id))) {
    const host = byNode.get(a.nodeName);
    const center = host ? host.key : HUB_KEY;
    const slot = slotOf.get(center) ?? 0;
    slotOf.set(center, slot + 1);
    const base = host ? host.radius : 1.2;
    const radius = base + 1.1 + 0.5 * (slot % 4) + 0.18 * Math.floor(slot / 4);
    const orch = agentKind(a) === 'orchestrator';
    agents.push({
      key: agentKey(a.id),
      id: a.id,
      name: a.name,
      nodeName: a.nodeName,
      state: agentState(a),
      kind: orch ? 'orchestrator' : 'worker',
      orbit: {
        center,
        radius,
        phase: hash01(a.id, 1) * Math.PI * 2 + slot * GOLDEN,
        speed: (orch ? 0.07 : 0.11) / Math.sqrt(radius),
        tilt: (hash01(a.id, 2) - 0.5) * 0.9,
        node: hash01(a.id, 3) * Math.PI * 2,
      },
    });
  }

  // Moons: one per running sandbox, close to its node.
  const moons: SceneMoon[] = [];
  const moonSlot = new Map<string, number>();
  for (const j of inp.jobs.filter(isActiveJob).sort((x, y) => x.id.localeCompare(y.id))) {
    const host = byNode.get(j.node);
    if (!host) continue;
    const slot = moonSlot.get(host.key) ?? 0;
    moonSlot.set(host.key, slot + 1);
    const radius = host.radius + 0.5 + 0.2 * (slot % 3);
    moons.push({
      key: jobKey(j.id),
      jobId: j.id,
      nodeName: j.node,
      label: j.spec?.name || j.spec?.command.join(' ') || j.id,
      orbit: {
        center: host.key,
        radius,
        phase: hash01(j.id, 4) * Math.PI * 2,
        speed: 0.26 / Math.sqrt(radius),
        tilt: (hash01(j.id, 5) - 0.5) * 0.5,
        node: hash01(j.id, 6) * Math.PI * 2,
      },
    });
  }

  // Channels: anchors on an outer ring above the disc, each turned toward
  // the nodes its agents live on (short links), then spread apart.
  const chans = inp.channels.filter((c) => !c.archivedAt && c.type !== ChannelType.DIRECT).sort((a, b) => a.name.localeCompare(b.name));
  const ringR = extent + 8;
  const angles = chans.map((c, i) => {
    let x = 0;
    let z = 0;
    const ids = new Set(c.members.map((m) => m.id));
    for (const a of visible) {
      if (!ids.has(a.id) && a.homeChannel !== c.name) continue;
      const host = byNode.get(a.nodeName);
      if (!host) continue;
      x += host.pos[0];
      z += host.pos[2];
    }
    return { i, a: x === 0 && z === 0 ? (i / Math.max(1, chans.length)) * Math.PI * 2 + 0.35 : Math.atan2(z, x) };
  });
  spreadAngles(angles, Math.min(0.6, (Math.PI * 2) / Math.max(1, chans.length)));
  const channels: SceneChannel[] = chans.map((c, i) => {
    const a = angles.find((x) => x.i === i)!.a;
    return { key: channelKey(c.name), name: c.name, pos: [ringR * Math.cos(a), 5, ringR * Math.sin(a)], radius: 0.7 };
  });
  if (channels.length) extent = Math.max(extent, ringR + 1);

  // Links: hub → online node, agent → home node, worker → parent, agent → its channels.
  const links: SceneLink[] = [];
  for (const n of nodes) if (n.online) links.push({ a: HUB_KEY, b: n.key, kind: 'hub' });
  const agentKeys = new Set(agents.map((a) => a.key));
  const chanKeys = new Set(channels.map((c) => c.key));
  const memberOf = new Map<string, string[]>();
  for (const c of chans) {
    for (const m of c.members) {
      if (!m.id.startsWith('agent:')) continue;
      const arr = memberOf.get(m.id) ?? [];
      arr.push(channelKey(c.name));
      memberOf.set(m.id, arr);
    }
  }
  const rowById = new Map(visible.map((a) => [a.id, a]));
  for (const sa of agents) {
    links.push({ a: sa.key, b: sa.orbit.center, kind: 'home' });
    const row = rowById.get(sa.id)!;
    if (row.parent && agentKeys.has(agentKey(row.parent))) links.push({ a: sa.key, b: agentKey(row.parent), kind: 'parent' });
    const cs = new Set(memberOf.get(sa.id) ?? []);
    if (row.homeChannel) cs.add(channelKey(row.homeChannel));
    let k = 0;
    for (const c of cs) {
      if (!chanKeys.has(c) || k >= MAX_CHANNEL_LINKS) continue;
      links.push({ a: sa.key, b: c, kind: 'channel' });
      k++;
    }
  }

  return { nodes, agents, moons, channels, links, extent };
}

/** spreadAngles nudges angles apart until neighbours are at least `min` radians apart (in place). */
export function spreadAngles(items: Array<{ a: number }>, min: number): void {
  if (items.length < 2) return;
  const TAU = Math.PI * 2;
  for (const it of items) it.a = ((it.a % TAU) + TAU) % TAU;
  for (let pass = 0; pass < 24; pass++) {
    items.sort((p, q) => p.a - q.a);
    let moved = false;
    for (let k = 0; k < items.length; k++) {
      const p = items[k];
      const q = items[(k + 1) % items.length];
      const gap = (q.a - p.a + TAU) % TAU;
      if (gap < min) {
        const push = (min - gap) / 2;
        p.a = (p.a - push + TAU) % TAU;
        q.a = (q.a + push) % TAU;
        moved = true;
      }
    }
    if (!moved) return;
  }
}

/** orbitOffset writes the offset of an orbiting body from its centre at time t (seconds). */
export function orbitOffset(o: Orbit, t: number, out: Vec3 = [0, 0, 0]): Vec3 {
  const th = o.phase + o.speed * t;
  // A point on a circle in the disc (XZ), rotated by `tilt` about the axis u = (cos node, 0, sin node).
  const vx = o.radius * Math.cos(th);
  const vz = o.radius * Math.sin(th);
  const ux = Math.cos(o.node);
  const uz = Math.sin(o.node);
  const c = Math.cos(o.tilt);
  const s = Math.sin(o.tilt);
  const dot = ux * vx + uz * vz;
  // Rodrigues: v cos + (u × v) sin + u (u·v)(1 − cos); with v.y = u.y = 0 only (u × v).y survives.
  out[0] = vx * c + ux * dot * (1 - c);
  out[1] = (uz * vx - ux * vz) * s;
  out[2] = vz * c + uz * dot * (1 - c);
  return out;
}

/**
 * Positions resolves every object's position at time t into a map of
 * reusable vectors (no allocation per frame once warmed up).
 */
export class Positions {
  readonly map = new Map<string, Vec3>();
  private tmp: Vec3 = [0, 0, 0];

  update(m: SceneModel, t: number): void {
    const set = (k: string, x: number, y: number, z: number) => {
      let v = this.map.get(k);
      if (!v) {
        v = [0, 0, 0];
        this.map.set(k, v);
      }
      v[0] = x;
      v[1] = y;
      v[2] = z;
    };
    set(HUB_KEY, 0, 0, 0);
    for (const n of m.nodes) set(n.key, n.pos[0], n.pos[1], n.pos[2]);
    for (const c of m.channels) set(c.key, c.pos[0], c.pos[1], c.pos[2]);
    for (const list of [m.agents, m.moons]) {
      for (const b of list) {
        const c = this.map.get(b.orbit.center) ?? [0, 0, 0];
        const o = orbitOffset(b.orbit, t, this.tmp);
        set(b.key, c[0] + o[0], c[1] + o[1], c[2] + o[2]);
      }
    }
  }

  /** prune drops vectors of objects that left the model. */
  prune(m: SceneModel): void {
    const keep = new Set<string>([
      HUB_KEY,
      ...m.nodes.map((n) => n.key),
      ...m.channels.map((c) => c.key),
      ...m.agents.map((a) => a.key),
      ...m.moons.map((x) => x.key),
    ]);
    for (const k of this.map.keys()) if (!keep.has(k)) this.map.delete(k);
  }
}

/**
 * messagePaths: the routes a message's light takes. From the poster (its
 * agent, or the hub for humans and the hub itself) to the channel anchor
 * and on to each mentioned agent; direct messages go straight to the
 * mentioned agents. Keys not in the scene are skipped.
 */
export function messagePaths(m: Pick<Message, 'from' | 'channel' | 'to'>, has: (key: string) => boolean): string[][] {
  const from = m.from.startsWith('agent:') && has(agentKey(m.from)) ? agentKey(m.from) : HUB_KEY;
  const ch = channelKey(m.channel);
  const via = has(ch) ? [ch] : [];
  const targets = m.to
    .filter((t) => t.startsWith('agent:'))
    .map(agentKey)
    .filter((k) => has(k) && k !== from);
  const paths = targets.length ? targets.map((t) => [from, ...via, t]) : [[from, ...via]];
  return paths.filter((p) => p.length >= 2 && new Set(p).size === p.length);
}
