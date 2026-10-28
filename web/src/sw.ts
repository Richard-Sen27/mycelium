/// <reference lib="webworker" />
// The service worker (built by vite-plugin-pwa, strategy injectManifest).
// It does three things and nothing else:
//   1. precaches the app shell so the UI opens offline (the node UI then
//      shows its last snapshot, the hub UI a sign-in or stale screen);
//   2. shows a notification for a Web Push from the hub. Payloads carry ids
//      and kinds only: {"type":"approval","grant_id","kind","node"} or
//      {"type":"freeze"} (internal/hub/uiapi.go);
//   3. opens (or focuses) the app on the approvals route when one is tapped.
import { cleanupOutdatedCaches, createHandlerBoundToURL, precacheAndRoute } from 'workbox-precaching';
import { NavigationRoute, registerRoute } from 'workbox-routing';

declare const self: ServiceWorkerGlobalScope;

precacheAndRoute(self.__WB_MANIFEST);
cleanupOutdatedCaches();

// Client-side routes fall back to the cached index.html; server routes never do.
registerRoute(
  new NavigationRoute(createHandlerBoundToURL('/index.html'), {
    denylist: [/^\/api\//, /^\/ui(\/|$)/, /^\/ca\.pem$/, /^\/healthz$/, /^\/sw\.js$/],
  }),
);

self.addEventListener('install', () => {
  void self.skipWaiting();
});
self.addEventListener('activate', (event) => {
  event.waitUntil(self.clients.claim());
});

interface PushPayload {
  type?: string;
  grant_id?: string;
  kind?: string;
  node?: string;
}

export function notificationFor(p: PushPayload): { title: string; body: string; url: string; tag: string } {
  if (p.type === 'approval') {
    const what = [p.kind, p.node ? `on ${p.node}` : ''].filter(Boolean).join(' ');
    const id = p.grant_id ?? '';
    return {
      title: 'Approval requested',
      body: what ? `${what} · ${id}` : id,
      url: id ? `/approvals?grant=${encodeURIComponent(id)}` : '/approvals',
      tag: `approval:${id}`,
    };
  }
  if (p.type === 'freeze') {
    return { title: 'Network frozen', body: 'Every sandbox and agent on the network is stopped.', url: '/approvals', tag: 'freeze' };
  }
  return { title: 'Mycelium', body: 'Something needs your attention.', url: '/approvals', tag: 'mycel' };
}

self.addEventListener('push', (event) => {
  let payload: PushPayload = {};
  try {
    payload = (event.data?.json() ?? {}) as PushPayload;
  } catch {
    payload = {};
  }
  const n = notificationFor(payload);
  event.waitUntil(
    self.registration.showNotification(n.title, {
      body: n.body,
      tag: n.tag,
      icon: '/icons/icon-192.png',
      badge: '/icons/badge-72.png',
      data: { url: n.url },
    }),
  );
});

self.addEventListener('notificationclick', (event) => {
  event.notification.close();
  const url = ((event.notification.data as { url?: string } | null)?.url ?? '/approvals').replace(/^(?!\/)/, '/');
  event.waitUntil(
    (async () => {
      const wins = await self.clients.matchAll({ type: 'window', includeUncontrolled: true });
      const win = wins.find((w) => new URL(w.url).origin === self.location.origin);
      if (win) {
        await win.focus();
        win.postMessage({ type: 'mycel:navigate', url });
        return;
      }
      await self.clients.openWindow(url);
    })(),
  );
});
