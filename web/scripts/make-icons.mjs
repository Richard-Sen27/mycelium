// Renders the app icons (public/icons/*.png) from the product mark: a body
// (ring and core) with one satellite. Pure Node (zlib), no image libraries.
// Run `pnpm icons` after changing the mark; the PNGs are committed.
import { mkdirSync, writeFileSync } from 'node:fs';
import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { deflateSync } from 'node:zlib';

const here = dirname(fileURLToPath(import.meta.url));
const out = resolve(here, '../public/icons');
mkdirSync(out, { recursive: true });

const BG = [0x07, 0x09, 0x0b];
const LINE = [0xa7, 0xb2, 0xbc];
const ACCENT = [0x8e, 0xd1, 0xe6];

const crcTable = new Uint32Array(256).map((_, n) => {
  let c = n;
  for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1;
  return c >>> 0;
});
function crc32(buf) {
  let c = 0xffffffff;
  for (const b of buf) c = crcTable[(c ^ b) & 0xff] ^ (c >>> 8);
  return (c ^ 0xffffffff) >>> 0;
}
function chunk(type, data) {
  const len = Buffer.alloc(4);
  len.writeUInt32BE(data.length);
  const td = Buffer.concat([Buffer.from(type, 'ascii'), data]);
  const crc = Buffer.alloc(4);
  crc.writeUInt32BE(crc32(td));
  return Buffer.concat([len, td, crc]);
}
function png(size, rgba) {
  const raw = Buffer.alloc((size * 4 + 1) * size);
  for (let y = 0; y < size; y++) {
    raw[y * (size * 4 + 1)] = 0;
    rgba.copy(raw, y * (size * 4 + 1) + 1, y * size * 4, (y + 1) * size * 4);
  }
  const ihdr = Buffer.alloc(13);
  ihdr.writeUInt32BE(size, 0);
  ihdr.writeUInt32BE(size, 4);
  ihdr[8] = 8;
  ihdr[9] = 6;
  return Buffer.concat([
    Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]),
    chunk('IHDR', ihdr),
    chunk('IDAT', deflateSync(raw, { level: 9 })),
    chunk('IEND', Buffer.alloc(0)),
  ]);
}

// Coverage of a pixel by a shape given its signed distance (anti-aliased).
const cover = (d, px) => Math.max(0, Math.min(1, 0.5 - d / px));

/**
 * draw renders the mark at `size` px. `scale` shrinks it inside the canvas
 * (maskable icons keep a safe zone); `mono` draws white on transparent
 * (notification badge).
 */
function draw(size, { scale = 1, mono = false, background = true } = {}) {
  const buf = Buffer.alloc(size * size * 4);
  const c = size / 2;
  const u = (size / 40) * scale; // the mark is drawn on a 40-unit grid (see SignIn.tsx)
  const ringR = 13 * u;
  const stroke = Math.max(1, 1.1 * u);
  const core = 3 * u;
  const sat = { x: c + 11.2 * u, y: c - 6.4 * u, r: 2.4 * u };
  for (let y = 0; y < size; y++) {
    for (let x = 0; x < size; x++) {
      const px = x + 0.5;
      const py = y + 0.5;
      const dc = Math.hypot(px - c, py - c);
      const ring = cover(Math.abs(dc - ringR) - stroke / 2, 1);
      const coreA = cover(dc - core, 1);
      const satA = cover(Math.hypot(px - sat.x, py - sat.y) - sat.r, 1);
      // A gap in the ring around the satellite, so it reads as orbiting.
      const gap = cover(Math.hypot(px - sat.x, py - sat.y) - sat.r * 1.9, 1);
      const lineA = Math.max(ring * (1 - gap), coreA);
      let r = background ? BG[0] : 0;
      let g = background ? BG[1] : 0;
      let b = background ? BG[2] : 0;
      let a = background ? 1 : 0;
      const over = (col, alpha) => {
        r = r * (1 - alpha) + col[0] * alpha;
        g = g * (1 - alpha) + col[1] * alpha;
        b = b * (1 - alpha) + col[2] * alpha;
        a = a + alpha * (1 - a);
      };
      if (mono) {
        const alpha = Math.max(lineA, satA);
        r = g = b = 255;
        a = alpha;
      } else {
        over(LINE, lineA);
        over(ACCENT, satA);
      }
      const i = (y * size + x) * 4;
      buf[i] = Math.round(r);
      buf[i + 1] = Math.round(g);
      buf[i + 2] = Math.round(b);
      buf[i + 3] = Math.round(a * 255);
    }
  }
  return png(size, buf);
}

writeFileSync(resolve(out, 'icon-192.png'), draw(192, { scale: 1.15 }));
writeFileSync(resolve(out, 'icon-512.png'), draw(512, { scale: 1.15 }));
writeFileSync(resolve(out, 'maskable-512.png'), draw(512, { scale: 0.85 }));
writeFileSync(resolve(out, 'badge-72.png'), draw(72, { scale: 1.2, mono: true, background: false }));
console.log('icons written to', out);
