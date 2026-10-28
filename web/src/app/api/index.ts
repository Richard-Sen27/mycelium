import { createClient, type Client, type Transport } from '@connectrpc/connect';
import { ChannelService } from '../../gen/mycel/v1/channel_pb';
import { ControlService, GrantService } from '../../gen/mycel/v1/grant_pb';
import { AgentService, NodeService, TokenService, UserService } from '../../gen/mycel/v1/hub_pb';
import { ArtifactService, JobService } from '../../gen/mycel/v1/job_pb';
import { EventService } from '../../gen/mycel/v1/ui_pb';
import type { Mode } from '../config';
import { HubHttp } from './http';
import { LocalApi } from './local';
import { hubTransport, nodeTransport, type HubStatus } from './transport';

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
}

export function makeHubApi(o: { getCsrf: () => string | null; onUnauthenticated?: () => void; fetch?: typeof fetch }): Api {
  const transport = hubTransport({ getCsrf: o.getCsrf, onUnauthenticated: o.onUnauthenticated, fetch: o.fetch });
  return { mode: 'hub', transport, rpc: makeClients(transport), hub: new HubHttp({ getCsrf: o.getCsrf, fetch: o.fetch }), local: null };
}

export function makeNodeApi(o: { getToken: () => string | null; onHubStatus?: (s: HubStatus) => void; fetch?: typeof fetch }): Api {
  const transport = nodeTransport({ getToken: o.getToken, onHubStatus: o.onHubStatus, fetch: o.fetch });
  return { mode: 'node', transport, rpc: makeClients(transport), hub: null, local: new LocalApi(o.getToken, o.fetch) };
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
