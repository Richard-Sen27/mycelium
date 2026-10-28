import { Code, ConnectError, type Interceptor, type Transport } from '@connectrpc/connect';
import { createConnectTransport } from '@connectrpc/connect-web';

// One Connect transport per mode.
//
// Hub mode: same origin, the session cookie (credentials "same-origin") and
// the session's CSRF token in `Mycel-Csrf` on every call (Connect calls are
// POSTs; the hub ignores a cookie without the header).
//
// Node mode: base URL /api/hub (the daemon's read-only proxy to the hub),
// the local token as a bearer, and credentials "omit" so no cookie of the
// localhost origin is ever sent. A 502 from the proxy means the hub is
// unreachable: the app switches to its last snapshot, marked stale.

export const CSRF_HEADER = 'Mycel-Csrf';

export type HubStatus = 'ok' | 'stale';

export interface HubTransportOptions {
  /** Origin-relative base, default "/" (resolved against location.origin). */
  baseUrl?: string;
  getCsrf: () => string | null;
  /** Called when the hub rejects the session (expired or signed out elsewhere). */
  onUnauthenticated?: () => void;
  fetch?: typeof fetch;
}

export function hubTransport(opts: HubTransportOptions): Transport {
  const f = opts.fetch ?? globalThis.fetch.bind(globalThis);
  const csrf: Interceptor = (next) => async (req) => {
    const t = opts.getCsrf();
    if (t) req.header.set(CSRF_HEADER, t);
    req.header.delete('Authorization');
    try {
      return await next(req);
    } catch (err) {
      if (err instanceof ConnectError && err.code === Code.Unauthenticated) opts.onUnauthenticated?.();
      throw err;
    }
  };
  return createConnectTransport({
    baseUrl: absolute(opts.baseUrl ?? '/'),
    interceptors: [csrf],
    fetch: (input, init) => f(input, { ...init, credentials: 'same-origin' }),
  });
}

export interface NodeTransportOptions {
  /** Default "/api/hub". */
  baseUrl?: string;
  getToken: () => string | null;
  /** Reports whether the proxy reached the hub (502 = stale). */
  onHubStatus?: (s: HubStatus) => void;
  fetch?: typeof fetch;
}

export function nodeTransport(opts: NodeTransportOptions): Transport {
  const f = opts.fetch ?? globalThis.fetch.bind(globalThis);
  const bearer: Interceptor = (next) => async (req) => {
    const t = opts.getToken();
    if (t) req.header.set('Authorization', `Bearer ${t}`);
    req.header.delete(CSRF_HEADER);
    return next(req);
  };
  return createConnectTransport({
    baseUrl: absolute(opts.baseUrl ?? '/api/hub'),
    interceptors: [bearer],
    fetch: localFetch(f, opts.onHubStatus),
  });
}

/**
 * localFetch wraps fetch for node mode: never send cookies and report the
 * proxy's view of the hub. Exported for the local REST client and tests.
 */
export function localFetch(f: typeof fetch, onHubStatus?: (s: HubStatus) => void): typeof fetch {
  return async (input, init) => {
    const res = await f(input, { ...init, credentials: 'omit' });
    if (onHubStatus) {
      if (res.status === 502) onHubStatus('stale');
      else if (res.ok) onHubStatus('ok');
    }
    return res;
  };
}

function absolute(base: string): string {
  if (/^https?:\/\//.test(base)) return base.replace(/\/$/, '');
  const origin = typeof location !== 'undefined' ? location.origin : 'http://localhost';
  return (origin + (base.startsWith('/') ? base : `/${base}`)).replace(/\/$/, '');
}
