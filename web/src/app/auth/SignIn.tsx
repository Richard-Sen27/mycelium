import { useState, type FormEvent } from 'react';
import { Button, Frame, Input } from '../../ds';
import { getApi } from '../api';
import { enterHub } from '../boot';
import { networkName } from '../config';
import { useSession } from '../session';
import { describeWebAuthnError, loginWithPasskey, passkeysSupported } from './webauthn';
import s from './SignIn.module.css';

/** SignIn: hub mode offers a passkey and the API-key form; node mode explains `mycel ui`. */
export function SignIn() {
  const config = useSession((st) => st.config);
  const notice = useSession((st) => st.error);
  if (config?.mode === 'node') return <NodeSignIn />;
  return (
    <div className={s.page}>
      <div className={s.column}>
        <Header subtitle={config ? networkName(config) : undefined} />
        <Frame title="Sign in" index="01" state="active">
          <HubSignIn notice={notice} passkeys={!!config?.passkeys} />
        </Frame>
        <p className={s.footnote}>
          No JavaScript? The approvals page at <a href="/ui/login">/ui/login</a> works without it.
        </p>
      </div>
    </div>
  );
}

function Header({ subtitle }: { subtitle?: string }) {
  return (
    <div className={s.header}>
      <svg className={s.mark} viewBox="0 0 40 40" aria-hidden>
        <circle cx="20" cy="20" r="13" fill="none" stroke="currentColor" strokeWidth="1" />
        <circle cx="20" cy="20" r="3" fill="currentColor" />
        <circle cx="31.2" cy="13.6" r="2.4" fill="var(--accent)" />
      </svg>
      <div>
        <h1 className={s.title}>Mycelium</h1>
        {subtitle && <div className={s.subtitle}>{subtitle}</div>}
      </div>
    </div>
  );
}

function HubSignIn({ notice, passkeys }: { notice: string | null; passkeys: boolean }) {
  const [key, setKey] = useState('');
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState<'key' | 'passkey' | null>(null);
  const canPasskey = passkeys && passkeysSupported();

  const finish = async () => {
    const me = await getApi().hub!.me();
    enterHub(me);
  };

  const onKey = async (e: FormEvent) => {
    e.preventDefault();
    if (!key.trim()) {
      setError('Paste your API key (`mycel user add` or `mycel hub init` printed it).');
      return;
    }
    setBusy('key');
    setError(null);
    try {
      await getApi().hub!.apiKeyLogin(key);
      setKey('');
      await finish();
    } catch (err) {
      setError((err as Error).message);
      setBusy(null);
    }
  };

  const onPasskey = async () => {
    setBusy('passkey');
    setError(null);
    try {
      await loginWithPasskey(getApi().hub!);
      await finish();
    } catch (err) {
      setError(describeWebAuthnError(err));
      setBusy(null);
    }
  };

  return (
    <div className={s.form}>
      {notice && !error && (
        <p className={s.notice} role="status">
          {notice}
        </p>
      )}
      {canPasskey && (
        <>
          <Button variant="primary" onClick={() => void onPasskey()} busy={busy === 'passkey'} disabled={busy !== null}>
            Sign in with a passkey
          </Button>
          <div className={s.or} aria-hidden>
            <span>or</span>
          </div>
        </>
      )}
      <form onSubmit={(e) => void onKey(e)} className={s.form} noValidate>
        <Input
          label="API key"
          type="password"
          mono
          autoComplete="current-password"
          value={key}
          onChange={(e) => setKey(e.target.value)}
          error={error ?? undefined}
          hint={!canPasskey ? 'After signing in, add a passkey under Settings.' : undefined}
          disabled={busy !== null}
        />
        <Button type="submit" variant={canPasskey ? 'default' : 'primary'} busy={busy === 'key'} disabled={busy !== null}>
          Sign in with the key
        </Button>
      </form>
    </div>
  );
}

function NodeSignIn() {
  const config = useSession((st) => st.config);
  return (
    <div className={s.page}>
      <div className={s.column}>
        <Header subtitle={config?.node ? `node ${config.node}` : undefined} />
        <Frame title="Local sign-in" index="01">
          <div className={s.form}>
            <p>This is the node UI. It signs in with the local token of the user running the daemon.</p>
            <p>
              Open it with <code className={s.code}>mycel ui</code> on this machine: the command reads the token and opens this page signed
              in.
            </p>
            <p className={s.muted}>The token lives only in this tab and is forgotten when the tab closes.</p>
          </div>
        </Frame>
      </div>
    </div>
  );
}
