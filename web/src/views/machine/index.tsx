import { useCallback, useEffect, useState, type FormEvent } from 'react';
import {
  Ago,
  radioGroupKeyDown,
  Badge,
  Button,
  Checkbox,
  ConfirmButton,
  EmptyState,
  fmtClock,
  Frame,
  Input,
  Readout,
  Skeleton,
  Table,
  type Column,
} from '../../ds';
import { getApi } from '../../app/api';
import type { LocalFreeze, LocalSharing, LocalWorker } from '../../app/api/local';
import { useFreeze } from '../../app/events/stores';
import { useConnection } from '../../app/session';
import type { Grant } from '../../gen/mycel/v1/grant_pb';
import { grantKind, STATE_LABEL, STATE_TONE } from '../approvals/model';
import { errText } from '../channels/model';
import { CAPS, DAYS, describe, draftOf, hubBase, problems, toJson, type SharingDraft, type WindowDraft } from './sharing';
import s from './Machine.module.css';

/** NodeStatus is GET /api/status on the daemon (internal/daemon/status.go). */
export interface NodeStatus {
  node: string;
  hub_url: string;
  state: string;
  connected: boolean;
  last_heartbeat_at: string | null;
  cert_not_after: string;
  caps: string[];
  local_agents: number;
  workers: number;
  jobs: number;
  sandbox_backends_available: string[];
  egress_proxy: string;
  grants: number;
  frozen: boolean;
  sharing_active: boolean;
  idle: string;
}

async function fetchStatus(): Promise<NodeStatus> {
  const res = await fetch('/api/status', { cache: 'no-store', credentials: 'omit', headers: { Accept: 'application/json' } });
  if (!res.ok) throw new Error(`status ${res.status}`);
  return (await res.json()) as NodeStatus;
}

function useLoad<T>(load: () => Promise<T>): { data: T | null; error: string | null; reload: () => Promise<void> } {
  const [data, setData] = useState<T | null>(null);
  const [error, setError] = useState<string | null>(null);
  const reload = useCallback(async () => {
    try {
      setData(await load());
      setError(null);
    } catch (err) {
      setError(errText(err));
    }
  }, [load]);
  useEffect(() => {
    void reload();
  }, [reload]);
  return { data, error, reload };
}

const local = () => {
  const l = getApi().local;
  if (!l) throw new Error('This machine is only available in the node UI.');
  return l;
};
const loadSharing = () => local().sharing();
const loadFreeze = () => local().freeze();
const loadGrants = () => local().grants();
const loadWorkers = () => local().workers();

/** This machine: what this node shares, its grants and workers, the freeze state and the daemon's status. Restrict only. */
export function Component() {
  const status = useLoad(fetchStatus);
  const sharing = useLoad(loadSharing);
  const freeze = useLoad(loadFreeze);
  const grants = useLoad(loadGrants);
  const workers = useLoad(loadWorkers);

  // Local state has no event stream: refresh when the tab comes back.
  useEffect(() => {
    const onFocus = () => {
      void status.reload();
      void freeze.reload();
      void grants.reload();
      void workers.reload();
    };
    window.addEventListener('focus', onFocus);
    return () => window.removeEventListener('focus', onFocus);
  }, [status.reload, freeze.reload, grants.reload, workers.reload]);

  return (
    <section className={s.page} aria-labelledby="machine-heading">
      <header className={s.head}>
        <h1 className={s.heading} id="machine-heading">
          This machine
        </h1>
        <span className={s.sub}>{status.data?.node ?? ''}</span>
        <Button
          size="sm"
          variant="ghost"
          onClick={() => {
            void status.reload();
            void sharing.reload();
            void freeze.reload();
            void grants.reload();
            void workers.reload();
          }}
        >
          Refresh
        </Button>
      </header>
      <p className={s.note}>
        Settings here are a local ceiling: they can only restrict what the hub allows, they hold while the hub is unreachable, and they sync
        back to the hub. Widening what this machine offers is done from the hub.
      </p>
      <StatusFrame status={status.data} error={status.error} />
      <FreezeFrame freeze={freeze.data} error={freeze.error} />
      <SharingFrame data={sharing.data} error={sharing.error} onSaved={() => void Promise.all([sharing.reload(), status.reload()])} />
      <GrantsFrame data={grants.data} error={grants.error} reload={grants.reload} />
      <WorkersFrame data={workers.data} error={workers.error} reload={workers.reload} />
    </section>
  );
}

function StatusFrame({ status, error }: { status: NodeStatus | null; error: string | null }) {
  const hub = useConnection((c) => c.hub);
  if (!status)
    return (
      <Frame title="Node" index="01">
        {error ? <EmptyState title="The daemon's status is not available">{error}</EmptyState> : <Skeleton rows={3} />}
      </Frame>
    );
  const connected = status.connected && hub === 'ok';
  return (
    <Frame
      title="Node"
      index="01"
      state={connected ? 'idle' : 'alert'}
      meta={
        <Badge tone={connected ? 'ok' : 'alert'} pulse={!connected}>
          {connected ? 'hub connected' : 'hub unreachable'}
        </Badge>
      }
    >
      <div className={s.readouts}>
        <Readout size="sm" label="State" value={status.state} />
        <Readout size="sm" label="Last heartbeat" value={status.last_heartbeat_at ? fmtClock(status.last_heartbeat_at) : 'never'} />
        <Readout size="sm" label="Jobs running" value={status.jobs} />
        <Readout size="sm" label="Workers" value={status.workers} />
        <Readout size="sm" label="Local agents" value={status.local_agents} />
        <Readout size="sm" label="Grants attached" value={status.grants} />
        <Readout
          size="sm"
          label="Sharing"
          value={status.sharing_active ? 'active now' : 'inactive now'}
          tone={status.sharing_active ? 'accent' : 'muted'}
        />
        <Readout size="sm" label="Certificate until" value={status.cert_not_after ? fmtClock(status.cert_not_after) : '—'} />
      </div>
      <dl className={s.kv}>
        <dt>Hub</dt>
        <dd>{status.hub_url || '—'}</dd>
        <dt>Backends</dt>
        <dd>
          {status.sandbox_backends_available.length
            ? status.sandbox_backends_available.join(', ')
            : 'none available: sandboxed jobs cannot run here'}
        </dd>
        <dt>Egress proxy</dt>
        <dd data-alert={status.egress_proxy.startsWith('unavailable') || undefined}>{status.egress_proxy || '—'}</dd>
        <dt>Capabilities</dt>
        <dd>{status.caps.length ? status.caps.join(', ') : 'none'}</dd>
      </dl>
    </Frame>
  );
}

function FreezeFrame({ freeze, error }: { freeze: LocalFreeze | null; error: string | null }) {
  const net = useFreeze((st) => st.status);
  useEffect(() => {
    if (!useFreeze.getState().loaded) void useFreeze.getState().load();
  }, []);
  return (
    <Frame title="Freeze" index="02" state={freeze?.frozen ? 'alert' : 'idle'}>
      {!freeze ? (
        error ? (
          <EmptyState title="Freeze state unavailable">{error}</EmptyState>
        ) : (
          <Skeleton rows={2} />
        )
      ) : (
        <div className={s.readouts}>
          <Readout
            size="sm"
            label="This node"
            value={freeze.frozen ? `frozen${freeze.scope ? ` (${freeze.scope})` : ''}` : 'running'}
            tone={freeze.frozen ? 'alert' : 'default'}
            sub={freeze.frozen ? freeze.reason || 'no reason given' : 'no freeze recorded in frozen.json'}
          />
          <Readout
            size="sm"
            label="Since"
            value={freeze.since ? fmtClock(freeze.since) : '—'}
            sub={freeze.since ? <Ago at={freeze.since} /> : undefined}
          />
          <Readout
            size="sm"
            label="Network (hub)"
            value={net ? (net.allFrozen ? 'frozen' : 'running') : 'unknown'}
            tone={net?.allFrozen ? 'alert' : 'default'}
            sub={net?.frozenChannels.length ? `${net.frozenChannels.length} channels frozen` : undefined}
          />
        </div>
      )}
      <p className={s.note}>
        A freeze pauses every sandbox and worker here and makes every broker deny. The node keeps it in frozen.json, so it holds across
        restarts and while the hub is unreachable; only the hub unfreezes.
      </p>
    </Frame>
  );
}

function SharingFrame({ data, error, onSaved }: { data: LocalSharing | null; error: string | null; onSaved: () => void }) {
  return (
    <Frame title="Sharing" index="03">
      {!data ? (
        error ? (
          <EmptyState title="Sharing settings unavailable">{error}</EmptyState>
        ) : (
          <Skeleton rows={4} />
        )
      ) : (
        <SharingForm data={data} onSaved={onSaved} />
      )}
    </Frame>
  );
}

export function SharingForm({ data, onSaved }: { data: LocalSharing; onSaved?: () => void }) {
  const [d, setD] = useState<SharingDraft>(() => draftOf(data.effective));
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [saved, setSaved] = useState(false);
  const base = hubBase(data.hub);
  const issues = problems(data.hub, d);
  const set = <K extends keyof SharingDraft>(k: K, v: SharingDraft[K]) => {
    setSaved(false);
    setD((x) => ({ ...x, [k]: v }));
  };
  const setWindow = (i: number, w: Partial<WindowDraft>) =>
    set(
      'schedule',
      d.schedule.map((x, j) => (j === i ? { ...x, ...w } : x)),
    );

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    if (issues.length) return;
    setBusy(true);
    setError(null);
    try {
      await local().setSharing(toJson(d));
      setSaved(true);
      onSaved?.();
    } catch (err) {
      setError(errText(err));
    } finally {
      setBusy(false);
    }
  };

  const hub = describe(data.hub);
  const loc = describe(data.local);
  const eff = describe(data.effective);
  const localSet = !!(data.local.when || data.local.offer || data.local.maxJobs || data.local.idleAfterMinutes || data.local.localFirst);
  const rows: [string, keyof typeof hub][] = [
    ['Offers', 'offers'],
    ['When', 'when'],
    ['Local first', 'localFirst'],
    ['Max jobs', 'maxJobs'],
    ['Idle after', 'idleAfter'],
  ];

  return (
    <div className={s.sharing}>
      <form className={s.form} onSubmit={(e) => void submit(e)} aria-label="Local sharing ceiling">
        <fieldset className={s.fieldset} disabled={busy}>
          <legend className={s.legend}>Offer</legend>
          <div className={s.checks}>
            {CAPS.map((c) => {
              const allowed = base.offer.includes(c);
              return (
                <Checkbox
                  key={c}
                  label={c}
                  checked={d.offer.includes(c)}
                  disabled={!allowed}
                  hint={allowed ? undefined : 'not offered by the hub'}
                  onCheckedChange={(v) => set('offer', v ? [...d.offer, c] : d.offer.filter((x) => x !== c))}
                />
              );
            })}
          </div>
        </fieldset>
        <fieldset className={s.fieldset} disabled={busy}>
          <legend className={s.legend}>When</legend>
          <div className={s.segment} role="radiogroup" aria-label="When to share" onKeyDown={radioGroupKeyDown}>
            {(['always', 'idle', 'schedule'] as const).map((w) => {
              const allowed = base.when === 'always' || w === base.when || (base.when === 'schedule' && w === 'idle');
              return (
                <button
                  key={w}
                  type="button"
                  role="radio"
                  aria-checked={d.when === w}
                  tabIndex={d.when === w ? 0 : -1}
                  className={s.segmentItem}
                  disabled={!allowed}
                  title={allowed ? undefined : 'Wider than the hub allows'}
                  onClick={() => set('when', w)}
                >
                  {w === 'idle' ? 'when idle' : w === 'schedule' ? 'on a schedule' : 'always'}
                </button>
              );
            })}
          </div>
          {d.when !== 'always' && (
            <div className={s.windows}>
              <p className={s.hint}>
                {d.when === 'idle'
                  ? 'Optional: only while idle and inside these windows.'
                  : 'Share only inside these windows (local time).'}
                {base.when === 'schedule' ? ' They must lie inside the hub windows.' : ''}
              </p>
              {d.schedule.map((w, i) => (
                <div key={i} className={s.window}>
                  <div className={s.days} role="group" aria-label={`Window ${i + 1} days`}>
                    {DAYS.map((day) => (
                      <button
                        key={day}
                        type="button"
                        className={s.day}
                        aria-pressed={w.days.includes(day)}
                        onClick={() =>
                          setWindow(i, {
                            days: w.days.includes(day)
                              ? w.days.filter((x) => x !== day)
                              : DAYS.filter((x) => x === day || w.days.includes(x)),
                          })
                        }
                      >
                        {day}
                      </button>
                    ))}
                  </div>
                  <Input
                    aria-label={`Window ${i + 1} from`}
                    mono
                    value={w.from}
                    onChange={(e) => setWindow(i, { from: e.target.value })}
                    placeholder="22:00"
                    className={s.time}
                  />
                  <span className={s.dash}>to</span>
                  <Input
                    aria-label={`Window ${i + 1} to`}
                    mono
                    value={w.to}
                    onChange={(e) => setWindow(i, { to: e.target.value })}
                    placeholder="07:00"
                    className={s.time}
                  />
                  <Button
                    size="sm"
                    variant="ghost"
                    onClick={() =>
                      set(
                        'schedule',
                        d.schedule.filter((_, j) => j !== i),
                      )
                    }
                    aria-label={`Remove window ${i + 1}`}
                  >
                    Remove
                  </Button>
                </div>
              ))}
              <Button
                size="sm"
                onClick={() => set('schedule', [...d.schedule, { days: ['mon', 'tue', 'wed', 'thu', 'fri'], from: '22:00', to: '07:00' }])}
              >
                Add window
              </Button>
            </div>
          )}
        </fieldset>
        <fieldset className={s.fieldset} disabled={busy}>
          <legend className={s.legend}>Limits</legend>
          <div className={s.limits}>
            <Input
              label="Max concurrent jobs"
              mono
              inputMode="numeric"
              value={String(d.maxJobs)}
              onChange={(e) => set('maxJobs', parseInt(e.target.value, 10) || 0)}
              hint={`1 to ${base.maxJobs}`}
            />
            <Input
              label="Idle after (minutes)"
              mono
              inputMode="numeric"
              value={String(d.idleAfter)}
              onChange={(e) => set('idleAfter', parseInt(e.target.value, 10) || 0)}
              hint={`at least ${base.idleAfter}`}
            />
          </div>
          <Checkbox
            label="Local first"
            checked={d.localFirst}
            disabled={base.localFirst}
            hint={base.localFirst ? 'on at the hub; it stays on' : 'pause remote sandboxes while you use this machine'}
            onCheckedChange={(v) => set('localFirst', v)}
          />
        </fieldset>
        {issues.length > 0 && (
          <ul className={s.issues} aria-label="Why this cannot be saved">
            {issues.map((i) => (
              <li key={i}>{i}</li>
            ))}
          </ul>
        )}
        {error && (
          <p className={s.error} role="alert">
            {error}
          </p>
        )}
        <div className={s.formActions}>
          {saved && <span className={s.saved}>Saved; the hub is told at its next report.</span>}
          <Button variant="ghost" onClick={() => setD(draftOf(data.effective))} disabled={busy}>
            Reset
          </Button>
          <Button type="submit" variant="primary" busy={busy} disabled={issues.length > 0}>
            Restrict
          </Button>
        </div>
      </form>
      <aside className={s.compare} aria-label="Hub, local and effective settings">
        <table className={s.compareTable}>
          <thead>
            <tr>
              <th />
              <th>Hub</th>
              <th>Local</th>
              <th>Effective</th>
            </tr>
          </thead>
          <tbody>
            {rows.map(([label, k]) => (
              <tr key={k}>
                <th scope="row">{label}</th>
                <td>{hub[k]}</td>
                <td>{localSet ? loc[k] : '—'}</td>
                <td>{eff[k]}</td>
              </tr>
            ))}
          </tbody>
        </table>
        <div className={s.idle}>
          <Readout
            size="sm"
            label="Idle"
            value={data.idle === 'unknown' ? 'unknown' : data.idle}
            tone={data.idle === 'unknown' ? 'muted' : 'default'}
          />
          {data.idle === 'unknown' && (
            <p className={s.hint}>
              The idle detector cannot tell here (no interactive session, or its tool is missing). "When idle" then never starts sharing,
              and local first pauses nothing.
            </p>
          )}
        </div>
        <p className={s.hint}>
          The hub column is the ceiling for this form; the effective column is what the brokers and the job runner enforce.
        </p>
      </aside>
    </div>
  );
}

function GrantsFrame({
  data,
  error,
  reload,
}: {
  data: { grants: Grant[]; endpoints: Record<string, string> } | null;
  error: string | null;
  reload: () => Promise<void>;
}) {
  const columns: Column<Grant>[] = [
    { key: 'kind', header: 'Kind', mono: true, width: '110px', cell: (g) => grantKind(g) },
    { key: 'subject', header: 'Subject', mono: true, cell: (g) => g.subject },
    { key: 'endpoint', header: 'Endpoint', mono: true, hideOnNarrow: true, cell: (g) => data?.endpoints[g.id] || '—' },
    { key: 'state', header: 'State', width: '116px', cell: (g) => <Badge tone={STATE_TONE[g.state]}>{STATE_LABEL[g.state]}</Badge> },
    {
      key: 'expires',
      header: 'Expires',
      mono: true,
      width: '100px',
      align: 'right',
      hideOnNarrow: true,
      cell: (g) => (g.expiresAt ? fmtClock(g.expiresAt) : '—'),
    },
    {
      key: 'revoke',
      header: '',
      width: '96px',
      align: 'right',
      cell: (g) => (
        <ConfirmButton
          size="sm"
          variant="warn"
          tone="alert"
          confirmTitle={`Revoke ${g.id}?`}
          confirmBody={`${grantKind(g)} for ${g.subject} detaches at once; the hub marks it revoked by this node.`}
          action="Revoke"
          describeError={errText}
          onConfirm={async () => {
            await local().revokeGrant(g.id);
            await reload();
          }}
        >
          Revoke
        </ConfirmButton>
      ),
    },
  ];
  return (
    <Frame title="Grants on this machine" index="04" flush>
      {!data ? (
        error ? (
          <EmptyState title="Grants unavailable">{error}</EmptyState>
        ) : (
          <Skeleton rows={2} />
        )
      ) : (
        <Table
          aria-label="Grants on this machine"
          columns={columns}
          rows={data.grants}
          rowKey={(g) => g.id}
          empty={
            <EmptyState title="No grants attached">
              Grants other agents hold on this machine (GPU, Ollama, Docker, mounts) appear here.
            </EmptyState>
          }
        />
      )}
    </Frame>
  );
}

function WorkersFrame({ data, error, reload }: { data: LocalWorker[] | null; error: string | null; reload: () => Promise<void> }) {
  const columns: Column<LocalWorker>[] = [
    { key: 'agent', header: 'Worker', mono: true, cell: (w) => w.agent_id },
    { key: 'harness', header: 'Harness', mono: true, hideOnNarrow: true, cell: (w) => w.harness },
    {
      key: 'state',
      header: 'State',
      width: '116px',
      cell: (w) => <Badge tone={w.state === 'running' ? 'ok' : w.state === 'paused' ? 'alert' : 'off'}>{w.state}</Badge>,
    },
    { key: 'pid', header: 'PID', mono: true, width: '70px', align: 'right', hideOnNarrow: true, cell: (w) => w.pid || '—' },
    {
      key: 'started',
      header: 'Started',
      mono: true,
      width: '90px',
      align: 'right',
      cell: (w) => (w.started_at ? <Ago at={w.started_at} /> : '—'),
    },
    {
      key: 'usage',
      header: 'Usage',
      mono: true,
      align: 'right',
      hideOnNarrow: true,
      cell: (w) => `${w.usage.tokens.toLocaleString('en-US')} tok · $${w.usage.usd.toFixed(2)}`,
    },
    {
      key: 'stop',
      header: '',
      width: '80px',
      align: 'right',
      cell: (w) =>
        w.exited_at ? (
          <span className={s.dim}>exit {w.exit_code}</span>
        ) : (
          <ConfirmButton
            size="sm"
            variant="warn"
            tone="alert"
            confirmTitle={`Stop ${w.agent_id}?`}
            confirmBody="The worker process is killed; its spawner sees it exit."
            action="Stop"
            describeError={errText}
            onConfirm={async () => {
              await local().stopWorker(w.agent_id);
              await reload();
            }}
          >
            Stop
          </ConfirmButton>
        ),
    },
  ];
  return (
    <Frame title="Workers" index="05" flush>
      {!data ? (
        error ? (
          <EmptyState title="Workers unavailable">{error}</EmptyState>
        ) : (
          <Skeleton rows={2} />
        )
      ) : (
        <Table
          aria-label="Local workers"
          columns={columns}
          rows={data}
          rowKey={(w) => w.agent_id}
          empty={<EmptyState title="No local workers">Agents spawned on this machine by a local orchestrator appear here.</EmptyState>}
        />
      )}
    </Frame>
  );
}
