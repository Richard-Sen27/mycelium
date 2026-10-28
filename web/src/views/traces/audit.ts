import { useCallback, useEffect, useMemo, useState } from 'react';
import { getApi } from '../../app/api';
import { useAudit } from '../../app/events/stores';
import type { AuditEntry } from '../../gen/mycel/v1/ui_pb';
import { errText } from '../channels/model';

export interface AuditFilter {
  traceId?: string;
  actor?: string;
  resource?: string;
  /** Substring of the action (client-side; the API has no action filter). */
  action?: string;
  since?: string;
  until?: string;
}

/** matchesAudit applies a filter to one entry (live entries are filtered here like the server does). */
export function matchesAudit(e: AuditEntry, f: AuditFilter): boolean {
  if (f.traceId && e.traceId !== f.traceId) return false;
  if (f.actor && e.actor !== f.actor) return false;
  if (f.resource && e.resource !== f.resource) return false;
  if (f.action && !e.action.toLowerCase().includes(f.action.toLowerCase())) return false;
  const at = Date.parse(e.at);
  if (f.since && Number.isFinite(at) && at < Date.parse(f.since)) return false;
  if (f.until && Number.isFinite(at) && at >= Date.parse(f.until)) return false;
  return true;
}

/** auditSeq orders entries by their stream sequence (ids are sequence numbers as strings). */
export function auditSeq(e: AuditEntry): bigint {
  try {
    return BigInt(e.id);
  } catch {
    return 0n;
  }
}

const newestFirst = (a: AuditEntry, b: AuditEntry) => {
  const x = auditSeq(a);
  const y = auditSeq(b);
  return x === y ? b.at.localeCompare(a.at) : x < y ? 1 : -1;
};

export function mergeAudit(a: AuditEntry[], b: AuditEntry[]): AuditEntry[] {
  const m = new Map<string, AuditEntry>();
  for (const e of a) m.set(e.id, e);
  for (const e of b) m.set(e.id, e);
  return [...m.values()].sort(newestFirst);
}

/** parseDetails reads an entry's details_json; {} when empty or unreadable. */
export function parseDetails(e: AuditEntry): Record<string, unknown> {
  if (!e.detailsJson) return {};
  try {
    const v = JSON.parse(e.detailsJson) as unknown;
    return v && typeof v === 'object' && !Array.isArray(v) ? (v as Record<string, unknown>) : {};
  } catch {
    return {};
  }
}

export function isDenied(e: AuditEntry): boolean {
  return e.outcome === 'denied' || e.outcome === 'error';
}

/**
 * useAuditQuery loads ListAudit for a server-side filter (with paging) and
 * merges the live tail (useAudit) entries that match, newest first.
 */
export function useAuditQuery(
  filter: AuditFilter,
  opts: { enabled?: boolean; live?: boolean; limit?: number } = {},
): {
  entries: AuditEntry[];
  /** Entries loaded before the client-side action filter. */
  loaded: number;
  loading: boolean;
  error: string | null;
  more: boolean;
  loadMore: () => void;
  reload: () => void;
} {
  const { enabled = true, live = true, limit = 200 } = opts;
  const [hist, setHist] = useState<AuditEntry[]>([]);
  const [token, setToken] = useState('');
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [gen, setGen] = useState(0);
  const key = JSON.stringify([filter.traceId, filter.actor, filter.resource, filter.since, filter.until]);

  const fetchPage = useCallback(
    async (pageToken: string) => {
      const f = JSON.parse(key) as (string | undefined)[];
      const r = await getApi().rpc.events.listAudit({
        traceId: f[0] ?? '',
        actor: f[1] ?? '',
        resource: f[2] ?? '',
        since: f[3] ?? '',
        until: f[4] ?? '',
        pageToken,
        limit,
      });
      return r;
    },
    [key, limit],
  );

  useEffect(() => {
    if (!enabled) return;
    let cancelled = false;
    setLoading(true);
    setError(null);
    setHist([]);
    setToken('');
    fetchPage('')
      .then((r) => {
        if (cancelled) return;
        setHist(r.entries);
        setToken(r.nextPageToken);
      })
      .catch((err: unknown) => !cancelled && setError(errText(err)))
      .finally(() => !cancelled && setLoading(false));
    return () => {
      cancelled = true;
    };
  }, [fetchPage, enabled, gen]);

  const loadMore = useCallback(() => {
    if (!token || loading) return;
    setLoading(true);
    fetchPage(token)
      .then((r) => {
        setHist((h) => mergeAudit(h, r.entries));
        setToken(r.nextPageToken);
      })
      .catch((err: unknown) => setError(errText(err)))
      .finally(() => setLoading(false));
  }, [token, loading, fetchPage]);

  const tail = useAudit((st) => st.entries);
  const { entries, loaded } = useMemo(() => {
    // `key` and `filter.action` capture every field of the filter.
    const merged = live
      ? mergeAudit(
          hist,
          tail.filter((e) => matchesAudit(e, filter)),
        )
      : mergeAudit(hist, []);
    const noAction = { ...filter, action: undefined };
    const base = merged.filter((e) => matchesAudit(e, noAction));
    return { entries: base.filter((e) => matchesAudit(e, filter)), loaded: base.length };
  }, [hist, tail, live, key, filter.action]);

  return { entries, loaded, loading, error, more: !!token, loadMore, reload: () => setGen((g) => g + 1) };
}
