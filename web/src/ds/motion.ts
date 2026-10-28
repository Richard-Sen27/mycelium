import { useEffect, useRef, useState } from 'react';

/**
 * prefersReducedMotion reads the in-app switch (html[data-motion]) first and
 * falls back to the OS setting. Primitives that animate in JavaScript call
 * it; CSS animations read the duration tokens, which base.css zeroes.
 */
export function prefersReducedMotion(): boolean {
  if (typeof document === 'undefined') return true;
  const m = document.documentElement.dataset.motion;
  if (m === 'reduced') return true;
  if (m === 'full') return false;
  return typeof matchMedia === 'function' && matchMedia('(prefers-reduced-motion: reduce)').matches;
}

/** setMotion applies the in-app motion preference to the document. */
export function setMotion(pref: 'system' | 'reduced' | 'full'): void {
  const el = document.documentElement;
  if (pref === 'system') delete el.dataset.motion;
  else el.dataset.motion = pref;
}

const TICK_MS = 600;

/**
 * useTicker returns a number that runs from its previous value to `value`
 * over TICK_MS with an ease-out curve, so readouts tick rather than jump.
 * Under reduced motion (and for non-finite values) it returns `value` at once.
 */
export function useTicker(value: number, duration = TICK_MS): number {
  const [shown, setShown] = useState(value);
  const from = useRef(value);
  const shownRef = useRef(value);
  shownRef.current = shown;

  useEffect(() => {
    if (!Number.isFinite(value) || prefersReducedMotion() || typeof requestAnimationFrame !== 'function') {
      from.current = value;
      setShown(value);
      return;
    }
    const start = shownRef.current;
    if (start === value) return;
    from.current = start;
    let raf = 0;
    let t0 = -1;
    const step = (t: number) => {
      if (t0 < 0) t0 = t;
      const p = Math.min(1, (t - t0) / duration);
      const eased = 1 - Math.pow(1 - p, 3);
      setShown(start + (value - start) * eased);
      if (p < 1) raf = requestAnimationFrame(step);
    };
    raf = requestAnimationFrame(step);
    return () => cancelAnimationFrame(raf);
  }, [value, duration]);

  return shown;
}
