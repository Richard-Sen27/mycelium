import type { ReactNode } from 'react';
import { create } from 'zustand';

/**
 * The inspector is the right-hand drawer any view can fill: a node, an
 * agent, a grant, a sandbox. On a phone it opens as a bottom sheet.
 * `key` identifies what is shown (e.g. "node:gpu-box") so a view can mark
 * the selected row and toggle on a second click.
 */
export interface InspectorState {
  open: boolean;
  key: string | null;
  title: ReactNode;
  content: ReactNode;
  show(o: { key: string; title: ReactNode; content: ReactNode }): void;
  /** show, or close when the same key is already open. */
  toggle(o: { key: string; title: ReactNode; content: ReactNode }): void;
  close(): void;
}

export const useInspector = create<InspectorState>((set, get) => ({
  open: false,
  key: null,
  title: null,
  content: null,
  show: ({ key, title, content }) => set({ open: true, key, title, content }),
  toggle: (o) => (get().open && get().key === o.key ? get().close() : get().show(o)),
  close: () => set({ open: false, key: null, title: null, content: null }),
}));

/** inspect is the non-hook form of useInspector().show. */
export function inspect(o: { key: string; title: ReactNode; content: ReactNode }): void {
  useInspector.getState().show(o);
}
