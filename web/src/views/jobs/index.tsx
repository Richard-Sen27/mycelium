import { useEffect, useMemo, useState } from 'react';
import { Link, useNavigate, useParams, useSearchParams } from 'react-router';
import { Ago, Badge, Button, Elapsed, EmptyState, Frame, Input, Select, Skeleton, Table, type Column } from '../../ds';
import { useEntityList } from '../../app/events/entities';
import { jobsStore } from '../../app/events/stores';
import { useRights } from '../../app/session';
import { JobState, type Job, type JobSpec } from '../../gen/mycel/v1/job_pb';
import { parseSender } from '../channels/model';
import { JobDetail, type JobTab } from './JobDetail';
import { commandLine, isActive, isFinished, JOB_STATE, JOB_TONE } from './model';
import { NewJob } from './NewJob';
import s from './Jobs.module.css';

const newestFirst = (a: Job, b: Job) => b.createdAt.localeCompare(a.createdAt);
const TABS: JobTab[] = ['overview', 'logs', 'artifacts', 'egress', 'events', 'terminal'];

type StateFilter = 'all' | 'active' | 'failed' | 'finished';
const STATE_FILTERS: { value: StateFilter; label: string }[] = [
  { value: 'all', label: 'all states' },
  { value: 'active', label: 'active' },
  { value: 'failed', label: 'failed or timed out' },
  { value: 'finished', label: 'finished' },
];

function stateMatch(j: Job, f: StateFilter): boolean {
  switch (f) {
    case 'active':
      return isActive(j);
    case 'failed':
      return j.state === JobState.FAILED || j.state === JobState.TIMED_OUT;
    case 'finished':
      return isFinished(j);
    default:
      return true;
  }
}

/** Jobs: the table with filters (`/jobs`) and the sandbox inspector for one job (`/jobs/<id>`). */
export function Component() {
  const params = useParams();
  const id = params['*'] ?? '';
  const navigate = useNavigate();
  const [search, setSearch] = useSearchParams();
  const rights = useRights();
  const [rerun, setRerun] = useState<JobSpec | undefined>(undefined);
  const newOpen = search.get('new') === '1';
  const canRun = rights.mode === 'hub' && !rights.readOnly;
  const tabParam = search.get('tab') as JobTab | null;
  const tab: JobTab = tabParam && TABS.includes(tabParam) ? tabParam : 'overview';

  const setNewOpen = (o: boolean) => {
    const next = new URLSearchParams(search);
    if (o) next.set('new', '1');
    else next.delete('new');
    if (!o) setRerun(undefined);
    setSearch(next, { replace: true });
  };

  return (
    <div className={s.page}>
      {id ? (
        <>
          <div className={s.crumbs}>
            <Link to="/jobs">Jobs</Link>
            <span aria-hidden>/</span>
            <span className={s.mono}>{id}</span>
          </div>
          <JobDetail
            id={id}
            tab={tab}
            onTab={(t) => {
              const next = new URLSearchParams(search);
              if (t === 'overview') next.delete('tab');
              else next.set('tab', t);
              setSearch(next, { replace: true });
            }}
            onRunAgain={
              canRun
                ? (j) => {
                    setRerun(j.spec);
                    setNewOpen(true);
                  }
                : undefined
            }
          />
        </>
      ) : (
        <JobsTable canRun={canRun} onNew={() => setNewOpen(true)} />
      )}
      {canRun && <NewJob open={newOpen} onOpenChange={setNewOpen} from={rerun} onStarted={(jid) => navigate(`/jobs/${jid}?tab=logs`)} />}
    </div>
  );
}

const columns: Column<Job>[] = [
  { key: 'id', header: 'Job', mono: true, width: '150px', cell: (j) => <span title={j.id}>{j.id.slice(0, 14)}</span> },
  {
    key: 'state',
    header: 'State',
    width: '124px',
    cell: (j) => (
      <Badge tone={JOB_TONE[j.state]} pulse={j.state === JobState.RUNNING}>
        {JOB_STATE[j.state]}
      </Badge>
    ),
  },
  {
    key: 'cmd',
    header: 'Command',
    mono: true,
    cell: (j) => <span className={s.cmdCell}>{j.spec?.name || commandLine(j.spec?.command ?? [])}</span>,
  },
  { key: 'node', header: 'Node', mono: true, width: '110px', cell: (j) => j.node || '—' },
  { key: 'profile', header: 'Profile', mono: true, width: '90px', hideOnNarrow: true, cell: (j) => j.spec?.profile || '—' },
  { key: 'channel', header: 'Channel', mono: true, hideOnNarrow: true, cell: (j) => j.spec?.channel || '—' },
  { key: 'by', header: 'Submitter', mono: true, hideOnNarrow: true, cell: (j) => parseSender(j.submittedBy).name },
  {
    key: 'started',
    header: 'Started',
    mono: true,
    width: '90px',
    align: 'right',
    hideOnNarrow: true,
    cell: (j) => <Ago at={j.stats?.startedAt || j.createdAt} />,
  },
  {
    key: 'dur',
    header: 'Duration',
    mono: true,
    width: '84px',
    align: 'right',
    cell: (j) => (j.stats?.startedAt ? <Elapsed from={j.stats.startedAt} to={j.stats.finishedAt || undefined} /> : '—'),
  },
  {
    key: 'exit',
    header: 'Exit',
    mono: true,
    width: '52px',
    align: 'right',
    cell: (j) => (isFinished(j) ? <span data-alert={j.exitCode !== 0 || undefined}>{j.exitCode}</span> : '—'),
  },
];

function JobsTable({ canRun, onNew }: { canRun: boolean; onNew: () => void }) {
  const navigate = useNavigate();
  const { items, loaded, error, stale } = useEntityList(jobsStore, newestFirst);
  const [state, setState] = useState<StateFilter>('all');
  const [node, setNode] = useState('');
  const [q, setQ] = useState('');
  const nodes = useMemo(() => [...new Set(items.map((j) => j.node).filter(Boolean))].sort(), [items]);
  const rows = useMemo(() => {
    const ql = q.trim().toLowerCase();
    return items.filter(
      (j) =>
        stateMatch(j, state) &&
        (!node || j.node === node) &&
        (!ql ||
          [j.id, j.spec?.name, j.spec?.profile, j.spec?.channel, j.submittedBy, ...(j.spec?.command ?? [])].some((x) =>
            x?.toLowerCase().includes(ql),
          )),
    );
  }, [items, state, node, q]);
  const running = items.filter(isActive).length;

  useEffect(() => {
    if (node && !nodes.includes(node)) setNode('');
  }, [node, nodes]);

  return (
    <Frame
      title="Jobs"
      index="04"
      flush
      meta={
        <>
          {stale && <Badge tone="off">stale</Badge>}
          <span>{running} active</span>
          <span>{rows.length} shown</span>
        </>
      }
    >
      <div className={s.filters}>
        <Input
          aria-label="Filter jobs"
          placeholder="Filter by id, command, channel, submitter"
          mono
          value={q}
          onChange={(e) => setQ(e.target.value)}
          className={s.grow}
        />
        <Select aria-label="State" options={STATE_FILTERS} value={state} onValueChange={setState} className={s.filterSelect} />
        <Select
          aria-label="Node"
          options={[{ value: '__all__', label: 'all nodes' }, ...nodes.map((n) => ({ value: n, label: n }))]}
          value={node || '__all__'}
          onValueChange={(v) => setNode(v === '__all__' ? '' : v)}
          className={s.filterSelect}
        />
        {canRun && (
          <Button variant="primary" onClick={onNew}>
            Run a job
          </Button>
        )}
      </div>
      {!loaded ? (
        error ? (
          <EmptyState title="Could not load jobs">{error}</EmptyState>
        ) : (
          <Skeleton rows={6} />
        )
      ) : (
        <Table
          aria-label="Jobs"
          columns={columns}
          rows={rows}
          rowKey={(j) => j.id}
          onActivate={(j) => navigate(`/jobs/${j.id}`)}
          rowTone={(j) =>
            j.state === JobState.FAILED || j.state === JobState.TIMED_OUT ? 'alert' : j.state === JobState.RUNNING ? 'ok' : undefined
          }
          empty={
            <EmptyState title={items.length ? 'No job matches the filters' : 'No jobs yet'}>
              {items.length ? 'Clear a filter to see more.' : 'Jobs started by agents or with `mycel job run` appear here.'}
            </EmptyState>
          }
        />
      )}
    </Frame>
  );
}
