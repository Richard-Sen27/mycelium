import { create } from 'zustand';
import type { Me } from './api/http';
import type { Mode, UIConfig } from './config';

/**
 * Rights is what the UI may offer. The server decides; this only hides
 * actions that would be refused. Node mode is read-only for the network
 * (the proxy forwards read methods only) and restrict-only for "This machine".
 */
export interface Rights {
  mode: Mode;
  isAdmin: boolean;
  /** True in node mode: everything except "This machine" is read-only. */
  readOnly: boolean;
  userId: string | null;
  userName: string | null;
}

export type SessionStatus = 'booting' | 'signed-out' | 'signed-in' | 'error';

export interface SessionState {
  status: SessionStatus;
  error: string | null;
  config: UIConfig | null;
  /** Hub mode: GET /api/auth/me (includes the CSRF token and passkeys). */
  me: Me | null;
  rights: Rights;
}

export const anonymousRights: Rights = { mode: 'hub', isAdmin: false, readOnly: true, userId: null, userName: null };

export const useSession = create<SessionState>(() => ({
  status: 'booting',
  error: null,
  config: null,
  me: null,
  rights: anonymousRights,
}));

/** csrfToken is read by the hub transport on every call. */
export function csrfToken(): string | null {
  return useSession.getState().me?.csrf ?? null;
}

export function useRights(): Rights {
  return useSession((s) => s.rights);
}

export type StreamState = 'idle' | 'connecting' | 'live' | 'retrying' | 'stopped';

export interface ConnectionState {
  /** The WatchEvents stream. */
  stream: StreamState;
  /** Node mode: whether the daemon's proxy reaches the hub. */
  hub: 'ok' | 'stale';
  /** When the hub was last reached (RFC 3339), for the stale banner. */
  lastOkAt: string | null;
}

export const useConnection = create<ConnectionState>(() => ({ stream: 'idle', hub: 'ok', lastOkAt: null }));

export function setHubStatus(s: 'ok' | 'stale'): void {
  const cur = useConnection.getState();
  if (s === 'ok') {
    if (cur.hub !== 'ok' || !cur.lastOkAt || Date.now() - Date.parse(cur.lastOkAt) > 5000) {
      useConnection.setState({ hub: 'ok', lastOkAt: new Date().toISOString() });
    }
  } else if (cur.hub !== 'stale') {
    useConnection.setState({ hub: 'stale' });
  }
}
