import { createBrowserRouter, isRouteErrorResponse, Navigate, useRouteError, type RouteObject } from 'react-router';
import { Button, EmptyState, Frame, Skeleton } from '../ds';
import { startPath } from './nav';
import { useRights } from './session';
import { Shell } from './shell/Shell';

// Every view is a lazily loaded route module exporting `Component`, so the
// initial load carries only the shell; the constellation module may pull in
// three.js / react-three-fiber, which then lands in its own chunk.

function NodeOnly({ children }: { children: React.ReactNode }) {
  const rights = useRights();
  if (rights.mode !== 'node') return <Navigate to="/" replace />;
  return <>{children}</>;
}

/** The start route: Approvals on a phone, the constellation elsewhere (decided at visit time). */
function StartRedirect() {
  return <Navigate to={startPath()} replace />;
}

function NotFound() {
  return (
    <Frame title="Not found" index="—">
      <EmptyState title="No view at this address" />
    </Frame>
  );
}

function RouteError() {
  const err = useRouteError();
  const msg = isRouteErrorResponse(err) ? `${err.status} ${err.statusText}` : err instanceof Error ? err.message : String(err);
  return (
    <Frame title="Error" index="—" state="alert">
      <EmptyState title="This view failed" action={<Button onClick={() => location.reload()}>Reload</Button>}>
        {msg}
      </EmptyState>
    </Frame>
  );
}

function Loading() {
  return (
    <Frame title="Loading" index="—">
      <Skeleton rows={4} />
    </Frame>
  );
}

export const routes: RouteObject[] = [
  {
    path: '/',
    Component: Shell,
    HydrateFallback: Loading,
    children: [
      {
        ErrorBoundary: RouteError,
        children: [
          { index: true, Component: StartRedirect },
          { path: 'constellation/*', lazy: () => import('../views/constellation') },
          { path: 'channels/*', lazy: () => import('../views/channels') },
          { path: 'approvals/*', lazy: () => import('../views/approvals') },
          { path: 'jobs/*', lazy: () => import('../views/jobs') },
          { path: 'grants/*', lazy: () => import('../views/grants') },
          { path: 'nodes/*', lazy: () => import('../views/nodes') },
          { path: 'agents/*', lazy: () => import('../views/agents') },
          { path: 'traces/*', lazy: () => import('../views/traces') },
          {
            path: 'machine/*',
            lazy: async () => {
              const m = await import('../views/machine');
              const View = m.Component;
              return {
                Component: () => (
                  <NodeOnly>
                    <View />
                  </NodeOnly>
                ),
              };
            },
          },
          { path: 'settings', lazy: () => import('./settings/Settings') },
          { path: '*', Component: NotFound },
        ],
      },
    ],
  },
];

export function makeRouter() {
  return createBrowserRouter(routes);
}
