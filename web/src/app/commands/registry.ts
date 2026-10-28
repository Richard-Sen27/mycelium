import { create } from 'zustand';
import type { Rights } from '../session';
import { rank, type Ranked } from './fuzzy';

/** CommandContext is what a command's predicate and action see. */
export interface CommandContext {
  rights: Rights;
  navigate: (to: string) => void;
}

export interface Command {
  /** Stable id, e.g. "nav.approvals" or "grant.approve.grt_01…". */
  id: string;
  title: string;
  /** Section heading in the palette ("Go to", "Network", "Approvals", …). */
  group?: string;
  /** Extra words the fuzzy matcher considers (synonyms, ids, CLI verbs). */
  keywords?: string[];
  /** Short right-aligned hint (e.g. the CLI equivalent). */
  hint?: string;
  /** Key chord shown next to the command (display only). */
  shortcut?: string[];
  /** Permission predicate: the command is hidden unless it returns true. */
  allowed?: (ctx: CommandContext) => boolean;
  /** Ask before running (destructive or network-wide actions). */
  confirm?: { title: string; body?: string; action: string; tone?: 'default' | 'alert' };
  run: (ctx: CommandContext) => void | Promise<void>;
}

/** A source builds commands on demand (e.g. one per pending grant). */
export type CommandSource = (ctx: CommandContext) => Command[];

interface RegistryState {
  sources: Map<number, CommandSource>;
}

const useRegistry = create<RegistryState>(() => ({ sources: new Map() }));
let nextId = 1;

/**
 * registerCommands adds static commands or a dynamic source; it returns an
 * unregister function (call it from a view's effect cleanup).
 */
export function registerCommands(cmds: Command[] | CommandSource): () => void {
  const id = nextId++;
  const source: CommandSource = typeof cmds === 'function' ? cmds : () => cmds;
  useRegistry.setState((st) => {
    const m = new Map(st.sources);
    m.set(id, source);
    return { sources: m };
  });
  return () =>
    useRegistry.setState((st) => {
      const m = new Map(st.sources);
      m.delete(id);
      return { sources: m };
    });
}

/** availableCommands returns every command whose predicate allows it. */
export function availableCommands(ctx: CommandContext): Command[] {
  const out: Command[] = [];
  const seen = new Set<string>();
  for (const src of useRegistry.getState().sources.values()) {
    let cmds: Command[];
    try {
      cmds = src(ctx);
    } catch (err) {
      console.error('command source failed', err);
      continue;
    }
    for (const c of cmds) {
      if (seen.has(c.id)) continue;
      if (c.allowed && !c.allowed(ctx)) continue;
      seen.add(c.id);
      out.push(c);
    }
  }
  return out;
}

/** searchCommands ranks the allowed commands for a query (all of them, in registry order, for an empty query). */
export function searchCommands(query: string, ctx: CommandContext): Ranked<Command>[] {
  const cmds = availableCommands(ctx);
  if (query.trim() === '') return cmds.map((item) => ({ item, match: { score: 0, positions: [] } }));
  return rank(query, cmds, (c) => [c.title, ...(c.keywords ?? []), c.group ?? '']);
}

export function useCommandSources() {
  return useRegistry((s) => s.sources);
}

/** Palette open state, so any view or shortcut can open it. */
export const usePalette = create<{ open: boolean; query: string; setOpen: (o: boolean, query?: string) => void }>((set) => ({
  open: false,
  query: '',
  setOpen: (open, query = '') => set({ open, query }),
}));
