import { useEffect } from 'react';
import { create } from 'zustand';
import type { GetNodeSharingResponse, SharingSettings } from '../../gen/mycel/v1/grant_pb';
import { EventKind } from '../../gen/mycel/v1/ui_pb';
import { getApi } from '../../app/api';
import { onEvent } from '../../app/events/stores';
import { errText } from './shared';

// Sharing settings are not part of the Node row, so the nodes table and the
// inspector read them with GetNodeSharing, cached per node. A NODE event for
// a cached node (online/offline, caps, heartbeat changes) triggers a reload,
// at most every few seconds; a SetNodeSharing answer is written in directly.

const REFRESH_MIN_MS = 5000;

interface SharingEntry {
  value: GetNodeSharingResponse | null;
  error: string | null;
  at: number;
  loading: boolean;
}

interface SharingState {
  byNode: Record<string, SharingEntry>;
  load(name: string, force?: boolean): void;
  /** put stores a fresh answer (e.g. SetNodeSharingResponse.sharing). */
  put(name: string, value: GetNodeSharingResponse): void;
  reset(): void;
}

export const useSharing = create<SharingState>((set, get) => ({
  byNode: {},
  load(name, force) {
    const cur = get().byNode[name];
    if (cur?.loading) return;
    if (cur && !force && Date.now() - cur.at < REFRESH_MIN_MS) return;
    set((st) => ({ byNode: { ...st.byNode, [name]: { value: cur?.value ?? null, error: null, at: Date.now(), loading: true } } }));
    getApi()
      .rpc.control.getNodeSharing({ name })
      .then((value) => set((st) => ({ byNode: { ...st.byNode, [name]: { value, error: null, at: Date.now(), loading: false } } })))
      .catch((err: unknown) =>
        set((st) => ({
          byNode: { ...st.byNode, [name]: { value: cur?.value ?? null, error: errText(err), at: Date.now(), loading: false } },
        })),
      );
  },
  put(name, value) {
    set((st) => ({ byNode: { ...st.byNode, [name]: { value, error: null, at: Date.now(), loading: false } } }));
  },
  reset() {
    set({ byNode: {} });
  },
}));

let listening = false;
const timers = new Map<string, ReturnType<typeof setTimeout>>();
function listen() {
  if (listening) return;
  listening = true;
  onEvent((e) => {
    if (e.kind !== EventKind.NODE) return;
    const name = e.body.case === 'node' ? e.body.value.name : e.id;
    const cur = useSharing.getState().byNode[name];
    if (!cur || timers.has(name)) return;
    // Heartbeats also emit node events: reload at most every few seconds.
    const wait = Math.max(0, REFRESH_MIN_MS - (Date.now() - cur.at));
    timers.set(
      name,
      setTimeout(() => {
        timers.delete(name);
        useSharing.getState().load(name, true);
      }, wait),
    );
  });
}

/** useNodeSharing loads (once, cached) and returns a node's sharing view. */
export function useNodeSharing(name: string | undefined): SharingEntry | undefined {
  useEffect(() => {
    if (!name) return;
    listen();
    useSharing.getState().load(name);
  }, [name]);
  return useSharing((s) => (name ? s.byNode[name] : undefined));
}

/** sharingSummary: "docker, ollama · idle" (what the node offers, and when). */
export function sharingSummary(s: SharingSettings | undefined): string {
  if (!s) return '—';
  const offer = s.offer?.names ?? [];
  if (offer.length === 0) return 'nothing';
  return `${offer.join(', ')} · ${s.when || 'always'}`;
}
