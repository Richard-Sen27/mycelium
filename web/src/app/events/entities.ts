import { fromJson, toJson, type DescMessage, type JsonValue, type MessageShape } from '@bufbuild/protobuf';
import { Code, ConnectError } from '@connectrpc/connect';
import { useEffect, useMemo } from 'react';
import { create, type StoreApi, type UseBoundStore } from 'zustand';
import { loadSnapshot, saveSnapshot } from '../snapshot';

/**
 * An entity store keeps one kind of row (nodes, agents, channels, jobs,
 * grants) by key. It is hydrated from the List RPC on first use and then
 * kept current by the WatchEvents stream (see stream.ts / dispatch.ts).
 * Views subscribe with useEntityList / useEntity and never poll.
 */
export interface EntityState<T> {
  byId: Record<string, T>;
  loaded: boolean;
  loading: boolean;
  error: string | null;
  /** The rows come from the last snapshot because the hub is unreachable. */
  stale: boolean;
  snapshotAt: string | null;
}

export interface EntityStore<T> {
  kind: string;
  use: UseBoundStore<StoreApi<EntityState<T>>>;
  key: (row: T) => string;
  /** Hydrate once (no-op when loaded or loading). */
  ensure(): Promise<void>;
  /** Re-run the List RPC (after "too old", or when the hub is back). */
  reload(): Promise<void>;
  upsert(row: T): void;
  /** Remove by key, or by any id the store's `matches` accepts (event ids). */
  remove(id: string): void;
  reset(): void;
  list(): T[];
}

interface Options<D extends DescMessage> {
  kind: string;
  schema: D;
  key: (row: MessageShape<D>) => string;
  /** Extra match for delete events whose id is not the key (e.g. agent name). */
  matches?: (row: MessageShape<D>, id: string) => boolean;
  load: () => Promise<MessageShape<D>[]>;
}

let persist = false;
let scope = '';
const SAVE_DELAY = 1500;

/**
 * configureEntityStores turns snapshots on (node mode: the stale view needs
 * them). `scope` (the hub URL) keeps snapshots of different networks apart.
 */
export function configureEntityStores(o: { persist: boolean; scope?: string }): void {
  persist = o.persist;
  scope = o.scope ?? '';
}

/** snapshotKey is the IndexedDB key of one kind's snapshot. */
export function snapshotKey(kind: string): string {
  return scope ? `entities:${scope}:${kind}` : `entities:${kind}`;
}

/** isUnreachable reports errors that mean "hub not reachable" (the node proxy's 502). */
export function isUnreachable(err: unknown): boolean {
  const ce = ConnectError.from(err);
  return ce.code === Code.Unavailable || /\b502\b|hub unreachable/i.test(ce.rawMessage);
}

const registry: EntityStore<unknown>[] = [];

/** allEntityStores lists every store created (for "too old" reloads and sign-out resets). */
export function allEntityStores(): EntityStore<unknown>[] {
  return registry;
}

const initial = <T>(): EntityState<T> => ({ byId: {}, loaded: false, loading: false, error: null, stale: false, snapshotAt: null });

export function createEntityStore<D extends DescMessage>(o: Options<D>): EntityStore<MessageShape<D>> {
  type T = MessageShape<D>;
  const use = create<EntityState<T>>(() => initial<T>());
  // Events that arrive while a List call is in flight are replayed on top of its result.
  let pending: Array<{ op: 'upsert'; row: T } | { op: 'remove'; id: string }> | null = null;
  let inflight: Promise<void> | null = null;
  let saveTimer: ReturnType<typeof setTimeout> | null = null;
  let generation = 0;

  const scheduleSave = () => {
    if (!persist) return;
    if (saveTimer) clearTimeout(saveTimer);
    saveTimer = setTimeout(() => {
      saveTimer = null;
      const st = use.getState();
      if (st.stale || !st.loaded) return;
      const rows = Object.values(st.byId).map((r) => toJson(o.schema, r));
      void saveSnapshot(snapshotKey(o.kind), rows).catch(() => undefined);
    }, SAVE_DELAY);
  };

  const applyRemove = (byId: Record<string, T>, id: string): Record<string, T> => {
    if (id in byId) {
      const next = { ...byId };
      delete next[id];
      return next;
    }
    if (o.matches) {
      const hit = Object.entries(byId).find(([, r]) => o.matches!(r, id));
      if (hit) {
        const next = { ...byId };
        delete next[hit[0]];
        return next;
      }
    }
    return byId;
  };

  const load = async (): Promise<void> => {
    const gen = ++generation;
    pending = [];
    use.setState({ loading: true, error: null });
    try {
      const rows = await o.load();
      if (gen !== generation) return;
      let byId: Record<string, T> = {};
      for (const r of rows) byId[o.key(r)] = r;
      for (const p of pending ?? []) {
        byId = p.op === 'upsert' ? { ...byId, [o.key(p.row)]: p.row } : applyRemove(byId, p.id);
      }
      pending = null;
      use.setState({ byId, loaded: true, loading: false, error: null, stale: false, snapshotAt: null });
      scheduleSave();
    } catch (err) {
      if (gen !== generation) return;
      pending = null;
      const msg = ConnectError.from(err).rawMessage;
      if (persist && isUnreachable(err)) {
        const snap = await loadSnapshot<JsonValue[]>(snapshotKey(o.kind));
        if (snap) {
          const byId: Record<string, T> = {};
          for (const j of snap.value) {
            try {
              const r = fromJson(o.schema, j, { ignoreUnknownFields: true });
              byId[o.key(r)] = r;
            } catch {
              /* skip a row the schema no longer reads */
            }
          }
          use.setState({ byId, loaded: true, loading: false, error: null, stale: true, snapshotAt: snap.at });
          return;
        }
      }
      use.setState({ loading: false, error: msg });
    }
  };

  const store: EntityStore<T> = {
    kind: o.kind,
    use,
    key: o.key,
    ensure() {
      const st = use.getState();
      if (st.loaded || inflight) return inflight ?? Promise.resolve();
      inflight = load().finally(() => {
        inflight = null;
      });
      return inflight;
    },
    reload() {
      inflight = load().finally(() => {
        inflight = null;
      });
      return inflight;
    },
    upsert(row) {
      if (pending) pending.push({ op: 'upsert', row });
      use.setState((st) => ({ byId: { ...st.byId, [o.key(row)]: row } }));
      scheduleSave();
    },
    remove(id) {
      if (pending) pending.push({ op: 'remove', id });
      use.setState((st) => ({ byId: applyRemove(st.byId, id) }));
      scheduleSave();
    },
    reset() {
      generation++;
      pending = null;
      inflight = null;
      if (saveTimer) clearTimeout(saveTimer);
      use.setState(initial<T>(), true);
    },
    list() {
      return Object.values(use.getState().byId);
    },
  };
  registry.push(store as EntityStore<unknown>);
  return store;
}

export interface EntityList<T> {
  items: T[];
  loaded: boolean;
  loading: boolean;
  error: string | null;
  stale: boolean;
  snapshotAt: string | null;
}

/** useEntityList subscribes to a store and hydrates it on first use. */
export function useEntityList<T>(store: EntityStore<T>, compare?: (a: T, b: T) => number): EntityList<T> {
  useEffect(() => {
    void store.ensure();
  }, [store]);
  const byId = store.use((s) => s.byId);
  const loaded = store.use((s) => s.loaded);
  const loading = store.use((s) => s.loading);
  const error = store.use((s) => s.error);
  const stale = store.use((s) => s.stale);
  const snapshotAt = store.use((s) => s.snapshotAt);
  const items = useMemo(() => {
    const arr = Object.values(byId);
    return compare ? arr.sort(compare) : arr;
  }, [byId, compare]);
  return { items, loaded, loading, error, stale, snapshotAt };
}

/** useEntity returns one row by key (hydrating the store on first use). */
export function useEntity<T>(store: EntityStore<T>, id: string | null | undefined): T | undefined {
  useEffect(() => {
    void store.ensure();
  }, [store]);
  return store.use((s) => (id ? s.byId[id] : undefined));
}
