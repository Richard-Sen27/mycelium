import { useEffect, useMemo, useState } from 'react';
import { Link } from 'react-router';
import {
  Ago,
  Badge,
  ConfirmButton,
  fmtElapsed,
  EmptyState,
  fmtBytes,
  fmtClock,
  fmtDuration,
  Frame,
  Gauge,
  parseGoDuration,
  Readout,
  Skeleton,
  Table,
  Tabs,
  Timeline,
  useNow,
  type Column,
} from '../../ds';
import { getApi } from '../../app/api';
import { useEntity, useEntityList } from '../../app/events/entities';
import { grantsStore, jobsStore } from '../../app/events/stores';
import { useRights } from '../../app/session';
import { GrantState, type Grant } from '../../gen/mycel/v1/grant_pb';
import { JobState, type Artifact, type Job, type Profile } from '../../gen/mycel/v1/job_pb';
import type { AuditEntry } from '../../gen/mycel/v1/ui_pb';
import { STATE_LABEL, STATE_TONE, grantKind } from '../approvals/model';
import { channelPath, errText, parseSender } from '../channels/model';
import { isDenied, parseDetails, useAuditQuery } from '../traces/audit';
import { ArtifactLink } from './artifacts';
import { JobLogs } from './JobLogs';
import { commandLine, endpointKind, stripUserinfo, isActive, isFinished, JOB_STATE, JOB_TONE } from './model';
import s from './Jobs.module.css';

export type JobTab = 'overview' | 'logs' | 'artifacts' | 'egress' | 'events' | 'terminal';

let profilesCache: Promise<Profile[]> | null = null;
export function loadProfiles(): Promise<Profile[]> {
  profilesCache ??= getApi()
    .rpc.jobs.listProfiles({})
    .then((r) => r.profiles)
    .catch((err: unknown) => {
      profilesCache = null;
      throw err;
    });
  return profilesCache;
}

export function useProfiles(): Profile[] | null {
  const [p, setP] = useState<Profile[] | null>(null);
  useEffect(() => {
    let live = true;
    loadProfiles()
      .then((x) => live && setP(x))
      .catch(() => live && setP([]));
    return () => {
      live = false;
    };
  }, []);
  return p;
}

/** JobDetail is the sandbox inspector for one job. */
export function JobDetail({
  id,
  onRunAgain,
  tab,
  onTab,
}: {
  id: string;
  onRunAgain?: (j: Job) => void;
  tab?: JobTab;
  onTab?: (t: JobTab) => void;
}) {
  const rights = useRights();
  const job = useEntity(jobsStore, id);
  const loaded = jobsStore.use((st) => st.loaded);
  const [fetched, setFetched] = useState<Job | null>(null);
  const [error, setError] = useState<string | null>(null);

  // Older jobs may be outside the store's window: fetch it directly.
  useEffect(() => {
    if (job || !loaded) return;
    getApi()
      .rpc.jobs.getJob({ id })
      .then((r) => (r.job ? setFetched(r.job) : setError(`No job ${id}.`)))
      .catch((err: unknown) => setError(errText(err)));
  }, [id, job, loaded]);

  const j = job ?? fetched;
  if (!j) return <Frame title={id}>{error ? <EmptyState title="Job not found">{error}</EmptyState> : <Skeleton rows={5} />}</Frame>;

  const canWrite = rights.mode === 'hub' && !rights.readOnly;
  return (
    <div className={s.detail}>
      <Frame
        title={j.id}
        index={j.spec?.profile}
        state={j.state === JobState.FAILED || j.state === JobState.TIMED_OUT ? 'alert' : isActive(j) ? 'active' : 'idle'}
        aria-label={`Job ${j.id}`}
        meta={
          <>
            <Badge tone={JOB_TONE[j.state]} pulse={isActive(j)}>
              {JOB_STATE[j.state]}
            </Badge>
          </>
        }
      >
        <div className={s.summary}>
          <code className={s.command}>{commandLine(j.spec?.command ?? []) || '—'}</code>
          <div className={s.readouts}>
            <Readout size="sm" label="Node" value={j.node || 'unscheduled'} sub={j.backend || undefined} />
            <Readout
              size="sm"
              label="Started"
              value={j.stats?.startedAt ? fmtClock(j.stats.startedAt) : '—'}
              sub={<Ago at={j.createdAt} />}
            />
            <DurationReadout from={j.stats?.startedAt} to={j.stats?.finishedAt} />
            <Readout
              size="sm"
              label="Exit"
              value={isFinished(j) ? String(j.exitCode) : '—'}
              tone={isFinished(j) && j.exitCode !== 0 ? 'alert' : 'default'}
            />
            <Readout size="sm" label="Submitter" value={parseSender(j.submittedBy).name} sub={j.submittedBy.split(':')[0]} />
          </div>
          {j.error && (
            <p className={s.error} role="alert">
              {j.error}
            </p>
          )}
          {canWrite && (
            <div className={s.jobActions}>
              {onRunAgain && (
                <button type="button" className={s.textButton} onClick={() => onRunAgain(j)}>
                  Run again
                </button>
              )}
              {isActive(j) && (
                <ConfirmButton
                  size="sm"
                  variant="warn"
                  tone="alert"
                  confirmTitle={`Cancel ${j.id}?`}
                  confirmBody="The sandbox is stopped and the job ends as cancelled. Collected outputs are kept."
                  action="Cancel job"
                  describeError={errText}
                  onConfirm={async () => {
                    await getApi().rpc.jobs.cancelJob({ id: j.id });
                  }}
                >
                  Cancel
                </ConfirmButton>
              )}
            </div>
          )}
        </div>
      </Frame>
      <Frame flush className={s.tabsFrame} bodyClassName={s.tabsBody}>
        <Tabs<JobTab>
          aria-label="Job"
          value={tab}
          defaultValue={tab ? undefined : 'overview'}
          onValueChange={onTab}
          items={[
            { value: 'overview', label: 'Overview', panel: <Overview j={j} /> },
            { value: 'logs', label: 'Logs', panel: <JobLogs job={j} /> },
            { value: 'artifacts', label: 'Artifacts', count: j.artifacts.length || undefined, panel: <Artifacts j={j} /> },
            { value: 'egress', label: 'Egress', panel: <Egress j={j} /> },
            { value: 'events', label: 'Events', panel: <Events j={j} /> },
            { value: 'terminal', label: 'Terminal', panel: <Terminal /> },
          ]}
        />
      </Frame>
    </div>
  );
}

/** DurationReadout ticks a job's run time from the shared clock. */
function DurationReadout({ from, to }: { from?: string; to?: string }) {
  const now = useNow();
  const a = Date.parse(from ?? '');
  const b = to ? Date.parse(to) : now;
  return <Readout size="sm" label="Duration" value={Number.isFinite(a) ? fmtElapsed(b - a) : '—'} />;
}

const byCreated = (a: Grant, b: Grant) => b.createdAt.localeCompare(a.createdAt);

function Overview({ j }: { j: Job }) {
  const profiles = useProfiles();
  const profile = profiles?.find((p) => p.name === j.spec?.profile);
  const { items: grants } = useEntityList(grantsStore, byCreated);
  const held = grants.filter((g) => g.subject === `job:${j.id}`);
  const now = useNow();
  const lim = profile?.limits;
  const wallMs = parseGoDuration(j.spec?.timeout || lim?.wallClock);
  const started = Date.parse(j.stats?.startedAt ?? '');
  const ended = j.stats?.finishedAt ? Date.parse(j.stats.finishedAt) : now;
  const elapsed = Number.isFinite(started) ? ended - started : 0;
  const mem = Number(j.stats?.maxMemoryBytes ?? 0n);
  const env = Object.keys(j.spec?.env ?? {}).sort();
  const ws = j.spec?.workspace?.source;

  return (
    <div className={s.overview}>
      <section className={s.section} aria-label="Limits">
        <h3 className={s.h3}>
          Limits{' '}
          {profile ? (
            <span className={s.dim}>
              profile {profile.name} v{profile.version}
            </span>
          ) : null}
        </h3>
        <div className={s.gauges}>
          <Gauge
            label="Wall clock"
            value={elapsed}
            max={Number.isFinite(wallMs) ? wallMs : Math.max(elapsed, 1)}
            valueText={`${fmtDuration(elapsed)} / ${Number.isFinite(wallMs) ? fmtDuration(wallMs) : '∞'}`}
          />
          <Gauge
            label="Memory peak"
            value={mem}
            max={Number(lim?.memoryBytes ?? 0n) || Math.max(mem, 1)}
            valueText={`${fmtBytes(mem)} / ${lim && Number(lim.memoryBytes) ? fmtBytes(lim.memoryBytes) : 'profile default'}`}
          />
        </div>
        <div className={s.readouts}>
          <Readout size="sm" label="CPUs" value={lim?.cpus || 'default'} />
          <Readout size="sm" label="CPU time" value={fmtDuration(Number(j.stats?.cpuSecondsMilli ?? 0n))} />
          <Readout size="sm" label="Disk" value={lim && Number(lim.diskBytes) ? fmtBytes(lim.diskBytes) : 'default'} />
          <Readout size="sm" label="Processes" value={lim?.maxProcesses || 'default'} />
          <Readout size="sm" label="Egress" value={fmtBytes(j.stats?.egressBytes ?? 0n)} />
          <Readout size="sm" label="Log" value={fmtBytes(j.logBytes)} />
        </div>
      </section>
      <section className={s.section} aria-label="Environment">
        <h3 className={s.h3}>Environment</h3>
        <dl className={s.kv}>
          <dt>Workspace</dt>
          <dd>
            {ws?.case === 'git' ? (
              `${stripUserinfo(ws.value.url)}${ws.value.ref ? ` @ ${ws.value.ref}` : ''}`
            ) : ws?.case === 'artifact' ? (
              <ArtifactLink id={ws.value} />
            ) : (
              'empty'
            )}
          </dd>
          <dt>Channel</dt>
          <dd>{j.spec?.channel ? <Link to={channelPath(j.spec.channel)}>{j.spec.channel}</Link> : '—'}</dd>
          <dt>Sandbox</dt>
          <dd>{j.sandboxId || '—'}</dd>
          <dt>Env keys</dt>
          <dd>{env.length ? env.join('  ') : 'none'}</dd>
          <dt>Inputs</dt>
          <dd>{j.spec?.inputs.length ? j.spec.inputs.map((i) => `${i.artifact}${i.path ? ` → ${i.path}` : ''}`).join(', ') : 'none'}</dd>
          <dt>Outputs</dt>
          <dd>{j.spec?.outputs.length ? j.spec.outputs.join(', ') : 'none'}</dd>
          <dt>Egress allow</dt>
          <dd>{profile?.egress?.allow.length ? profile.egress.allow.join(', ') : 'none (default deny)'}</dd>
        </dl>
      </section>
      <section className={s.section} aria-label="Grants held">
        <h3 className={s.h3}>Grants held</h3>
        {held.length === 0 ? (
          <p className={s.dim}>None. Brokers (GPU, Ollama, host Docker) reach a job only through a grant requested before it starts.</p>
        ) : (
          <Table
            aria-label="Grants held by this job"
            columns={grantColumns}
            rows={held}
            rowKey={(g) => g.id}
            rowTone={(g) => (g.state === GrantState.PENDING ? 'busy' : undefined)}
          />
        )}
      </section>
    </div>
  );
}

const grantColumns: Column<Grant>[] = [
  { key: 'kind', header: 'Kind', mono: true, width: '110px', cell: (g) => grantKind(g) },
  { key: 'endpoint', header: 'Endpoint', mono: true, cell: (g) => endpointKind(g) },
  { key: 'state', header: 'State', width: '116px', cell: (g) => <Badge tone={STATE_TONE[g.state]}>{STATE_LABEL[g.state]}</Badge> },
  {
    key: 'expires',
    header: 'Expires',
    mono: true,
    align: 'right',
    hideOnNarrow: true,
    cell: (g) => (g.expiresAt ? fmtClock(g.expiresAt) : '—'),
  },
];

const artifactColumns: Column<Artifact>[] = [
  { key: 'name', header: 'Name', cell: (a) => <ArtifactLink id={a.id} name={a.name} /> },
  { key: 'size', header: 'Size', mono: true, align: 'right', width: '96px', cell: (a) => fmtBytes(a.size) },
  { key: 'type', header: 'Type', mono: true, hideOnNarrow: true, cell: (a) => a.mediaType || '—' },
  { key: 'sha', header: 'SHA-256', mono: true, hideOnNarrow: true, cell: (a) => <span title={a.sha256}>{a.sha256.slice(0, 12)}</span> },
  { key: 'id', header: 'Id', mono: true, hideOnNarrow: true, cell: (a) => a.id },
];

function Artifacts({ j }: { j: Job }) {
  const [rows, setRows] = useState<Artifact[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const key = j.artifacts.join(',');
  useEffect(() => {
    getApi()
      .rpc.artifacts.listArtifacts({ jobId: j.id, limit: 500 })
      .then((r) => setRows(r.artifacts))
      .catch((err: unknown) => {
        setError(errText(err));
        setRows([]);
      });
  }, [j.id, key]);
  if (rows === null) return <Skeleton rows={3} />;
  if (error) return <EmptyState title="Could not list artifacts">{error}</EmptyState>;
  return (
    <Table
      aria-label="Artifacts"
      columns={artifactColumns}
      rows={rows}
      rowKey={(a) => a.id}
      empty={
        <EmptyState title="No artifacts">
          {isFinished(j)
            ? j.spec?.outputs.length
              ? `Nothing matched the output globs ${j.spec.outputs.join(', ')}.`
              : 'The job declared no outputs.'
            : 'Outputs are collected when the job finishes.'}
        </EmptyState>
      }
    />
  );
}

interface EgressRow {
  e: AuditEntry;
  host: string;
  port: string;
  allowed: boolean;
  bytes: number | null;
}

/** egressRows picks the audit entries that describe connections and reads host, port and verdict from their details. */
export function egressRows(entries: AuditEntry[]): EgressRow[] {
  const out: EgressRow[] = [];
  for (const e of entries) {
    const d = parseDetails(e);
    const host = typeof d.host === 'string' ? d.host : '';
    if (!host && !e.action.includes('egress')) continue;
    const allowed = typeof d.allowed === 'boolean' ? d.allowed : !isDenied(e);
    const bytes = typeof d.bytes === 'number' ? d.bytes : null;
    out.push({ e, host: host || e.resource, port: d.port != null ? String(d.port) : '', allowed, bytes });
  }
  return out;
}

const egressColumns: Column<EgressRow>[] = [
  { key: 'at', header: 'Time', mono: true, width: '110px', cell: (r) => fmtClock(r.e.at) },
  { key: 'host', header: 'Host', mono: true, cell: (r) => r.host },
  { key: 'port', header: 'Port', mono: true, width: '70px', align: 'right', cell: (r) => r.port || '—' },
  {
    key: 'verdict',
    header: 'Verdict',
    width: '116px',
    cell: (r) => <Badge tone={r.allowed ? 'ok' : 'alert'}>{r.allowed ? 'allowed' : 'denied'}</Badge>,
  },
  {
    key: 'bytes',
    header: 'Bytes',
    mono: true,
    align: 'right',
    width: '90px',
    hideOnNarrow: true,
    cell: (r) => (r.bytes == null ? '—' : fmtBytes(r.bytes)),
  },
];

function Egress({ j }: { j: Job }) {
  const { entries, loading, error } = useAuditQuery({ resource: `job:${j.id}` }, { limit: 500 });
  const rows = useMemo(() => egressRows(entries), [entries]);
  if (loading && entries.length === 0) return <Skeleton rows={3} />;
  if (error) return <EmptyState title="Could not read the audit log">{error}</EmptyState>;
  return (
    <div className={s.egress}>
      <div className={s.readouts}>
        <Readout size="sm" label="Egress total" value={fmtBytes(j.stats?.egressBytes ?? 0n)} sub="reported by the node" />
        <Readout size="sm" label="Connections recorded" value={rows.length} />
        <Readout
          size="sm"
          label="Denied"
          value={rows.filter((r) => !r.allowed).length}
          tone={rows.some((r) => !r.allowed) ? 'alert' : 'default'}
        />
      </div>
      <Table
        aria-label="Egress"
        columns={egressColumns}
        rows={rows}
        rowKey={(r) => r.e.id}
        rowTone={(r) => (r.allowed ? undefined : 'alert')}
        empty={
          <EmptyState title="No per-connection records for this job">
            Egress is denied by default. In v1 the node logs every connection its egress proxy allows or refuses locally and reports only
            the byte total to the hub (above); per-connection records appear here once nodes report them.
          </EmptyState>
        }
      />
    </div>
  );
}

function Events({ j }: { j: Job }) {
  const { entries, loading, error } = useAuditQuery({ resource: `job:${j.id}` }, { limit: 500 });
  const steps = useMemo(() => {
    const asc = [...entries].reverse();
    return asc.map((e, i) => {
      const prev = i > 0 ? Date.parse(asc[i - 1].at) : NaN;
      const gap = Date.parse(e.at) - prev;
      return {
        key: e.id,
        time: fmtClock(e.at),
        gap: Number.isFinite(gap) ? `+${fmtDuration(gap)}` : undefined,
        actor: parseSender(e.actor).name,
        title: `${e.action}${e.outcome && e.outcome !== 'allowed' ? ` · ${e.outcome}` : ''}`,
        tone: isDenied(e) ? ('alert' as const) : ('default' as const),
      };
    });
  }, [entries]);
  if (loading && entries.length === 0) return <Skeleton rows={4} />;
  if (error) return <EmptyState title="Could not read the audit log">{error}</EmptyState>;
  if (steps.length === 0) return <EmptyState title="No audit events for this job yet" />;
  return <Timeline steps={steps} aria-label="Job events" />;
}

function Terminal() {
  return (
    <EmptyState title="No live terminal in v1">
      Attaching a terminal to a running sandbox needs the PTY adapter, which comes after v1. Use the Logs tab for the job's output, or run
      an interactive command on your own machine with `mycel job run`.
    </EmptyState>
  );
}
