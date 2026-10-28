import { useEffect, useMemo } from 'react';
import { useNavigate } from 'react-router';
import { Badge, Button, ConfirmButton, EmptyState, Fields, Gauge, Readout, Ago, MessageBody, fmtCount, fmtUsd } from '../../ds';
import type { Agent } from '../../gen/mycel/v1/hub_pb';
import type { Message } from '../../gen/mycel/v1/envelope_pb';
import { getApi } from '../../app/api';
import { useEntity, useEntityList } from '../../app/events/entities';
import { agentsStore, grantsStore, jobsStore, nodesStore, useMessages } from '../../app/events/stores';
import { inspect, useInspector } from '../../app/shell/inspector';
import { useRights, type Rights } from '../../app/session';
import { inspectGrant } from '../grants/GrantDetail';
import { grantKind, stateName } from '../grants/grants';
import { inspectNode, jobStateName } from '../nodes/NodeDetail';
import { act, agentShort, canManageAgent, errText, useUserNames } from '../nodes/shared';
import { channelPath, isUntrusted, mergeMessages } from '../channels/model';
import s from './page.module.css';
import { BudgetForm, PauseForm, SpawnForm } from './AgentForms';
import { agentKind, agentState, agentTone, canSpawn, hasBudget, lastSeen } from './agents';

export function inspectAgent(id: string): void {
  const name = agentShort(id);
  inspect({ key: `agent:${id}`, title: `Agent · ${name}`, content: <AgentDetail id={id} /> });
}

const EMPTY: Message[] = [];
const historyLoaded = new Set<string>();
// Sign-out (and a stream reset) empties the message store: load history again.
useMessages.subscribe((st) => {
  if (Object.keys(st.byChannel).length === 0) historyLoaded.clear();
});

/** useAgentMessages: the last 20 messages from the agent in its home channel (history loaded once with ListMessages). */
export function useAgentMessages(a: Agent | undefined): Message[] {
  const channel = a?.homeChannel ?? '';
  const msgs = useMessages((st) => (channel ? (st.byChannel[channel] ?? EMPTY) : EMPTY));
  useEffect(() => {
    if (!channel || historyLoaded.has(channel)) return;
    historyLoaded.add(channel);
    getApi()
      .rpc.channels.listMessages({ channel, limit: 200 })
      .then((r) => {
        useMessages.getState().setChannel(channel, mergeMessages(r.messages, useMessages.getState().byChannel[channel] ?? []));
      })
      .catch(() => historyLoaded.delete(channel));
  }, [channel]);
  return useMemo(
    () =>
      a
        ? msgs
            .filter((m) => m.from === a.id)
            .slice(-20)
            .reverse()
        : EMPTY,
    [msgs, a],
  );
}

/** AgentDetail is the inspector content for one agent; reads the stores by id and stays live. */
export function AgentDetail({ id }: { id: string }) {
  const rights = useRights();
  const agent = useEntity(agentsStore, id);
  const grants = useEntityList(grantsStore).items;
  const jobs = useEntityList(jobsStore).items;
  const agents = useEntityList(agentsStore).items;
  useEntityList(nodesStore);
  const userName = useUserNames(rights);
  const messages = useAgentMessages(agent);
  const navigate = useNavigate();

  const myGrants = useMemo(() => grants.filter((g) => g.subject === id || g.requestedBy === id), [grants, id]);
  const myJobs = useMemo(() => jobs.filter((j) => j.submittedBy === id).sort((a, b) => b.createdAt.localeCompare(a.createdAt)), [jobs, id]);
  const children = useMemo(() => agents.filter((a) => a.parent === id), [agents, id]);

  if (!agent) return <EmptyState title="This agent is gone">It was revoked, or it is not visible to you.</EmptyState>;

  const st = agentState(agent);
  const usedTokens = Number(agent.used?.tokens ?? 0n);
  const budgetTokens = Number(agent.budget?.tokens ?? 0n);
  const usedUsd = agent.used?.usd ?? 0;
  const budgetUsd = agent.budget?.usd ?? 0;

  return (
    <div className={s.detail}>
      <div className={s.readouts}>
        <Readout
          size="sm"
          label="State"
          value={st}
          tone={st === 'running' ? 'accent' : st === 'paused' ? 'alert' : 'muted'}
          sub={agent.pausedReason || undefined}
        />
        <Readout size="sm" label="Tokens" value={usedTokens} format={fmtCount} />
        <Readout size="sm" label="Spent" value={usedUsd} format={fmtUsd} />
        <Readout size="sm" label="Turns" value={agent.turns} />
      </div>

      <section className={s.section}>
        <h3 className={s.sectionTitle}>Budget</h3>
        {hasBudget(agent) ? (
          <div className={s.gauges}>
            {budgetTokens > 0 && (
              <Gauge
                label="Tokens"
                value={usedTokens}
                max={budgetTokens}
                valueText={`${fmtCount(usedTokens)} / ${fmtCount(budgetTokens)}`}
              />
            )}
            {budgetUsd > 0 && (
              <Gauge label="Spend" value={usedUsd} max={budgetUsd} valueText={`${fmtUsd(usedUsd)} / ${fmtUsd(budgetUsd)}`} />
            )}
          </div>
        ) : (
          <p className={s.note}>No per-agent budget; the home channel's budget still applies.</p>
        )}
      </section>

      <section className={s.section}>
        <h3 className={s.sectionTitle}>Identity</h3>
        <Fields
          items={[
            { label: 'Id', value: agent.id, mono: true },
            { label: 'Kind', value: agentKind(agent) },
            {
              label: 'Node',
              value: (
                <button type="button" className={s.rowButton} onClick={() => inspectNode(agent.nodeName)}>
                  {agent.nodeName}
                </button>
              ),
            },
            { label: 'Owner', value: userName(agent.ownerId) },
            { label: 'Harness', value: agent.harness, mono: true },
            {
              label: 'Parent',
              hidden: !agent.parent,
              value: (
                <button type="button" className={s.rowButton} onClick={() => inspectAgent(agent.parent)}>
                  {agentShort(agent.parent)}
                </button>
              ),
            },
            {
              label: 'Home channel',
              value: agent.homeChannel ? (
                <button type="button" className={s.rowButton} onClick={() => void navigate(channelPath(agent.homeChannel))}>
                  {agent.homeChannel}
                </button>
              ) : (
                ''
              ),
            },
            { label: 'Sandbox', value: agent.sandboxId || 'trusted host', mono: true },
            { label: 'Registered', value: <Ago at={agent.createdAt} /> },
            {
              label: 'Last seen (est.)',
              value: <Ago at={lastSeen(agent, messages)} title="Estimated: the agent's last token refresh or message" />,
            },
            {
              label: 'Exited',
              hidden: !agent.exitedAt,
              value: (
                <span className="mono">
                  {`${agent.exitCode} · `}
                  <Ago at={agent.exitedAt} />
                </span>
              ),
            },
          ]}
        />
      </section>

      <section className={s.section}>
        <h3 className={s.sectionTitle}>
          Recent messages <span className={s.count}>{messages.length}</span>
        </h3>
        {messages.length === 0 ? (
          <p className={s.note}>{agent.homeChannel ? `Nothing from this agent in ${agent.homeChannel} yet.` : 'No home channel.'}</p>
        ) : (
          <div>
            {messages.map((m) => (
              <div key={m.id} className={s.message}>
                <div className={s.messageMeta}>
                  <span>{m.channel}</span>
                  <Ago at={m.ts} />
                </div>
                {/* Agent output is untrusted: quoted plain text, never markup. */}
                <MessageBody text={m.payload?.text ?? ''} untrusted={isUntrusted(m)} clamp={4} />
              </div>
            ))}
          </div>
        )}
      </section>

      <section className={s.section}>
        <h3 className={s.sectionTitle}>
          Grants <span className={s.count}>{myGrants.length}</span>
        </h3>
        {myGrants.length === 0 ? (
          <p className={s.note}>No grants.</p>
        ) : (
          <ul className={s.rows}>
            {myGrants.slice(0, 20).map((g) => (
              <li key={g.id} className={s.rowItem}>
                <button type="button" className={s.rowButton} onClick={() => inspectGrant(g.id)}>
                  {grantKind(g)} on {g.node}
                </button>
                <span className={s.rowMeta}>{stateName(g.state)}</span>
              </li>
            ))}
          </ul>
        )}
      </section>

      <section className={s.section}>
        <h3 className={s.sectionTitle}>
          Jobs <span className={s.count}>{myJobs.length}</span>
        </h3>
        {myJobs.length === 0 ? (
          <p className={s.note}>No jobs submitted.</p>
        ) : (
          <ul className={s.rows}>
            {myJobs.slice(0, 12).map((j) => (
              <li key={j.id} className={s.rowItem}>
                <button type="button" className={s.rowButton} onClick={() => void navigate(`/jobs/${encodeURIComponent(j.id)}`)}>
                  {j.spec?.name || j.spec?.command.join(' ') || j.id}
                </button>
                <span className={s.rowMeta}>{jobStateName(j.state)}</span>
              </li>
            ))}
          </ul>
        )}
      </section>

      {children.length > 0 && (
        <section className={s.section}>
          <h3 className={s.sectionTitle}>
            Workers <span className={s.count}>{children.length}</span>
          </h3>
          <ul className={s.rows}>
            {children.map((c) => (
              <li key={c.id} className={s.rowItem}>
                <button type="button" className={s.rowButton} onClick={() => inspectAgent(c.id)}>
                  {c.name}
                </button>
                <Badge tone={agentTone(c)}>{agentState(c)}</Badge>
              </li>
            ))}
          </ul>
        </section>
      )}

      <AgentActions agent={agent} rights={rights} />
    </div>
  );
}

function AgentActions({ agent, rights }: { agent: Agent; rights: Rights }) {
  const close = useInspector((st) => st.close);
  const manage = canManageAgent(rights, agent);
  const spawn = canSpawn(rights, agent);
  if (!manage && !spawn) return null;
  const st = agentState(agent);
  const id = agent.id;
  return (
    <section className={s.section}>
      <h3 className={s.sectionTitle}>Manage</h3>
      <div className={s.actions}>
        {manage && st === 'running' && (
          <Button
            size="sm"
            onClick={() => inspect({ key: `agent:${id}:pause`, title: `Pause · ${agent.name}`, content: <PauseForm id={id} /> })}
          >
            Pause
          </Button>
        )}
        {manage && st === 'paused' && (
          <Button
            size="sm"
            onClick={() => void act(() => getApi().rpc.agents.resumeAgent({ id }), `Resumed ${agent.name}`).catch(() => undefined)}
          >
            Resume
          </Button>
        )}
        {manage && (
          <Button
            size="sm"
            onClick={() => inspect({ key: `agent:${id}:budget`, title: `Budget · ${agent.name}`, content: <BudgetForm id={id} /> })}
          >
            Set budget
          </Button>
        )}
        {spawn && (
          <Button
            size="sm"
            onClick={() => inspect({ key: `agent:${id}:spawn`, title: `Spawn a worker · ${agent.name}`, content: <SpawnForm id={id} /> })}
          >
            Spawn worker
          </Button>
        )}
        {manage && st !== 'stopped' && (
          <ConfirmButton
            size="sm"
            variant="warn"
            tone="alert"
            describeError={errText}
            confirmTitle={`Stop ${agent.name}?`}
            confirmBody="The node stops the agent's process. It can be started again from its node."
            action="Stop agent"
            onConfirm={() => act(() => getApi().rpc.agents.stopAgent({ id }), `Stopped ${agent.name}`)}
          >
            Stop
          </ConfirmButton>
        )}
        {manage && (
          <ConfirmButton
            size="sm"
            variant="warn"
            tone="alert"
            describeError={errText}
            confirmTitle={`Revoke ${agent.name}?`}
            confirmBody="Its token is revoked and it is removed from the hub. Its grants end."
            action="Revoke agent"
            onConfirm={async () => {
              await act(() => getApi().rpc.agents.revokeAgent({ id }), `Revoked ${agent.name}`);
              close();
            }}
          >
            Revoke
          </ConfirmButton>
        )}
      </div>
    </section>
  );
}
