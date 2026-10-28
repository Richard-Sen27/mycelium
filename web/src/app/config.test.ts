import { describe, expect, it, vi } from 'vitest';
import { ConfigError, fetchUIConfig, networkName, parseUIConfig } from './config';

const ok = (body: unknown) => new Response(JSON.stringify(body), { status: 200, headers: { 'Content-Type': 'application/json' } });

describe('mode detection', () => {
  it('reads hub mode with its WebAuthn and push flags', async () => {
    const f = vi.fn(async () =>
      ok({ mode: 'hub', hub: 'https://hub.example:8443', rp_id: 'hub.example', passkeys: true, push: false, version: 'v1' }),
    );
    const cfg = await fetchUIConfig(f as unknown as typeof fetch);
    expect(cfg).toEqual({
      mode: 'hub',
      hub: 'https://hub.example:8443',
      node: undefined,
      rp_id: 'hub.example',
      passkeys: true,
      push: false,
      version: 'v1',
    });
    expect(f).toHaveBeenCalledWith('/api/ui-config', expect.objectContaining({ credentials: 'omit' }));
    expect(networkName(cfg)).toBe('hub.example');
  });

  it('reads node mode', async () => {
    const cfg = await fetchUIConfig((async () =>
      ok({ mode: 'node', node: 'mac', hub: 'https://hub.example:8443' })) as unknown as typeof fetch);
    expect(cfg.mode).toBe('node');
    expect(cfg.node).toBe('mac');
    expect(cfg.passkeys).toBe(false);
  });

  it('rejects an unknown mode and non-2xx answers', async () => {
    expect(() => parseUIConfig({ mode: 'admin' })).toThrow(ConfigError);
    expect(() => parseUIConfig(null)).toThrow(ConfigError);
    await expect(fetchUIConfig((async () => new Response('nope', { status: 404 })) as unknown as typeof fetch)).rejects.toThrow(/404/);
    await expect(
      fetchUIConfig((async () => {
        throw new TypeError('offline');
      }) as unknown as typeof fetch),
    ).rejects.toThrow(/cannot reach/);
  });

  it('ignores fields of the wrong type', () => {
    expect(parseUIConfig({ mode: 'hub', hub: 42, passkeys: 'yes' })).toMatchObject({ hub: undefined, passkeys: false });
  });
});
