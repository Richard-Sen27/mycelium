import type { KeyboardEvent } from 'react';

const NEXT = ['ArrowRight', 'ArrowDown'];
const PREV = ['ArrowLeft', 'ArrowUp'];

/**
 * radioGroupKeyDown gives a role="radiogroup" of buttons the arrow-key
 * behaviour of native radios: arrows move to the next enabled option,
 * Home/End to the ends, and moving selects. Put it on the group element and
 * give the checked option tabIndex 0, the others -1.
 */
export function radioGroupKeyDown(e: KeyboardEvent<HTMLElement>): void {
  const k = e.key;
  if (!NEXT.includes(k) && !PREV.includes(k) && k !== 'Home' && k !== 'End') return;
  const radios = [...e.currentTarget.querySelectorAll<HTMLElement>('[role="radio"]:not(:disabled)')];
  if (radios.length === 0) return;
  const i = radios.indexOf(document.activeElement as HTMLElement);
  let j: number;
  if (k === 'Home') j = 0;
  else if (k === 'End') j = radios.length - 1;
  else if (NEXT.includes(k)) j = i < 0 ? 0 : (i + 1) % radios.length;
  else j = i < 0 ? radios.length - 1 : (i - 1 + radios.length) % radios.length;
  e.preventDefault();
  radios[j].focus();
  radios[j].click();
}
