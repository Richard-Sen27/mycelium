import { useCallback, useMemo, useState } from 'react';
import {
  Badge,
  Chips,
  EmptyState,
  Frame,
  Gauge,
  Input,
  Readout,
  Ago,
  Skeleton,
  Table,
  TickingNumber,
  fmtCount,
  fmtUsd,
  type Column,
} from '../../ds';
import type { Agent } from '../../gen/mycel/v1/hub_pb';
import { useEntityList } from '../../app/events/entities';
import { agentsStore, useMessages } from '../../app/events/stores';
import { useInspector } from '../../app/shell/inspector';
import { useRights, type Rights } from '../../app/session';
import { agentShort, useUserNames } from '../nodes/shared';
import s from './page.module.css';
import { inspectAgent } from './AgentDetail';
import { agentKind, agentState, agentTone, budgetFrac, byNameNode, hasBudget, lastSeen } from './agents';
import { registerAgentCommands } from './commands';

registerAgentCommands();

export type AgentFilter = 'running' | 'paused' | 'stopped' | 'orchestrator' | 'worker' | 'mine';

/** filterAgents applies the text query and the pressed chips (exported for tests). */
export function filterAgents(agents: Agent[], query: string, chips: AgentFilter[], rights: Pick<Rights, 'userId'>): Agent[] {
  const q = query.trim().toLowerCase();
  const states = chips.filter((c): c is 'running' | 'paused' | 'stopped' => c === 'running' || c === 'paused' || c === 'stopped');
  const kinds = chips.filter((c): c is 'orchestrator' | 'worker' => c === 'orchestrator' || c === 'worker');
  return agents.filter((a) => {
    if (q && !`${a.id} ${a.harness} ${a.homeChannel} ${a.ownerId}`.toLowerCase().includes(q)) return false;
    if (states.length && !states.includes(agentState(a))) return false;
    if (kinds.length && !kinds.includes(agentKind(a))) return false;
    if (chips.includes('mine') && a.ownerId !== rights.userId) return false;
    return true;
  });
}

function LastSeen({ agent }: { agent: Agent }) {
  const msgs = useMessages((st) => (agent.homeChannel ? st.byChannel[agent.homeChannel] : undefined));
  return <Ago at={lastSeen(agent, msgs)} title="Estimated: the agent's last token refresh or message" />;
}

export function Component() {
  const rights = useRights();
  const { items, loaded, error, stale } = useEntityList(agentsStore, byNameNode);
  const userName = useUserNames(rights);
  const [query, setQuery] = useState('');
  const [chips, setChips] = useState<AgentFilter[]>([]);
  const selected = useInspector((st) =>
    st.open && st.key?.startsWith('agent:') ? st.key.replace(/:(pause|budget|spawn)$/, '').slice(6) : null,
  );

  const rows = useMemo(() => filterAgents(items, query, chips, rights), [items, query, chips, rights]);
  const count = (st: string) => items.filter((a) => agentState(a) === st).length;
  const totalTokens = items.reduce((n, a) => n + Number(a.used?.tokens ?? 0n), 0);
  const totalUsd = items.reduce((n, a) => n + (a.used?.usd ?? 0), 0);

  // With the inspector beside the table, the secondary columns step aside.
  const compact = useInspector((st) => st.open);
  const all: Array<Column<Agent> & { secondary?: boolean }> = [
    { key: 'name', header: 'Agent', cell: (a) => <span className={s.name}>{a.name}</span> },
    { key: 'node', header: 'Node', width: '10%', mono: true, hideOnNarrow: true, cell: (a) => <span className={s.dim}>{a.nodeName}</span> },
    {
      key: 'owner',
      header: 'Owner',
      width: '8%',
      secondary: true,
      hideOnNarrow: true,
      cell: (a) => <span className={s.dim}>{userName(a.ownerId)}</span>,
    },
    { key: 'kind', header: 'Kind', width: '96px', mono: true, secondary: true, hideOnNarrow: true, cell: (a) => agentKind(a) },
    {
      key: 'parent',
      header: 'Parent',
      width: '12%',
      mono: true,
      secondary: true,
      hideOnNarrow: true,
      cell: (a) => (a.parent ? <span className={s.dim}>{agentShort(a.parent)}</span> : <span className={s.faint}>—</span>),
    },
    {
      key: 'channel',
      header: 'Home',
      width: '8%',
      mono: true,
      secondary: true,
      hideOnNarrow: true,
      cell: (a) => a.homeChannel || <span className={s.faint}>—</span>,
    },
    {
      key: 'state',
      header: 'State',
      width: '124px',
      cell: (a) => (
        <Badge tone={agentTone(a)} title={a.pausedReason || undefined}>
          {agentState(a)}
          {a.pausedReason ? ` · ${a.pausedReason}` : ''}
        </Badge>
      ),
    },
    {
      key: 'tokens',
      header: 'Tokens',
      width: '76px',
      hideOnNarrow: true,
      align: 'right',
      mono: true,
      cell: (a) => <TickingNumber value={Number(a.used?.tokens ?? 0n)} format={fmtCount} />,
    },
    {
      key: 'usd',
      header: 'USD',
      width: '76px',
      align: 'right',
      mono: true,
      secondary: true,
      hideOnNarrow: true,
      cell: (a) => <TickingNumber value={a.used?.usd ?? 0} format={fmtUsd} />,
    },
    {
      key: 'budget',
      header: 'Budget',
      width: '88px',
      hideOnNarrow: true,
      cell: (a) =>
        hasBudget(a) ? (
          <Gauge className={s.cellGauge} value={budgetFrac(a)} max={1} ticks={false} valueText={null} />
        ) : (
          <span className={s.faint}>none</span>
        ),
    },
    { key: 'seen', header: 'Last seen', width: '116px', align: 'right', cell: (a) => <LastSeen agent={a} /> },
  ];
  const columns = compact ? all.filter((c) => !c.secondary) : all;

  const onActivate = useCallback((a: Agent) => inspectAgent(a.id), []);

  return (
    <div className={s.page}>
      <div className={s.head}>
        <h1 className={s.heading}>Agents</h1>
        <div className={s.summary}>
          <Readout size="sm" label="Running" value={count('running')} tone="accent" sub={`of ${items.length}`} />
          <Readout size="sm" label="Paused" value={count('paused')} tone={count('paused') ? 'alert' : 'default'} />
          <Readout size="sm" label="Tokens used" value={totalTokens} format={fmtCount} />
          <Readout size="sm" label="Spent" value={totalUsd} format={fmtUsd} />
        </div>
      </div>
      <Frame
        title="Agents"
        index={String(items.length).padStart(2, '0')}
        flush
        meta={stale ? <span className={s.stale}>stale</span> : undefined}
      >
        <div className={s.filters}>
          <Input
            className={s.search}
            aria-label="Filter agents"
            placeholder="Filter by name, node, harness, channel"
            value={query}
            onChange={(e) => setQuery(e.target.value)}
          />
          <Chips
            aria-label="Agent filters"
            value={chips}
            onChange={setChips}
            options={[
              { value: 'running', label: 'running', count: count('running') },
              { value: 'paused', label: 'paused', count: count('paused') },
              { value: 'stopped', label: 'stopped', count: count('stopped') },
              { value: 'orchestrator', label: 'orchestrators' },
              { value: 'worker', label: 'workers' },
              ...(rights.userId ? [{ value: 'mine' as const, label: 'mine' }] : []),
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
            aria-label="Agents"
            columns={columns}
            rows={rows}
            rowKey={(a) => a.id}
            onActivate={onActivate}
            selectedKey={selected}
            rowTone={(a) => (agentState(a) === 'paused' ? 'alert' : agentState(a) === 'stopped' ? 'off' : undefined)}
            empty={
              <EmptyState title={items.length ? 'No agent matches these filters' : 'No agents yet'}>
                {items.length ? 'Clear a filter to see more.' : 'Agents appear when a harness registers through `mycel mcp` on a node.'}
              </EmptyState>
            }
          />
        )}
      </Frame>
    </div>
  );
}
