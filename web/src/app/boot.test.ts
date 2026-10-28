import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { LOCAL_TOKEN_KEY } from './localToken';

describe('takeFragmentCode', () => {
  it('takes the code and strips the fragment', async () => {
    const { takeFragmentCode } = await import('./boot');
    const replaceState = vi.fn();
    expect(takeFragmentCode({ hash: '#code=c0de_X-1', pathname: '/', search: '?a=1' }, { replaceState, state: null })).toBe('c0de_X-1');
    expect(replaceState).toHaveBeenCalledWith(null, '', '/?a=1');
  });

  it('leaves other fragments alone', async () => {
    const { takeFragmentCode } = await import('./boot');
    const replaceState = vi.fn();
    expect(takeFragmentCode({ hash: '#local=tok', pathname: '/', search: '' }, { replaceState, state: null })).toBeNull();
    expect(replaceState).not.toHaveBeenCalled();
  });
});

describe('boot with a #code fragment', () => {
  const original = window.location.href;
  beforeEach(() => {
    vi.resetModules();
    sessionStorage.clear();
  });
  afterEach(() => {
    window.history.replaceState(null, '', original);
    vi.unstubAllGlobals();
    sessionStorage.clear();
  });

  const stub = (sessionAnswer: Response) => {
    const calls: { url: string; body: string | null }[] = [];
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const url = typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
        calls.push({ url, body: typeof init?.body === 'string' ? init.body : null });
        if (url.endsWith('/api/ui-config'))
          return new Response(JSON.stringify({ mode: 'node', node: 'mac', hub: 'https://hub.example' }), { status: 200 });
        if (url.endsWith('/api/local/ui-session')) return sessionAnswer.clone();
        if (url.includes('UserService/Whoami'))
          return new Response(JSON.stringify({ user: { id: 'u1', name: 'ada' } }), {
            status: 200,
            headers: { 'Content-Type': 'application/json' },
          });
        if (url.includes('FreezeStatus')) return new Response('{}', { status: 200, headers: { 'Content-Type': 'application/json' } });
        return new Response(JSON.stringify({ error: 'hub unreachable' }), { status: 502 });
      }),
    );
    return calls;
  };

  it('trades the code for the token and keeps it in sessionStorage only', async () => {
    window.history.replaceState(null, '', '/agents#code=one-time');
    const calls = stub(new Response(JSON.stringify({ token: 'local-secret' }), { status: 200 }));
    const { boot, signOut } = await import('./boot');
    const { useSession } = await import('./session');
    await boot();
    expect(window.location.hash).toBe('');
    expect(window.location.pathname).toBe('/agents');
    const redeem = calls.find((c) => c.url.endsWith('/api/local/ui-session'));
    expect(redeem?.body).toBe(JSON.stringify({ code: 'one-time' }));
    expect(sessionStorage.getItem(LOCAL_TOKEN_KEY)).toBe('local-secret');
    expect(JSON.stringify(localStorage)).not.toContain('local-secret');
    expect(useSession.getState().status).toBe('signed-in');
    await signOut();
  });

  it('stays signed out when the code is refused', async () => {
    window.history.replaceState(null, '', '/#code=used');
    stub(new Response(JSON.stringify({ error: 'unknown, used or expired code' }), { status: 404 }));
    const { boot } = await import('./boot');
    const { useSession } = await import('./session');
    await boot();
    expect(window.location.hash).toBe('');
    expect(sessionStorage.getItem(LOCAL_TOKEN_KEY)).toBeNull();
    expect(useSession.getState().status).toBe('signed-out');
  });
});
