import type { HubHttp } from '../api/http';
import { b64urlToBytes } from '../auth/webauthn';

// The PWA shell: service worker registration and the Web Push subscription
// (hub mode; the hub's VAPID key from GET /api/push/vapid). The worker
// itself (src/sw.ts) shows notifications and opens the approvals route.

export const SW_URL = '/sw.js';
export const NAVIGATE_MESSAGE = 'mycel:navigate';

export function swSupported(): boolean {
  return typeof navigator !== 'undefined' && 'serviceWorker' in navigator;
}

export function pushSupported(): boolean {
  return swSupported() && typeof window !== 'undefined' && 'PushManager' in window && 'Notification' in window;
}

/** registerServiceWorker registers /sw.js in production builds; failures are not fatal. */
export async function registerServiceWorker(): Promise<ServiceWorkerRegistration | null> {
  if (!swSupported() || import.meta.env.DEV) return null;
  try {
    return await navigator.serviceWorker.register(SW_URL, { scope: '/' });
  } catch (err) {
    console.warn('service worker registration failed', err);
    return null;
  }
}

/**
 * listenForNavigation forwards "open this route" messages from the service
 * worker (a tapped notification while the app is open) to the router.
 */
export function listenForNavigation(navigate: (to: string) => void): () => void {
  if (!swSupported()) return () => undefined;
  const onMsg = (e: MessageEvent) => {
    const d = e.data as { type?: string; url?: string } | null;
    if (d?.type === NAVIGATE_MESSAGE && typeof d.url === 'string' && /^\/(?!\/)/.test(d.url)) navigate(d.url);
  };
  navigator.serviceWorker.addEventListener('message', onMsg);
  return () => navigator.serviceWorker.removeEventListener('message', onMsg);
}

export type PushState = 'unsupported' | 'denied' | 'off' | 'on';

export async function pushState(): Promise<PushState> {
  if (!pushSupported()) return 'unsupported';
  if (Notification.permission === 'denied') return 'denied';
  const reg = await navigator.serviceWorker.getRegistration('/');
  if (!reg) return 'off';
  const sub = await reg.pushManager.getSubscription();
  return sub ? 'on' : 'off';
}

/** enablePush asks for permission, subscribes with the hub's VAPID key and registers the subscription. */
export async function enablePush(http: HubHttp): Promise<PushState> {
  if (!pushSupported()) return 'unsupported';
  const perm = await Notification.requestPermission();
  if (perm !== 'granted') return perm === 'denied' ? 'denied' : 'off';
  const reg = (await navigator.serviceWorker.getRegistration('/')) ?? (await registerServiceWorker());
  if (!reg) throw new Error('The service worker is not available.');
  await navigator.serviceWorker.ready;
  const { public_key } = await http.vapidKey();
  const sub =
    (await reg.pushManager.getSubscription()) ??
    (await reg.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey: b64urlToBytes(public_key) }));
  await http.pushSubscribe(sub.toJSON());
  return 'on';
}

export async function disablePush(http: HubHttp): Promise<PushState> {
  if (!pushSupported()) return 'unsupported';
  const reg = await navigator.serviceWorker.getRegistration('/');
  const sub = await reg?.pushManager.getSubscription();
  if (sub) {
    try {
      await http.pushUnsubscribe(sub.endpoint);
    } finally {
      await sub.unsubscribe();
    }
  }
  return 'off';
}

/** dropLocalPushSubscription unsubscribes this browser without telling the hub (session already gone). */
export async function dropLocalPushSubscription(): Promise<void> {
  if (!pushSupported()) return;
  try {
    const reg = await navigator.serviceWorker.getRegistration('/');
    await (await reg?.pushManager.getSubscription())?.unsubscribe();
  } catch {
    /* nothing subscribed */
  }
}
