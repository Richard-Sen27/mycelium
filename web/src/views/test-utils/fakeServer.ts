import { vi } from 'vitest';
import { makeHubApi, makeNodeApi, setApi } from '../../app/api';
import { allEntityStores } from '../../app/events/entities';
import { resetStores } from '../../app/events/stores';
import { useSession } from '../../app/session';
import { resetChannelUsageCache } from '../channels/usage';

// Test helper for the views: a fake fetch that answers Connect calls (JSON
// unary and enveloped server streams) and plain JSON routes from handlers.

export type Handler = (body: unknown, url: string, init?: RequestInit) => unknown | Response | Promise<unknown | Response>;

export interface FakeServer {
  fetch: typeof fetch;
  calls: { url: string; method: string; body: unknown }[];
  /** Calls to a Connect method, e.g. "GrantService/ApproveGrant". */
  bodies(method: string): unknown[];
}

const enc = new TextEncoder();

function frame(flags: number, json: unknown): Uint8Array {
  const payload = enc.encode(JSON.stringify(json));
  const out = new Uint8Array(5 + payload.length);
  out[0] = flags;
  new DataView(out.buffer).setUint32(1, payload.length);
  out.set(payload, 5);
  return out;
}

/** stream answers a Connect server-stream call with the given messages, then end-of-stream. */
export function stream(messages: unknown[]): Response {
  const parts = [...messages.map((m) => frame(0, m)), frame(2, {})];
  const body = new ReadableStream<Uint8Array>({
    start(c) {
      for (const p of parts) c.enqueue(p);
      c.close();
    },
  });
  return new Response(body, { status: 200, headers: { 'Content-Type': 'application/connect+json' } });
}

/** b64 encodes text as proto3 JSON bytes. */
export function b64(text: string): string {
  return btoa(String.fromCharCode(...enc.encode(text)));
}

export function json(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });
}

/**
 * fakeServer routes by the URL's last two path segments for Connect
 * ("ChannelService/ListMessages") or by "<METHOD> <path>" for REST routes.
 * Unknown Connect calls answer {}; WatchEvents never ends.
 */
export function fakeServer(handlers: Record<string, Handler> = {}): FakeServer {
  const calls: FakeServer['calls'] = [];
  const f = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input);
    const method = init?.method ?? 'GET';
    let body: unknown = undefined;
    const raw = init?.body;
    const text = typeof raw === 'string' ? raw : ArrayBuffer.isView(raw) ? new TextDecoder().decode(raw as Uint8Array) : null;
    if (text !== null) {
      try {
        body = JSON.parse(text);
      } catch {
        // A streaming request is one enveloped message: skip the 5-byte prefix.
        try {
          body = JSON.parse(text.slice(5));
        } catch {
          body = text;
        }
      }
    } else if (raw != null) {
      body = raw;
    }
    calls.push({ url, method, body });
    const path = new URL(url, 'http://localhost').pathname;
    const rpc = path.split('/').slice(-2).join('/');
    if (rpc.endsWith('/WatchEvents'))
      return new Response(new ReadableStream(), { status: 200, headers: { 'Content-Type': 'application/connect+json' } });
    const h = handlers[rpc.replace(/^mycel\.v1\./, '')] ?? handlers[`${method} ${path}`];
    if (!h) return json({});
    const out = await h(body, url, init);
    return out instanceof Response ? out : json(out ?? {});
  }) as unknown as typeof fetch;
  return {
    fetch: f,
    calls,
    bodies: (m) => calls.filter((c) => c.url.endsWith(`/mycel.v1.${m}`)).map((c) => c.body),
  };
}

/** signIn installs a hub API over the fake server with the given rights. */
export function signIn(server: FakeServer, o: { admin?: boolean; userId?: string; mode?: 'hub' | 'node' } = {}): void {
  const mode = o.mode ?? 'hub';
  const userId = o.userId ?? 'u1';
  setApi(
    mode === 'hub' ? makeHubApi({ getCsrf: () => 'c', fetch: server.fetch }) : makeNodeApi({ getToken: () => 't', fetch: server.fetch }),
  );
  useSession.setState({
    status: 'signed-in',
    config: mode === 'hub' ? { mode: 'hub', hub: 'https://hub.example:8443', passkeys: false, push: false } : { mode: 'node', node: 'mac' },
    me: mode === 'hub' ? { user_id: userId, name: 'ada', role: o.admin ? 'admin' : 'member', csrf: 'c', passkeys: [] } : null,
    rights: { mode, isAdmin: mode === 'hub' && !!o.admin, readOnly: mode === 'node', userId, userName: 'ada' },
  });
}

/** seed marks a store loaded with the given rows (no List call). */
export function seed<T>(store: { use: { setState: (s: object) => void }; key: (r: T) => string }, rows: T[]): void {
  const byId: Record<string, T> = {};
  for (const r of rows) byId[store.key(r)] = r;
  store.use.setState({ byId, loaded: true, loading: false, error: null, stale: false });
}

export function resetAll(): void {
  resetStores();
  resetChannelUsageCache();
  for (const s of allEntityStores()) s.reset();
}
