import { useMemo, useState } from 'react';
import { Badge, Button, Checkbox, EmptyState, fmtClock, Frame, Input, Skeleton, Table, type Column } from '../../ds';
import { useRights } from '../../app/session';
import type { AuditEntry } from '../../gen/mycel/v1/ui_pb';
import { isDenied, useAuditQuery, type AuditFilter } from './audit';
import s from './Traces.module.css';

/** toIso turns a datetime-local value into RFC 3339 (empty stays empty). */
function toIso(v: string): string {
  if (!v) return '';
  const t = new Date(v);
  return Number.isNaN(t.getTime()) ? '' : t.toISOString();
}

/** AuditExplorer: ListAudit with filters and the live tail. Admins see every entry; members their own. */
export function AuditExplorer({ onTrace }: { onTrace: (trace: string) => void }) {
  const rights = useRights();
  const [actor, setActor] = useState('');
  const [action, setAction] = useState('');
  const [resource, setResource] = useState('');
  const [since, setSince] = useState('');
  const [until, setUntil] = useState('');
  const [live, setLive] = useState(true);
  const [applied, setApplied] = useState<AuditFilter>({});
  const filter = useMemo<AuditFilter>(() => ({ ...applied, action: action.trim() || undefined }), [applied, action]);
  const { entries, loaded, loading, error, more, loadMore } = useAuditQuery(filter, { live, limit: 200 });

  const apply = (e: React.FormEvent) => {
    e.preventDefault();
    setApplied({
      actor: actor.trim() || undefined,
      resource: resource.trim() || undefined,
      since: toIso(since) || undefined,
      until: toIso(until) || undefined,
    });
  };

  const columns: Column<AuditEntry>[] = [
    { key: 'at', header: 'Time', mono: true, width: '150px', cell: (e) => fmtClock(e.at) },
    { key: 'actor', header: 'Actor', mono: true, cell: (e) => e.actor },
    { key: 'action', header: 'Action', mono: true, cell: (e) => e.action },
    { key: 'resource', header: 'Resource', mono: true, hideOnNarrow: true, cell: (e) => e.resource || '—' },
    {
      key: 'outcome',
      header: 'Outcome',
      width: '96px',
      cell: (e) => <Badge tone={isDenied(e) ? 'alert' : 'idle'}>{e.outcome || '—'}</Badge>,
    },
    {
      key: 'trace',
      header: 'Trace',
      mono: true,
      width: '100px',
      hideOnNarrow: true,
      cell: (e) =>
        e.traceId ? (
          <button
            type="button"
            className={s.traceLink}
            onClick={(ev) => {
              ev.stopPropagation();
              onTrace(e.traceId);
            }}
            title={`Replay ${e.traceId}`}
          >
            {e.traceId.slice(0, 10)}
          </button>
        ) : (
          '—'
        ),
    },
    {
      key: 'details',
      header: 'Details',
      mono: true,
      hideOnNarrow: true,
      cell: (e) => (
        <span className={s.detailsCell} title={e.detailsJson}>
          {e.detailsJson || '—'}
        </span>
      ),
    },
  ];

  return (
    <Frame
      title="Audit"
      index={String(entries.length).padStart(2, '0')}
      flush
      meta={
        <span>
          {rights.isAdmin ? 'all entries' : 'your entries, your nodes and agents'}
          {live ? ' · live' : ''}
        </span>
      }
    >
      <form className={s.filters} onSubmit={apply}>
        <Input label="Actor" mono value={actor} onChange={(e) => setActor(e.target.value)} placeholder="user id, agent:…, node:…" />
        <Input label="Action" mono value={action} onChange={(e) => setAction(e.target.value)} placeholder="grant., post, job.run" />
        <Input label="Resource" mono value={resource} onChange={(e) => setResource(e.target.value)} placeholder="job:job_…" />
        <Input label="Since" type="datetime-local" mono value={since} onChange={(e) => setSince(e.target.value)} />
        <Input label="Until" type="datetime-local" mono value={until} onChange={(e) => setUntil(e.target.value)} />
        <div className={s.filterActions}>
          <Checkbox label="live tail" checked={live} onCheckedChange={setLive} />
          <Button type="submit" size="sm">
            Apply
          </Button>
        </div>
      </form>
      {loading && entries.length === 0 ? (
        <Skeleton rows={6} />
      ) : error ? (
        <EmptyState title="Could not read the audit log">{error}</EmptyState>
      ) : (
        <>
          <Table
            aria-label="Audit entries"
            columns={columns}
            rows={entries}
            rowKey={(e) => e.id}
            rowTone={(e) => (isDenied(e) ? 'alert' : undefined)}
            empty={
              filter.action && loaded > 0 ? (
                <EmptyState title="The action filter hides every loaded entry">
                  {`None of the ${loaded} entries loaded so far has an action containing "${filter.action}"; load older entries or clear the action filter.`}
                </EmptyState>
              ) : (
                <EmptyState title="No audit entries match">Widen the time range or clear a filter.</EmptyState>
              )
            }
          />
          {more && (
            <div className={s.more}>
              <Button size="sm" variant="ghost" busy={loading} onClick={loadMore}>
                Load older
              </Button>
            </div>
          )}
        </>
      )}
    </Frame>
  );
}
