import { useMemo } from 'react';
import { Ago, Badge, EmptyState, Frame, Readout, Skeleton, Table, type Column } from '../../ds';
import { useEntityList } from '../../app/events/entities';
import { agentsStore, channelsStore, grantsStore } from '../../app/events/stores';
import type { Channel } from '../../gen/mycel/v1/channel_pb';
import { GrantState, type Grant } from '../../gen/mycel/v1/grant_pb';
import type { Agent } from '../../gen/mycel/v1/hub_pb';
import { grantKind, STATE_LABEL, STATE_TONE } from '../approvals/model';
import { BudgetGauge, fmtTokens, fmtUsd, hasBudget, remaining } from '../channels/Budget';
import { useChannelUsage } from '../channels/usage';
import s from './Traces.module.css';

const byCost = (a: Agent, b: Agent) => (b.used?.usd ?? 0) - (a.used?.usd ?? 0) || Number((b.used?.tokens ?? 0n) - (a.used?.tokens ?? 0n));
const byName = (a: Channel, b: Channel) => a.name.localeCompare(b.name);
const byUsage = (a: Grant, b: Grant) =>
  Number((b.usage?.gpuSeconds ?? 0n) - (a.usage?.gpuSeconds ?? 0n)) || Number((b.usage?.tokens ?? 0n) - (a.usage?.tokens ?? 0n));

const agentColumns: Column<Agent>[] = [
  { key: 'agent', header: 'Agent', mono: true, cell: (a) => `${a.name}@${a.nodeName}` },
  {
    key: 'state',
    header: 'State',
    width: '116px',
    cell: (a) => <Badge tone={a.state === 'running' ? 'ok' : a.state === 'paused' ? 'alert' : 'off'}>{a.state || '—'}</Badge>,
  },
  { key: 'home', header: 'Home', mono: true, hideOnNarrow: true, cell: (a) => a.homeChannel || '—' },
  { key: 'turns', header: 'Turns', mono: true, align: 'right', width: '64px', hideOnNarrow: true, cell: (a) => a.turns },
  { key: 'tokens', header: 'Tokens', mono: true, align: 'right', width: '90px', cell: (a) => fmtTokens(a.used?.tokens ?? 0n) },
  { key: 'usd', header: 'USD', mono: true, align: 'right', width: '80px', cell: (a) => fmtUsd(a.used?.usd ?? 0) },
  {
    key: 'budget',
    header: 'Budget',
    width: '180px',
    hideOnNarrow: true,
    cell: (a) => (hasBudget(a.budget) ? <BudgetGauge budget={a.budget} used={a.used} compact /> : <span className={s.dim}>unlimited</span>),
  },
  { key: 'left', header: 'Remaining', mono: true, align: 'right', width: '130px', cell: (a) => remaining(a.budget, a.used) },
];

const grantColumns: Column<Grant>[] = [
  { key: 'id', header: 'Grant', mono: true, width: '150px', cell: (g) => <span title={g.id}>{g.id.slice(0, 14)}</span> },
  { key: 'kind', header: 'Kind', mono: true, width: '100px', cell: (g) => grantKind(g) },
  { key: 'subject', header: 'Subject', mono: true, cell: (g) => g.subject },
  { key: 'node', header: 'Node', mono: true, hideOnNarrow: true, cell: (g) => g.node },
  {
    key: 'state',
    header: 'State',
    width: '116px',
    hideOnNarrow: true,
    cell: (g) => <Badge tone={STATE_TONE[g.state]}>{STATE_LABEL[g.state]}</Badge>,
  },
  {
    key: 'gpu',
    header: 'GPU-s',
    mono: true,
    align: 'right',
    width: '80px',
    cell: (g) => Number(g.usage?.gpuSeconds ?? 0n).toLocaleString('en-US'),
  },
  { key: 'tokens', header: 'Tokens', mono: true, align: 'right', width: '84px', cell: (g) => fmtTokens(g.usage?.tokens ?? 0n) },
  {
    key: 'req',
    header: 'Requests',
    mono: true,
    align: 'right',
    width: '84px',
    cell: (g) => Number(g.usage?.requests ?? 0n).toLocaleString('en-US'),
  },
  {
    key: 'upd',
    header: 'Reported',
    mono: true,
    align: 'right',
    width: '90px',
    hideOnNarrow: true,
    cell: (g) => (g.usage?.updatedAt ? <Ago at={g.usage.updatedAt} /> : '—'),
  },
];

/** Cost: per agent, per channel and per grant usage, from the stores and GetChannelUsage. */
export function Cost() {
  const agents = useEntityList(agentsStore, byCost);
  const channels = useEntityList(channelsStore, byName);
  const grants = useEntityList(grantsStore, byUsage);
  const brokered = useMemo(
    () =>
      grants.items.filter(
        (g) =>
          g.state === GrantState.ACTIVE ||
          Number(g.usage?.gpuSeconds ?? 0n) > 0 ||
          Number(g.usage?.tokens ?? 0n) > 0 ||
          Number(g.usage?.requests ?? 0n) > 0,
      ),
    [grants.items],
  );
  const totals = useMemo(() => {
    let tokens = 0;
    let usd = 0;
    for (const a of agents.items) {
      tokens += Number(a.used?.tokens ?? 0n);
      usd += a.used?.usd ?? 0;
    }
    let gpu = 0;
    let brokerTokens = 0;
    for (const g of grants.items) {
      gpu += Number(g.usage?.gpuSeconds ?? 0n);
      brokerTokens += Number(g.usage?.tokens ?? 0n);
    }
    return { tokens, usd, gpuHours: gpu / 3600, brokerTokens };
  }, [agents.items, grants.items]);
  const live = channels.items.filter((c) => !c.archivedAt);

  return (
    <div className={s.stack}>
      <div className={s.totals}>
        <Readout label="Agent tokens" value={totals.tokens} format={(n) => fmtTokens(Math.round(n))} />
        <Readout label="Agent spend" value={totals.usd} format={(n) => fmtUsd(n)} />
        <Readout label="GPU hours" value={totals.gpuHours} decimals={2} />
        <Readout label="Broker tokens" value={totals.brokerTokens} format={(n) => fmtTokens(Math.round(n))} />
      </div>
      <Frame title="Per agent" index={String(agents.items.length).padStart(2, '0')} flush>
        {!agents.loaded ? (
          <Skeleton rows={4} />
        ) : (
          <Table
            aria-label="Cost per agent"
            columns={agentColumns}
            rows={agents.items}
            rowKey={(a) => a.id}
            rowTone={(a) => (a.pausedReason.startsWith('budget') ? 'alert' : undefined)}
            empty={<EmptyState title="No agents" />}
          />
        )}
      </Frame>
      <Frame title="Per channel" index={String(live.length).padStart(2, '0')} flush>
        {!channels.loaded ? (
          <Skeleton rows={3} />
        ) : live.length === 0 ? (
          <EmptyState title="No channels" />
        ) : (
          <Table aria-label="Cost per channel" columns={channelColumns} rows={live} rowKey={(c) => c.name} />
        )}
      </Frame>
      <Frame title="Per grant · broker usage" index={String(brokered.length).padStart(2, '0')} flush>
        {!grants.loaded ? (
          <Skeleton rows={3} />
        ) : (
          <Table
            aria-label="Broker usage per grant"
            columns={grantColumns}
            rows={brokered}
            rowKey={(g) => g.id}
            empty={
              <EmptyState title="No broker usage yet">
                GPU leases, Ollama tokens and host Docker requests are reported per grant.
              </EmptyState>
            }
          />
        )}
      </Frame>
    </div>
  );
}

type UsageField = 'tokens' | 'usd' | 'budget' | 'left' | 'name';

/** ChannelUsageCell reads one channel's usage (cached GetChannelUsage) for a table cell. */
function ChannelUsageCell({ c, field }: { c: Channel; field: UsageField }) {
  const u = useChannelUsage(c.name);
  const budget = u?.budget ?? c.policy?.budget;
  switch (field) {
    case 'name':
      return (
        <>
          {c.name}
          {u?.paused && (
            <>
              {' '}
              <Badge tone="alert">paused</Badge>
            </>
          )}
        </>
      );
    case 'tokens':
      return <>{u ? fmtTokens(u.used?.tokens ?? 0n) : '—'}</>;
    case 'usd':
      return <>{u ? fmtUsd(u.used?.usd ?? 0) : '—'}</>;
    case 'budget':
      return hasBudget(budget) ? <BudgetGauge budget={budget} used={u?.used} compact /> : <span className={s.dim}>unlimited</span>;
    case 'left':
      return <>{remaining(budget, u?.used)}</>;
  }
}

const channelColumns: Column<Channel>[] = [
  { key: 'name', header: 'Channel', mono: true, cell: (c) => <ChannelUsageCell c={c} field="name" /> },
  { key: 'tokens', header: 'Tokens', mono: true, align: 'right', width: '90px', cell: (c) => <ChannelUsageCell c={c} field="tokens" /> },
  { key: 'usd', header: 'USD', mono: true, align: 'right', width: '80px', cell: (c) => <ChannelUsageCell c={c} field="usd" /> },
  { key: 'budget', header: 'Budget', width: '200px', hideOnNarrow: true, cell: (c) => <ChannelUsageCell c={c} field="budget" /> },
  { key: 'left', header: 'Remaining', mono: true, align: 'right', width: '130px', cell: (c) => <ChannelUsageCell c={c} field="left" /> },
];
