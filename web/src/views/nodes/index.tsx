import { useCallback, useMemo, useState } from 'react';
import { Badge, Chips, EmptyState, Frame, Input, Readout, Ago, Skeleton, Table, type Column } from '../../ds';
import { GrantState } from '../../gen/mycel/v1/grant_pb';
import type { Node } from '../../gen/mycel/v1/node_pb';
import { useEntityList } from '../../app/events/entities';
import { agentsStore, grantsStore, jobsStore, nodesStore } from '../../app/events/stores';
import { useInspector } from '../../app/shell/inspector';
import { useRights, type Rights } from '../../app/session';
import { agentState } from '../agents/agents';
import { registerNodeCommands } from './commands';
import { inspectNode, isActiveJob } from './NodeDetail';
import { sharingSummary, useNodeSharing } from './sharing';
import { useUserNames } from './shared';
import s from './page.module.css';

registerNodeCommands();

const byName = (a: Node, b: Node) => a.name.localeCompare(b.name);

export type NodeFilter = 'online' | 'offline' | 'mine' | 'docker' | 'ollama' | 'gpu' | 'sandbox';

/** filterNodes applies the text query and the pressed chips (exported for tests). */
export function filterNodes(nodes: Node[], query: string, chips: NodeFilter[], rights: Pick<Rights, 'userId'>): Node[] {
  const q = query.trim().toLowerCase();
  return nodes.filter((n) => {
    if (q && !`${n.name} ${n.ownerId} ${n.inventory?.hostname ?? ''} ${n.inventory?.os ?? ''}`.toLowerCase().includes(q)) return false;
    if (chips.includes('online') && !n.online) return false;
    if (chips.includes('offline') && n.online) return false;
    if (chips.includes('mine') && n.ownerId !== rights.userId) return false;
    for (const c of ['docker', 'ollama', 'gpu', 'sandbox'] as const) {
      if (chips.includes(c) && !(n.capsEnabled?.names ?? []).includes(c)) return false;
    }
    return true;
  });
}

function SharingCell({ name }: { name: string }) {
  const sh = useNodeSharing(name);
  if (!sh?.value) return <span className={s.faint}>{sh?.error ? 'n/a' : '…'}</span>;
  return (
    <span title={`hub: ${sharingSummary(sh.value.hub)}; local: ${sharingSummary(sh.value.local)}`}>
      {sharingSummary(sh.value.effective)}
    </span>
  );
}

/** Caps: the switched-on capabilities, then (dim) those the ceiling allows but are off. */
function Caps({ on, off }: { on: string[]; off: string[] }) {
  if (on.length === 0 && off.length === 0) return <span className={s.faint}>none</span>;
  return (
    <span title={off.length ? `off: ${off.join(', ')}` : undefined}>
      {on.join(' ')}
      {off.length > 0 && <span className={s.faint}> {off.join(' ')}</span>}
    </span>
  );
}

export function Component() {
  const rights = useRights();
  const { items, loaded, error, stale } = useEntityList(nodesStore, byName);
  const jobs = useEntityList(jobsStore).items;
  const grants = useEntityList(grantsStore).items;
  const agents = useEntityList(agentsStore).items;
  const userName = useUserNames(rights);
  const [query, setQuery] = useState('');
  const [chips, setChips] = useState<NodeFilter[]>([]);
  const selected = useInspector((st) => (st.open && st.key?.startsWith('node:') ? st.key.split(':')[1] : null));

  const activity = useMemo(() => {
    const m = new Map<string, { jobs: number; agents: number; grants: number }>();
    const get = (n: string) => m.get(n) ?? (m.set(n, { jobs: 0, agents: 0, grants: 0 }), m.get(n)!);
    for (const j of jobs) if (j.node && isActiveJob(j)) get(j.node).jobs++;
    for (const a of agents) if (agentState(a) === 'running') get(a.nodeName).agents++;
    for (const g of grants) if (g.state === GrantState.ACTIVE) get(g.node).grants++;
    return m;
  }, [jobs, agents, grants]);

  const rows = useMemo(() => filterNodes(items, query, chips, rights), [items, query, chips, rights]);
  const online = items.filter((n) => n.online).length;
  const cores = items.reduce((sum, n) => sum + (n.online ? (n.inventory?.cpuCount ?? 0) : 0), 0);
  const gpus = items.reduce((sum, n) => sum + (n.online ? (n.inventory?.gpus.length ?? 0) : 0), 0);

  const allColumns: Array<Column<Node> & { secondary?: boolean }> = useMemo(
    () => [
      { key: 'name', header: 'Node', cell: (n) => <span className={s.name}>{n.name}</span> },
      {
        key: 'owner',
        header: 'Owner',
        width: '10%',
        secondary: true,
        hideOnNarrow: true,
        cell: (n) => <span className={s.dim}>{userName(n.ownerId)}</span>,
      },
      {
        key: 'online',
        header: 'State',
        width: '112px',
        cell: (n) => <Badge tone={n.online ? 'ok' : 'off'}>{n.online ? 'online' : 'offline'}</Badge>,
      },
      {
        key: 'caps',
        header: 'Caps',
        width: '16%',
        mono: true,
        hideOnNarrow: true,
        cell: (n) => {
          const on = n.capsEnabled?.names ?? [];
          return <Caps on={on} off={(n.capsCeiling?.names ?? []).filter((c) => !on.includes(c))} />;
        },
      },
      {
        key: 'backends',
        header: 'Backends',
        secondary: true,
        width: '11%',
        mono: true,
        hideOnNarrow: true,
        cell: (n) => <span className={s.dim}>{n.inventory?.sandboxBackends.join(', ') || '—'}</span>,
      },
      {
        key: 'sharing',
        header: 'Sharing',
        width: '14%',
        mono: true,
        secondary: true,
        hideOnNarrow: true,
        cell: (n) => <SharingCell name={n.name} />,
      },
      {
        key: 'activity',
        header: 'Jobs · agents',
        width: '104px',
        hideOnNarrow: true,
        mono: true,
        align: 'right',
        cell: (n) => {
          const a = activity.get(n.name);
          return (
            <span>
              {a?.jobs ?? 0} <span className={s.faint}>·</span> {a?.agents ?? 0}
            </span>
          );
        },
      },
      {
        key: 'grants',
        header: 'Grants',
        secondary: true,
        width: '64px',
        mono: true,
        align: 'right',
        hideOnNarrow: true,
        cell: (n) => activity.get(n.name)?.grants ?? 0,
      },
      { key: 'seen', header: 'Heartbeat', width: '124px', align: 'right', cell: (n) => <Ago at={n.lastSeenAt} /> },
    ],
    [activity, userName],
  );

  // With the inspector beside the table, the secondary columns step aside.
  const compact = useInspector((st) => st.open);
  const onActivate = useCallback((n: Node) => inspectNode(n.name), []);

  return (
    <div className={s.page}>
      <div className={s.head}>
        <h1 className={s.heading}>Nodes</h1>
        <div className={s.summary}>
          <Readout size="sm" label="Online" value={online} sub={`of ${items.length}`} tone="accent" />
          <Readout size="sm" label="Cores online" value={cores} />
          <Readout size="sm" label="GPUs online" value={gpus} />
        </div>
      </div>
      <Frame
        title="Nodes"
        index={String(items.length).padStart(2, '0')}
        flush
        meta={stale ? <span className={s.stale}>stale</span> : undefined}
      >
        <div className={s.filters}>
          <Input
            className={s.search}
            aria-label="Filter nodes"
            placeholder="Filter by name, owner, host"
            value={query}
            onChange={(e) => setQuery(e.target.value)}
          />
          <Chips
            aria-label="Node filters"
            value={chips}
            onChange={(v) => setChips(v)}
            options={[
              { value: 'online', label: 'online', count: online },
              { value: 'offline', label: 'offline', count: items.length - online },
              ...(rights.userId ? [{ value: 'mine' as const, label: 'mine' }] : []),
              { value: 'gpu', label: 'gpu' },
              { value: 'ollama', label: 'ollama' },
              { value: 'docker', label: 'docker' },
              { value: 'sandbox', label: 'sandbox' },
            ]}
          />
          <span className={s.filterCount}>
            {rows.length} / {items.length}
          </span>
        </div>
        {error && !loaded ? (
          <p className={s.error}>{error}</p>
        ) : !loaded ? (
          <Skeleton rows={6} />
        ) : (
          <Table
            aria-label="Nodes"
            columns={compact ? allColumns.filter((c) => !c.secondary) : allColumns}
            rows={rows}
            rowKey={(n) => n.name}
            onActivate={onActivate}
            selectedKey={selected}
            rowTone={(n) => (n.online ? undefined : 'off')}
            empty={
              <EmptyState title={items.length ? 'No node matches these filters' : 'No nodes yet'}>
                {items.length
                  ? 'Clear a filter to see more.'
                  : 'Create a join token with `mycel token create` and run `mycel join` on a machine.'}
              </EmptyState>
            }
          />
        )}
      </Frame>
    </div>
  );
}
