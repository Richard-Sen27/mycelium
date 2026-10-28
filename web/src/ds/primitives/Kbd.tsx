import { cx } from '../cx';
import s from './Kbd.module.css';

/** Kbd renders a key or chord, e.g. <Kbd keys={['mod', 'K']} />. "mod" is ⌘ on Apple platforms, Ctrl elsewhere. */
export function Kbd({ keys, className }: { keys: string[]; className?: string }) {
  return (
    <span className={cx(s.chord, className)}>
      {keys.map((k, i) => (
        <kbd key={i} className={s.key}>
          {k === 'mod' ? modKey() : k}
        </kbd>
      ))}
    </span>
  );
}

export function isApple(): boolean {
  return typeof navigator !== 'undefined' && /Mac|iPhone|iPad|iPod/.test(navigator.platform || navigator.userAgent);
}

export function modKey(): string {
  return isApple() ? '⌘' : 'Ctrl';
}
