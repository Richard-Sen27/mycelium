import { create } from 'zustand';
import { setMotion } from '../ds/motion';

// Display preferences (not secrets) persisted in localStorage.

export type MotionPref = 'system' | 'reduced' | 'full';

/** Shipped themes. A theme is a token file under ds/themes selected by html[data-theme]. */
export const THEMES = [{ id: 'hud', label: 'HUD' }] as const;
export type ThemeId = (typeof THEMES)[number]['id'];

interface PrefsState {
  motion: MotionPref;
  theme: ThemeId;
  setMotion(m: MotionPref): void;
  setTheme(t: ThemeId): void;
}

const KEY = 'mycel.prefs';

function read(): { motion: MotionPref; theme: ThemeId } {
  try {
    const j = JSON.parse(localStorage.getItem(KEY) ?? '{}') as Partial<{ motion: MotionPref; theme: ThemeId }>;
    const motion = j.motion === 'reduced' || j.motion === 'full' ? j.motion : 'system';
    const theme = THEMES.some((t) => t.id === j.theme) ? (j.theme as ThemeId) : 'hud';
    return { motion, theme };
  } catch {
    return { motion: 'system', theme: 'hud' };
  }
}

function write(p: { motion: MotionPref; theme: ThemeId }): void {
  try {
    localStorage.setItem(KEY, JSON.stringify(p));
  } catch {
    /* private mode */
  }
}

export const usePrefs = create<PrefsState>((set, get) => ({
  ...read(),
  setMotion(motion) {
    setMotion(motion);
    set({ motion });
    write({ motion, theme: get().theme });
  },
  setTheme(theme) {
    document.documentElement.dataset.theme = theme;
    set({ theme });
    write({ motion: get().motion, theme });
  },
}));

/** applyPrefs puts the stored preferences on <html> at startup. */
export function applyPrefs(): void {
  const p = usePrefs.getState();
  setMotion(p.motion);
  document.documentElement.dataset.theme = p.theme;
}
