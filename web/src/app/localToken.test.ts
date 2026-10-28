import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { LOCAL_TOKEN_KEY, clearLocalToken, readLocalToken, storeLocalToken, takeFragmentToken } from './localToken';

describe('local token handling', () => {
  it('takes the token from the fragment and strips the fragment', () => {
    const replaceState = vi.fn();
    const t = takeFragmentToken(
      { hash: '#local=abc_DEF-123', pathname: '/approvals', search: '?grant=g1' },
      { replaceState, state: { k: 1 } },
    );
    expect(t).toBe('abc_DEF-123');
    expect(replaceState).toHaveBeenCalledWith({ k: 1 }, '', '/approvals?grant=g1');
  });

  it('leaves other fragments alone', () => {
    const replaceState = vi.fn();
    expect(takeFragmentToken({ hash: '#section', pathname: '/', search: '' }, { replaceState, state: null })).toBeNull();
    expect(takeFragmentToken({ hash: '', pathname: '/', search: '' }, { replaceState, state: null })).toBeNull();
    expect(replaceState).not.toHaveBeenCalled();
  });

  it('strips an empty token fragment but returns nothing', () => {
    const replaceState = vi.fn();
    expect(takeFragmentToken({ hash: '#local=', pathname: '/', search: '' }, { replaceState, state: null })).toBeNull();
    expect(replaceState).toHaveBeenCalled();
  });

  it('keeps the token in sessionStorage only', () => {
    storeLocalToken('tok');
    expect(sessionStorage.getItem(LOCAL_TOKEN_KEY)).toBe('tok');
    expect(readLocalToken()).toBe('tok');
    expect(localStorage.length).toBe(0);
    expect(document.cookie).toBe('');
    clearLocalToken();
    expect(readLocalToken()).toBeNull();
  });
});

describe('boot with a #local fragment', () => {
  const original = window.location.href;
  beforeEach(() => {
    vi.resetModules();
  });
  afterEach(() => {
    window.history.replaceState(null, '', original);
    vi.unstubAllGlobals();
  });

  const stubFetch = (config: unknown) =>
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL) => {
        const url = typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
        if (url.endsWith('/api/ui-config')) return new Response(JSON.stringify(config), { status: 200 });
        // Whoami through the proxy, Connect JSON.
        if (url.includes('UserService/Whoami'))
          return new Response(JSON.stringify({ user: { id: 'u1', name: 'ada' } }), {
            status: 200,
            headers: { 'Content-Type': 'application/json' },
          });
        if (url.includes('FreezeStatus')) return new Response('{}', { status: 200, headers: { 'Content-Type': 'application/json' } });
        // Event stream: answer "unavailable" so the stream just backs off.
        return new Response(JSON.stringify({ error: 'hub unreachable' }), { status: 502 });
      }),
    );

  it('node mode: moves the token into sessionStorage and strips the URL', async () => {
    window.history.replaceState(null, '', '/approvals#local=secret-token');
    stubFetch({ mode: 'node', node: 'mac', hub: 'https://hub.example' });
    const { boot, signOut } = await import('./boot');
    const { useSession } = await import('./session');
    await boot();
    expect(window.location.hash).toBe('');
    expect(window.location.pathname).toBe('/approvals');
    expect(sessionStorage.getItem(LOCAL_TOKEN_KEY)).toBe('secret-token');
    expect(localStorage.getItem(LOCAL_TOKEN_KEY)).toBeNull();
    expect(JSON.stringify(localStorage)).not.toContain('secret-token');
    expect(useSession.getState().status).toBe('signed-in');
    expect(useSession.getState().rights).toMatchObject({ mode: 'node', readOnly: true, isAdmin: false, userId: 'u1' });
    await signOut();
    expect(sessionStorage.getItem(LOCAL_TOKEN_KEY)).toBeNull();
  });

  it('hub mode: strips the fragment and never stores the token', async () => {
    window.history.replaceState(null, '', '/#local=should-not-stay');
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL) => {
        const url = String(input);
        if (url.endsWith('/api/ui-config'))
          return new Response(JSON.stringify({ mode: 'hub', hub: 'https://hub.example' }), { status: 200 });
        return new Response('{"error":"no session"}', { status: 401 });
      }),
    );
    const { boot } = await import('./boot');
    const { useSession } = await import('./session');
    await boot();
    expect(window.location.hash).toBe('');
    expect(sessionStorage.getItem(LOCAL_TOKEN_KEY)).toBeNull();
    expect(useSession.getState().status).toBe('signed-out');
  });
});
