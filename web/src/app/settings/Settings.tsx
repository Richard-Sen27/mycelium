import { useEffect, useState, type FormEvent } from 'react';
import { Badge, Button, Dialog, EmptyState, Frame, Input, Readout, Select, Table, toast, type Column } from '../../ds';
import { getApi } from '../api';
import type { Passkey } from '../api/http';
import { describeWebAuthnError, passkeysSupported, registerPasskey } from '../auth/webauthn';
import { refreshMe, signOut } from '../boot';
import { THEMES, usePrefs, type MotionPref, type ThemeId } from '../prefs';
import { disablePush, enablePush, pushState, type PushState } from '../pwa/push';
import { useRights, useSession } from '../session';
import s from './Settings.module.css';

/** Settings: passkeys, push notifications, display preferences and sign-out. */
export function Component() {
  const rights = useRights();
  const config = useSession((st) => st.config);
  // Frames are numbered in the order they render (some are hub-only).
  const showPasskeys = rights.mode === 'hub';
  const showPush = rights.mode === 'hub' && !!config?.push;
  let n = 1;
  const idx = () => String(n++).padStart(2, '0');
  const iAccount = idx();
  const iPasskeys = showPasskeys ? idx() : '';
  const iPush = showPush ? idx() : '';
  const iDisplay = idx();
  const iSession = idx();
  return (
    <div className={s.page}>
      <h1 className={s.heading}>Settings</h1>
      <div className={s.grid}>
        <Frame title="Account" index={iAccount}>
          <div className={s.readouts}>
            <Readout size="sm" label="User" value={rights.userName ?? '—'} />
            <Readout size="sm" label="Role" value={rights.mode === 'node' ? 'node owner' : rights.isAdmin ? 'admin' : 'member'} />
            <Readout size="sm" label="Interface" value={rights.mode === 'node' ? `node ${config?.node ?? ''}` : 'hub'} />
            {config?.version && <Readout size="sm" label="Version" value={config.version} />}
          </div>
          {rights.mode === 'node' && (
            <p className={s.note}>
              The node UI shows the network read-only and controls this machine within the limits set from the hub. Network-wide changes are
              made in the hub UI.
            </p>
          )}
        </Frame>
        {showPasskeys && <Passkeys index={iPasskeys} enabled={!!config?.passkeys} />}
        {showPush && <Push index={iPush} />}
        <Display index={iDisplay} />
        <Frame title="Session" index={iSession}>
          <div className={s.row}>
            <p className={s.note}>
              {rights.mode === 'hub'
                ? 'Ends this browser session on the hub.'
                : 'Forgets the local token in this tab. Run `mycel ui` to sign in again.'}
            </p>
            <Button variant="warn" onClick={() => void signOut()}>
              Sign out
            </Button>
          </div>
        </Frame>
      </div>
    </div>
  );
}

function fmtTime(t: string): string {
  if (!t) return '—';
  const d = new Date(t);
  return Number.isNaN(d.getTime()) ? t : d.toLocaleString();
}

function Passkeys({ enabled, index }: { enabled: boolean; index: string }) {
  const passkeys = useSession((st) => st.me?.passkeys ?? []);
  const [name, setName] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [toDelete, setToDelete] = useState<Passkey | null>(null);
  const [deleting, setDeleting] = useState(false);
  const supported = passkeysSupported();

  const add = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError(null);
    try {
      await registerPasskey(getApi().hub!, name.trim() || defaultPasskeyName());
      await refreshMe();
      setName('');
      toast({ title: 'Passkey added' });
    } catch (err) {
      setError(describeWebAuthnError(err));
    } finally {
      setBusy(false);
    }
  };

  const del = async () => {
    if (!toDelete) return;
    setDeleting(true);
    try {
      await getApi().hub!.passkeyDelete(toDelete.id);
      await refreshMe();
      toast({ title: `Removed passkey ${toDelete.name}` });
      setToDelete(null);
    } catch (err) {
      toast({ title: 'Could not remove the passkey', body: (err as Error).message, tone: 'alert' });
    } finally {
      setDeleting(false);
    }
  };

  const columns: Column<Passkey>[] = [
    { key: 'name', header: 'Name', cell: (p) => p.name || 'passkey' },
    { key: 'created', header: 'Added', cell: (p) => fmtTime(p.created_at), mono: true, hideOnNarrow: true },
    { key: 'used', header: 'Last used', cell: (p) => fmtTime(p.last_used_at), mono: true },
    {
      key: 'actions',
      header: <span className="visually-hidden">Actions</span>,
      align: 'right',
      width: '96px',
      cell: (p) => (
        <Button size="sm" variant="ghost" onClick={() => setToDelete(p)} aria-label={`Remove passkey ${p.name}`}>
          Remove
        </Button>
      ),
    },
  ];

  return (
    <Frame title="Passkeys" index={index} meta={<span>{passkeys.length}</span>} flush>
      {!enabled ? (
        <EmptyState title="Passkeys are off on this hub">An admin can turn them on in hub.yaml (ui.rp_id, ui.origins).</EmptyState>
      ) : (
        <>
          <Table
            aria-label="Passkeys"
            columns={columns}
            rows={passkeys}
            rowKey={(p) => p.id}
            empty={<EmptyState title="No passkeys yet">Add one to sign in without the API key, on this device or a phone.</EmptyState>}
          />
          <form className={s.add} onSubmit={(e) => void add(e)}>
            <Input
              label="New passkey name"
              placeholder={defaultPasskeyName()}
              value={name}
              onChange={(e) => setName(e.target.value)}
              maxLength={64}
              error={error ?? undefined}
              hint={supported ? undefined : 'This browser does not support passkeys.'}
              disabled={busy || !supported}
            />
            <Button type="submit" busy={busy} disabled={!supported}>
              Add a passkey
            </Button>
          </form>
        </>
      )}
      <Dialog
        open={toDelete !== null}
        onOpenChange={(o) => !o && setToDelete(null)}
        title="Remove this passkey?"
        description={`“${toDelete?.name ?? ''}” will no longer sign you in. Your other passkeys and API keys keep working.`}
        tone="alert"
        footer={
          <>
            <Button variant="ghost" onClick={() => setToDelete(null)}>
              Cancel
            </Button>
            <Button variant="warn" busy={deleting} onClick={() => void del()}>
              Remove
            </Button>
          </>
        }
      />
    </Frame>
  );
}

function defaultPasskeyName(): string {
  const ua = typeof navigator !== 'undefined' ? navigator.userAgent : '';
  if (/iPhone|iPad/.test(ua)) return 'iPhone';
  if (/Android/.test(ua)) return 'Android';
  if (/Mac/.test(ua)) return 'Mac';
  if (/Windows/.test(ua)) return 'Windows';
  return 'This device';
}

function Push({ index }: { index: string }) {
  const [state, setState] = useState<PushState | null>(null);
  const [busy, setBusy] = useState(false);
  useEffect(() => {
    void pushState().then(setState);
  }, []);
  const toggle = async () => {
    setBusy(true);
    try {
      const http = getApi().hub!;
      setState(state === 'on' ? await disablePush(http) : await enablePush(http));
    } catch (err) {
      toast({ title: 'Notifications', body: (err as Error).message, tone: 'alert' });
    } finally {
      setBusy(false);
    }
  };
  const label: Record<PushState, string> = {
    unsupported: 'Not supported in this browser',
    denied: 'Blocked in the browser settings',
    off: 'Off',
    on: 'On',
  };
  return (
    <Frame title="Notifications" index={index}>
      <div className={s.row}>
        <div className={s.stack}>
          <p className={s.note}>
            Push a notice to this device when a grant waits for your approval or the network is frozen. It carries ids only.
          </p>
          {state && <Badge tone={state === 'on' ? 'ok' : state === 'denied' ? 'alert' : 'idle'}>{label[state]}</Badge>}
        </div>
        <Button onClick={() => void toggle()} busy={busy} disabled={!state || state === 'unsupported' || state === 'denied'}>
          {state === 'on' ? 'Turn off' : 'Turn on'}
        </Button>
      </div>
    </Frame>
  );
}

function Display({ index }: { index: string }) {
  const motion = usePrefs((st) => st.motion);
  const theme = usePrefs((st) => st.theme);
  const setMotion = usePrefs((st) => st.setMotion);
  const setTheme = usePrefs((st) => st.setTheme);
  return (
    <Frame title="Display" index={index}>
      <div className={s.fields}>
        <Select<MotionPref>
          label="Motion"
          value={motion}
          onValueChange={setMotion}
          options={[
            { value: 'system', label: 'Follow the system setting' },
            { value: 'reduced', label: 'Reduced: no animation' },
            { value: 'full', label: 'Full' },
          ]}
        />
        <Select<ThemeId>
          label="Theme"
          value={theme}
          onValueChange={setTheme}
          options={THEMES.map((t) => ({ value: t.id, label: t.label }))}
        />
      </div>
    </Frame>
  );
}
