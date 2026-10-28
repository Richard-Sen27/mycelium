// Size budget for the initial load (spec phase 6: under 1 MB without the 3D
// chunk). Counts index.html plus every JS and CSS file the browser fetches
// before the app can render: the entry script, its static imports
// (transitively), modulepreloads and stylesheets linked from index.html,
// and CSS files a JS chunk imports eagerly. Uncompressed bytes. Dynamic
// imports (lazy routes, the 3D scene) are not counted, and the scene chunk
// must not be reachable eagerly at all.
import { existsSync, readFileSync, statSync } from 'node:fs';
import { dirname, join, posix, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const BUDGET = 1024 * 1024;
const here = dirname(fileURLToPath(import.meta.url));
const dist = resolve(process.env.MYCEL_WEB_DIST ?? join(here, '../../internal/web/dist'));
const indexPath = join(dist, 'index.html');

if (!existsSync(indexPath)) {
  console.error(`size-check: ${indexPath} not found; run \`pnpm build\` first`);
  process.exit(2);
}

const html = readFileSync(indexPath, 'utf8');
const eager = new Set();
const queue = [];

const add = (urlPath) => {
  if (!urlPath || /^(https?:)?\/\//.test(urlPath) || urlPath.startsWith('data:')) return;
  const p = posix.normalize(urlPath.replace(/^\//, ''));
  if (eager.has(p)) return;
  eager.add(p);
  if (p.endsWith('.js')) queue.push(p);
};

// Inline scripts are forbidden by the CSP; fail loudly if one appears.
const inline = [...html.matchAll(/<script\b(?![^>]*\bsrc=)[^>]*>([\s\S]*?)<\/script>/gi)].filter((m) => m[1].trim() !== '');
if (inline.length) {
  console.error('size-check: index.html contains an inline <script>; the CSP forbids it');
  process.exit(1);
}
if (/<style\b/i.test(html)) {
  console.error('size-check: index.html contains an inline <style>; the CSP forbids it');
  process.exit(1);
}

for (const m of html.matchAll(/<script\b[^>]*\bsrc="([^"]+)"/gi)) add(m[1]);
for (const m of html.matchAll(/<link\b[^>]*>/gi)) {
  const tag = m[0];
  const rel = /\brel="([^"]+)"/i.exec(tag)?.[1] ?? '';
  const href = /\bhref="([^"]+)"/i.exec(tag)?.[1];
  if (/\b(stylesheet|modulepreload)\b/.test(rel)) add(href);
}

// Follow static imports: `import ... from "./x.js"`, `import "./x.js"`,
// `export ... from "./x.js"`. Dynamic `import("./x.js")` has a parenthesis
// and is deliberately not matched.
const staticImport = /(?:^|[;\s}])(?:import|export)\s*(?:[\w$*{}\s,]+?\s*from\s*)?["']([^"']+\.(?:js|css))["']/g;
while (queue.length) {
  const file = queue.shift();
  const src = readFileSync(join(dist, file), 'utf8');
  for (const m of src.matchAll(staticImport)) {
    const spec = m[1];
    add(spec.startsWith('/') ? spec : posix.join(posix.dirname(file), spec));
  }
}

let total = statSync(indexPath).size;
const rows = [['index.html', total]];
for (const f of eager) {
  const full = join(dist, f);
  if (!existsSync(full)) {
    console.error(`size-check: index.html references missing ${f}`);
    process.exit(1);
  }
  const size = statSync(full).size;
  total += size;
  rows.push([f, size]);
}

const scene = [...eager].filter((f) => /(^|\/)scene[-.]/.test(f));
const kb = (n) => `${(n / 1024).toFixed(1)} KiB`;
for (const [f, n] of rows.sort((a, b) => b[1] - a[1])) console.log(`${kb(n).padStart(11)}  ${f}`);
console.log(`${kb(total).padStart(11)}  total initial load (budget ${kb(BUDGET)})`);

if (scene.length) {
  console.error(`size-check: the 3D scene chunk is loaded eagerly: ${scene.join(', ')}`);
  process.exit(1);
}
if (total > BUDGET) {
  console.error(`size-check: initial load ${kb(total)} exceeds the ${kb(BUDGET)} budget`);
  process.exit(1);
}
