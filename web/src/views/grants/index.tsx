import { useCallback, useMemo, useState } from 'react';
import {
  Badge,
  Button,
  Chips,
  EmptyState,
  Frame,
  Input,
  Readout,
  Ago,
  Countdown,
  Select,
  Skeleton,
  Table,
  fmtCount,
  type Column,
} from '../../ds';
import { GrantState, type Grant } from '../../gen/mycel/v1/grant_pb';
import { useEntityList } from '../../app/events/entities';
import { grantsStore, nodesStore } from '../../app/events/stores';
import { useInspector } from '../../app/shell/inspector';
import { useRights, type Rights } from '../../app/session';
import { agentShort, hubWrite } from '../nodes/shared';
import s from './page.module.css';
import { registerGrantCommands } from './commands';
import { inspectGrant } from './GrantDetail';
import { byCreatedDesc, grantKind, specSummary, stateName, stateTone } from './grants';
import { openRequestGrant } from './RequestGrantForm';

registerGrantCommands();

export type GrantFilter = 'pending' | 'active' | 'finished' | 'mine';

/** filterGrants applies the query, the chips and the node select (exported for tests). */
export function filterGrants(grants: Grant[], query: string, chips: GrantFilter[], node: string, rights: Pick<Rights, 'userId'>): Grant[] {
  const q = query.trim().toLowerCase();
  const states: GrantState[] = [];
  if (chips.includes('pending')) states.push(GrantState.PENDING);
  if (chips.includes('active')) states.push(GrantState.ACTIVE);
  if (chips.includes('finished')) states.push(GrantState.EXPIRED, GrantState.REVOKED, GrantState.DENIED);
  return grants.filter((g) => {
    if (states.length && !states.includes(g.state)) return false;
    if (chips.includes('mine') && g.ownerId !== rights.userId && g.requestedBy !== rights.userId) return false;
    if (node && g.node !== node) return false;
    if (q && !`${g.id} ${g.subject} ${g.node} ${grantKind(g)} ${specSummary(g.spec)} ${g.reason}`.toLowerCase().includes(q)) return false;
    return true;
  });
}

function usageText(g: Grant): string {
  const u = g.usage;
  if (!u || (u.requests === 0n && u.tokens === 0n && u.gpuSeconds === 0n)) return '—';
  if (g.spec?.kind.case === 'ollama') return `${fmtCount(u.tokens)} tok · ${fmtCount(u.requests)} req`;
  if (g.spec?.kind.case === 'gpu') return `${(Number(u.gpuSeconds) / 3600).toFixed(2)} GPU-h`;
  return `${fmtCount(u.requests)} req`;
}

const ALL = '*';

export function Component() {
  const rights = useRights();
  const { items, loaded, error, stale } = useEntityList(grantsStore, byCreatedDesc);
  const nodes = useEntityList(nodesStore).items;
  const [query, setQuery] = useState('');
  const [chips, setChips] = useState<GrantFilter[]>([]);
  const [node, setNode] = useState(ALL);
  const selected = useInspector((st) => (st.open && st.key?.startsWith('grant:') ? st.key.slice(6) : null));

  const rows = useMemo(() => filterGrants(items, query, chips, node === ALL ? '' : node, rights), [items, query, chips, node, rights]);
  const pending = items.filter((g) => g.state === GrantState.PENDING).length;
  const active = items.filter((g) => g.state === GrantState.ACTIVE).length;
  const nodeOptions = useMemo(
    () => [
      { value: ALL, label: 'All nodes' },
      ...[...new Set([...nodes.map((n) => n.name), ...items.map((g) => g.node)])]
        .filter(Boolean)
        .sort()
        .map((n) => ({ value: n, label: n })),
    ],
    [nodes, items],
  );

  const allColumns: Array<Column<Grant> & { secondary?: boolean }> = [
    {
      key: 'id',
      header: 'Id',
      width: '15%',
      mono: true,
      secondary: true,
      hideOnNarrow: true,
      cell: (g) => <span className={s.dim}>{g.id}</span>,
    },
    {
      key: 'kind',
      header: 'Kind',
      width: '104px',
      mono: true,
      hideOnNarrow: true,
      cell: (g) => <span className={s.name}>{grantKind(g)}</span>,
    },
    {
      key: 'subject',
      header: 'Subject',
      mono: true,
      cell: (g) => (
        <>
          {/* Phones drop the kind column; the kind leads the subject instead. */}
          <span className={s.narrowOnly}>{grantKind(g)} · </span>
          {agentShort(g.subject)}
        </>
      ),
    },
    { key: 'node', header: 'Node', width: '11%', mono: true, hideOnNarrow: true, cell: (g) => <span className={s.dim}>{g.node}</span> },
    {
      key: 'state',
      header: 'State',
      width: '196px',
      cell: (g) => (
        <span className={s.tags}>
          <Badge tone={stateTone(g.state)}>{stateName(g.state)}</Badge>
          {g.state === GrantState.ACTIVE && <Countdown className={s.wideOnly} to={g.expiresAt} warnUnderMs={5 * 60_000} />}
          {g.state === GrantState.PENDING && (
            <span className={`${s.dim} ${s.wideOnly}`}>
              <Ago at={g.createdAt} />
            </span>
          )}
        </span>
      ),
    },
    {
      key: 'spec',
      header: 'Spec',
      width: '18%',
      secondary: true,
      hideOnNarrow: true,
      cell: (g) => <span className={s.dim}>{specSummary(g.spec)}</span>,
    },
    {
      key: 'requester',
      header: 'Requester',
      width: '12%',
      mono: true,
      secondary: true,
      hideOnNarrow: true,
      cell: (g) => <span className={s.dim}>{agentShort(g.requestedBy)}</span>,
    },
    {
      key: 'usage',
      header: 'Usage',
      width: '120px',
      mono: true,
      align: 'right',
      secondary: true,
      hideOnNarrow: true,
      cell: (g) => usageText(g),
    },
  ];

  // With the inspector beside the table, the secondary columns step aside.
  const compact = useInspector((st) => st.open);
  const onActivate = useCallback((g: Grant) => inspectGrant(g.id), []);

  return (
    <div className={s.page}>
      <div className={s.head}>
        <h1 className={s.heading}>Grants</h1>
        <div className={s.summary}>
          <Readout size="sm" label="Pending" value={pending} tone={pending ? 'alert' : 'default'} />
          <Readout size="sm" label="Active" value={active} tone="accent" />
          <Readout size="sm" label="All" value={items.length} />
        </div>
        {hubWrite(rights) && (
          <div className={s.headActions}>
            <Button variant="primary" onClick={() => openRequestGrant()}>
              Request a grant
            </Button>
          </div>
        )}
      </div>
      <Frame
        title="Grants"
        index={String(items.length).padStart(2, '0')}
        flush
        meta={stale ? <span className={s.stale}>stale</span> : undefined}
      >
        <div className={s.filters}>
          <Input
            className={s.search}
            aria-label="Filter grants"
            placeholder="Filter by id, subject, kind, reason"
            value={query}
            onChange={(e) => setQuery(e.target.value)}
          />
          <Chips
            aria-label="Grant filters"
            value={chips}
            onChange={setChips}
            options={[
              { value: 'pending', label: 'pending', count: pending },
              { value: 'active', label: 'active', count: active },
              { value: 'finished', label: 'finished' },
              ...(rights.userId ? [{ value: 'mine' as const, label: 'mine' }] : []),
            ]}
          />
          <Select aria-label="Node" value={node} onValueChange={setNode} options={nodeOptions} />
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
            aria-label="Grants"
            columns={compact ? allColumns.filter((c) => !c.secondary) : allColumns}
            rows={rows}
            rowKey={(g) => g.id}
            onActivate={onActivate}
            selectedKey={selected}
            rowTone={(g) => (g.state === GrantState.PENDING ? 'alert' : g.state === GrantState.ACTIVE ? undefined : 'off')}
            empty={
              <EmptyState title={items.length ? 'No grant matches these filters' : 'No grants yet'}>
                {items.length
                  ? 'Clear a filter to see more.'
                  : 'Agents request grants for models, GPUs, Docker, mounts and egress; they show up here.'}
              </EmptyState>
            }
          />
        )}
      </Frame>
    </div>
  );
}
