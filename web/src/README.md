# web/src: contracts for the views packages

P6-web-core delivers the toolchain, the generated client, the design system,
the shell, sign-in, the command bar and the PWA shell. The views packages
(P6-views-a, P6-views-b) fill the routes. This file is the contract.

## Layout

| Path                   | What                                                                                                                              |
| ---------------------- | --------------------------------------------------------------------------------------------------------------------------------- |
| `gen/`                 | Generated TypeScript (`make gen-web`, protoc-gen-es v2). Never edit.                                                              |
| `ds/`                  | Design system: tokens (`ds/themes/hud.css`), base styles, HUD primitives. Import from `ds` only. No API or store imports in here. |
| `app/`                 | Shell and core: mode, sign-in, API clients, event stream and stores, command bar, settings, PWA.                                  |
| `views/<id>/index.tsx` | One route module per rail entry. **Replace these files**; nothing else needs to change to ship a view.                            |
| `sw.ts`                | Service worker (precache, push, notification click).                                                                              |

## Routes

Each route module exports a component named `Component` (React Router
`lazy`), and is loaded on first visit, so it does not count toward the 1 MB
initial load. Paths are `/<id>/*`, so a view may use nested relative routes
(`<Routes>` inside the module, or `useParams()['*']`).

| id            | path                                                  | package                                          |
| ------------- | ----------------------------------------------------- | ------------------------------------------------ |
| constellation | `/constellation`                                      | P6-views-a (three.js / r3f)                      |
| channels      | `/channels`                                           | P6-views-b                                       |
| approvals     | `/approvals` (`?grant=<id>` from a push notification) | P6-views-b                                       |
| jobs          | `/jobs`                                               | P6-views-b (sandbox inspector)                   |
| grants        | `/grants`                                             | P6-views-a                                       |
| nodes         | `/nodes`                                              | P6-views-a                                       |
| agents        | `/agents`                                             | P6-views-a                                       |
| traces        | `/traces`                                             | P6-views-b (traces and cost, audit replay)       |
| machine       | `/machine` (node mode only; guarded)                  | P6-views-b ("This machine", restrict only)       |
| settings      | `/settings`                                           | done here                                        |

The constellation must import `three` / `@react-three/*` only from its own
module tree. `vite.config.ts` puts those packages in the `scene` chunk;
`pnpm run size-check` fails if that chunk becomes reachable eagerly or the
initial load exceeds 1 MiB. A search box or list for every object kind must
work without the 3D view.

## Talking to the server

```ts
import { getApi } from '../../app/api';
const api = getApi();
api.mode; // 'hub' | 'node'
api.rpc.grants.approveGrant({ id }); // every Connect service: tokens, nodes, users, agents,
// channels, grants, control, jobs, artifacts, events
api.local?.grants(); // node mode: /api/local/* ("This machine", restrict only)
api.hub?.me(); // hub mode: /api/auth/*, /api/push/*
await api.download('/api/artifacts/art_01…'); // Blob; hub: session cookie, node: bearer via /api/hub
await api.upload('/api/artifacts?name=x.csv', file); // hub only: cookie + CSRF; returns the JSON reply
```

`download` and `upload` are for the hub's plain HTTP routes that are not
Connect calls (artifact bytes); they set the mode's auth headers, so views
never build those headers themselves. A plain `<a href="/api/artifacts/…">`
works in hub mode only (the node proxy needs the bearer a link cannot carry).

Headers, cookies, CSRF (`Mycel-Csrf`) and the local bearer are handled by
the transports; views never set them. In node mode `api.rpc` goes through the
daemon's read-only proxy: writes fail with PermissionDenied, so hide them
(see Rights). Errors are `ConnectError`; show `ConnectError.from(err).rawMessage`.

## Rights

```ts
import { useRights } from '../../app/session';
const { mode, isAdmin, readOnly, userId, userName } = useRights();
```

`readOnly` is true in node mode for everything except `/machine`. Hide (do not
just disable) actions the caller cannot perform; the server enforces anyway.
For grant decisions use `canDecide(rights, grant)` from `app/commands/core`
(admins, or the owner of the grant's node).

## Live data: stores, never polling

One `EventService.WatchEvents` stream runs for the whole app (resumed with
`after_seq`; on "too old" every loaded list reloads). It fans into stores in
`app/events/stores.ts`:

| Store           | Key                            | Hydrated from                                                  |
| --------------- | ------------------------------ | -------------------------------------------------------------- |
| `nodesStore`    | `node.name`                    | `ListNodes`                                                    |
| `agentsStore`   | `agent.id`                     | `ListAgents`                                                   |
| `channelsStore` | `channel.name`                 | `ListChannels{include_archived}`                               |
| `jobsStore`     | `job.id`                       | `ListJobs{include_finished, limit 500}`                        |
| `grantsStore`   | `grant.id`                     | `ListGrants{include_finished}`                                 |
| `useFreeze`     | –                              | `FreezeStatus`                                                 |
| `useMessages`   | per channel, by seq (last 500) | live only; load history with `ListMessages` and `setChannel()` |
| `useAudit`      | newest first (last 500)        | live only; history via `ListAudit`                             |

```ts
import { useEntityList, useEntity } from '../../app/events/entities';
import { grantsStore, onEvent, useMessages } from '../../app/events/stores';

const { items, loaded, loading, error, stale, snapshotAt } = useEntityList(grantsStore, byCreatedDesc);
const node = useEntity(nodesStore, name);
useEffect(
  () =>
    onEvent((e) => {
      /* animate a message along an edge */
    }),
  [],
);
```

Hydration happens on first use. `compare` must be stable (module-level
function or `useCallback`). After a write RPC there is no need to patch the
store: the event arrives on the stream. In node mode, rows come from an
IndexedDB snapshot when the hub is unreachable: `stale` is true; mark the view
stale (the shell already shows a banner). Do not open further streams for
these kinds; `ChannelService.Watch` is not needed. `JobService.StreamLogs` is
per job and fine to open from the inspector.

## Inspector

```ts
import { inspect, useInspector } from '../../app/shell/inspector';
inspect({ key: `node:${n.name}`, title: n.name, content: <NodeDetail name={n.name} /> });
const selected = useInspector((s) => (s.open ? s.key : null)); // for Table selectedKey
```

Right drawer on a desktop, a sheet on narrow screens and phones; Escape closes.
Pass content that reads from stores by key so it stays live.

## Commands (⌘K)

```ts
import { registerCommands } from '../../app/commands/registry';
useEffect(() => registerCommands((ctx) => [
  { id: 'job.cancel.' + id, title: `Cancel ${id}`, group: 'Jobs', keywords: [...],
    allowed: (c) => !c.rights.readOnly,
    confirm: { title: 'Cancel this job?', action: 'Cancel job', tone: 'alert' },
    run: async (c) => { await getApi().rpc.jobs.cancelJob({ id }); } },
]), [id]);
```

Commands that must be in the palette before their view is first visited
(every CLI-equivalent action: D025 wants the palette to cover all of them)
live in a light `views/<id>/commands.ts` that exports a `register…()`
function; `app/commands/eager.ts` imports and calls it at startup (main.tsx).
Such a file may import stores, the API, `ds` helpers and `registerCommands`
only, never view components or heavy libraries, because it lands in the
initial load. View-local commands register in an effect. Ids must be unique. Destructive and network-wide actions
take a `confirm`. Already registered here: navigation (`g` + digit too),
sign-out, theme, motion, freeze/unfreeze, approve/deny per pending grant,
and free text to the caller's orchestrator.

## Design system

`Frame` (panel with title readout; `flush` for tables), `Readout` (ticking
number), `Gauge` (role meter), `Table` / `List` (roving keyboard focus,
`onActivate`, `selectedKey`, `rowTone`), `Button` (`primary` | `warn` |
`ghost`, `busy`), `Input`, `Select`, `Dialog`, `Sheet`, `toast()`, `Badge`
(`ok` | `busy` | `idle` | `off` | `alert`, `pulse`), `Tabs`, `Kbd`,
`EmptyState`, `Skeleton`. Rules:

- Colours, sizes, durations only through tokens (`var(--accent)`, `--s-4`,
  `--t-move`). One accent, one warning. No gradients, glass, shadows-as-glow
  or emoji. Radii stay at `--r-1`/`--r-2`.
- Monospace (`--font-data`, class `mono`) for ids, numbers, times.
- Motion must mean something (arrival, state change). Use the duration
  tokens; they become 0 under reduced motion. JS animation: check
  `prefersReducedMotion()` from `ds`.
- No inline `<style>` or `style` attributes in HTML strings (CSP). React's
  `style={{…}}` prop is fine (it goes through the CSSOM). For Base UI parts
  that inject a `<style>`, the app runs with `CSPProvider disableStyleElements`;
  supply the CSS yourself.
- Views style with CSS Modules next to the component.

## Tests

`pnpm test` (vitest, jsdom). The test setup provides IndexedDB
(fake-indexeddb), a `matchMedia` stub (desktop, reduced motion) and clears
storage between tests. See `app/shell/Shell.test.tsx` for rendering the app
with a fake `fetch`.
