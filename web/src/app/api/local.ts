import { fromJson, type JsonValue } from '@bufbuild/protobuf';
import { GrantSchema, SharingSettingsSchema, type Grant, type SharingSettings } from '../../gen/mycel/v1/grant_pb';
import { HttpError } from './http';
import { localFetch } from './transport';

// The node daemon's local routes for "This machine" (restrict only), see
// internal/daemon/localapi.go phase 2–5. Authenticated with the local
// bearer; cookies are never sent.

export interface LocalSharing {
  hub: SharingSettings;
  local: SharingSettings;
  effective: SharingSettings;
  idle: string;
}

export interface LocalFreeze {
  frozen: boolean;
  scope: string;
  reason: string;
  since: string;
}

export interface LocalWorker {
  agent_id: string;
  parent: string;
  harness: string;
  state: string;
  pid: number;
  started_at: string;
  exited_at: string;
  exit_code: number;
  usage: { tokens: number; usd: number; turns: number };
}

export class LocalApi {
  private f: typeof fetch;
  constructor(
    private getToken: () => string | null,
    fetchFn?: typeof fetch,
  ) {
    this.f = localFetch(fetchFn ?? globalThis.fetch.bind(globalThis));
  }

  async json<T>(method: string, path: string, body?: unknown): Promise<T> {
    const headers: Record<string, string> = { Accept: 'application/json' };
    const t = this.getToken();
    if (t) headers.Authorization = `Bearer ${t}`;
    if (body !== undefined) headers['Content-Type'] = 'application/json';
    const res = await this.f(`/api/local${path}`, {
      method,
      headers,
      body: body === undefined ? undefined : JSON.stringify(body),
      cache: 'no-store',
    });
    if (!res.ok) {
      let msg = `${res.status}`;
      try {
        const j = (await res.json()) as { error?: string };
        if (j.error) msg = j.error;
      } catch {
        /* not JSON */
      }
      throw new HttpError(res.status, msg);
    }
    if (res.status === 204) return undefined as T;
    return (await res.json()) as T;
  }

  async sharing(): Promise<LocalSharing> {
    const j = await this.json<Record<string, JsonValue>>('GET', '/sharing');
    const s = (k: string) => fromJson(SharingSettingsSchema, (j[k] ?? {}) as JsonValue, { ignoreUnknownFields: true });
    return { hub: s('hub'), local: s('local'), effective: s('effective'), idle: typeof j.idle === 'string' ? j.idle : 'unknown' };
  }

  /** setSharing may only tighten; the daemon answers 400 for anything wider. */
  async setSharing(settings: JsonValue): Promise<SharingSettings> {
    const j = await this.json<JsonValue>('PUT', '/sharing', settings);
    return fromJson(SharingSettingsSchema, j, { ignoreUnknownFields: true });
  }

  async grants(): Promise<{ grants: Grant[]; endpoints: Record<string, string> }> {
    const j = await this.json<{ grants?: JsonValue[]; endpoints?: Record<string, string> }>('GET', '/grants');
    return {
      grants: (j.grants ?? []).map((g) => fromJson(GrantSchema, g, { ignoreUnknownFields: true })),
      endpoints: j.endpoints ?? {},
    };
  }

  revokeGrant(id: string): Promise<void> {
    return this.json('DELETE', `/grants/${encodeURIComponent(id)}`);
  }

  freeze(): Promise<LocalFreeze> {
    return this.json('GET', '/freeze');
  }

  async workers(): Promise<LocalWorker[]> {
    const j = await this.json<{ workers?: LocalWorker[] }>('GET', '/workers');
    return j.workers ?? [];
  }

  stopWorker(agent: string): Promise<void> {
    return this.json('DELETE', `/workers/${encodeURIComponent(agent)}`);
  }
}
