import { createClient, type Client, type Transport } from '@connectrpc/connect';
import { ChannelService } from '../../gen/mycel/v1/channel_pb';
import { ControlService, GrantService } from '../../gen/mycel/v1/grant_pb';
import { AgentService, NodeService, TokenService, UserService } from '../../gen/mycel/v1/hub_pb';
import { ArtifactService, JobService } from '../../gen/mycel/v1/job_pb';
import { EventService } from '../../gen/mycel/v1/ui_pb';
import type { Mode } from '../config';
import { HttpError, HubHttp } from './http';
import { LocalApi } from './local';
import { CSRF_HEADER, hubTransport, localFetch, nodeTransport, type HubStatus } from './transport';

export interface Clients {
  tokens: Client<typeof TokenService>;
  nodes: Client<typeof NodeService>;
  users: Client<typeof UserService>;
  agents: Client<typeof AgentService>;
  channels: Client<typeof ChannelService>;
  grants: Client<typeof GrantService>;
  control: Client<typeof ControlService>;
  jobs: Client<typeof JobService>;
  artifacts: Client<typeof ArtifactService>;
  events: Client<typeof EventService>;
}

export function makeClients(t: Transport): Clients {
  return {
    tokens: createClient(TokenService, t),
    nodes: createClient(NodeService, t),
    users: createClient(UserService, t),
    agents: createClient(AgentService, t),
    channels: createClient(ChannelService, t),
    grants: createClient(GrantService, t),
    control: createClient(ControlService, t),
    jobs: createClient(JobService, t),
    artifacts: createClient(ArtifactService, t),
    events: createClient(EventService, t),
  };
}

/**
 * Api is everything a view needs to talk to the server. In node mode
 * `rpc` goes through the daemon's read-only proxy (writes are refused there
 * with PermissionDenied) and `local` reaches "This machine"; in hub mode
 * `hub` carries the browser routes (auth, passkeys, push).
 */
export interface Api {
  mode: Mode;
  transport: Transport;
  rpc: Clients;
  /** Hub mode only. */
  hub: HubHttp | null;
  /** Node mode only. */
  local: LocalApi | null;
  /**
   * download GETs a hub path that is not a Connect call (e.g.
   * /api/artifacts/{id}) with this mode's auth: the session cookie on the
   * hub, the local bearer through the node's read-only proxy (/api/hub).
   */
  download(path: string): Promise<Blob>;
  /**
   * upload PUTs bytes to a hub path (e.g. /api/artifacts?name=…) with the
   * session cookie and CSRF header; hub mode only (the node proxy is
   * read-only). Returns the JSON reply.
   */
  upload(path: string, body: Blob | ArrayBuffer | Uint8Array): Promise<unknown>;
}

async function httpFailure(res: Response): Promise<HttpError> {
  let msg = res.status === 502 ? 'The hub is unreachable.' : `${res.status} ${res.statusText}`.trim();
  try {
    const j = (await res.json()) as { error?: string };
    if (j.error) msg = j.error;
  } catch {
    /* not JSON */
  }
  return new HttpError(res.status, msg);
}

export function makeHubApi(o: { getCsrf: () => string | null; onUnauthenticated?: () => void; fetch?: typeof fetch }): Api {
  const transport = hubTransport({ getCsrf: o.getCsrf, onUnauthenticated: o.onUnauthenticated, fetch: o.fetch });
  const f = o.fetch ?? globalThis.fetch.bind(globalThis);
  return {
    mode: 'hub',
    transport,
    rpc: makeClients(transport),
    hub: new HubHttp({ getCsrf: o.getCsrf, fetch: o.fetch }),
    local: null,
    async download(path) {
      const res = await f(path, { credentials: 'same-origin', cache: 'no-store' });
      if (!res.ok) throw await httpFailure(res);
      return res.blob();
    },
    async upload(path, body) {
      const headers: Record<string, string> = { 'Content-Type': 'application/octet-stream', Accept: 'application/json' };
      const t = o.getCsrf();
      if (t) headers[CSRF_HEADER] = t;
      const res = await f(path, { method: 'PUT', body: body as BodyInit, headers, credentials: 'same-origin', cache: 'no-store' });
      if (!res.ok) throw await httpFailure(res);
      const text = await res.text();
      return text ? (JSON.parse(text) as unknown) : undefined;
    },
  };
}

export function makeNodeApi(o: { getToken: () => string | null; onHubStatus?: (s: HubStatus) => void; fetch?: typeof fetch }): Api {
  const transport = nodeTransport({ getToken: o.getToken, onHubStatus: o.onHubStatus, fetch: o.fetch });
  const f = localFetch(o.fetch ?? globalThis.fetch.bind(globalThis), o.onHubStatus);
  return {
    mode: 'node',
    transport,
    rpc: makeClients(transport),
    hub: null,
    local: new LocalApi(o.getToken, o.fetch),
    async download(path) {
      const t = o.getToken();
      const res = await f(`/api/hub${path}`, { headers: t ? { Authorization: `Bearer ${t}` } : {}, cache: 'no-store' });
      if (!res.ok) throw await httpFailure(res);
      return res.blob();
    },
    upload() {
      return Promise.reject(new HttpError(403, 'Uploads go through the hub UI; the node UI is read-only for the network.'));
    },
  };
}

let current: Api | null = null;

/** setApi installs the active API (done once at boot, or after sign-in). */
export function setApi(api: Api | null): void {
  current = api;
}

/** getApi returns the active API; throws before boot finished. */
export function getApi(): Api {
  if (!current) throw new Error('API not ready');
  return current;
}

export function maybeApi(): Api | null {
  return current;
}

export { HttpError, isAdminRole, type Me, type Passkey } from './http';
export { CSRF_HEADER, type HubStatus } from './transport';
