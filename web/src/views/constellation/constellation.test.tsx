import { create } from '@bufbuild/protobuf';
import { act, render, renderHook, screen } from '@testing-library/react';
import { MemoryRouter } from 'react-router';
import { afterEach, describe, expect, it } from 'vitest';
import { MessageSchema } from '../../gen/mycel/v1/envelope_pb';
import { JobState } from '../../gen/mycel/v1/job_pb';
import { EventKind, EventOp, EventSchema } from '../../gen/mycel/v1/ui_pb';
import { agentsStore, channelsStore, dispatchEvent, jobsStore, nodesStore } from '../../app/events/stores';
import { ADMIN, agent, channel, job, node, resetAll, seed, signIn } from '../nodes/fixtures.test-utils';
import { Component as ConstellationView, useMessageLight } from './index';
import { FLIP_AT, placeTags } from './labels';
import { HUB_KEY, Positions, bodyRadius, buildScene, messagePaths, orbitOffset } from './layout';
import { ParticlePool, ease } from './particles';

const NOW = Date.parse('2026-10-28T12:00:00Z');

function network() {
  return {
    nodes: [
      node('alpha', { cpu: 32, memGiB: 128, createdAt: '2026-10-01T00:00:00Z' }),
      node('beta', { cpu: 4, memGiB: 8, online: false, createdAt: '2026-10-02T00:00:00Z' }),
    ],
    agents: [
      agent('lead', 'alpha', { homeChannel: '#backend' }),
      agent('coder', 'alpha', { parent: 'agent:lead@alpha', homeChannel: '#backend' }),
      agent('nomad', 'gone'),
      agent('done', 'alpha', { state: 'stopped', exitedAt: '2026-10-20T00:00:00Z' }),
      agent('paused', 'beta', { state: 'paused' }),
    ],
    jobs: [job('job_1', 'alpha'), job('job_2', 'alpha', { state: JobState.SUCCEEDED }), job('job_3', 'nowhere')],
    channels: [
      channel('#backend', ['agent:lead@alpha']),
      channel('dm:a:b', [], { type: 3 }),
      channel('#old', [], { archivedAt: '2026-10-01T00:00:00Z' }),
    ],
    now: NOW,
  };
}

describe('scene model', () => {
  it('maps store rows to bodies, satellites, moons and anchors', () => {
    const m = buildScene(network());
    expect(m.nodes.map((n) => n.key)).toEqual(['node:alpha', 'node:beta']);
    // Bigger machines are bigger bodies.
    expect(m.nodes[0].radius).toBeGreaterThan(m.nodes[1].radius);
    expect(m.nodes[1].online).toBe(false);
    // Long-stopped agents are left out; agents of unknown nodes orbit the hub.
    expect(m.agents.map((a) => a.name).sort()).toEqual(['coder', 'lead', 'nomad', 'paused']);
    expect(m.agents.find((a) => a.name === 'nomad')?.orbit.center).toBe(HUB_KEY);
    expect(m.agents.find((a) => a.name === 'coder')?.orbit.center).toBe('node:alpha');
    expect(m.agents.find((a) => a.name === 'paused')?.state).toBe('paused');
    // Only running sandboxes on known nodes are moons.
    expect(m.moons.map((x) => x.jobId)).toEqual(['job_1']);
    // Direct and archived channels are not anchors.
    expect(m.channels.map((c) => c.name)).toEqual(['#backend']);
  });

  it('links nodes to the hub, agents to home, parent and channels', () => {
    const m = buildScene(network());
    const has = (a: string, b: string, kind: string) => m.links.some((l) => l.a === a && l.b === b && l.kind === kind);
    expect(has(HUB_KEY, 'node:alpha', 'hub')).toBe(true);
    expect(has(HUB_KEY, 'node:beta', 'hub')).toBe(false); // offline
    expect(has('agent:coder@alpha', 'node:alpha', 'home')).toBe(true);
    expect(has('agent:coder@alpha', 'agent:lead@alpha', 'parent')).toBe(true);
    expect(has('agent:lead@alpha', 'channel:#backend', 'channel')).toBe(true);
    expect(has('agent:coder@alpha', 'channel:#backend', 'channel')).toBe(true); // home channel
  });

  it('is deterministic and keeps existing nodes in place when one joins', () => {
    const a = buildScene(network());
    const b = buildScene(network());
    expect(a).toEqual(b);
    const more = network();
    more.nodes.push(node('gamma', { createdAt: '2026-10-03T00:00:00Z' }));
    const c = buildScene(more);
    expect(c.nodes[0].pos).toEqual(a.nodes[0].pos);
    expect(c.nodes[1].pos).toEqual(a.nodes[1].pos);
  });

  it('scales to the frame budget (20 nodes, 100 agents, 50 jobs)', () => {
    const nodes = Array.from({ length: 20 }, (_, i) => node(`n${i}`, { createdAt: `2026-10-01T00:00:${String(i).padStart(2, '0')}Z` }));
    const agents = Array.from({ length: 100 }, (_, i) =>
      agent(`a${i}`, `n${i % 20}`, i % 5 ? { parent: `agent:a${i - (i % 5)}@n${(i - (i % 5)) % 20}` } : {}),
    );
    const jobs = Array.from({ length: 50 }, (_, i) => job(`job_${i}`, `n${i % 20}`));
    const m = buildScene({ nodes, agents, jobs, channels: [], now: NOW });
    expect(m.nodes).toHaveLength(20);
    expect(m.agents).toHaveLength(100);
    expect(m.moons).toHaveLength(50);
    // No two bodies overlap.
    for (let i = 0; i < m.nodes.length; i++)
      for (let j = i + 1; j < m.nodes.length; j++) {
        const [p, q] = [m.nodes[i], m.nodes[j]];
        const d = Math.hypot(p.pos[0] - q.pos[0], p.pos[1] - q.pos[1], p.pos[2] - q.pos[2]);
        expect(d).toBeGreaterThan(p.radius + q.radius);
      }
    const pos = new Positions();
    pos.update(m, 0);
    expect(pos.map.size).toBe(1 + 20 + 100 + 50);
  });

  it('orbits keep their radius and move with time', () => {
    const o = { center: HUB_KEY, radius: 3, phase: 0.3, speed: 0.2, tilt: 0.4, node: 1.1 };
    const p0 = orbitOffset(o, 0);
    const p1 = orbitOffset(o, 5);
    expect(Math.hypot(...p0)).toBeCloseTo(3, 6);
    expect(Math.hypot(...p1)).toBeCloseTo(3, 6);
    expect(p0).not.toEqual(p1);
    expect(bodyRadius(0, 0)).toBe(0.6);
    expect(bodyRadius(256, 2048)).toBeLessThanOrEqual(2.4);
  });

  it('routes a message from the poster via the channel to each mentioned agent', () => {
    const m = buildScene(network());
    const keys = new Set([HUB_KEY, ...m.nodes.map((n) => n.key), ...m.agents.map((a) => a.key), ...m.channels.map((c) => c.key)]);
    const has = (k: string) => keys.has(k);
    const msg = create(MessageSchema, { from: 'agent:lead@alpha', channel: '#backend', to: ['agent:coder@alpha', 'user:u1'] });
    expect(messagePaths(msg, has)).toEqual([['agent:lead@alpha', 'channel:#backend', 'agent:coder@alpha']]);
    const human = create(MessageSchema, { from: 'user:u1', channel: '#backend', to: [] });
    expect(messagePaths(human, has)).toEqual([[HUB_KEY, 'channel:#backend']]);
    const dm = create(MessageSchema, { from: 'agent:lead@alpha', channel: 'dm:x:y', to: ['agent:coder@alpha'] });
    expect(messagePaths(dm, has)).toEqual([['agent:lead@alpha', 'agent:coder@alpha']]);
  });
});

describe('particle pool', () => {
  it('caps live particles and recycles finished ones', () => {
    const pool = new ParticlePool(4);
    for (let i = 0; i < 10; i++) pool.spawn(['a', 'b', 'c'], 1);
    expect(pool.live).toBe(4);
    expect(pool.dropped).toBe(6);
    pool.update(1.5); // halfway along the second segment
    expect(pool.live).toBe(4);
    pool.forEach((p) => expect(p.seg).toBe(1));
    pool.update(1); // past the end
    expect(pool.live).toBe(0);
    expect(pool.spawn(['a', 'b'])).toBe(true);
    expect(pool.spawn(['a'])).toBe(false);
    expect(ease(0)).toBe(0);
    expect(ease(1)).toBe(1);
  });
});

describe('constellation route', () => {
  afterEach(resetAll);

  it('degrades to a notice and the lists without WebGL', () => {
    signIn(ADMIN);
    seed(nodesStore, network().nodes);
    seed(agentsStore, network().agents);
    seed(jobsStore, network().jobs);
    seed(channelsStore, network().channels);
    render(
      <MemoryRouter>
        <ConstellationView />
      </MemoryRouter>,
    );
    expect(screen.getByText('The network map needs WebGL')).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Nodes' })).toHaveAttribute('href', '/nodes');
    expect(screen.getByRole('link', { name: 'Agents' })).toBeInTheDocument();
    // The readouts still count the network.
    expect(screen.getByText('of 2 online')).toBeInTheDocument();
  });
});

describe('message light and reduced motion', () => {
  afterEach(resetAll);

  const msgEvent = (seq: number) =>
    create(EventSchema, {
      seq: BigInt(seq),
      kind: EventKind.MESSAGE,
      op: EventOp.CREATED,
      id: `msg_${seq}`,
      body: {
        case: 'message',
        value: create(MessageSchema, {
          id: `msg_${seq}`,
          seq: BigInt(seq),
          from: 'agent:lead@alpha',
          channel: '#backend',
          to: ['agent:coder@alpha'],
        }),
      },
    });

  it('spawns a particle per mention, none under reduced motion, and still counts the rate', () => {
    const model = buildScene(network());
    const pool = new ParticlePool(8);
    const { rerender, result } = renderHook(({ on }) => useMessageLight(model, pool, on), { initialProps: { on: true } });
    act(() => dispatchEvent(msgEvent(1)));
    expect(pool.live).toBe(1);
    rerender({ on: false }); // reduced motion: the pool empties and stays empty
    expect(pool.live).toBe(0);
    act(() => dispatchEvent(msgEvent(2)));
    expect(pool.live).toBe(0);
    rerender({ on: false });
    expect(result.current.length).toBe(2);
  });
});

describe('label placement', () => {
  it('flips right-hand tags and hides the lower-priority one of two colliding tags', () => {
    const tags = [
      { key: 'a', x: 100, y: 100, r: 8, width: 80, height: 14, priority: 200 },
      { key: 'b', x: 104, y: 104, r: 8, width: 80, height: 14, priority: 100 },
      { key: 'c', x: 100, y: 300, r: 8, width: 80, height: 14, priority: 50 },
      { key: 'd', x: 900, y: 100, r: 8, width: 80, height: 14, priority: 50 },
    ];
    const out = placeTags(tags, 1000);
    expect(out.get('a')).toEqual({ flip: false, hidden: false });
    expect(out.get('b')?.hidden).toBe(true);
    expect(out.get('c')?.hidden).toBe(false);
    expect(out.get('d')?.flip).toBe(900 > 1000 * FLIP_AT);
  });
});
