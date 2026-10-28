import { useEffect, type ReactNode } from 'react';
import { create } from 'zustand';
import s from './Toast.module.css';

export interface ToastInput {
  title: ReactNode;
  body?: ReactNode;
  /** "alert" for failures and freezes; default is informational. */
  tone?: 'default' | 'alert';
  /** Milliseconds before it leaves on its own; 0 keeps it until dismissed. Default 5000, alerts 0. */
  timeout?: number;
}

interface ToastItem extends ToastInput {
  id: number;
}

interface ToastState {
  items: ToastItem[];
  push: (t: ToastInput) => number;
  dismiss: (id: number) => void;
}

let nextId = 1;

export const useToasts = create<ToastState>((set) => ({
  items: [],
  push: (t) => {
    const id = nextId++;
    set((st) => ({ items: [...st.items.slice(-4), { ...t, id }] }));
    return id;
  },
  dismiss: (id) => set((st) => ({ items: st.items.filter((i) => i.id !== id) })),
}));

/** toast shows a transient notice in the viewport; returns its id. */
export function toast(t: ToastInput): number {
  return useToasts.getState().push(t);
}

/** ToastViewport renders the live region; mount it once in the shell. */
export function ToastViewport() {
  const items = useToasts((st) => st.items);
  return (
    <div className={s.viewport} role="status" aria-live="polite" aria-relevant="additions">
      <ol className={s.list}>
        {items.map((t) => (
          <ToastRow key={t.id} item={t} />
        ))}
      </ol>
    </div>
  );
}

function ToastRow({ item }: { item: ToastItem }) {
  const dismiss = useToasts((st) => st.dismiss);
  const timeout = item.timeout ?? (item.tone === 'alert' ? 0 : 5000);
  useEffect(() => {
    if (!timeout) return;
    const h = setTimeout(() => dismiss(item.id), timeout);
    return () => clearTimeout(h);
  }, [item.id, timeout, dismiss]);
  return (
    <li className={s.toast} data-tone={item.tone ?? 'default'}>
      <div className={s.text}>
        <div className={s.title}>{item.title}</div>
        {item.body != null && <div className={s.body}>{item.body}</div>}
      </div>
      <button type="button" className={s.close} onClick={() => dismiss(item.id)} aria-label="Dismiss">
        <span aria-hidden>×</span>
      </button>
      {timeout > 0 && <span className={s.timer} aria-hidden style={{ animationDuration: `${timeout}ms` }} />}
    </li>
  );
}
