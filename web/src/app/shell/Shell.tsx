import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { NavLink, Outlet, useLocation, useNavigate } from 'react-router';
import { Badge, Kbd, Sheet, ToastViewport, cx } from '../../ds';
import { networkName } from '../config';
import { CommandBar } from '../commands/CommandBar';
import { usePalette } from '../commands/registry';
import { agentsStore, channelsStore, grantsStore, jobsStore, nodesStore, useFreeze } from '../events/stores';
import { GrantState } from '../../gen/mycel/v1/grant_pb';
import { navFor, PHONE_QUERY, type NavItem } from '../nav';
import { listenForNavigation } from '../pwa/push';
import { useConnection, useRights, useSession } from '../session';
import { useInspector } from './inspector';
import { useMedia } from './useMedia';
import s from './Shell.module.css';

/** Shell is the app frame: top bar, rail (bottom bar on phones), main area, inspector. */
export function Shell() {
  const rights = useRights();
  const phone = useMedia(PHONE_QUERY);
  const navigate = useNavigate();
  const location = useLocation();
  const items = useMemo(() => navFor(rights.mode), [rights.mode]);
  const mainRef = useRef<HTMLElement>(null);

  const go = useCallback((to: string) => void navigate(to), [navigate]);
  useEffect(() => listenForNavigation(go), [go]);
  useGoShortcuts(items, go);

  // Move focus to the main area on route change, for keyboard and screen-reader users.
  useEffect(() => {
    mainRef.current?.focus({ preventScroll: true });
  }, [location.pathname]);

  return (
    <div className={s.shell} data-mode={rights.mode}>
      <a href="#main" className={s.skip}>
        Skip to content
      </a>
      <TopBar />
      <StaleBanner />
      {!phone && <Rail items={items} />}
      <main id="main" ref={mainRef} tabIndex={-1} className={s.main}>
        <Outlet />
      </main>
      <Inspector phone={phone} />
      {phone && <BottomBar items={items} />}
      <CommandBar />
      <ToastViewport />
    </div>
  );
}

function TopBar() {
  const config = useSession((st) => st.config);
  const rights = useRights();
  const freeze = useFreeze((st) => st.status);
  const stream = useConnection((st) => st.stream);
  const hub = useConnection((st) => st.hub);
  const openPalette = usePalette((st) => st.setOpen);
  const frozen = !!freeze?.allFrozen;
  const frozenChannels = freeze?.frozenChannels.length ?? 0;
  const phone = useMedia(PHONE_QUERY);
  const coarse = useMedia('(pointer: coarse)');

  const conn =
    rights.mode === 'node' && hub === 'stale'
      ? { tone: 'alert' as const, label: 'Hub unreachable' }
      : stream === 'live'
        ? { tone: 'ok' as const, label: 'Live' }
        : stream === 'retrying'
          ? { tone: 'alert' as const, label: 'Reconnecting' }
          : stream === 'stopped'
            ? { tone: 'off' as const, label: 'Offline' }
            : { tone: 'idle' as const, label: 'Connecting' };

  const freezeBadge = frozen ? (
    <Badge tone="alert" pulse title={freeze?.allReason || undefined}>
      Network frozen
    </Badge>
  ) : frozenChannels > 0 ? (
    <Badge tone="alert">{frozenChannels} ch frozen</Badge>
  ) : null;
  const connAlert = conn.tone === 'alert';

  return (
    <header className={s.topbar}>
      <div className={s.brand}>
        <Mark />
        <span className={s.network} title={config?.hub}>
          {config ? networkName(config) : 'mycelium'}
        </span>
        {rights.mode === 'node' && config?.node && <span className={s.node}>via {config.node}</span>}
      </div>
      {phone ? (
        // Phones show only what needs attention; the calm state is a single mark.
        <div className={s.status}>
          {freezeBadge}
          {connAlert ? (
            <Badge tone="alert" pulse>
              {conn.label}
            </Badge>
          ) : (
            !freezeBadge && (
              <span
                className={s.stateMark}
                data-tone={conn.tone}
                role="img"
                aria-label={`${conn.label}${rights.readOnly ? ', read-only' : ''}`}
                title={conn.label}
              />
            )
          )}
        </div>
      ) : (
        <div className={s.status}>
          {freezeBadge ?? (freeze ? <Badge tone="ok">Running</Badge> : null)}
          <Badge tone={conn.tone} pulse={connAlert}>
            {conn.label}
          </Badge>
          {rights.readOnly && <Badge tone="idle">Read-only</Badge>}
        </div>
      )}
      <button type="button" className={s.cmd} onClick={() => openPalette(true)} aria-label="Open the command bar">
        {coarse ? (
          'Search'
        ) : (
          <>
            <span className={s.cmdText}>Command</span>
            <Kbd keys={['mod', 'K']} />
          </>
        )}
      </button>
      <div className={s.user}>
        <span className={s.userName}>{rights.userName ?? '—'}</span>
        <span className={s.role}>{rights.mode === 'node' ? 'owner' : rights.isAdmin ? 'admin' : 'member'}</span>
      </div>
    </header>
  );
}

/** Mark: the product glyph, a body with one satellite. */
function Mark() {
  return (
    <svg className={s.mark} viewBox="0 0 20 20" aria-hidden>
      <circle cx="10" cy="10" r="6.5" fill="none" stroke="currentColor" strokeWidth="1" />
      <circle cx="10" cy="10" r="1.6" fill="currentColor" />
      <circle cx="15.6" cy="6.8" r="1.4" fill="var(--accent)" />
    </svg>
  );
}

function usePendingCount(): number {
  const byId = grantsStore.use((st) => st.byId);
  useEffect(() => {
    void grantsStore.ensure();
  }, []);
  return useMemo(() => Object.values(byId).filter((g) => g.state === GrantState.PENDING).length, [byId]);
}

function Rail({ items }: { items: NavItem[] }) {
  const rights = useRights();
  const config = useSession((st) => st.config);
  const pending = usePendingCount();
  return (
    <nav className={s.rail} aria-label="Views">
      <ol className={s.railList}>
        {items.map((n, i) => (
          <li key={n.id}>
            <NavLink to={n.path} className={({ isActive }) => cx(s.railItem, isActive && s.active)}>
              <span className={s.railIndex}>{String(i + 1).padStart(2, '0')}</span>
              <span className={s.railLabel}>{n.label}</span>
              {n.id === 'approvals' && pending > 0 && (
                <span className={s.count} aria-label={`${pending} pending`}>
                  {pending}
                </span>
              )}
              {rights.readOnly && n.id !== 'machine' && n.id !== 'settings' && (
                <span className={s.ro} aria-label="read-only" title="Read-only from this node">
                  ro
                </span>
              )}
            </NavLink>
          </li>
        ))}
      </ol>
      <div className={s.railFoot}>
        <span>{rights.mode === 'node' ? 'node ui' : 'hub ui'}</span>
        {config?.version && <span>{config.version}</span>}
      </div>
    </nav>
  );
}

function BottomBar({ items }: { items: NavItem[] }) {
  const [more, setMore] = useState(false);
  const navigate = useNavigate();
  const pending = usePendingCount();
  const primary = items.filter((n) => n.phone || n.id === 'machine').slice(0, 4);
  const rest = items.filter((n) => !primary.includes(n));
  return (
    <>
      <nav className={s.bottombar} aria-label="Views">
        {primary.map((n) => (
          <NavLink key={n.id} to={n.path} className={({ isActive }) => cx(s.tab, isActive && s.active)}>
            <span className={s.tabLabel}>{n.short}</span>
            {n.id === 'approvals' && pending > 0 && <span className={s.count}>{pending}</span>}
          </NavLink>
        ))}
        <button type="button" className={s.tab} onClick={() => setMore(true)} aria-haspopup="dialog">
          <span className={s.tabLabel}>More</span>
        </button>
      </nav>
      <Sheet open={more} onOpenChange={setMore} title="Views">
        <ol className={s.moreList}>
          {rest.map((n) => (
            <li key={n.id}>
              <button
                type="button"
                className={s.moreItem}
                onClick={() => {
                  setMore(false);
                  void navigate(n.path);
                }}
              >
                {n.label}
              </button>
            </li>
          ))}
        </ol>
      </Sheet>
    </>
  );
}

function Inspector({ phone }: { phone: boolean }) {
  const { open, title, content, close } = useInspector();
  const narrow = useMedia('(max-width: 1100px)');
  useEffect(() => {
    if (!open) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape' && !usePalette.getState().open) close();
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [open, close]);

  if (phone || narrow) {
    return (
      <Sheet open={open} onOpenChange={(o) => !o && close()} title={title ?? 'Inspector'} side={phone ? 'bottom' : 'right'}>
        {content}
      </Sheet>
    );
  }
  if (!open) return null;
  return (
    <aside className={s.inspector} aria-label="Inspector">
      <div className={s.inspectorHead}>
        <h2 className={s.inspectorTitle}>{title}</h2>
        <button type="button" className={s.close} onClick={close} aria-label="Close the inspector">
          <span aria-hidden>×</span>
        </button>
      </div>
      <div className={s.inspectorBody}>{content}</div>
    </aside>
  );
}

function StaleBanner() {
  const rights = useRights();
  const hub = useConnection((st) => st.hub);
  const lastOkAt = useConnection((st) => st.lastOkAt);
  // Without a successful call in this tab, the newest snapshot tells the age.
  const snaps = [
    nodesStore.use((st) => st.snapshotAt),
    agentsStore.use((st) => st.snapshotAt),
    channelsStore.use((st) => st.snapshotAt),
    jobsStore.use((st) => st.snapshotAt),
    grantsStore.use((st) => st.snapshotAt),
  ];
  if (rights.mode !== 'node' || hub !== 'stale') return null;
  const since =
    lastOkAt ??
    snaps
      .filter((x): x is string => !!x)
      .sort()
      .at(-1) ??
    null;
  return (
    <div className={s.stale} role="status">
      <span className={s.staleMark} aria-hidden />
      Hub unreachable. The network view is the last known state
      {since ? ` from ${new Date(since).toLocaleString()}` : ''}, marked stale. This machine still works.
    </div>
  );
}

/** "g" then a digit jumps to the nth rail entry (vim-style, outside inputs). */
function useGoShortcuts(items: NavItem[], go: (to: string) => void) {
  useEffect(() => {
    let armed = 0;
    const onKey = (e: KeyboardEvent) => {
      const t = e.target as HTMLElement | null;
      if (e.metaKey || e.ctrlKey || e.altKey) return;
      if (t && (t.isContentEditable || /^(INPUT|TEXTAREA|SELECT)$/.test(t.tagName))) return;
      if (usePalette.getState().open) return;
      if (e.key === 'g') {
        armed = Date.now();
        return;
      }
      if (armed && Date.now() - armed < 1200 && /^[1-9]$/.test(e.key)) {
        const n = items[Number(e.key) - 1];
        if (n) go(n.path);
      }
      armed = 0;
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [items, go]);
}
