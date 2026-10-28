// Mode detection. Both the hub and every node daemon serve the same build;
// the app asks the server which one it is talking to (internal/hub/uiapi.go
// and internal/daemon/localapi.go, GET /api/ui-config, no auth).

export type Mode = 'hub' | 'node';

export interface UIConfig {
  mode: Mode;
  /** The hub's external URL (both modes). */
  hub?: string;
  /** Node mode: this node's name. */
  node?: string;
  /** Hub mode: the WebAuthn relying party id. */
  rp_id?: string;
  /** Hub mode: passkeys are enabled. */
  passkeys?: boolean;
  /** Hub mode: Web Push is enabled. */
  push?: boolean;
  version?: string;
}

export class ConfigError extends Error {}

/** fetchUIConfig loads and validates /api/ui-config. */
export async function fetchUIConfig(fetchFn: typeof fetch = fetch): Promise<UIConfig> {
  let res: Response;
  try {
    res = await fetchFn('/api/ui-config', { credentials: 'omit', headers: { Accept: 'application/json' }, cache: 'no-store' });
  } catch (err) {
    throw new ConfigError(`cannot reach the server: ${(err as Error).message}`);
  }
  if (!res.ok) throw new ConfigError(`/api/ui-config answered ${res.status}`);
  let body: unknown;
  try {
    body = await res.json();
  } catch {
    throw new ConfigError('/api/ui-config is not JSON');
  }
  return parseUIConfig(body);
}

/** parseUIConfig narrows an untrusted JSON value to a UIConfig. */
export function parseUIConfig(body: unknown): UIConfig {
  if (typeof body !== 'object' || body === null) throw new ConfigError('ui-config: not an object');
  const b = body as Record<string, unknown>;
  if (b.mode !== 'hub' && b.mode !== 'node') throw new ConfigError(`ui-config: unknown mode ${JSON.stringify(b.mode)}`);
  const str = (k: string) => (typeof b[k] === 'string' ? (b[k] as string) : undefined);
  return {
    mode: b.mode,
    hub: str('hub'),
    node: str('node'),
    rp_id: str('rp_id'),
    passkeys: b.passkeys === true,
    push: b.push === true,
    version: str('version'),
  };
}

/** networkName is the label shown in the top bar: the hub's host name. */
export function networkName(cfg: UIConfig): string {
  if (cfg.hub) {
    try {
      return new URL(cfg.hub).hostname;
    } catch {
      return cfg.hub;
    }
  }
  return cfg.node ?? 'mycelium';
}
