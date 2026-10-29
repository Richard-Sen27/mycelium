import { useCallback, useEffect, useState, type FormEvent, type ReactNode } from 'react';
import { Button, Checkbox, ConfirmButton, EmptyState, Input, Select, Skeleton } from '../../ds';
import { Role, type User } from '../../gen/mycel/v1/common_pb';
import type { JoinToken } from '../../gen/mycel/v1/identity_pb';
import type { Member } from '../../gen/mycel/v1/channel_pb';
import { ChannelType } from '../../gen/mycel/v1/channel_pb';
import { getApi } from '../../app/api';
import { inspect, useInspector } from '../../app/shell/inspector';
import { useRights } from '../../app/session';
import { act, errText } from '../nodes/shared';
import s from '../nodes/page.module.css';
import a from './Admin.module.css';

// Forms behind the admin palette commands (views/admin/commands.ts): the
// CLI verbs that have no view of their own (`mycel user add|rm`, `mycel
// token create|revoke`, `mycel channels create|add|rm`, `mycel artifacts
// rm`). Loaded only when a command runs; each opens in the inspector.

export type AdminForm =
  'user.add' | 'user.remove' | 'token.create' | 'token.revoke' | 'channel.create' | 'channel.members' | 'artifact.remove';

const TITLES: Record<AdminForm, string> = {
  'user.add': 'Add a user',
  'user.remove': 'Remove a user',
  'token.create': 'Create a join token',
  'token.revoke': 'Revoke a join token',
  'channel.create': 'Create a channel',
  'channel.members': 'Channel members',
  'artifact.remove': 'Delete an artifact',
};

/** openAdminForm shows one of the admin forms in the inspector. */
export function openAdminForm(which: AdminForm, arg = ''): void {
  let content: ReactNode;
  switch (which) {
    case 'user.add':
      content = <UserAddForm />;
      break;
    case 'user.remove':
      content = <UserRemoveForm />;
      break;
    case 'token.create':
      content = <TokenCreateForm />;
      break;
    case 'token.revoke':
      content = <TokenRevokeForm />;
      break;
    case 'channel.create':
      content = <ChannelCreateForm />;
      break;
    case 'channel.members':
      content = <ChannelMembersForm channel={arg} />;
      break;
    case 'artifact.remove':
      content = <ArtifactRemoveForm />;
      break;
  }
  inspect({ key: `admin:${which}:${arg}`, title: arg ? `${TITLES[which]} · ${arg}` : TITLES[which], content });
}

function Actions({ children }: { children: ReactNode }) {
  const close = useInspector((st) => st.close);
  return (
    <div className={s.formActions}>
      <Button variant="ghost" type="button" onClick={close}>
        Close
      </Button>
      {children}
    </div>
  );
}

/** Secret shows a one-time secret (API key, join token) to copy now. */
function Secret({ label, value }: { label: string; value: string }) {
  return (
    <Input
      label={label}
      value={value}
      readOnly
      mono
      hint="Shown once: copy it now; the hub keeps only a hash."
      onFocus={(e) => e.currentTarget.select()}
    />
  );
}

/** useSubmit wraps a form's write: busy flag, error line, toast. */
function useSubmit() {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const run = useCallback(async (fn: () => Promise<unknown>, ok: string, body?: string) => {
    setBusy(true);
    setError(null);
    try {
      await act(fn, ok, body);
      return true;
    } catch (err) {
      setError(errText(err));
      return false;
    } finally {
      setBusy(false);
    }
  }, []);
  return { busy, error, run };
}

/** UserAddForm mirrors `mycel user add <name> --role`; the new API key is shown once. */
export function UserAddForm() {
  const [name, setName] = useState('');
  const [role, setRole] = useState<'member' | 'admin'>('member');
  const [key, setKey] = useState<string | null>(null);
  const { busy, error, run } = useSubmit();
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    let apiKey = '';
    const ok = await run(
      async () => {
        const r = await getApi().rpc.users.createUser({ name: name.trim(), role: role === 'admin' ? Role.ADMIN : Role.MEMBER });
        apiKey = r.apiKey;
      },
      `Added ${name.trim()}`,
      role,
    );
    if (ok) setKey(apiKey);
  };
  if (key) {
    return (
      <div className={s.form}>
        <Secret label={`API key of ${name.trim()}`} value={key} />
        <Actions>{null}</Actions>
      </div>
    );
  }
  return (
    <form className={s.form} onSubmit={(e) => void submit(e)} aria-label="Add a user">
      <Input label="Name" value={name} onChange={(e) => setName(e.target.value)} required autoFocus />
      <Select<'member' | 'admin'>
        label="Role"
        value={role}
        onValueChange={setRole}
        options={[
          { value: 'member', label: 'member' },
          { value: 'admin', label: 'admin' },
        ]}
      />
      {error && <p className={s.formError}>{error}</p>}
      <Actions>
        <Button type="submit" variant="primary" busy={busy} disabled={!name.trim()}>
          Add user
        </Button>
      </Actions>
    </form>
  );
}

function useList<T>(load: () => Promise<T[]>): { rows: T[] | null; error: string | null; reload: () => void } {
  const [rows, setRows] = useState<T[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [n, setN] = useState(0);
  useEffect(() => {
    let live = true;
    load()
      .then((r) => live && setRows(r))
      .catch((err: unknown) => {
        if (!live) return;
        setError(errText(err));
        setRows([]);
      });
    return () => {
      live = false;
    };
  }, [load, n]);
  return { rows, error, reload: () => setN((x) => x + 1) };
}

const loadUsers = async (): Promise<User[]> => (await getApi().rpc.users.listUsers({})).users;

/** UserRemoveForm mirrors `mycel user rm <id>`. */
export function UserRemoveForm() {
  const { rows, error, reload } = useList(loadUsers);
  const self = useRights().userId;
  if (rows === null) return <Skeleton rows={3} />;
  if (error) return <EmptyState title="Could not list users">{error}</EmptyState>;
  return (
    <div className={s.form}>
      <ul className={a.list} aria-label="Users">
        {rows
          .filter((u) => u.id !== self)
          .map((u) => (
            <li key={u.id}>
              <span className="mono">{u.name}</span> <span className={a.meta}>{u.role === Role.ADMIN ? 'admin' : 'member'}</span>{' '}
              <ConfirmButton
                size="sm"
                variant="warn"
                confirmTitle={`Remove ${u.name}?`}
                confirmBody="Their API keys stop working at once. A user who still owns nodes cannot be removed."
                action="Remove user"
                tone="alert"
                describeError={errText}
                onConfirm={async () => {
                  await act(() => getApi().rpc.users.removeUser({ id: u.id }), `Removed ${u.name}`);
                  reload();
                }}
              >
                Remove
              </ConfirmButton>
            </li>
          ))}
      </ul>
      <Actions>{null}</Actions>
    </div>
  );
}

const CAPS = ['docker', 'ollama', 'gpu', 'sandbox'] as const;
const TTLS = ['15m', '1h', '4h', '24h'] as const;

/** TokenCreateForm mirrors `mycel token create --name --caps --ttl`; the token is shown once. */
export function TokenCreateForm() {
  const [name, setName] = useState('');
  const [caps, setCaps] = useState<string[]>([]);
  const [ttl, setTtl] = useState<(typeof TTLS)[number]>('15m');
  const [token, setToken] = useState<string | null>(null);
  const { busy, error, run } = useSubmit();
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    let tok = '';
    const ok = await run(
      async () => {
        const r = await getApi().rpc.tokens.createJoinToken({ nodeName: name.trim(), caps: { names: caps }, ttl });
        tok = r.token;
      },
      `Join token for ${name.trim()}`,
      `valid for ${ttl}`,
    );
    if (ok) setToken(tok);
  };
  if (token) {
    return (
      <div className={s.form}>
        <Secret label={`Join token for ${name.trim()}`} value={token} />
        <p className={s.note}>
          On the new machine: <span className="mono">mycel join &lt;token&gt;</span>
        </p>
        <Actions>{null}</Actions>
      </div>
    );
  }
  return (
    <form className={s.form} onSubmit={(e) => void submit(e)} aria-label="Create a join token">
      <Input label="Node name" value={name} onChange={(e) => setName(e.target.value)} required autoFocus mono />
      <div className={s.checks} role="group" aria-label="Capabilities">
        {CAPS.map((c) => (
          <Checkbox
            key={c}
            label={c}
            checked={caps.includes(c)}
            onCheckedChange={(v) => setCaps((cur) => (v ? [...cur, c] : cur.filter((x) => x !== c)))}
          />
        ))}
      </div>
      <Select<(typeof TTLS)[number]>
        label="Valid for"
        value={ttl}
        onValueChange={setTtl}
        options={TTLS.map((v) => ({ value: v, label: v }))}
      />
      {error && <p className={s.formError}>{error}</p>}
      <Actions>
        <Button type="submit" variant="primary" busy={busy} disabled={!name.trim()}>
          Create token
        </Button>
      </Actions>
    </form>
  );
}

const loadTokens = async (): Promise<JoinToken[]> =>
  (await getApi().rpc.tokens.listJoinTokens({})).tokens.filter((t) => !t.usedAt && !t.revokedAt);

/** TokenRevokeForm mirrors `mycel token revoke <id>` for unused tokens. */
export function TokenRevokeForm() {
  const { rows, error, reload } = useList(loadTokens);
  if (rows === null) return <Skeleton rows={3} />;
  if (error) return <EmptyState title="Could not list join tokens">{error}</EmptyState>;
  if (rows.length === 0) return <EmptyState title="No unused join tokens" />;
  return (
    <div className={s.form}>
      <ul className={a.list} aria-label="Unused join tokens">
        {rows.map((t) => (
          <li key={t.id}>
            <span className="mono">{t.nodeName}</span> <span className={a.meta}>expires {t.expiresAt}</span>{' '}
            <ConfirmButton
              size="sm"
              variant="warn"
              confirmTitle={`Revoke the token for ${t.nodeName}?`}
              action="Revoke token"
              tone="alert"
              describeError={errText}
              onConfirm={async () => {
                await act(() => getApi().rpc.tokens.revokeJoinToken({ id: t.id }), `Revoked the token for ${t.nodeName}`);
                reload();
              }}
            >
              Revoke
            </ConfirmButton>
          </li>
        ))}
      </ul>
      <Actions>{null}</Actions>
    </div>
  );
}

/** ChannelCreateForm mirrors `mycel channels create <name> [--type] [--open]`. */
export function ChannelCreateForm() {
  const [name, setName] = useState('#');
  const [type, setType] = useState<'topic' | 'task'>('topic');
  const [open, setOpen] = useState(false);
  const { busy, error, run } = useSubmit();
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    const n = name.trim().startsWith('#') ? name.trim() : `#${name.trim()}`;
    const ok = await run(
      () =>
        getApi().rpc.channels.createChannel({
          name: n,
          type: type === 'task' ? ChannelType.TASK : ChannelType.TOPIC,
          policy: open ? { openToMembers: true, agentsMayPost: true, humansMayPost: true } : undefined,
        }),
      `Created ${n}`,
    );
    if (ok) useInspector.getState().close();
  };
  return (
    <form className={s.form} onSubmit={(e) => void submit(e)} aria-label="Create a channel">
      <Input label="Name" value={name} onChange={(e) => setName(e.target.value)} required autoFocus mono />
      <Select<'topic' | 'task'>
        label="Type"
        value={type}
        onValueChange={setType}
        options={[
          { value: 'topic', label: 'topic' },
          { value: 'task', label: 'task' },
        ]}
      />
      <Checkbox label="Open to every member" checked={open} onCheckedChange={setOpen} />
      {error && <p className={s.formError}>{error}</p>}
      <Actions>
        <Button type="submit" variant="primary" busy={busy} disabled={name.trim().replace(/^#/, '') === ''}>
          Create channel
        </Button>
      </Actions>
    </form>
  );
}

/** ChannelMembersForm mirrors `mycel channels add|rm <channel> <member>`. */
export function ChannelMembersForm({ channel }: { channel: string }) {
  const load = useCallback(
    async (): Promise<Member[]> => (await getApi().rpc.channels.getChannel({ name: channel })).channel?.members ?? [],
    [channel],
  );
  const { rows, error, reload } = useList(load);
  const [member, setMember] = useState('');
  const { busy, error: addError, run } = useSubmit();
  const add = async (e: FormEvent) => {
    e.preventDefault();
    if (await run(() => getApi().rpc.channels.addMember({ channel, member: member.trim() }), `Added ${member.trim()} to ${channel}`)) {
      setMember('');
      reload();
    }
  };
  if (rows === null) return <Skeleton rows={3} />;
  if (error) return <EmptyState title="Could not read the channel">{error}</EmptyState>;
  return (
    <form className={s.form} onSubmit={(e) => void add(e)} aria-label={`Members of ${channel}`}>
      <ul className={a.list} aria-label="Members">
        {rows.map((m) => (
          <li key={m.id}>
            <span className="mono">{m.id}</span> <span className={a.meta}>{m.role}</span>{' '}
            {m.role !== 'owner' && (
              <ConfirmButton
                size="sm"
                variant="warn"
                confirmTitle={`Remove ${m.id} from ${channel}?`}
                action="Remove member"
                tone="alert"
                describeError={errText}
                onConfirm={async () => {
                  await act(() => getApi().rpc.channels.removeMember({ channel, member: m.id }), `Removed ${m.id}`);
                  reload();
                }}
              >
                Remove
              </ConfirmButton>
            )}
          </li>
        ))}
      </ul>
      <Input
        label="Add member"
        value={member}
        onChange={(e) => setMember(e.target.value)}
        mono
        placeholder="agent:name@node or user:usr_…"
      />
      {addError && <p className={s.formError}>{addError}</p>}
      <Actions>
        <Button type="submit" variant="primary" busy={busy} disabled={!member.trim()}>
          Add member
        </Button>
      </Actions>
    </form>
  );
}

/** ArtifactRemoveForm mirrors `mycel artifacts rm <id>`, after a confirmation. */
export function ArtifactRemoveForm() {
  const [id, setId] = useState('');
  const art = id.trim();
  return (
    <form className={s.form} onSubmit={(e) => e.preventDefault()} aria-label="Delete an artifact">
      <Input label="Artifact id" value={id} onChange={(e) => setId(e.target.value)} required autoFocus mono placeholder="art_…" />
      <Actions>
        <ConfirmButton
          variant="warn"
          disabled={!art.startsWith('art_')}
          confirmTitle={`Delete artifact ${art}?`}
          confirmBody="It cannot be recovered."
          action="Delete artifact"
          tone="alert"
          describeError={errText}
          onConfirm={async () => {
            await act(() => getApi().rpc.artifacts.deleteArtifact({ id: art }), `Deleted ${art}`);
            setId('');
          }}
        >
          Delete artifact
        </ConfirmButton>
      </Actions>
    </form>
  );
}
