import { useCallback, useEffect, useState, type FormEvent } from 'react';
import { Ago, Badge, Button, Checkbox, ConfirmButton, EmptyState, Input, MessageBody, Skeleton, TextArea, toast } from '../../ds';
import { getApi } from '../../app/api';
import { useEntity } from '../../app/events/entities';
import { channelsStore } from '../../app/events/stores';
import { useRights } from '../../app/session';
import type { BlackboardEntry, ChannelPolicy } from '../../gen/mycel/v1/channel_pb';
import { canManageChannel, errText, parseSender } from './model';
import s from './Channels.module.css';

// Inspector panels for one channel: blackboard, members, policy. Each reads
// the channel row from the store by name so it stays live.

export function BlackboardPanel({ channel }: { channel: string }) {
  const rights = useRights();
  const ch = useEntity(channelsStore, channel);
  const [entries, setEntries] = useState<BlackboardEntry[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [open, setOpen] = useState<string | null>(null);
  const [key, setKey] = useState('');
  const [value, setValue] = useState('');
  const [busy, setBusy] = useState(false);
  const [putError, setPutError] = useState<string | null>(null);
  const canWrite = rights.mode === 'hub' && !rights.readOnly && !ch?.archivedAt;

  const load = useCallback(async () => {
    try {
      const r = await getApi().rpc.channels.blackboardGet({ channel, key: '' });
      setEntries([...r.entries].sort((a, b) => a.key.localeCompare(b.key)));
      setError(null);
    } catch (err) {
      setError(errText(err));
      setEntries([]);
    }
  }, [channel]);
  useEffect(() => {
    void load();
  }, [load]);

  const put = async (e: FormEvent) => {
    e.preventDefault();
    if (!key.trim()) return;
    setBusy(true);
    setPutError(null);
    try {
      const prev = entries?.find((x) => x.key === key.trim());
      await getApi().rpc.channels.blackboardPut({ channel, key: key.trim(), value, expectVersion: prev?.version ?? 0n });
      setKey('');
      setValue('');
      await load();
    } catch (err) {
      setPutError(errText(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className={s.panel}>
      <p className={s.panelNote}>Facts the channel's agents keep instead of repeating them in messages. Values are shown as plain text.</p>
      {entries === null ? (
        <Skeleton rows={3} />
      ) : error ? (
        <p className={s.error}>{error}</p>
      ) : entries.length === 0 ? (
        <EmptyState title="The blackboard is empty">Agents write here with the blackboard tools.</EmptyState>
      ) : (
        <ul className={s.bb}>
          {entries.map((e) => (
            <li key={e.key} className={s.bbEntry}>
              <button
                type="button"
                className={s.bbHead}
                onClick={() => setOpen(open === e.key ? null : e.key)}
                aria-expanded={open === e.key}
              >
                <span className={s.mono}>{e.key}</span>
                <span className={s.bbMeta}>
                  v{e.version.toString()} · {parseSender(e.updatedBy).name} · <Ago at={e.updatedAt} />
                </span>
              </button>
              {open === e.key ? (
                <div className={s.bbValue}>
                  <MessageBody text={e.value} untrusted={e.updatedBy.startsWith('agent:')} />
                  {canWrite && (
                    <div className={s.bbActions}>
                      <Button
                        size="sm"
                        variant="ghost"
                        onClick={() => {
                          setKey(e.key);
                          setValue(e.value);
                        }}
                      >
                        Edit
                      </Button>
                      <ConfirmButton
                        size="sm"
                        variant="warn"
                        confirmTitle={`Remove ${e.key}?`}
                        confirmBody={`The key is deleted from ${channel}'s blackboard for every member.`}
                        action="Remove"
                        tone="alert"
                        describeError={errText}
                        onConfirm={async () => {
                          await getApi().rpc.channels.blackboardDelete({ channel, key: e.key });
                          await load();
                        }}
                      >
                        Remove
                      </ConfirmButton>
                    </div>
                  )}
                </div>
              ) : (
                <MessageBody text={e.value} clamp={1} className={s.bbPreview} />
              )}
            </li>
          ))}
        </ul>
      )}
      {canWrite && (
        <form className={s.bbForm} onSubmit={(e) => void put(e)}>
          <Input label="Key" mono value={key} onChange={(e) => setKey(e.target.value)} placeholder="decision/db" />
          <TextArea label="Value" value={value} onChange={(e) => setValue(e.target.value)} rows={3} error={putError} />
          <Button type="submit" busy={busy} disabled={!key.trim()}>
            Put
          </Button>
        </form>
      )}
    </div>
  );
}

export function MembersPanel({ channel }: { channel: string }) {
  const ch = useEntity(channelsStore, channel);
  if (!ch) return <Skeleton rows={3} />;
  return (
    <div className={s.panel}>
      {ch.members.length === 0 ? (
        <EmptyState title="No members" />
      ) : (
        <ul className={s.members}>
          {ch.members.map((m) => {
            const who = parseSender(m.id);
            return (
              <li key={m.id} className={s.member}>
                <span className={s.mono}>
                  {who.name}
                  {who.node && <span className={s.node}>@{who.node}</span>}
                </span>
                <Badge tone={m.role === 'owner' ? 'ok' : m.role === 'observer' ? 'off' : 'idle'}>{m.role}</Badge>
              </li>
            );
          })}
        </ul>
      )}
    </div>
  );
}

interface PolicyDraft {
  maxHops: string;
  ratePerMinute: string;
  agentsMayPost: boolean;
  humansMayPost: boolean;
  openToMembers: boolean;
  allowedProfiles: string;
  retention: string;
  budgetTokens: string;
  budgetUsd: string;
}

function draftOf(p: ChannelPolicy | undefined): PolicyDraft {
  return {
    maxHops: p?.maxHops ? String(p.maxHops) : '',
    ratePerMinute: p?.ratePerMinute ? String(p.ratePerMinute) : '',
    agentsMayPost: p?.agentsMayPost ?? false,
    humansMayPost: p?.humansMayPost ?? false,
    openToMembers: p?.openToMembers ?? false,
    allowedProfiles: (p?.allowedProfiles ?? []).join(', '),
    retention: p?.retention ?? '',
    budgetTokens: p?.budget && Number(p.budget.tokens) > 0 ? String(p.budget.tokens) : '',
    budgetUsd: p?.budget && p.budget.usd > 0 ? String(p.budget.usd) : '',
  };
}

const intOrZero = (v: string) => {
  const n = parseInt(v, 10);
  return Number.isFinite(n) && n > 0 ? n : 0;
};

/** PolicyPanel edits a channel's policy (owner or admin; zero means "hub default"). */
export function PolicyPanel({ channel }: { channel: string }) {
  const rights = useRights();
  const ch = useEntity(channelsStore, channel);
  const [d, setD] = useState<PolicyDraft>(() => draftOf(ch?.policy));
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  if (!ch) return <Skeleton rows={4} />;
  const editable = canManageChannel(rights, ch);
  const set = <K extends keyof PolicyDraft>(k: K, v: PolicyDraft[K]) => setD((x) => ({ ...x, [k]: v }));

  const save = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError(null);
    try {
      await getApi().rpc.channels.updateChannelPolicy({
        name: channel,
        policy: {
          maxHops: intOrZero(d.maxHops),
          ratePerMinute: intOrZero(d.ratePerMinute),
          agentsMayPost: d.agentsMayPost,
          humansMayPost: d.humansMayPost,
          openToMembers: d.openToMembers,
          allowedProfiles: d.allowedProfiles
            .split(',')
            .map((x) => x.trim())
            .filter(Boolean),
          retention: d.retention.trim(),
          budget: { tokens: BigInt(intOrZero(d.budgetTokens)), usd: Math.max(0, parseFloat(d.budgetUsd) || 0) },
        },
      });
      toast({ title: `Policy saved for ${channel}` });
    } catch (err) {
      setError(errText(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <form className={s.panel} onSubmit={(e) => void save(e)}>
      <p className={s.panelNote}>
        Empty or zero fields use the hub default. The form shows the stored policy: an unchecked box is stored as false, which the hub reads
        as its default, so a policy with both "may post" boxes unchecked behaves like a new channel of this kind.{' '}
        {editable ? '' : 'Only the owner or an admin can change the policy.'}
      </p>
      <fieldset className={s.fieldset} disabled={!editable || busy}>
        <div className={s.formGrid}>
          <Input
            label="Max hops"
            mono
            inputMode="numeric"
            value={d.maxHops}
            onChange={(e) => set('maxHops', e.target.value)}
            placeholder="8"
          />
          <Input
            label="Rate / min / agent"
            mono
            inputMode="numeric"
            value={d.ratePerMinute}
            onChange={(e) => set('ratePerMinute', e.target.value)}
            placeholder="60"
          />
          <Input
            label="Budget tokens"
            mono
            inputMode="numeric"
            value={d.budgetTokens}
            onChange={(e) => set('budgetTokens', e.target.value)}
            placeholder="unlimited"
          />
          <Input
            label="Budget USD"
            mono
            inputMode="decimal"
            value={d.budgetUsd}
            onChange={(e) => set('budgetUsd', e.target.value)}
            placeholder="unlimited"
          />
          <Input
            label="Retention"
            mono
            value={d.retention}
            onChange={(e) => set('retention', e.target.value)}
            placeholder="keep"
            hint="Go duration, e.g. 720h"
          />
          <Input
            label="Allowed profiles"
            mono
            value={d.allowedProfiles}
            onChange={(e) => set('allowedProfiles', e.target.value)}
            placeholder="default, dev"
          />
        </div>
        <div className={s.checks}>
          <Checkbox label="Agents may post" checked={d.agentsMayPost} onCheckedChange={(v) => set('agentsMayPost', v)} />
          <Checkbox label="Humans may post" checked={d.humansMayPost} onCheckedChange={(v) => set('humansMayPost', v)} />
          <Checkbox label="Open to members" checked={d.openToMembers} onCheckedChange={(v) => set('openToMembers', v)} />
        </div>
      </fieldset>
      {error && (
        <p className={s.error} role="alert">
          {error}
        </p>
      )}
      {editable && (
        <div className={s.formActions}>
          <Button variant="ghost" onClick={() => setD(draftOf(ch.policy))} disabled={busy}>
            Reset
          </Button>
          <Button type="submit" variant="primary" busy={busy}>
            Save policy
          </Button>
        </div>
      )}
    </form>
  );
}
