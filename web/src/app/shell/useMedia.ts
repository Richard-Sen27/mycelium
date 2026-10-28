import { useSyncExternalStore } from 'react';

/** useMedia tracks a CSS media query. */
export function useMedia(query: string): boolean {
  return useSyncExternalStore(
    (cb) => {
      if (typeof matchMedia !== 'function') return () => undefined;
      const m = matchMedia(query);
      m.addEventListener('change', cb);
      return () => m.removeEventListener('change', cb);
    },
    () => typeof matchMedia === 'function' && matchMedia(query).matches,
    () => false,
  );
}
