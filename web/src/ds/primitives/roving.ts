import { useCallback, useEffect, useRef, useState, type KeyboardEvent } from 'react';

/**
 * useRoving implements one-tab-stop keyboard navigation over a vertical
 * collection (WAI-ARIA roving tabindex): Arrow keys, Home/End and
 * PageUp/PageDown move focus, Enter or Space activates.
 */
export function useRoving(count: number, onActivate?: (index: number) => void, initial = 0) {
  const [active, setActive] = useState(initial);
  const refs = useRef<Array<HTMLElement | null>>([]);

  useEffect(() => {
    if (active >= count) setActive(Math.max(0, count - 1));
  }, [count, active]);

  const move = useCallback(
    (i: number) => {
      if (count === 0) return;
      const next = Math.max(0, Math.min(count - 1, i));
      setActive(next);
      refs.current[next]?.focus();
    },
    [count],
  );

  const onKeyDown = useCallback(
    (e: KeyboardEvent<HTMLElement>, i: number) => {
      // Keys typed into controls inside a row belong to those controls.
      if (e.target !== e.currentTarget) return;
      switch (e.key) {
        case 'ArrowDown':
        case 'j':
          e.preventDefault();
          move(i + 1);
          break;
        case 'ArrowUp':
        case 'k':
          e.preventDefault();
          move(i - 1);
          break;
        case 'Home':
          e.preventDefault();
          move(0);
          break;
        case 'End':
          e.preventDefault();
          move(count - 1);
          break;
        case 'PageDown':
          e.preventDefault();
          move(i + 10);
          break;
        case 'PageUp':
          e.preventDefault();
          move(i - 10);
          break;
        case 'Enter':
        case ' ':
          if (onActivate) {
            e.preventDefault();
            onActivate(i);
          }
          break;
      }
    },
    [count, move, onActivate],
  );

  const itemProps = useCallback(
    (i: number) => ({
      ref: (el: HTMLElement | null) => {
        refs.current[i] = el;
      },
      tabIndex: i === Math.min(active, count - 1) ? 0 : -1,
      onKeyDown: (e: KeyboardEvent<HTMLElement>) => onKeyDown(e, i),
      onFocus: () => setActive(i),
    }),
    [active, count, onKeyDown],
  );

  return { active, setActive: move, itemProps };
}
