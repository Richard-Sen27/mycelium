import { useMemo } from 'react';
import { useNavigate } from 'react-router';
import { Badge, Button, ConfirmButton, EmptyState, Fields, Gauge, Readout, Ago, Countdown, fmtBytes, gib } from '../../ds';
import { GrantState } from '../../gen/mycel/v1/grant_pb';
import { JobState, type Job } from '../../gen/mycel/v1/job_pb';
import type { Node } from '../../gen/mycel/v1/node_pb';
import { getApi } from '../../app/api';
import { useEntity, useEntityList } from '../../app/events/entities';
import { agentsStore, grantsStore, jobsStore, nodesStore } from '../../app/events/stores';
import { inspect, useInspector } from '../../app/shell/inspector';
import { useRights } from '../../app/session';
import { inspectAgent } from '../agents/AgentDetail';
import { inspectGrant } from '../grants/GrantDetail';
import { grantKind, stateName } from '../grants/grants';
import { CapsForm, SharingForm } from './NodeForms';
import { sharingSummary, useNodeSharing } from './sharing';
import { act, agentShort, canManageNode, errText, useUserNames } from './shared';
import s from './page.module.css';

export function inspectNode(name: string): void {
  inspect({ key: `node:${name}`, title: `Node · ${name}`, content: <NodeDetail name={name} /> });
}

export function isActiveJob(j: Job): boolean {
  return j.state === JobState.QUEUED || j.state === JobState.STARTING || j.state === JobState.RUNNING;
}

export function jobStateName(st: JobState): string {
  return JobState[st]?.toLowerCase().replace('_', ' ') ?? 'unknown';
}

/** NodeDetail is the inspector content for one node; it reads the stores by name and stays live. */
export function NodeDetail({ name }: { name: string }) {
  const rights = useRights();
  const node = useEntity(nodesStore, name);
  const nodes = useEntityList(nodesStore).items;
  const jobs = useEntityList(jobsStore).items;
  const grants = useEntityList(grantsStore).items;
  const agents = useEntityList(agentsStore).items;
  const sharing = useNodeSharing(name);
  const userName = useUserNames(rights);
  const navigate = useNavigate();

  const nodeJobs = useMemo(
    () =>
      jobs
        .filter((j) => j.node === name)
        .sort((a, b) => Number(isActiveJob(b)) - Number(isActiveJob(a)) || b.createdAt.localeCompare(a.createdAt)),
    [jobs, name],
  );
  const nodeGrants = useMemo(
    () => grants.filter((g) => g.node === name).sort((a, b) => a.state - b.state || b.createdAt.localeCompare(a.createdAt)),
    [grants, name],
  );
  const nodeAgents = useMemo(() => agents.filter((a) => a.nodeName === name), [agents, name]);

  if (!node) return <EmptyState title="This node is gone">It was removed, or it is not visible to you.</EmptyState>;

  const inv = node.inventory;
  const maxCpu = Math.max(1, ...nodes.map((n) => n.inventory?.cpuCount ?? 0));
  const maxMem = Math.max(1, ...nodes.map((n) => gib(n.inventory?.memBytes)));
  const runningJobs = nodeJobs.filter(isActiveJob).length;
  const maxJobs = sharing?.value?.effective?.maxJobs || 0;
  const gpus = inv?.gpus ?? [];
  const leasedGpus = new Set(
    nodeGrants
      .filter((g) => g.state === GrantState.ACTIVE && g.spec?.kind.case === 'gpu')
      .map((g) => (g.spec?.kind.case === 'gpu' ? g.spec.kind.value.device : -1)),
  );
  const manage = canManageNode(rights, node);

  return (
    <div className={s.detail}>
      <div className={s.readouts}>
        <Readout size="sm" label="State" value={node.online ? 'online' : 'offline'} tone={node.online ? 'accent' : 'muted'} />
        <Readout size="sm" label="CPU" value={inv?.cpuCount ?? null} unit="cores" />
        <Readout size="sm" label="Memory" value={inv?.memBytes ? Math.round(gib(inv.memBytes)) : null} unit="GiB" />
        <Readout size="sm" label="Jobs" value={runningJobs} sub={maxJobs ? `of ${maxJobs} remote` : undefined} />
      </div>

      <section className={s.section}>
        <h3 className={s.sectionTitle}>Capacity</h3>
        <div className={s.gauges}>
          <Gauge
            label="CPU vs the largest node"
            value={inv?.cpuCount ?? 0}
            max={maxCpu}
            warnAt={2}
            valueText={`${inv?.cpuCount ?? 0} / ${maxCpu}`}
          />
          <Gauge
            label="Memory vs the largest node"
            value={gib(inv?.memBytes)}
            max={maxMem}
            warnAt={2}
            valueText={`${fmtBytes(inv?.memBytes ?? 0)} / ${Math.round(maxMem)} GiB`}
          />
          {maxJobs > 0 && <Gauge label="Remote jobs" value={runningJobs} max={maxJobs} valueText={`${runningJobs} / ${maxJobs}`} />}
          {gpus.map((g) => (
            <Gauge
              key={g.index}
              label={`GPU ${g.index} · ${g.model || g.vendor}`}
              value={leasedGpus.has(g.index) ? 1 : 0}
              max={1}
              warnAt={2}
              ticks={false}
              valueText={`${fmtBytes(g.vramBytes)} · ${leasedGpus.has(g.index) ? 'leased' : 'free'}`}
            />
          ))}
        </div>
      </section>

      <section className={s.section}>
        <h3 className={s.sectionTitle}>Inventory</h3>
        <Fields
          items={[
            { label: 'Owner', value: userName(node.ownerId) },
            { label: 'Last seen', value: <Ago at={node.lastSeenAt} /> },
            { label: 'Host', value: inv?.hostname, mono: true },
            { label: 'System', value: inv ? `${inv.os} ${inv.osVersion} · ${inv.arch}` : '', mono: true },
            { label: 'CPU model', value: inv?.cpuModel },
            { label: 'Backends', value: inv?.sandboxBackends.join(', '), mono: true },
            { label: 'Docker', value: inv?.docker?.present ? `yes ${inv.docker.version}` : 'no', mono: true },
            { label: 'Ollama', value: inv?.ollama?.present ? `yes ${inv.ollama.version}` : 'no', mono: true },
            { label: 'Caps on', value: node.capsEnabled?.names.join(', '), mono: true },
            { label: 'Ceiling', value: node.capsCeiling?.names.join(', '), mono: true },
            { label: 'mycel', value: inv?.mycelVersion, mono: true },
            {
              label: 'Cert until',
              value: node.certNotAfter ? <Countdown to={node.certNotAfter} warnUnderMs={7 * 864e5} /> : '',
            },
          ]}
        />
      </section>

      <section className={s.section}>
        <h3 className={s.sectionTitle}>Sharing</h3>
        {sharing?.error && !sharing.value ? (
          <p className={s.note}>{sharing.error}</p>
        ) : (
          <Fields
            items={[
              { label: 'Effective', value: sharingSummary(sharing?.value?.effective), mono: true },
              { label: 'Hub setting', value: sharingSummary(sharing?.value?.hub), mono: true },
              { label: 'Local ceiling', value: sharingSummary(sharing?.value?.local), mono: true },
              { label: 'Local first', value: sharing?.value ? (sharing.value.effective?.localFirst ? 'yes' : 'no') : '' },
              {
                label: 'Schedule',
                value: (sharing?.value?.effective?.schedule ?? []).map((w) => `${w.days.join(',')} ${w.from}–${w.to}`).join('; '),
                mono: true,
                hidden: !sharing?.value?.effective?.schedule.length,
              },
            ]}
          />
        )}
      </section>

      <section className={s.section}>
        <h3 className={s.sectionTitle}>
          Grants <span className={s.count}>{nodeGrants.length}</span>
        </h3>
        {nodeGrants.length === 0 ? (
          <p className={s.note}>No grants on this node.</p>
        ) : (
          <ul className={s.rows}>
            {nodeGrants.slice(0, 20).map((g) => (
              <li key={g.id} className={s.rowItem}>
                <button type="button" className={s.rowButton} onClick={() => inspectGrant(g.id)}>
                  {grantKind(g)} · {agentShort(g.subject)}
                </button>
                <span className={s.rowMeta}>{stateName(g.state)}</span>
              </li>
            ))}
          </ul>
        )}
      </section>

      <section className={s.section}>
        <h3 className={s.sectionTitle}>
          Jobs <span className={s.count}>{nodeJobs.length}</span>
        </h3>
        {nodeJobs.length === 0 ? (
          <p className={s.note}>No jobs ran here.</p>
        ) : (
          <ul className={s.rows}>
            {nodeJobs.slice(0, 12).map((j) => (
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

      <section className={s.section}>
        <h3 className={s.sectionTitle}>
          Agents <span className={s.count}>{nodeAgents.length}</span>
        </h3>
        {nodeAgents.length === 0 ? (
          <p className={s.note}>No agents registered from this node.</p>
        ) : (
          <ul className={s.rows}>
            {nodeAgents.slice(0, 20).map((a) => (
              <li key={a.id} className={s.rowItem}>
                <button type="button" className={s.rowButton} onClick={() => inspectAgent(a.id)}>
                  {a.name}
                </button>
                <span className={s.rowMeta}>{a.state}</span>
              </li>
            ))}
          </ul>
        )}
      </section>

      {manage && <NodeActions node={node} />}
    </div>
  );
}

function NodeActions({ node }: { node: Node }) {
  const close = useInspector((st) => st.close);
  return (
    <section className={s.section}>
      <h3 className={s.sectionTitle}>Manage</h3>
      <div className={s.actions}>
        <Button
          size="sm"
          onClick={() => inspect({ key: `node:${node.name}:caps`, title: `Caps · ${node.name}`, content: <CapsForm name={node.name} /> })}
        >
          Set caps
        </Button>
        <Button
          size="sm"
          onClick={() =>
            inspect({ key: `node:${node.name}:sharing`, title: `Sharing · ${node.name}`, content: <SharingForm name={node.name} /> })
          }
        >
          Set sharing
        </Button>
        <ConfirmButton
          size="sm"
          variant="warn"
          tone="alert"
          describeError={errText}
          confirmTitle={`Remove ${node.name}?`}
          confirmBody="Its certificate is revoked, it is disconnected and its sandboxes are torn down. It must join again with a new token."
          action="Remove node"
          onConfirm={async () => {
            await act(() => getApi().rpc.nodes.removeNode({ name: node.name }), `Removed ${node.name}`);
            close();
          }}
        >
          Remove
        </ConfirmButton>
      </div>
      {!node.online && <Badge tone="off">offline: changes apply when it reconnects</Badge>}
    </section>
  );
}
