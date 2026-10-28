import { useNavigate } from 'react-router';
import { Button, EmptyState, Fields, Readout, Ago } from '../../ds';
import { ChannelType } from '../../gen/mycel/v1/channel_pb';
import { useEntity, useEntityList } from '../../app/events/entities';
import { agentsStore, channelsStore, jobsStore, nodesStore, useFreeze } from '../../app/events/stores';
import { inspect } from '../../app/shell/inspector';
import { inspectAgent } from '../agents/AgentDetail';
import { agentState } from '../agents/agents';
import { inspectNode, isActiveJob, jobStateName } from '../nodes/NodeDetail';
import { agentShort } from '../nodes/shared';
import s from './details.module.css';
import { HUB_KEY } from './layout';

// Inspector content for scene objects that have no list view in this
// package: a running sandbox (the jobs view owns the full JobDetail), a
// channel (the channels view owns the chat) and the hub.

function JobSummary({ id }: { id: string }) {
  const job = useEntity(jobsStore, id);
  const navigate = useNavigate();
  if (!job) return <EmptyState title="This job is gone" />;
  return (
    <div className={s.detail}>
      <div className={s.readouts}>
        <Readout size="sm" label="State" value={jobStateName(job.state)} tone={isActiveJob(job) ? 'accent' : 'muted'} />
        <Readout size="sm" label="Backend" value={job.backend} />
      </div>
      <Fields
        items={[
          { label: 'Id', value: job.id, mono: true },
          { label: 'Command', value: job.spec?.command.join(' '), mono: true },
          { label: 'Profile', value: job.spec?.profile, mono: true },
          {
            label: 'Node',
            value: (
              <button type="button" className={s.rowButton} onClick={() => inspectNode(job.node)}>
                {job.node}
              </button>
            ),
          },
          { label: 'Sandbox', value: job.sandboxId, mono: true },
          { label: 'Submitted by', value: agentShort(job.submittedBy), mono: true },
          { label: 'Started', value: <Ago at={job.stats?.startedAt || job.createdAt} /> },
          { label: 'Channel', value: job.spec?.channel, mono: true, hidden: !job.spec?.channel },
        ]}
      />
      <div className={s.actions}>
        <Button size="sm" onClick={() => void navigate(`/jobs/${encodeURIComponent(job.id)}`)}>
          Open in Jobs
        </Button>
      </div>
    </div>
  );
}

function ChannelSummary({ name }: { name: string }) {
  const ch = useEntity(channelsStore, name);
  const agents = useEntityList(agentsStore).items;
  const frozen = useFreeze((st) => st.status?.frozenChannels.includes(name) ?? false);
  const navigate = useNavigate();
  if (!ch) return <EmptyState title="This channel is gone" />;
  const members = ch.members.filter((m) => m.id.startsWith('agent:'));
  const homed = agents.filter((a) => a.homeChannel === name);
  return (
    <div className={s.detail}>
      <div className={s.readouts}>
        <Readout size="sm" label="Type" value={ChannelType[ch.type]?.toLowerCase() ?? '—'} />
        <Readout size="sm" label="Members" value={ch.members.length} />
        <Readout size="sm" label="State" value={frozen ? 'frozen' : 'open'} tone={frozen ? 'alert' : 'default'} />
      </div>
      <section className={s.section}>
        <h3 className={s.sectionTitle}>
          Agents <span className={s.count}>{members.length + homed.filter((a) => !members.some((m) => m.id === a.id)).length}</span>
        </h3>
        <ul className={s.rows}>
          {[...new Set([...members.map((m) => m.id), ...homed.map((a) => a.id)])].map((id) => {
            const a = agents.find((x) => x.id === id);
            return (
              <li key={id} className={s.rowItem}>
                <button type="button" className={s.rowButton} onClick={() => inspectAgent(id)}>
                  {agentShort(id)}
                </button>
                <span className={s.rowMeta}>{a ? agentState(a) : 'member'}</span>
              </li>
            );
          })}
        </ul>
      </section>
      <div className={s.actions}>
        <Button size="sm" onClick={() => void navigate(`/channels/${encodeURIComponent(name)}`)}>
          Open channel
        </Button>
      </div>
    </div>
  );
}

function HubSummary() {
  const nodes = useEntityList(nodesStore).items;
  const agents = useEntityList(agentsStore).items;
  const jobs = useEntityList(jobsStore).items;
  const freeze = useFreeze((st) => st.status);
  return (
    <div className={s.detail}>
      <div className={s.readouts}>
        <Readout size="sm" label="Nodes online" value={nodes.filter((n) => n.online).length} sub={`of ${nodes.length}`} tone="accent" />
        <Readout size="sm" label="Agents running" value={agents.filter((a) => agentState(a) === 'running').length} />
        <Readout size="sm" label="Sandboxes" value={jobs.filter(isActiveJob).length} />
        <Readout
          size="sm"
          label="Network"
          value={freeze?.allFrozen ? 'frozen' : 'running'}
          tone={freeze?.allFrozen ? 'alert' : 'default'}
        />
      </div>
      <p className={s.note}>
        The hub holds identity, policy and the bus. Every node connects to it; the hairlines from the centre are live node connections.
      </p>
    </div>
  );
}

/** inspectKey opens the inspector for a scene object by its key. */
export function inspectKey(key: string, label: string): void {
  if (key === HUB_KEY) return inspect({ key: 'hub', title: 'Hub', content: <HubSummary /> });
  const i = key.indexOf(':');
  const kind = key.slice(0, i);
  const rest = key.slice(i + 1);
  switch (kind) {
    case 'node':
      return inspectNode(rest);
    case 'agent':
      return inspectAgent(key);
    case 'job':
      return inspect({ key: `job:${rest}`, title: `Sandbox · ${label}`, content: <JobSummary id={rest} /> });
    case 'channel':
      return inspect({ key: `channel:${rest}`, title: `Channel · ${rest}`, content: <ChannelSummary name={rest} /> });
  }
}
