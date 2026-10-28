// Node-mode sign-in. `mycel ui` opens http://<local addr>/#local=<token>.
// The token is taken out of the URL at once (so it never stays in the
// address bar, history or a bookmark), kept only in sessionStorage (gone
// when the tab closes; never localStorage, cookies or IndexedDB) and sent
// as the local bearer.

export const LOCAL_TOKEN_KEY = 'mycel.localToken';

type Loc = Pick<Location, 'hash' | 'pathname' | 'search'>;
type Hist = Pick<History, 'replaceState' | 'state'>;

/**
 * takeFragmentToken reads `#local=<token>` from the URL and strips the whole
 * fragment with history.replaceState. Returns the token, or null when the
 * fragment carries none (then the URL is left alone).
 */
export function takeFragmentToken(loc: Loc = window.location, hist: Hist = window.history): string | null {
  const hash = loc.hash.startsWith('#') ? loc.hash.slice(1) : loc.hash;
  if (!hash) return null;
  const params = new URLSearchParams(hash);
  const token = params.get('local');
  if (token === null) return null;
  hist.replaceState(hist.state, '', loc.pathname + loc.search);
  const t = token.trim();
  return t === '' ? null : t;
}

export function storeLocalToken(token: string, storage: Storage = sessionStorage): void {
  storage.setItem(LOCAL_TOKEN_KEY, token);
}

export function readLocalToken(storage: Storage = sessionStorage): string | null {
  try {
    return storage.getItem(LOCAL_TOKEN_KEY);
  } catch {
    return null;
  }
}

export function clearLocalToken(storage: Storage = sessionStorage): void {
  storage.removeItem(LOCAL_TOKEN_KEY);
}
