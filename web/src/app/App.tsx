import { CSPProvider } from '@base-ui/react/csp-provider';
import { useMemo } from 'react';
import { RouterProvider } from 'react-router';
import { Button, EmptyState, Frame, Skeleton, ToastViewport } from '../ds';
import { SignIn } from './auth/SignIn';
import { makeRouter } from './routes';
import { useSession } from './session';
import s from './App.module.css';

/**
 * App gates on the session: a boot screen, sign-in, or the routed shell.
 * CSPProvider disables Base UI's inline <style> elements: the CSP allows
 * same-origin styles only (internal/web/web.go).
 */
export function App() {
  const status = useSession((st) => st.status);
  const error = useSession((st) => st.error);
  return (
    <CSPProvider disableStyleElements>
      {status === 'booting' && <Boot />}
      {status === 'error' && <BootError message={error} />}
      {status === 'signed-out' && (
        <>
          <SignIn />
          <ToastViewport />
        </>
      )}
      {status === 'signed-in' && <Routed />}
    </CSPProvider>
  );
}

function Routed() {
  // One router per sign-in, created at the current URL (deep links survive sign-in).
  const router = useMemo(() => makeRouter(), []);
  return <RouterProvider router={router} />;
}

function Boot() {
  return (
    <div className={s.center}>
      <Frame title="Mycelium" index="00" className={s.card}>
        <Skeleton rows={3} label="Starting" />
      </Frame>
    </div>
  );
}

function BootError({ message }: { message: string | null }) {
  return (
    <div className={s.center}>
      <Frame title="Mycelium" index="00" state="alert" className={s.card}>
        <EmptyState title="The interface could not start" action={<Button onClick={() => location.reload()}>Try again</Button>}>
          {message ?? 'Unknown error.'}
        </EmptyState>
      </Frame>
    </div>
  );
}
