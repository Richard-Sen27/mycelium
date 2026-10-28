import { Component as ReactComponent, Suspense, lazy, useEffect, useMemo, useRef, useState, type ReactNode } from 'react';
import { Link } from 'react-router';
import { Button, EmptyState, Frame, Readout, prefersReducedMotion, useNow } from '../../ds';
import { EventKind } from '../../gen/mycel/v1/ui_pb';
import { useEntityList } from '../../app/events/entities';
import { agentsStore, channelsStore, jobsStore, nodesStore, onEvent } from '../../app/events/stores';
import { isPhone } from '../../app/nav';
import { usePrefs } from '../../app/prefs';
import { useInspector } from '../../app/shell/inspector';
import { registerAgentCommands } from '../agents/commands';
import { registerGrantCommands } from '../grants/commands';
import { registerNodeCommands } from '../nodes/commands';
import { inspectKey } from './details';
import { buildScene, messagePaths, type SceneModel } from './layout';
import { ParticlePool } from './particles';
import s from './Constellation.module.css';

// The route module stays small: it maps the stores to a scene model, feeds
// message particles from the event stream and draws the HUD around the
// map. The three.js scene (./Scene) is a separate lazily loaded chunk,
// fetched only when WebGL is available.

registerNodeCommands();
registerAgentCommands();
registerGrantCommands();

const Scene = lazy(() => import('./Scene'));

/** At most this many message particles are alive at once. */
export const PARTICLE_CAPACITY = 256;

let webglCache: boolean | null = null;
/** hasWebGL reports whether a WebGL context can be created (cached). */
export function hasWebGL(): boolean {
  if (webglCache != null) return webglCache;
  try {
    const c = document.createElement('canvas');
    const gl = (c.getContext('webgl2') || c.getContext('webgl')) as WebGLRenderingContext | null;
    webglCache = !!gl;
    // Give the probe context back at once; browsers cap live contexts.
    gl?.getExtension('WEBGL_lose_context')?.loseContext();
  } catch {
    webglCache = false;
  }
  return webglCache;
}

class SceneBoundary extends ReactComponent<{ fallback: ReactNode; children: ReactNode }, { failed: boolean }> {
  state = { failed: false };
  static getDerivedStateFromError() {
    return { failed: true };
  }
  componentDidCatch(err: unknown) {
    console.error('constellation scene failed', err);
  }
  render() {
    return this.state.failed ? this.props.fallback : this.props.children;
  }
}

function NoScene({ reason }: { reason: string }) {
  return (
    <EmptyState title="The network map needs WebGL">
      <p>{reason} Every object is also in the lists:</p>
      <p className={s.lists}>
        <Link to="/nodes">Nodes</Link> · <Link to="/agents">Agents</Link> · <Link to="/jobs">Jobs</Link> ·{' '}
        <Link to="/channels">Channels</Link> · <Link to="/grants">Grants</Link>
      </p>
    </EmptyState>
  );
}

function useReducedMotion(): boolean {
  const motion = usePrefs((st) => st.motion);
  const [sys, setSys] = useState(0);
  useEffect(() => {
    if (typeof matchMedia !== 'function') return;
    const mq = matchMedia('(prefers-reduced-motion: reduce)');
    const on = () => setSys((n) => n + 1);
    mq.addEventListener?.('change', on);
    return () => mq.removeEventListener?.('change', on);
  }, []);
  // Recomputed when the in-app preference or the OS setting changes.
  return useMemo(() => prefersReducedMotion(), [motion, sys]);
}

function useHidden(): boolean {
  const [hidden, setHidden] = useState(() => typeof document !== 'undefined' && document.hidden);
  useEffect(() => {
    const on = () => setHidden(document.hidden);
    document.addEventListener('visibilitychange', on);
    return () => document.removeEventListener('visibilitychange', on);
  }, []);
  return hidden;
}

/** useMessageLight spawns particles for MESSAGE events and counts the rate (per minute). */
export function useMessageLight(model: SceneModel, pool: ParticlePool, enabled: boolean): number[] {
  const keys = useRef(new Set<string>());
  const times = useRef<number[]>([]);
  const on = useRef(enabled);
  on.current = enabled;
  useEffect(() => {
    keys.current = new Set([
      'hub',
      ...model.nodes.map((n) => n.key),
      ...model.agents.map((a) => a.key),
      ...model.channels.map((c) => c.key),
    ]);
  }, [model]);
  useEffect(() => {
    if (!on.current) pool.clear();
  }, [enabled, pool]);
  useEffect(
    () =>
      onEvent((e) => {
        if (e.kind !== EventKind.MESSAGE || e.body.case !== 'message') return;
        const now = Date.now();
        times.current = [...times.current.filter((t) => now - t < 60_000), now].slice(-500);
        if (!on.current || document.hidden) return;
        for (const p of messagePaths(e.body.value, (k) => keys.current.has(k))) pool.spawn(p);
      }),
    [pool],
  );
  return times.current;
}

export function Component() {
  const nodes = useEntityList(nodesStore).items;
  const agents = useEntityList(agentsStore).items;
  const jobs = useEntityList(jobsStore).items;
  const channels = useEntityList(channelsStore).items;
  const loaded = nodesStore.use((st) => st.loaded);
  const reduced = useReducedMotion();
  const hidden = useHidden();
  const setMotion = usePrefs((st) => st.setMotion);
  const pool = useMemo(() => new ParticlePool(PARTICLE_CAPACITY), []);
  const [legend, setLegend] = useState(() => !isPhone());
  // Inspector keys match scene keys, except agents ("agent:" + the id, which starts with "agent:").
  const selected = useInspector((st) => (st.open && st.key ? st.key.replace(/^agent:(?=agent:)/, '') : null));
  const now = useNow();

  const model = useMemo(() => buildScene({ nodes, agents, jobs, channels, now: Date.now() }), [nodes, agents, jobs, channels]);
  const times = useMessageLight(model, pool, !reduced);
  const perMin = times.filter((t) => now - t < 60_000).length;
  const webgl = useMemo(hasWebGL, []);

  const online = model.nodes.filter((n) => n.online).length;
  const running = model.agents.filter((a) => a.state === 'running').length;
  const paused = model.agents.filter((a) => a.state === 'paused').length;

  return (
    <div className={s.page}>
      <Frame
        title="Constellation"
        index="01"
        flush
        className={s.frame}
        bodyClassName={s.body}
        meta={
          <div className={s.meta}>
            <Readout size="sm" label="Nodes" value={online} sub={`of ${model.nodes.length} online`} tone="accent" />
            <Readout size="sm" label="Agents" value={running} sub={paused ? `running · ${paused} paused` : 'running'} />
            <Readout size="sm" label="Sandboxes" value={model.moons.length} sub="running" />
            <Readout size="sm" label="Messages" value={perMin} sub="per minute" />
          </div>
        }
      >
        <div className={s.stage}>
          {!webgl ? (
            <NoScene reason="This browser could not create a WebGL context." />
          ) : (
            <SceneBoundary fallback={<NoScene reason="The 3D view failed to start." />}>
              <Suspense fallback={<div className={s.loading}>Loading the map</div>}>
                <Scene
                  model={model}
                  reduced={reduced}
                  hidden={hidden}
                  selected={selected}
                  pool={pool}
                  onPick={(key, label) => (selected === key ? useInspector.getState().close() : inspectKey(key, label))}
                />
              </Suspense>
            </SceneBoundary>
          )}
          {webgl && loaded && model.nodes.length === 0 && (
            <div className={s.empty}>
              <EmptyState title="No nodes yet">Join a machine with `mycel join`; it appears here as a body around the hub.</EmptyState>
            </div>
          )}
          {webgl && (
            <aside className={s.legend} data-open={legend || undefined} aria-label="Legend">
              <button type="button" className={s.legendToggle} onClick={() => setLegend((v) => !v)} aria-expanded={legend}>
                Legend
              </button>
              {legend && <Legend />}
            </aside>
          )}
          {webgl && (
            <div className={s.controls}>
              <span className={s.hint}>drag to orbit · scroll to zoom · arrows · 0 resets</span>
              <Button size="sm" variant="ghost" aria-pressed={reduced} onClick={() => setMotion(reduced ? 'full' : 'reduced')}>
                Reduce motion
              </Button>
            </div>
          )}
        </div>
      </Frame>
    </div>
  );
}

function Legend() {
  return (
    <ul className={s.legendList}>
      <li>
        <svg viewBox="0 0 24 16" aria-hidden className={s.glyph}>
          <ellipse cx="12" cy="8" rx="10" ry="4" className={s.gRingOn} />
          <circle cx="12" cy="8" r="4" className={s.gBody} />
        </svg>
        node, sized by cores and memory
      </li>
      <li>
        <svg viewBox="0 0 24 16" aria-hidden className={s.glyph}>
          <ellipse cx="12" cy="8" rx="10" ry="4" className={s.gRingOff} />
          <circle cx="12" cy="8" r="4" className={s.gBodyOff} />
        </svg>
        node offline
      </li>
      <li>
        <svg viewBox="0 0 24 16" aria-hidden className={s.glyph}>
          <circle cx="5" cy="8" r="2" className={s.gOk} />
          <circle cx="12" cy="8" r="2" className={s.gWarn} />
          <circle cx="19" cy="8" r="2" className={s.gOff} />
        </svg>
        agent running · paused · stopped
      </li>
      <li>
        <svg viewBox="0 0 24 16" aria-hidden className={s.glyph}>
          <circle cx="12" cy="8" r="1.2" className={s.gMoon} />
          <circle cx="12" cy="8" r="4.5" className={s.gGuide} />
        </svg>
        sandbox (running job)
      </li>
      <li>
        <svg viewBox="0 0 24 16" aria-hidden className={s.glyph}>
          <circle cx="12" cy="8" r="5" className={s.gChan} />
          <circle cx="12" cy="8" r="2.2" className={s.gChan} />
        </svg>
        channel
      </li>
      <li>
        <svg viewBox="0 0 24 16" aria-hidden className={s.glyph}>
          <line x1="2" y1="8" x2="22" y2="8" className={s.gLink} />
          <circle cx="15" cy="8" r="1.6" className={s.gParticle} />
        </svg>
        message travelling a link
      </li>
      <li>
        <svg viewBox="0 0 24 16" aria-hidden className={s.glyph}>
          <line x1="2" y1="8" x2="22" y2="8" className={s.gParent} />
        </svg>
        worker to its orchestrator
      </li>
    </ul>
  );
}
