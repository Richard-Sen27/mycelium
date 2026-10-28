import { Code, ConnectError } from '@connectrpc/connect';
import { getApi, makeHubApi, makeNodeApi, maybeApi, setApi, type Api } from './api';
import { HttpError, isAdminRole, type Me } from './api/http';
import { fetchUIConfig, type UIConfig } from './config';
import { allEntityStores, configureEntityStores } from './events/entities';
import { dispatchEvent, resetStores, useFreeze } from './events/stores';
import { EventStream } from './events/stream';
import { clearLocalToken, readLocalToken, storeLocalToken, takeFragmentToken } from './localToken';
import { disablePush, dropLocalPushSubscription } from './pwa/push';
import { clearSnapshots } from './snapshot';
import { anonymousRights, csrfToken, setHubStatus, useConnection, useSession, type Rights } from './session';

// Boot: detect the mode, sign in, start the event stream. Everything that
// changes who the app is talking to goes through here.

let stream: EventStream | null = null;

export async function boot(): Promise<void> {
  // Strip a #local=<token> fragment before anything else, whatever the mode,
  // so the token never lingers in the address bar or history.
  const fragmentToken = takeFragmentToken();
  let config: UIConfig;
  try {
    config = await fetchUIConfig();
  } catch (err) {
    useSession.setState({ status: 'error', error: (err as Error).message });
    return;
  }
  useSession.setState({ config });

  if (config.mode === 'node') {
    if (fragmentToken) storeLocalToken(fragmentToken);
    configureEntityStores({ persist: true, scope: config.hub ?? '' });
    const api = makeNodeApi({ getToken: () => readLocalToken(), onHubStatus: setHubStatus });
    setApi(api);
    if (!readLocalToken()) {
      useSession.setState({ status: 'signed-out', rights: { ...anonymousRights, mode: 'node' } });
      return;
    }
    await enterNode(api, config);
    return;
  }

  configureEntityStores({ persist: false });
  const api = makeHubApi({ getCsrf: csrfToken, onUnauthenticated: sessionExpired });
  setApi(api);
  try {
    const me = await api.hub!.me();
    enterHub(me);
  } catch (err) {
    if (err instanceof HttpError && err.status === 401) {
      useSession.setState({ status: 'signed-out', rights: anonymousRights });
    } else {
      useSession.setState({ status: 'error', error: (err as Error).message });
    }
  }
}

function rightsFromMe(me: Me): Rights {
  return { mode: 'hub', isAdmin: isAdminRole(me.role), readOnly: false, userId: me.user_id, userName: me.name };
}

/** enterHub finishes a hub sign-in (boot, API key or passkey). */
export function enterHub(me: Me): void {
  useSession.setState({ status: 'signed-in', error: null, me, rights: rightsFromMe(me) });
  startStream();
}

/** refreshMe reloads /api/auth/me (after adding or deleting a passkey). */
export async function refreshMe(): Promise<void> {
  const api = getApi();
  if (!api.hub) return;
  const me = await api.hub.me();
  useSession.setState({ me, rights: rightsFromMe(me) });
}

async function enterNode(api: Api, config: UIConfig): Promise<void> {
  // The node owner's identity comes from the hub through the proxy; when the
  // hub is unreachable the UI still opens ("This machine" works locally).
  let rights: Rights = { mode: 'node', isAdmin: false, readOnly: true, userId: null, userName: config.node ?? null };
  try {
    const who = await api.rpc.users.whoami({});
    if (who.user) rights = { ...rights, userId: who.user.id, userName: who.user.name };
  } catch (err) {
    const ce = ConnectError.from(err);
    if (ce.code === Code.Unauthenticated) {
      clearLocalToken();
      useSession.setState({ status: 'signed-out', rights: { ...anonymousRights, mode: 'node' } });
      return;
    }
  }
  useSession.setState({ status: 'signed-in', error: null, me: null, rights });
  startStream();
  // When the hub comes back after a stale period, reload what is shown.
  let prev = useConnection.getState().hub;
  useConnection.subscribe((s) => {
    if (prev === 'stale' && s.hub === 'ok') {
      for (const st of allEntityStores()) if (st.use.getState().loaded) void st.reload();
      void useFreeze.getState().load();
      restartStream();
    }
    prev = s.hub;
  });
}

function startStream(): void {
  const api = maybeApi();
  if (!api || stream) return;
  void useFreeze.getState().load();
  stream = new EventStream(api.rpc.events, {
    onEvent: dispatchEvent,
    onTooOld: () => {
      for (const st of allEntityStores()) if (st.use.getState().loaded) void st.reload();
      void useFreeze.getState().load();
    },
    onState: (s) => useConnection.setState({ stream: s }),
    onFatal: () => {
      if (useSession.getState().config?.mode === 'hub') sessionExpired();
    },
  });
  stream.start();
}

function restartStream(): void {
  const s = stream;
  stream = null;
  void s?.stop().then(startStream);
}

async function stopStream(): Promise<void> {
  const s = stream;
  stream = null;
  await s?.stop();
}

/** sessionExpired drops back to the sign-in screen (hub mode). */
export function sessionExpired(): void {
  const st = useSession.getState();
  if (st.status !== 'signed-in') return;
  void stopStream();
  // The hub no longer accepts this session, so it cannot delete the
  // subscription there; at least stop this browser receiving pushes.
  void dropLocalPushSubscription();
  resetStores();
  useSession.setState({ status: 'signed-out', me: null, rights: anonymousRights, error: 'Your session ended; sign in again.' });
}

/** signOut ends the session (hub) or forgets the local token (node). */
export async function signOut(): Promise<void> {
  const api = maybeApi();
  const mode = useSession.getState().config?.mode;
  await stopStream();
  if (mode === 'hub' && api?.hub) {
    // Stop pushes to this device first, while the session can still delete
    // the subscription on the hub (best effort).
    try {
      await disablePush(api.hub);
    } catch {
      await dropLocalPushSubscription();
    }
    try {
      await api.hub.logout();
    } catch {
      /* the session may already be gone */
    }
  }
  if (mode === 'node') {
    clearLocalToken();
    await clearSnapshots();
  }
  resetStores();
  useConnection.setState({ stream: 'idle' });
  useSession.setState({
    status: 'signed-out',
    me: null,
    error: null,
    rights: { ...anonymousRights, mode: mode ?? 'hub' },
  });
}
