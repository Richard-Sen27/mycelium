import { Dialog as BaseDialog } from '@base-ui/react/dialog';
import { ConnectError } from '@connectrpc/connect';
import { useEffect, useId, useMemo, useRef, useState, type ReactNode } from 'react';
import { useNavigate } from 'react-router';
import { Button, Kbd, toast } from '../../ds';
import { agentsStore, grantsStore, nodesStore, useFreeze } from '../events/stores';
import { useRights } from '../session';
import type { SearchHit } from '../../gen/mycel/v1/ui_pb';
import { orchestratorFor, sendToOrchestrator } from './core';
import { searchCommands, useCommandSources, usePalette, type Command, type CommandContext } from './registry';
import { groupHits, openHit, useServerSearch } from './search';
import s from './CommandBar.module.css';

type Row =
  | { type: 'command'; cmd: Command; positions: number[] }
  | { type: 'hit'; hit: SearchHit; group: string }
  | { type: 'orchestrator'; text: string; agentName: string; agentId: string };

const MAX_ROWS = 60;
const MAX_COMMANDS_WITH_HITS = 8;

/** CommandBar is the ⌘K / Ctrl+K palette. Mount it once inside the router. */
export function CommandBar() {
  const open = usePalette((st) => st.open);
  const initialQuery = usePalette((st) => st.query);
  const setOpen = usePalette((st) => st.setOpen);
  const navigate = useNavigate();
  const rights = useRights();

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && !e.altKey && !e.shiftKey && e.key.toLowerCase() === 'k') {
        e.preventDefault();
        setOpen(!usePalette.getState().open);
      }
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [setOpen]);

  const ctx: CommandContext = useMemo(() => ({ rights, navigate: (to) => void navigate(to) }), [rights, navigate]);

  return (
    <BaseDialog.Root open={open} onOpenChange={(o) => setOpen(o)}>
      <BaseDialog.Portal>
        <BaseDialog.Backdrop className={s.backdrop} />
        <BaseDialog.Popup className={s.palette} aria-label="Command bar">
          {open && <Palette ctx={ctx} initialQuery={initialQuery} close={() => setOpen(false)} />}
        </BaseDialog.Popup>
      </BaseDialog.Portal>
    </BaseDialog.Root>
  );
}

function Palette({ ctx, initialQuery, close }: { ctx: CommandContext; initialQuery: string; close: () => void }) {
  const [query, setQuery] = useState(initialQuery);
  const [active, setActive] = useState(0);
  const [confirming, setConfirming] = useState<Command | null>(null);
  const [busy, setBusy] = useState(false);
  const inputRef = useRef<HTMLInputElement>(null);
  const listId = useId();
  const sources = useCommandSources();
  // Re-rank when the data behind dynamic commands changes.
  const grants = grantsStore.use((st) => st.byId);
  const agents = agentsStore.use((st) => st.byId);
  const freeze = useFreeze((st) => st.status);
  const search = useServerSearch(query);

  useEffect(() => {
    void nodesStore.ensure();
    void grantsStore.ensure();
    if (ctx.rights.mode === 'hub') void agentsStore.ensure();
  }, [ctx.rights.mode]);

  const rows: Row[] = useMemo(() => {
    const text = query.trim();
    const hits = text && search.query === text ? search.hits : [];
    // With server hits on screen, only the best few commands stay above them.
    const ranked = searchCommands(query, ctx).slice(0, hits.length ? MAX_COMMANDS_WITH_HITS : MAX_ROWS);
    const out: Row[] = ranked.map((r) => ({ type: 'command', cmd: r.item, positions: r.match.positions }));
    const words = text.split(/\s+/).filter(Boolean).length;
    const noCommand = out.length === 0;
    // Server search hits, grouped by kind, below the commands.
    for (const g of groupHits(hits)) for (const hit of g.hits) out.push({ type: 'hit', hit, group: `Search · ${g.label}` });
    // Free text for the orchestrator stays last.
    if (text && (noCommand || words >= 3)) {
      const orch = orchestratorFor(ctx);
      if (orch) out.push({ type: 'orchestrator', text, agentName: orch.name, agentId: orch.id });
    }
    return out;
  }, [query, ctx, sources, grants, agents, freeze, search]);

  useEffect(() => setActive(0), [query]);

  const run = async (row: Row) => {
    if (row.type === 'command' && row.cmd.confirm && confirming?.id !== row.cmd.id) {
      setConfirming(row.cmd);
      return;
    }
    setBusy(true);
    try {
      if (row.type === 'command') await row.cmd.run(ctx);
      else if (row.type === 'hit') await openHit(row.hit, ctx.navigate);
      else {
        const orch = agentsStore.use.getState().byId[row.agentId];
        if (orch) await sendToOrchestrator(orch, row.text);
      }
      close();
    } catch (err) {
      toast({ title: 'Command failed', body: ConnectError.from(err).rawMessage, tone: 'alert' });
      setBusy(false);
      setConfirming(null);
    }
  };

  const onKeyDown = (e: React.KeyboardEvent) => {
    if (confirming) return;
    if (e.key === 'ArrowDown') {
      e.preventDefault();
      setActive((a) => Math.min(rows.length - 1, a + 1));
    } else if (e.key === 'ArrowUp') {
      e.preventDefault();
      setActive((a) => Math.max(0, a - 1));
    } else if (e.key === 'Enter') {
      e.preventDefault();
      const row = rows[active];
      if (row && !busy) void run(row);
    }
  };

  useEffect(() => {
    document.getElementById(`${listId}-${active}`)?.scrollIntoView?.({ block: 'nearest' });
  }, [active, listId]);

  if (confirming?.confirm) {
    const c = confirming.confirm;
    return (
      <div className={s.confirm} data-tone={c.tone ?? 'default'}>
        <BaseDialog.Title className={s.confirmTitle}>{c.title}</BaseDialog.Title>
        {c.body && <p className={s.confirmBody}>{c.body}</p>}
        <div className={s.confirmActions}>
          <Button variant="ghost" onClick={() => setConfirming(null)} disabled={busy}>
            Back
          </Button>
          <Button
            variant={c.tone === 'alert' ? 'warn' : 'primary'}
            busy={busy}
            autoFocus
            onClick={() => void run({ type: 'command', cmd: confirming, positions: [] })}
          >
            {c.action}
          </Button>
        </div>
      </div>
    );
  }

  let lastGroup: string | undefined;
  return (
    <>
      <BaseDialog.Title className="visually-hidden">Command bar</BaseDialog.Title>
      <div className={s.inputRow}>
        <span className={s.prompt} aria-hidden>
          ›
        </span>
        <input
          ref={inputRef}
          className={s.input}
          autoFocus
          value={query}
          onChange={(e) => setQuery(e.target.value)}
          onKeyDown={onKeyDown}
          placeholder="Type a command, an id, or a message for your orchestrator"
          role="combobox"
          aria-expanded
          aria-controls={listId}
          aria-activedescendant={rows.length ? `${listId}-${active}` : undefined}
          aria-autocomplete="list"
          spellCheck={false}
          autoComplete="off"
        />
        <Kbd keys={['esc']} className={s.kbd} />
      </div>
      <ul className={s.list} id={listId} role="listbox" aria-label="Commands">
        {search.error && query.trim() && (
          <li className={s.none} role="presentation" data-tone="alert">
            Search failed: {search.error}
          </li>
        )}
        {rows.length === 0 && (
          <li className={s.none} role="presentation">
            {search.loading ? 'Searching… ' : 'No command matches.'}
            {ctx.rights.mode === 'hub' && ' Free text goes to your orchestrator once one is registered.'}
          </li>
        )}
        {rows.map((row, i) => {
          const group = row.type === 'command' ? row.cmd.group : row.type === 'hit' ? row.group : 'Orchestrator';
          const heading = group !== lastGroup ? group : undefined;
          lastGroup = group;
          return (
            <PaletteRow
              key={row.type === 'command' ? row.cmd.id : row.type === 'hit' ? `hit:${row.hit.kind}:${row.hit.id}` : 'orchestrator'}
              id={`${listId}-${i}`}
              heading={heading}
              active={i === active}
              onHover={() => setActive(i)}
              onRun={() => !busy && void run(row)}
            >
              {row.type === 'command' ? (
                <>
                  <span className={s.title}>{highlight(row.cmd.title, row.positions)}</span>
                  {row.cmd.hint && <span className={s.hint}>{row.cmd.hint}</span>}
                  {row.cmd.shortcut && <Kbd keys={row.cmd.shortcut} className={s.kbd} />}
                </>
              ) : row.type === 'hit' ? (
                <>
                  <span className={s.title}>{row.hit.title || row.hit.id}</span>
                  {row.hit.snippet && <span className={s.hint}>{row.hit.snippet}</span>}
                </>
              ) : (
                <>
                  <span className={s.title}>
                    Send to {row.agentName}: <span className={s.quote}>“{row.text}”</span>
                  </span>
                  <span className={s.hint}>direct message</span>
                </>
              )}
            </PaletteRow>
          );
        })}
      </ul>
      <div className={s.footer} aria-hidden>
        <span className={s.keys}>
          <Kbd keys={['↑', '↓']} /> move
        </span>
        <span className={s.keys}>
          <Kbd keys={['↵']} /> run
        </span>
        <span className={s.mode}>
          {ctx.rights.mode === 'node' ? 'node · read-only' : ctx.rights.isAdmin ? 'hub · admin' : 'hub · member'}
        </span>
      </div>
    </>
  );
}

function PaletteRow({
  id,
  heading,
  active,
  onHover,
  onRun,
  children,
}: {
  id: string;
  heading?: string;
  active: boolean;
  onHover: () => void;
  onRun: () => void;
  children: ReactNode;
}) {
  return (
    <>
      {heading && (
        <li className={s.group} role="presentation">
          {heading}
        </li>
      )}
      <li
        id={id}
        role="option"
        aria-selected={active}
        className={s.row}
        onMouseMove={onHover}
        onMouseDown={(e) => e.preventDefault()}
        onClick={onRun}
      >
        {children}
      </li>
    </>
  );
}

function highlight(text: string, positions: number[]): ReactNode {
  if (positions.length === 0) return text;
  const set = new Set(positions);
  const out: ReactNode[] = [];
  let buf = '';
  let inMatch = false;
  for (let i = 0; i < text.length; i++) {
    const m = set.has(i);
    if (m !== inMatch && buf) {
      out.push(inMatch ? <mark key={i}>{buf}</mark> : buf);
      buf = '';
    }
    inMatch = m;
    buf += text[i];
  }
  if (buf) out.push(inMatch ? <mark key="end">{buf}</mark> : buf);
  return out;
}
