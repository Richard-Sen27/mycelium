// Label placement for the constellation's readouts. Pure, so it is tested
// without a renderer: each tag sits right of its body, flips to the left
// past FLIP_AT of the width, and a greedy pass in priority order hides a
// tag that would overlap one already placed (the hit target stays).

export const FLIP_AT = 0.7;
const GAP = 10; // px between the body's edge and the tag
const PAD = 2; // px of air required between tags

export interface TagInput {
  key: string;
  /** Screen position of the body's centre (px). */
  x: number;
  y: number;
  /** Projected body radius (px). */
  r: number;
  /** Measured tag size (px). */
  width: number;
  height: number;
  /** Higher wins a collision. */
  priority: number;
}

export interface TagPlacement {
  flip: boolean;
  hidden: boolean;
}

interface Box {
  x0: number;
  y0: number;
  x1: number;
  y1: number;
}

const overlaps = (a: Box, b: Box) => a.x0 < b.x1 + PAD && b.x0 < a.x1 + PAD && a.y0 < b.y1 + PAD && b.y0 < a.y1 + PAD;

/** placeTags decides, per tag, which side it goes and whether it is shown. */
export function placeTags(tags: TagInput[], viewWidth: number): Map<string, TagPlacement> {
  const out = new Map<string, TagPlacement>();
  const placed: Box[] = [];
  const order = [...tags].sort((a, b) => b.priority - a.priority || a.key.localeCompare(b.key));
  for (const t of order) {
    const flip = t.x > viewWidth * FLIP_AT;
    const x0 = flip ? t.x - t.r - GAP - t.width : t.x + t.r + GAP;
    const box = { x0, x1: x0 + t.width, y0: t.y - t.height / 2, y1: t.y + t.height / 2 };
    const hidden = placed.some((b) => overlaps(b, box));
    if (!hidden) placed.push(box);
    out.set(t.key, { flip, hidden });
  }
  return out;
}
