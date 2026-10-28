import { CSRF_HEADER } from './transport';

// JSON helpers for the hub's browser routes (/api/auth/*, /api/push/*) and
// the API-key sign-in form at /ui/login. See internal/hub/uiapi.go.

export class HttpError extends Error {
  constructor(
    readonly status: number,
    message: string,
  ) {
    super(message);
  }
}

export interface Passkey {
  id: string;
  name: string;
  created_at: string;
  last_used_at: string;
}

/** Me is GET /api/auth/me. */
export interface Me {
  user_id: string;
  name: string;
  role: string;
  csrf: string;
  passkeys: Passkey[];
}

export interface HubHttpOptions {
  getCsrf: () => string | null;
  fetch?: typeof fetch;
}

/** HubHttp calls the hub's JSON routes with the session cookie and, for non-GETs, the CSRF header. */
export class HubHttp {
  private f: typeof fetch;
  constructor(private opts: HubHttpOptions) {
    this.f = opts.fetch ?? globalThis.fetch.bind(globalThis);
  }

  async json<T>(method: string, path: string, body?: unknown): Promise<T> {
    const headers: Record<string, string> = { Accept: 'application/json' };
    if (body !== undefined) headers['Content-Type'] = 'application/json';
    if (method !== 'GET' && method !== 'HEAD') {
      const t = this.opts.getCsrf();
      if (t) headers[CSRF_HEADER] = t;
    }
    const res = await this.f(path, {
      method,
      headers,
      body: body === undefined ? undefined : JSON.stringify(body),
      credentials: 'same-origin',
      cache: 'no-store',
    });
    if (!res.ok) throw new HttpError(res.status, await errorText(res));
    if (res.status === 204) return undefined as T;
    const text = await res.text();
    return (text ? JSON.parse(text) : undefined) as T;
  }

  me(): Promise<Me> {
    return this.json<Me>('GET', '/api/auth/me').then(normalizeMe);
  }
  logout(): Promise<void> {
    return this.json<void>('POST', '/api/auth/logout');
  }
  passkeyRegisterBegin(name: string): Promise<unknown> {
    return this.json('POST', '/api/auth/passkey/register/begin', { name });
  }
  passkeyRegisterFinish(credential: unknown): Promise<{ id: string }> {
    return this.json('POST', '/api/auth/passkey/register/finish', credential);
  }
  passkeyLoginBegin(user?: string): Promise<unknown> {
    return this.json('POST', '/api/auth/passkey/login/begin', user ? { user } : {});
  }
  passkeyLoginFinish(assertion: unknown): Promise<{ user_id: string; csrf: string }> {
    return this.json('POST', '/api/auth/passkey/login/finish', assertion);
  }
  passkeyDelete(id: string): Promise<void> {
    return this.json('DELETE', `/api/auth/passkey/${encodeURIComponent(id)}`);
  }
  vapidKey(): Promise<{ public_key: string }> {
    return this.json('GET', '/api/push/vapid');
  }
  pushSubscribe(sub: PushSubscriptionJSON): Promise<void> {
    return this.json('POST', '/api/push/subscriptions', sub);
  }
  pushUnsubscribe(endpoint: string): Promise<void> {
    return this.json('DELETE', '/api/push/subscriptions', { endpoint });
  }

  /**
   * apiKeyLogin signs in through the phase 5 form: GET /ui/login sets a
   * short-lived double-submit cookie and renders its token into the form;
   * POST /ui/login with that token and the key starts the session (303).
   */
  async apiKeyLogin(apiKey: string): Promise<void> {
    const page = await this.f('/ui/login', { credentials: 'same-origin', cache: 'no-store' });
    const html = await page.text();
    const m = /name="csrf"\s+value="([^"]+)"/.exec(html);
    if (!page.ok || !m) throw new HttpError(page.status || 500, 'The sign-in form is not available on this hub.');
    const form = new URLSearchParams({ csrf: m[1], api_key: apiKey.trim() });
    const res = await this.f('/ui/login', {
      method: 'POST',
      body: form,
      credentials: 'same-origin',
      redirect: 'manual',
      cache: 'no-store',
    });
    if (res.type === 'opaqueredirect' || (res.status >= 300 && res.status < 400) || res.redirected) return;
    if (res.status === 401) throw new HttpError(401, 'That API key is not valid.');
    if (res.status === 429) throw new HttpError(429, 'Too many sign-in attempts; wait a minute.');
    if (res.status === 403) throw new HttpError(403, 'The sign-in form expired; try again.');
    throw new HttpError(res.status, `Sign-in failed (${res.status}).`);
  }
}

function normalizeMe(m: Me): Me {
  return { ...m, passkeys: Array.isArray(m.passkeys) ? m.passkeys : [] };
}

async function errorText(res: Response): Promise<string> {
  try {
    const t = await res.text();
    try {
      const j = JSON.parse(t) as { error?: string; message?: string };
      return j.error ?? j.message ?? t;
    } catch {
      return t.trim() || `${res.status} ${res.statusText}`;
    }
  } catch {
    return `${res.status} ${res.statusText}`;
  }
}

/** isAdminRole accepts the hub's role spellings ("admin", "ROLE_ADMIN"). */
export function isAdminRole(role: string | undefined | null): boolean {
  if (!role) return false;
  const r = role.toLowerCase();
  return r === 'admin' || r === 'role_admin';
}
