// A fixed pool of message particles. Each particle walks a path of scene
// keys (poster → channel → mentioned agent), one segment at a time. The
// pool never grows: when it is full a new message is dropped, so a burst
// of traffic cannot cost more than `capacity` points per frame.

export const DEFAULT_CAPACITY = 256;
/** Seconds a particle takes per segment. */
export const SEGMENT_SECONDS = 1.6;

export interface Particle {
  alive: boolean;
  path: string[];
  /** Index of the current segment (path[seg] → path[seg + 1]). */
  seg: number;
  /** Progress along the current segment, 0..1. */
  t: number;
  /** Segments per second. */
  speed: number;
}

export class ParticlePool {
  readonly capacity: number;
  private items: Particle[];
  private free: number[];
  private count = 0;
  dropped = 0;

  constructor(capacity = DEFAULT_CAPACITY) {
    this.capacity = capacity;
    this.items = Array.from({ length: capacity }, () => ({ alive: false, path: [], seg: 0, t: 0, speed: 1 }));
    this.free = Array.from({ length: capacity }, (_, i) => capacity - 1 - i);
  }

  /** spawn starts a particle on `path`; false (and nothing spawned) when the pool is full or the path is too short. */
  spawn(path: string[], seconds = SEGMENT_SECONDS): boolean {
    if (path.length < 2) return false;
    const i = this.free.pop();
    if (i === undefined) {
      this.dropped++;
      return false;
    }
    const p = this.items[i];
    p.alive = true;
    p.path = path;
    p.seg = 0;
    p.t = 0;
    p.speed = 1 / Math.max(0.05, seconds);
    this.count++;
    return true;
  }

  /** update advances every live particle by dt seconds and frees finished ones. */
  update(dt: number): void {
    if (this.count === 0) return;
    for (let i = 0; i < this.capacity; i++) {
      const p = this.items[i];
      if (!p.alive) continue;
      p.t += dt * p.speed;
      while (p.t >= 1 && p.alive) {
        p.t -= 1;
        p.seg++;
        if (p.seg >= p.path.length - 1) this.release(i);
      }
    }
  }

  private release(i: number): void {
    const p = this.items[i];
    p.alive = false;
    p.path = [];
    this.free.push(i);
    this.count--;
  }

  clear(): void {
    for (let i = 0; i < this.capacity; i++) if (this.items[i].alive) this.release(i);
  }

  get live(): number {
    return this.count;
  }

  /** forEach visits the live particles (no allocation). */
  forEach(fn: (p: Particle) => void): void {
    if (this.count === 0) return;
    for (let i = 0; i < this.capacity; i++) if (this.items[i].alive) fn(this.items[i]);
  }
}

/** ease is the in-out curve a particle follows along a segment (it leaves and arrives gently). */
export function ease(t: number): number {
  return t < 0.5 ? 2 * t * t : 1 - Math.pow(-2 * t + 2, 2) / 2;
}
