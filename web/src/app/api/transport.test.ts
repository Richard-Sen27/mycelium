import { createClient } from '@connectrpc/connect';
import { describe, expect, it, vi } from 'vitest';
import { NodeService } from '../../gen/mycel/v1/hub_pb';
import { HubHttp } from './http';
import { LocalApi } from './local';
import { CSRF_HEADER, hubTransport, nodeTransport } from './transport';

interface Call {
  url: string;
  init: RequestInit;
  headers: Headers;
}

function recorder(status = 200, body = '{"nodes":[]}') {
  const calls: Call[] = [];
  const f = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
    calls.push({ url, init: init ?? {}, headers: new Headers(init?.headers) });
    return new Response(body, { status, headers: { 'Content-Type': 'application/json' } });
  });
  return { f: f as unknown as typeof fetch, calls };
}

describe('hub transport', () => {
  it('sends the CSRF header and the session cookie on every RPC', async () => {
    const r = recorder();
    const t = hubTransport({ getCsrf: () => 'csrf-1', fetch: r.f });
    await createClient(NodeService, t).listNodes({});
    expect(r.calls).toHaveLength(1);
    const c = r.calls[0];
    expect(c.url).toMatch(/\/mycel\.v1\.NodeService\/ListNodes$/);
    expect(c.init.method).toBe('POST');
    expect(c.headers.get(CSRF_HEADER)).toBe('csrf-1');
    expect(c.headers.get('Authorization')).toBeNull();
    expect(c.init.credentials).toBe('same-origin');
  });

  it('reports an unauthenticated session', async () => {
    const r = recorder(401, '{"code":"unauthenticated","message":"no session"}');
    const onUnauthenticated = vi.fn();
    const t = hubTransport({ getCsrf: () => null, onUnauthenticated, fetch: r.f });
    await expect(createClient(NodeService, t).listNodes({})).rejects.toThrow();
    expect(onUnauthenticated).toHaveBeenCalled();
    expect(r.calls[0].headers.has(CSRF_HEADER)).toBe(false);
  });
});

describe('node transport', () => {
  it('sends the local bearer through /api/hub and never cookies or CSRF', async () => {
    const r = recorder();
    const status = vi.fn();
    const t = nodeTransport({ getToken: () => 'local-tok', onHubStatus: status, fetch: r.f });
    await createClient(NodeService, t).listNodes({});
    const c = r.calls[0];
    expect(new URL(c.url).pathname).toBe('/api/hub/mycel.v1.NodeService/ListNodes');
    expect(c.headers.get('Authorization')).toBe('Bearer local-tok');
    expect(c.headers.has(CSRF_HEADER)).toBe(false);
    expect(c.init.credentials).toBe('omit');
    expect(status).toHaveBeenLastCalledWith('ok');
  });

  it('marks the hub stale on a 502 from the proxy', async () => {
    const r = recorder(502, '{"error":"hub unreachable"}');
    const status = vi.fn();
    const t = nodeTransport({ getToken: () => 'x', onHubStatus: status, fetch: r.f });
    await expect(createClient(NodeService, t).listNodes({})).rejects.toThrow();
    expect(status).toHaveBeenCalledWith('stale');
  });
});

describe('hub JSON routes', () => {
  it('adds CSRF to non-GET calls only', async () => {
    const r = recorder(200, '{"user_id":"u1","name":"ada","role":"admin","csrf":"c","passkeys":null}');
    const http = new HubHttp({ getCsrf: () => 'c-9', fetch: r.f });
    const me = await http.me();
    expect(me.passkeys).toEqual([]);
    expect(r.calls[0].headers.has(CSRF_HEADER)).toBe(false);
    expect(r.calls[0].init.credentials).toBe('same-origin');
    await http.json('POST', '/api/auth/logout');
    expect(r.calls[1].headers.get(CSRF_HEADER)).toBe('c-9');
  });

  it('signs in through the API-key form with its double-submit token', async () => {
    const calls: Call[] = [];
    const f = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      calls.push({ url: String(input), init: init ?? {}, headers: new Headers(init?.headers) });
      if (!init?.method) return new Response('<form><input type="hidden" name="csrf" value="tok123"></form>', { status: 200 });
      return new Response('', { status: 303, headers: { Location: '/ui/approvals' } });
    });
    await new HubHttp({ getCsrf: () => null, fetch: f as unknown as typeof fetch }).apiKeyLogin('  key-1 ');
    const body = calls[1].init.body as URLSearchParams;
    expect(body.get('csrf')).toBe('tok123');
    expect(body.get('api_key')).toBe('key-1');
    expect(calls[1].init.redirect).toBe('manual');
  });

  it('reports a wrong API key', async () => {
    const f = vi.fn(async (_: RequestInfo | URL, init?: RequestInit) =>
      !init?.method ? new Response('<input type="hidden" name="csrf" value="t">') : new Response('bad', { status: 401 }),
    );
    await expect(new HubHttp({ getCsrf: () => null, fetch: f as unknown as typeof fetch }).apiKeyLogin('x')).rejects.toThrow(/not valid/);
  });
});

describe('local routes', () => {
  it('use the bearer and omit cookies', async () => {
    const r = recorder(200, '{"frozen":false,"scope":"","reason":"","since":""}');
    const local = new LocalApi(() => 'local-tok', r.f);
    await local.freeze();
    expect(r.calls[0].url).toBe('/api/local/freeze');
    expect(r.calls[0].headers.get('Authorization')).toBe('Bearer local-tok');
    expect(r.calls[0].init.credentials).toBe('omit');
  });
});
