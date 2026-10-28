import { Canvas, useFrame, useThree } from '@react-three/fiber';
import { useEffect, useMemo, useRef, type KeyboardEvent, type MutableRefObject } from 'react';
import * as THREE from 'three';
import { OrbitControls } from 'three/examples/jsm/controls/OrbitControls.js';
import { HUB_KEY, Positions, orbitOffset, type SceneModel, type Vec3 } from './layout';
import { placeTags, type TagInput } from './labels';
import { ease, type ParticlePool } from './particles';
import s from './Constellation.module.css';

// The 3D network view (three.js through react-three-fiber). This module and
// everything it imports from three / @react-three is the lazily loaded
// `scene` chunk; the route module only loads it when WebGL is available.
//
// Drawing budget: one draw call per kind (instanced satellites and moons,
// one line set per link kind, one point set each for halos, particles and
// stars), node bodies as a handful of meshes. Labels are DOM buttons
// positioned from the projected positions once per frame.

export interface SceneProps {
  model: SceneModel;
  reduced: boolean;
  /** Tab hidden: stop rendering entirely. */
  hidden: boolean;
  selected: string | null;
  pool: ParticlePool;
  onPick: (key: string, label: string) => void;
}

interface Palette {
  bg: THREE.Color;
  line: THREE.Color;
  lineStrong: THREE.Color;
  text0: THREE.Color;
  text1: THREE.Color;
  text2: THREE.Color;
  text3: THREE.Color;
  accent: THREE.Color;
  accentStrong: THREE.Color;
  warn: THREE.Color;
}

/** readPalette takes the theme's tokens, so a second skin recolours the scene too. */
function readPalette(): Palette {
  const css = getComputedStyle(document.documentElement);
  const c = (name: string, fallback: string) => {
    const v = css.getPropertyValue(name).trim();
    try {
      return new THREE.Color(/^#[0-9a-f]{6}$/i.test(v) ? v : fallback);
    } catch {
      return new THREE.Color(fallback);
    }
  };
  return {
    bg: c('--bg-0', '#07090b'),
    line: c('--line', '#1c232a'),
    lineStrong: c('--line-strong', '#2b343e'),
    text0: c('--text-0', '#e6ebf0'),
    text1: c('--text-1', '#a7b2bc'),
    text2: c('--text-2', '#7f8b96'),
    text3: c('--text-3', '#4a545e'),
    accent: c('--accent', '#8ed1e6'),
    accentStrong: c('--accent-strong', '#addfef'),
    warn: c('--warn', '#f0a050'),
  };
}

// ---- Shared point material: round, soft-edged dots, optionally size-attenuated ----

const DOT_VERT = /* glsl */ `
  attribute float aSize;
  attribute vec3 aColor;
  attribute float aAlpha;
  uniform float uScale;
  uniform float uAttenuate;
  varying vec3 vColor;
  varying float vAlpha;
  void main() {
    vColor = aColor;
    vAlpha = aAlpha;
    vec4 mv = modelViewMatrix * vec4(position, 1.0);
    gl_PointSize = uAttenuate > 0.5 ? aSize * uScale / -mv.z : aSize * uScale;
    gl_Position = projectionMatrix * mv;
  }
`;
const DOT_FRAG = /* glsl */ `
  uniform float uSoft;
  varying vec3 vColor;
  varying float vAlpha;
  void main() {
    float d = length(gl_PointCoord - vec2(0.5));
    if (d > 0.5) discard;
    float a = uSoft > 0.5 ? exp(-d * d * 18.0) : smoothstep(0.5, 0.32, d);
    gl_FragColor = vec4(vColor, a * vAlpha);
  }
`;

function dotMaterial(o: { attenuate: boolean; soft: boolean; additive?: boolean }): THREE.ShaderMaterial {
  return new THREE.ShaderMaterial({
    vertexShader: DOT_VERT,
    fragmentShader: DOT_FRAG,
    uniforms: { uScale: { value: 1 }, uAttenuate: { value: o.attenuate ? 1 : 0 }, uSoft: { value: o.soft ? 1 : 0 } },
    transparent: true,
    depthWrite: false,
    blending: o.additive ? THREE.AdditiveBlending : THREE.NormalBlending,
  });
}

function dotGeometry(capacity: number): THREE.BufferGeometry {
  const g = new THREE.BufferGeometry();
  g.setAttribute('position', new THREE.BufferAttribute(new Float32Array(capacity * 3), 3).setUsage(THREE.DynamicDrawUsage));
  g.setAttribute('aColor', new THREE.BufferAttribute(new Float32Array(capacity * 3), 3));
  g.setAttribute('aSize', new THREE.BufferAttribute(new Float32Array(capacity), 1));
  g.setAttribute('aAlpha', new THREE.BufferAttribute(new Float32Array(capacity), 1));
  g.setDrawRange(0, 0);
  return g;
}

/** Keeps a point material's pixel scale in step with the canvas height and DPR. */
function useDotScale(mat: THREE.ShaderMaterial, attenuate: boolean) {
  const size = useThree((st) => st.size);
  const dpr = useThree((st) => st.viewport.dpr);
  const camera = useThree((st) => st.camera) as THREE.PerspectiveCamera;
  useEffect(() => {
    const fov = ((camera.fov ?? 40) * Math.PI) / 180;
    mat.uniforms.uScale.value = attenuate ? (size.height * dpr) / (2 * Math.tan(fov / 2)) : dpr;
  }, [mat, size.height, dpr, camera, attenuate]);
}

function useDispose(...objs: Array<{ dispose(): void }>) {
  useEffect(() => () => objs.forEach((o) => o.dispose()), objs);
}

// ---- Starfield: a few thousand faint points far out, turning very slowly ----

function mulberry32(seed: number) {
  return () => {
    seed |= 0;
    seed = (seed + 0x6d2b79f5) | 0;
    let t = Math.imul(seed ^ (seed >>> 15), 1 | seed);
    t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t;
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

function Starfield({ palette, reduced }: { palette: Palette; reduced: boolean }) {
  const N = 2200;
  const { points, geo, mat } = useMemo(() => {
    const rnd = mulberry32(42);
    const geo = dotGeometry(N);
    const pos = geo.getAttribute('position') as THREE.BufferAttribute;
    const col = geo.getAttribute('aColor') as THREE.BufferAttribute;
    const size = geo.getAttribute('aSize') as THREE.BufferAttribute;
    const alpha = geo.getAttribute('aAlpha') as THREE.BufferAttribute;
    const c = new THREE.Color();
    for (let i = 0; i < N; i++) {
      // Uniform on a sphere, at a depth between 300 and 900 units.
      const u = rnd() * 2 - 1;
      const th = rnd() * Math.PI * 2;
      const r = 300 + Math.pow(rnd(), 0.6) * 600;
      const q = Math.sqrt(1 - u * u);
      pos.setXYZ(i, r * q * Math.cos(th), r * u, r * q * Math.sin(th));
      const b = rnd();
      c.copy(b > 0.92 ? palette.text1 : b > 0.5 ? palette.text2 : palette.text3);
      col.setXYZ(i, c.r, c.g, c.b);
      size.setX(i, b > 0.97 ? 2.2 : b > 0.8 ? 1.6 : 1.1);
      alpha.setX(i, 0.35 + 0.5 * rnd());
    }
    geo.setDrawRange(0, N);
    const mat = dotMaterial({ attenuate: false, soft: false });
    const points = new THREE.Points(geo, mat);
    points.frustumCulled = false;
    return { points, geo, mat };
  }, [palette]);
  useDotScale(mat, false);
  useDispose(geo, mat);
  useFrame((_, dt) => {
    if (!reduced) points.rotation.y += Math.min(dt, 0.1) * 0.0035;
  });
  return <primitive object={points} />;
}

// ---- Static hairlines: range rings, body rings, channel rings, orbit guides ----

function circle(out: number[], cx: number, cy: number, cz: number, r: number, seg: number) {
  for (let i = 0; i < seg; i++) {
    const a0 = (i / seg) * Math.PI * 2;
    const a1 = ((i + 1) / seg) * Math.PI * 2;
    out.push(cx + r * Math.cos(a0), cy, cz + r * Math.sin(a0), cx + r * Math.cos(a1), cy, cz + r * Math.sin(a1));
  }
}

function lines(points: number[], color: THREE.Color, opacity: number): THREE.LineSegments {
  const g = new THREE.BufferGeometry();
  g.setAttribute('position', new THREE.Float32BufferAttribute(points, 3));
  const m = new THREE.LineBasicMaterial({ color, transparent: true, opacity, depthWrite: false });
  const l = new THREE.LineSegments(g, m);
  l.frustumCulled = false;
  return l;
}

function disposeLines(l: THREE.LineSegments) {
  l.geometry.dispose();
  (l.material as THREE.Material).dispose();
}

function Hairlines({ model, palette }: { model: SceneModel; palette: Palette }) {
  const group = useMemo(() => {
    const g = new THREE.Group();
    const range: number[] = [];
    for (let r = 10; r <= model.extent + 4; r += 10) circle(range, 0, 0, 0, r, 160);
    g.add(lines(range, palette.line, 0.9));
    const hub: number[] = [];
    circle(hub, 0, 0, 0, 1.5, 48);
    circle(hub, 0, 0, 0, 2.1, 48);
    g.add(lines(hub, palette.text3, 0.9));
    const on: number[] = [];
    const off: number[] = [];
    for (const n of model.nodes) circle(n.online ? on : off, n.pos[0], n.pos[1], n.pos[2], n.radius * 1.45, 64);
    g.add(lines(on, palette.accent, 0.55));
    g.add(lines(off, palette.text3, 0.7));
    const ch: number[] = [];
    for (const c of model.channels) {
      circle(ch, c.pos[0], c.pos[1], c.pos[2], c.radius, 40);
      circle(ch, c.pos[0], c.pos[1], c.pos[2], c.radius * 0.45, 24);
    }
    g.add(lines(ch, palette.text2, 0.7));
    // Orbit guides: where each satellite and moon travels.
    const guides: number[] = [];
    const tmp: Vec3 = [0, 0, 0];
    const centre = new Map<string, Vec3>([[HUB_KEY, [0, 0, 0]], ...model.nodes.map((n) => [n.key, n.pos] as [string, Vec3])]);
    for (const b of [...model.agents, ...model.moons]) {
      const c = centre.get(b.orbit.center) ?? [0, 0, 0];
      const seg = 48;
      let px = 0;
      let py = 0;
      let pz = 0;
      for (let i = 0; i <= seg; i++) {
        orbitOffset({ ...b.orbit, phase: (i / seg) * Math.PI * 2, speed: 0 }, 0, tmp);
        const x = c[0] + tmp[0];
        const y = c[1] + tmp[1];
        const z = c[2] + tmp[2];
        if (i > 0) guides.push(px, py, pz, x, y, z);
        px = x;
        py = y;
        pz = z;
      }
    }
    g.add(lines(guides, palette.line, 0.55));
    return g;
  }, [model, palette]);
  useEffect(() => () => group.children.forEach((c) => disposeLines(c as THREE.LineSegments)), [group]);
  return <primitive object={group} />;
}

// ---- Bodies: the hub and the nodes, lit from the hub ----

function Bodies({ model, palette }: { model: SceneModel; palette: Palette }) {
  const { geo, onMat, offMat, hubMat } = useMemo(
    () => ({
      geo: new THREE.SphereGeometry(1, 40, 28),
      onMat: new THREE.MeshStandardMaterial({ color: palette.lineStrong.clone().lerp(palette.text3, 0.35), roughness: 0.92, metalness: 0 }),
      // Offline bodies are ghosts: present, but not solid.
      offMat: new THREE.MeshStandardMaterial({
        color: palette.lineStrong,
        roughness: 1,
        metalness: 0,
        transparent: true,
        opacity: 0.35,
        depthWrite: false,
      }),
      hubMat: new THREE.MeshBasicMaterial({ color: palette.text1 }),
    }),
    [palette],
  );
  useDispose(geo, onMat, offMat, hubMat);
  return (
    <group>
      <ambientLight intensity={0.55} color={palette.text1} />
      <pointLight position={[0, 0, 0]} intensity={3.2} decay={0} color={palette.text0} />
      <mesh geometry={geo} material={hubMat} scale={0.75} />
      {model.nodes.map((n) => (
        <mesh key={n.key} geometry={geo} material={n.online ? onMat : offMat} position={n.pos} scale={n.radius} />
      ))}
    </group>
  );
}

// ---- Satellites (agents), their halos, and moons (sandboxes) ----

const tmpObj = new THREE.Object3D();

function Satellites({ model, palette, positions }: { model: SceneModel; palette: Palette; positions: Positions }) {
  const count = model.agents.length;
  const cap = Math.max(1, count);
  const { mesh, geo, mat } = useMemo(() => {
    const geo = new THREE.SphereGeometry(0.17, 14, 10);
    const mat = new THREE.MeshBasicMaterial({ color: 0xffffff });
    const mesh = new THREE.InstancedMesh(geo, mat, cap);
    mesh.instanceMatrix.setUsage(THREE.DynamicDrawUsage);
    mesh.frustumCulled = false;
    return { mesh, geo, mat };
  }, [cap]);
  const halo = useMemo(() => {
    const geo = dotGeometry(cap);
    const mat = dotMaterial({ attenuate: true, soft: true, additive: true });
    const pts = new THREE.Points(geo, mat);
    pts.frustumCulled = false;
    return { pts, geo, mat };
  }, [cap]);
  useDotScale(halo.mat, true);
  useDispose(geo, mat, halo.geo, halo.mat);

  useEffect(() => {
    const c = new THREE.Color();
    const col = halo.geo.getAttribute('aColor') as THREE.BufferAttribute;
    const size = halo.geo.getAttribute('aSize') as THREE.BufferAttribute;
    const alpha = halo.geo.getAttribute('aAlpha') as THREE.BufferAttribute;
    model.agents.forEach((a, i) => {
      c.copy(a.state === 'running' ? palette.accent : a.state === 'paused' ? palette.warn : palette.text3);
      mesh.setColorAt(i, c);
      col.setXYZ(i, c.r, c.g, c.b);
      size.setX(i, a.kind === 'orchestrator' ? 1.5 : 1.0);
      // Running agents carry a faint halo; paused a fainter one; stopped none.
      alpha.setX(i, a.state === 'running' ? 0.3 : a.state === 'paused' ? 0.22 : 0);
    });
    mesh.count = count;
    if (mesh.instanceColor) mesh.instanceColor.needsUpdate = true;
    col.needsUpdate = true;
    size.needsUpdate = true;
    alpha.needsUpdate = true;
    halo.geo.setDrawRange(0, count);
  }, [model, palette, mesh, halo, count]);

  useFrame(() => {
    const pos = halo.geo.getAttribute('position') as THREE.BufferAttribute;
    model.agents.forEach((a, i) => {
      const p = positions.map.get(a.key);
      if (!p) return;
      tmpObj.position.set(p[0], p[1], p[2]);
      tmpObj.scale.setScalar(a.kind === 'orchestrator' ? 1.35 : 1);
      tmpObj.updateMatrix();
      mesh.setMatrixAt(i, tmpObj.matrix);
      pos.setXYZ(i, p[0], p[1], p[2]);
    });
    mesh.instanceMatrix.needsUpdate = true;
    pos.needsUpdate = true;
  });
  return (
    <>
      <primitive object={mesh} />
      <primitive object={halo.pts} />
    </>
  );
}

function Moons({ model, palette, positions }: { model: SceneModel; palette: Palette; positions: Positions }) {
  const count = model.moons.length;
  const cap = Math.max(1, count);
  const { mesh, geo, mat } = useMemo(() => {
    const geo = new THREE.SphereGeometry(0.1, 10, 8);
    const mat = new THREE.MeshBasicMaterial({ color: palette.text1 });
    const mesh = new THREE.InstancedMesh(geo, mat, cap);
    mesh.instanceMatrix.setUsage(THREE.DynamicDrawUsage);
    mesh.frustumCulled = false;
    return { mesh, geo, mat };
  }, [cap, palette]);
  useDispose(geo, mat);
  useFrame(() => {
    mesh.count = count;
    model.moons.forEach((m, i) => {
      const p = positions.map.get(m.key);
      if (!p) return;
      tmpObj.position.set(p[0], p[1], p[2]);
      tmpObj.scale.setScalar(1);
      tmpObj.updateMatrix();
      mesh.setMatrixAt(i, tmpObj.matrix);
    });
    mesh.instanceMatrix.needsUpdate = true;
  });
  return <primitive object={mesh} />;
}

// ---- Links: hairlines that follow the orbits ----

function LinkSet({
  model,
  kind,
  color,
  opacity,
  positions,
}: {
  model: SceneModel;
  kind: string;
  color: THREE.Color;
  opacity: number;
  positions: Positions;
}) {
  const links = useMemo(() => model.links.filter((l) => l.kind === kind), [model, kind]);
  const { line, geo, mat } = useMemo(() => {
    const geo = new THREE.BufferGeometry();
    geo.setAttribute(
      'position',
      new THREE.BufferAttribute(new Float32Array(Math.max(1, links.length) * 6), 3).setUsage(THREE.DynamicDrawUsage),
    );
    geo.setDrawRange(0, links.length * 2);
    const mat = new THREE.LineBasicMaterial({ color, transparent: true, opacity, depthWrite: false });
    const line = new THREE.LineSegments(geo, mat);
    line.frustumCulled = false;
    return { line, geo, mat };
  }, [links, color, opacity]);
  useDispose(geo, mat);
  useFrame(() => {
    const pos = geo.getAttribute('position') as THREE.BufferAttribute;
    const arr = pos.array as Float32Array;
    links.forEach((l, i) => {
      const a = positions.map.get(l.a);
      const b = positions.map.get(l.b);
      if (!a || !b) return;
      arr.set(a, i * 6);
      arr.set(b, i * 6 + 3);
    });
    pos.needsUpdate = true;
  });
  return <primitive object={line} />;
}

// ---- Message particles ----

function Particles({ pool, palette, positions }: { pool: ParticlePool; palette: Palette; positions: Positions }) {
  const { pts, geo, mat } = useMemo(() => {
    const geo = dotGeometry(pool.capacity);
    const col = geo.getAttribute('aColor') as THREE.BufferAttribute;
    const size = geo.getAttribute('aSize') as THREE.BufferAttribute;
    const alpha = geo.getAttribute('aAlpha') as THREE.BufferAttribute;
    for (let i = 0; i < pool.capacity; i++) {
      col.setXYZ(i, palette.accentStrong.r, palette.accentStrong.g, palette.accentStrong.b);
      size.setX(i, 0.42);
      alpha.setX(i, 0.9);
    }
    const mat = dotMaterial({ attenuate: true, soft: false });
    const pts = new THREE.Points(geo, mat);
    pts.frustumCulled = false;
    return { pts, geo, mat };
  }, [pool, palette]);
  useDotScale(mat, true);
  useDispose(geo, mat);
  useFrame(() => {
    const pos = geo.getAttribute('position') as THREE.BufferAttribute;
    const arr = pos.array as Float32Array;
    let n = 0;
    pool.forEach((p) => {
      const a = positions.map.get(p.path[p.seg]);
      const b = positions.map.get(p.path[p.seg + 1]);
      if (!a || !b) return;
      const k = ease(p.t);
      arr[n * 3] = a[0] + (b[0] - a[0]) * k;
      arr[n * 3 + 1] = a[1] + (b[1] - a[1]) * k;
      arr[n * 3 + 2] = a[2] + (b[2] - a[2]) * k;
      n++;
    });
    geo.setDrawRange(0, n);
    pos.needsUpdate = true;
  });
  return <primitive object={pts} />;
}

// ---- Labels: DOM buttons placed at the projected positions ----

const proj = new THREE.Vector3();

interface LabelState {
  transform: string;
  r: string;
  z: string;
  visible: boolean;
  flip: boolean;
  hidden: boolean;
  /** Measured tag size, keyed by its text (re-measured when the text changes). */
  text: string;
  w: number;
  h: number;
}

function LabelSync({
  labels,
  positions,
  model,
  selected,
}: {
  labels: Map<string, HTMLElement>;
  positions: Positions;
  model: SceneModel;
  selected: string | null;
}) {
  const radii = useMemo(() => new Map(model.nodes.map((n) => [n.key, n.radius * 1.45])), [model]);
  const states = useMemo(() => new WeakMap<HTMLElement, LabelState>(), []);
  useFrame((state) => {
    const cam = state.camera as THREE.PerspectiveCamera;
    const w = state.size.width;
    const h = state.size.height;
    const focal = h / 2 / Math.tan((cam.fov * Math.PI) / 360);
    const tags: TagInput[] = [];
    const visibleEls: Array<[string, HTMLElement, LabelState]> = [];
    for (const [key, el] of labels) {
      const p = positions.map.get(key);
      if (!p) continue;
      let ls = states.get(el);
      if (!ls) {
        ls = { transform: '', r: '', z: '', visible: true, flip: false, hidden: false, text: '', w: 0, h: 0 };
        states.set(el, ls);
      }
      proj.set(p[0], p[1], p[2]);
      const dist = proj.distanceTo(cam.position);
      proj.project(cam);
      const visible = proj.z <= 1 && proj.z >= -1;
      if (visible !== ls.visible) {
        el.style.visibility = visible ? '' : 'hidden';
        ls.visible = visible;
      }
      if (!visible) continue;
      const x = (proj.x * 0.5 + 0.5) * w;
      const y = (-proj.y * 0.5 + 0.5) * h;
      // Only touch the DOM when something moved (a still map writes nothing).
      const transform = `translate3d(${x.toFixed(1)}px, ${y.toFixed(1)}px, 0)`;
      if (transform !== ls.transform) {
        el.style.transform = transform;
        ls.transform = transform;
      }
      const radius = radii.get(key);
      const rpx = radius != null ? Math.max(7, (radius * focal) / dist) : 7;
      const r = rpx.toFixed(1);
      if (radius != null && r !== ls.r) {
        el.style.setProperty('--r', `${r}px`);
        ls.r = r;
      }
      // Nearer objects draw over farther ones.
      const z = String(1000 - Math.round(dist));
      if (z !== ls.z) {
        el.style.zIndex = z;
        ls.z = z;
      }
      // Tags that are on screen by default take part in the collision pass.
      const kind = el.dataset.kind;
      const shown = kind === 'node' || kind === 'channel' || kind === 'hub' || key === selected;
      if (!shown) continue;
      const tag = el.lastElementChild as HTMLElement | null;
      if (!tag || tag === el.firstElementChild) continue;
      const text = tag.textContent ?? '';
      if (text !== ls.text || ls.w === 0) {
        ls.text = text;
        ls.w = tag.offsetWidth;
        ls.h = tag.offsetHeight || 14;
      }
      const priority = key === selected ? 1000 : kind === 'hub' ? 500 : kind === 'node' ? 100 + rpx : 50;
      tags.push({ key, x, y, r: rpx, width: ls.w, height: ls.h, priority });
      visibleEls.push([key, el, ls]);
    }
    const placed = placeTags(tags, w);
    for (const [key, el, ls] of visibleEls) {
      const pl = placed.get(key);
      if (!pl) continue;
      if (pl.flip !== ls.flip) {
        if (pl.flip) el.dataset.flip = '';
        else delete el.dataset.flip;
        ls.flip = pl.flip;
      }
      if (pl.hidden !== ls.hidden) {
        if (pl.hidden) el.dataset.tagHidden = '';
        else delete el.dataset.tagHidden;
        ls.hidden = pl.hidden;
      }
    }
  });
  return null;
}

// ---- Camera and controls ----

export interface SceneControls {
  orbit(dAzimuth: number, dPolar: number): void;
  zoom(factor: number): void;
  pan(dx: number, dy: number): void;
  reset(): void;
  invalidate(): void;
}

function World({
  model,
  reduced,
  pool,
  positions,
  labels,
  ctl,
  selected,
}: {
  model: SceneModel;
  reduced: boolean;
  pool: ParticlePool;
  positions: Positions;
  labels: Map<string, HTMLElement>;
  ctl: MutableRefObject<SceneControls | null>;
  selected: string | null;
}) {
  const camera = useThree((st) => st.camera) as THREE.PerspectiveCamera;
  const gl = useThree((st) => st.gl);
  const invalidate = useThree((st) => st.invalidate);
  const palette = useMemo(readPalette, []);
  const clock = useRef(0);
  const fitted = useRef(0);

  // Created detached; the effect attaches and its cleanup detaches, so
  // StrictMode's effect replay reconnects the pointer listeners.
  const controls = useMemo(() => new OrbitControls(camera), [camera]);
  useEffect(() => {
    controls.connect(gl.domElement);
    return () => controls.disconnect();
  }, [controls, gl]);
  useEffect(() => {
    controls.enableDamping = !reduced;
    controls.dampingFactor = 0.08;
    controls.rotateSpeed = 0.5;
    controls.zoomSpeed = 0.7;
    controls.panSpeed = 0.6;
    controls.minDistance = 6;
    controls.maxDistance = 400;
    controls.maxPolarAngle = Math.PI * 0.92;
    const onChange = () => invalidate();
    controls.addEventListener('change', onChange);
    return () => controls.removeEventListener('change', onChange);
  }, [controls, reduced, invalidate]);

  // Frame the network: once at first, and again when it grows a lot.
  const size = useThree((st) => st.size);
  const aspect = size.height > 0 ? size.width / size.height : 1.6;
  const home = useMemo(() => {
    // Far enough that the disc fits the narrower of the two fields of view
    // (portrait phones are limited by the horizontal one).
    const half = Math.tan(((camera.fov ?? 40) * Math.PI) / 360);
    const d = aspect >= 1 ? Math.max(26, model.extent * 1.75) : Math.max(26, (model.extent * 0.95) / (half * aspect));
    // Portrait screens look down more steeply, so the disc uses their height.
    const el = ((aspect < 1 ? 64 : 28) * Math.PI) / 180;
    return new THREE.Vector3(0, d * Math.sin(el), d * Math.cos(el));
  }, [model.extent, aspect, camera]);
  useEffect(() => {
    if (fitted.current && model.extent < fitted.current * 1.4) return;
    fitted.current = model.extent;
    camera.position.copy(home);
    controls.target.set(0, 0, 0);
    camera.lookAt(0, 0, 0);
    controls.update();
    invalidate();
  }, [home, model.extent, camera, controls, invalidate]);

  useEffect(() => {
    positions.prune(model);
    positions.update(model, clock.current);
    invalidate();
  }, [model, positions, invalidate]);

  useEffect(() => {
    const sph = new THREE.Spherical();
    const off = new THREE.Vector3();
    const right = new THREE.Vector3();
    const up = new THREE.Vector3();
    ctl.current = {
      orbit(dAz, dPol) {
        off.copy(camera.position).sub(controls.target);
        sph.setFromVector3(off);
        sph.theta += dAz;
        sph.phi = Math.min(controls.maxPolarAngle, Math.max(0.12, sph.phi + dPol));
        off.setFromSpherical(sph);
        camera.position.copy(controls.target).add(off);
        camera.lookAt(controls.target);
        controls.update();
        invalidate();
      },
      zoom(f) {
        off.copy(camera.position).sub(controls.target);
        const d = Math.min(controls.maxDistance, Math.max(controls.minDistance, off.length() * f));
        off.setLength(d);
        camera.position.copy(controls.target).add(off);
        controls.update();
        invalidate();
      },
      pan(dx, dy) {
        const d = camera.position.distanceTo(controls.target) * 0.06;
        right.setFromMatrixColumn(camera.matrix, 0).multiplyScalar(dx * d);
        up.setFromMatrixColumn(camera.matrix, 1).multiplyScalar(dy * d);
        camera.position.add(right).add(up);
        controls.target.add(right).add(up);
        controls.update();
        invalidate();
      },
      reset() {
        camera.position.copy(home);
        controls.target.set(0, 0, 0);
        camera.lookAt(0, 0, 0);
        controls.update();
        invalidate();
      },
      invalidate,
    };
    return () => {
      ctl.current = null;
    };
  }, [camera, controls, home, invalidate, ctl]);

  // The first subscriber: advance time, move the bodies, step the particles.
  useFrame((_, dt) => {
    const d = Math.min(dt, 0.1);
    if (!reduced) {
      clock.current += d;
      pool.update(d);
    }
    positions.update(model, clock.current);
    controls.update();
  }, -1);

  return (
    <>
      <Starfield palette={palette} reduced={reduced} />
      <Hairlines model={model} palette={palette} />
      <Bodies model={model} palette={palette} />
      <LinkSet model={model} kind="hub" color={palette.text3} opacity={0.45} positions={positions} />
      <LinkSet model={model} kind="home" color={palette.text2} opacity={0.16} positions={positions} />
      <LinkSet model={model} kind="parent" color={palette.accent} opacity={0.22} positions={positions} />
      <LinkSet model={model} kind="channel" color={palette.text2} opacity={0.06} positions={positions} />
      <Satellites model={model} palette={palette} positions={positions} />
      <Moons model={model} palette={palette} positions={positions} />
      <Particles pool={pool} palette={palette} positions={positions} />
      <LabelSync labels={labels} positions={positions} model={model} selected={selected} />
    </>
  );
}

// ---- The component the route renders ----

interface LabelSpec {
  key: string;
  kind: 'hub' | 'node' | 'agent' | 'moon' | 'channel';
  text?: string;
  meta?: string;
  aria: string;
  state?: string;
  worker?: boolean;
}

function labelSpecs(m: SceneModel): LabelSpec[] {
  const out: LabelSpec[] = [{ key: HUB_KEY, kind: 'hub', text: 'hub', aria: 'Hub' }];
  const agentsOf = new Map<string, SceneModel['agents']>();
  for (const a of m.agents) agentsOf.set(a.orbit.center, [...(agentsOf.get(a.orbit.center) ?? []), a]);
  const moonsOf = new Map<string, SceneModel['moons']>();
  for (const j of m.moons) moonsOf.set(j.orbit.center, [...(moonsOf.get(j.orbit.center) ?? []), j]);
  const pushAgents = (center: string) => {
    for (const a of agentsOf.get(center) ?? [])
      out.push({
        key: a.key,
        kind: 'agent',
        text: a.name,
        state: a.state,
        worker: a.kind === 'worker',
        aria: `Agent ${a.name} on ${a.nodeName}, ${a.kind}, ${a.state}`,
      });
  };
  pushAgents(HUB_KEY);
  for (const n of m.nodes) {
    const meta = [n.cpu ? `${n.cpu}c` : '', n.memGiB ? `${Math.round(n.memGiB)}G` : '', n.gpus ? `${n.gpus}gpu` : '']
      .filter(Boolean)
      .join(' · ');
    out.push({
      key: n.key,
      kind: 'node',
      text: n.name,
      meta: n.online ? meta : 'offline',
      state: n.online ? 'online' : 'offline',
      aria: `Node ${n.name}, ${n.online ? 'online' : 'offline'}${meta ? `, ${meta}` : ''}`,
    });
    pushAgents(n.key);
    for (const j of moonsOf.get(n.key) ?? []) out.push({ key: j.key, kind: 'moon', aria: `Sandbox ${j.label} on ${j.nodeName}` });
  }
  for (const c of m.channels) out.push({ key: c.key, kind: 'channel', text: c.name, aria: `Channel ${c.name}` });
  return out;
}

export default function Scene({ model, reduced, hidden, selected, pool, onPick }: SceneProps) {
  const labels = useRef(new Map<string, HTMLElement>()).current;
  const positions = useRef(new Positions()).current;
  const ctl = useRef<SceneControls | null>(null);
  const specs = useMemo(() => labelSpecs(model), [model]);

  // New label elements need a frame to be placed, also in on-demand mode.
  useEffect(() => {
    ctl.current?.invalidate();
  }, [specs, selected]);

  const onKeyDown = (e: KeyboardEvent<HTMLDivElement>) => {
    const c = ctl.current;
    if (!c || e.metaKey || e.ctrlKey || e.altKey) return;
    const step = 0.08;
    let handled = true;
    switch (e.key) {
      case 'ArrowLeft':
        if (e.shiftKey) c.pan(-1, 0);
        else c.orbit(step, 0);
        break;
      case 'ArrowRight':
        if (e.shiftKey) c.pan(1, 0);
        else c.orbit(-step, 0);
        break;
      case 'ArrowUp':
        if (e.shiftKey) c.pan(0, 1);
        else c.orbit(0, -step);
        break;
      case 'ArrowDown':
        if (e.shiftKey) c.pan(0, -1);
        else c.orbit(0, step);
        break;
      case '+':
      case '=':
        c.zoom(0.88);
        break;
      case '-':
      case '_':
        c.zoom(1.14);
        break;
      case '0':
        c.reset();
        break;
      default:
        handled = false;
    }
    if (handled) e.preventDefault();
  };

  return (
    <div
      className={s.scene}
      onKeyDown={onKeyDown}
      tabIndex={0}
      role="group"
      aria-roledescription="network map"
      aria-label="Network map. Arrow keys orbit, Shift and arrows pan, plus and minus zoom, 0 resets. Tab moves through the objects."
    >
      <Canvas
        className={s.canvas}
        frameloop={hidden ? 'never' : reduced ? 'demand' : 'always'}
        dpr={[1, 2]}
        camera={{ fov: 40, near: 0.1, far: 3000, position: [0, 40, 64] }}
        gl={{ antialias: true, alpha: false, powerPreference: 'high-performance' }}
        onCreated={({ gl }) => gl.setClearColor(readPalette().bg, 1)}
      >
        <World model={model} reduced={reduced} pool={pool} positions={positions} labels={labels} ctl={ctl} selected={selected} />
      </Canvas>
      <div className={s.overlay}>
        {specs.map((l) => (
          <button
            key={l.key}
            type="button"
            ref={(el) => {
              if (el) labels.set(l.key, el);
              else labels.delete(l.key);
            }}
            className={s.obj}
            data-kind={l.kind}
            data-state={l.state}
            data-worker={l.worker || undefined}
            data-selected={selected === l.key || undefined}
            aria-label={l.aria}
            aria-pressed={selected === l.key}
            onClick={() => onPick(l.key, l.text ?? l.aria)}
          >
            <span className={s.hit} aria-hidden />
            {l.text && (
              <span className={s.tag} aria-hidden>
                <span className={s.tagName}>{l.text}</span>
                {l.meta && <span className={s.tagMeta}>{l.meta}</span>}
              </span>
            )}
          </button>
        ))}
      </div>
    </div>
  );
}
