import { useState } from 'react';
import {
  Ago,
  Badge,
  Button,
  ConfirmButton,
  Countdown,
  EmptyState,
  Fields,
  Input,
  Readout,
  Timeline,
  ago,
  fmtCount,
  fmtUntil,
  parseTime,
  useNow,
  type FieldItem,
  type TimelineStep,
} from '../../ds';
import { GrantState, type Grant, type GrantSpec } from '../../gen/mycel/v1/grant_pb';
import { getApi } from '../../app/api';
import { canDecide } from '../../app/commands/core';
import { useEntity, useEntityList } from '../../app/events/entities';
import { grantsStore, nodesStore } from '../../app/events/stores';
import { inspect } from '../../app/shell/inspector';
import { useRights } from '../../app/session';
import { inspectAgent } from '../agents/AgentDetail';
import { inspectNode } from '../nodes/NodeDetail';
import { act, agentShort, canRevokeGrant, errText, useUserNames } from '../nodes/shared';
import s from './page.module.css';
import { grantKind, stateName } from './grants';

export function inspectGrant(id: string): void {
  inspect({ key: `grant:${id}`, title: `Grant · ${id}`, content: <GrantDetail id={id} /> });
}

/** specFields renders the full spec of one grant kind as label/value rows. */
export function specFields(spec: GrantSpec | undefined): FieldItem[] {
  const k = spec?.kind;
  switch (k?.case) {
    case 'ollama':
      return [
        { label: 'Models', value: k.value.models.length ? k.value.models.join(', ') : 'any model already present', mono: true },
        { label: 'Pull', value: k.value.allowPull ? 'allowed' : 'no' },
        { label: 'Delete', value: k.value.allowDelete ? 'allowed' : 'no' },
        { label: 'Rate', value: `${k.value.requestsPerMinute || 60} req/min${k.value.requestsPerMinute ? '' : ' (default)'}`, mono: true },
        { label: 'Max tokens', value: k.value.maxTokens ? fmtCount(k.value.maxTokens) : 'unlimited', mono: true },
      ];
    case 'gpu':
      return [
        { label: 'Device', value: String(k.value.device), mono: true },
        { label: 'MIG slice', value: k.value.migSlice, mono: true, hidden: !k.value.migSlice },
        { label: 'Mode', value: k.value.exclusive ? 'exclusive' : 'time-sliced' },
      ];
    case 'dockerHost':
      return [
        { label: 'Images', value: k.value.images.join(', '), mono: true },
        { label: 'GPU', value: k.value.gpu ? 'may use an active GPU grant' : 'no' },
        { label: 'CPUs', value: k.value.cpus ? String(k.value.cpus) : 'default', mono: true },
        {
          label: 'Memory',
          value: k.value.memoryBytes ? `${Math.round(Number(k.value.memoryBytes) / 2 ** 20)} MiB` : 'default',
          mono: true,
        },
      ];
    case 'mount':
      return [
        { label: 'Host path', value: k.value.hostPath, mono: true },
        { label: 'Guest path', value: k.value.guestPath, mono: true },
        { label: 'Access', value: k.value.readOnly ? 'read-only' : 'read-write' },
      ];
    case 'egress':
      return [{ label: 'Allow', value: k.value.allow.join(', '), mono: true }];
    default:
      return [{ label: 'Spec', value: 'unknown kind' }];
  }
}

/** grantSteps: the grant's lifecycle as Timeline steps (exported for tests). */
export function grantSteps(g: Grant): TimelineStep[] {
  const pending = g.state === GrantState.PENDING;
  const denied = g.state === GrantState.DENIED;
  const ended = g.state === GrantState.EXPIRED || g.state === GrantState.REVOKED;
  const when = (at: string | undefined) => (at ? <Ago at={at} /> : '—');
  const steps: TimelineStep[] = [
    {
      key: 'req',
      time: when(g.createdAt),
      actor: agentShort(g.requestedBy),
      title: 'requested',
      body: g.reason || undefined,
      kind: 'message',
    },
  ];
  steps.push({
    key: 'dec',
    time: pending || denied ? '—' : when(g.activatedAt),
    actor: g.decidedBy || (pending ? 'waiting' : 'hub'),
    title: pending ? 'decision pending' : denied ? 'denied' : g.decidedBy === 'policy' ? 'auto-granted' : 'approved',
    body: g.decisionNote || undefined,
    tone: denied ? 'alert' : pending ? 'muted' : 'default',
  });
  if (denied) return steps;
  steps.push({ key: 'att', time: when(g.activatedAt), actor: g.node, title: 'attached', tone: g.activatedAt ? 'default' : 'muted' });
  steps.push({
    key: 'end',
    time: g.state === GrantState.ACTIVE && g.expiresAt ? <Countdown to={g.expiresAt} warnUnderMs={5 * 60_000} /> : when(g.expiresAt),
    actor: g.state === GrantState.REVOKED ? g.decidedBy || 'hub' : 'hub',
    title: g.state === GrantState.REVOKED ? 'revoked' : ended ? 'expired' : 'expires',
    tone: ended ? 'default' : 'muted',
  });
  return steps;
}

/** GrantDetail is the inspector content for one grant; it reads the store by id and stays live. */
export function GrantDetail({ id }: { id: string }) {
  const rights = useRights();
  const g = useEntity(grantsStore, id);
  useEntityList(nodesStore); // canDecide reads node owners
  const userName = useUserNames(rights);
  const now = useNow();
  if (!g) return <EmptyState title="This grant is gone">It is not visible to you.</EmptyState>;
  const live = g.state === GrantState.ACTIVE;
  const usage = g.usage;
  return (
    <div className={s.detail}>
      <div className={s.readouts}>
        <Readout size="sm" label="Kind" value={grantKind(g)} />
        <Readout
          size="sm"
          label="State"
          value={stateName(g.state)}
          tone={g.state === GrantState.ACTIVE ? 'accent' : g.state === GrantState.PENDING ? 'alert' : 'muted'}
        />
        <Readout
          size="sm"
          label={live ? 'Expires in' : 'Duration'}
          value={live ? fmtUntil(g.expiresAt, now) : g.duration || '1h'}
          tone={live && parseTime(g.expiresAt) - now < 5 * 60_000 ? 'alert' : 'default'}
        />
      </div>

      <section className={s.section}>
        <h3 className={s.sectionTitle}>Grant</h3>
        <Fields
          items={[
            { label: 'Id', value: g.id, mono: true },
            {
              label: 'Subject',
              value: g.subject.startsWith('agent:') ? (
                <button type="button" className={s.rowButton} onClick={() => inspectAgent(g.subject)}>
                  {agentShort(g.subject)}
                </button>
              ) : (
                g.subject
              ),
              mono: true,
            },
            {
              label: 'Node',
              value: (
                <button type="button" className={s.rowButton} onClick={() => inspectNode(g.node)}>
                  {g.node}
                </button>
              ),
            },
            { label: 'Owner', value: userName(g.ownerId) },
            { label: 'Requester', value: agentShort(g.requestedBy), mono: true },
            { label: 'Channel', value: g.channel, mono: true, hidden: !g.channel },
            { label: 'Reason', value: g.reason },
            { label: 'Approval', value: g.requiresApproval ? 'required' : 'policy auto-grant' },
            { label: 'Note', value: g.decisionNote, hidden: !g.decisionNote },
          ]}
        />
      </section>

      <section className={s.section}>
        <h3 className={s.sectionTitle}>Spec · {grantKind(g)}</h3>
        <Fields items={specFields(g.spec)} />
      </section>

      <section className={s.section}>
        <h3 className={s.sectionTitle}>Usage</h3>
        {usage && (usage.requests > 0n || usage.tokens > 0n || usage.gpuSeconds > 0n) ? (
          <div className={s.readouts}>
            <Readout size="sm" label="Requests" value={Number(usage.requests)} format={fmtCount} />
            {g.spec?.kind.case === 'ollama' && <Readout size="sm" label="Tokens" value={Number(usage.tokens)} format={fmtCount} />}
            {g.spec?.kind.case === 'gpu' && (
              <Readout size="sm" label="GPU time" value={Number(usage.gpuSeconds) / 3600} decimals={2} unit="h" />
            )}
            <Readout size="sm" label="Reported" value={ago(usage.updatedAt, now)} />
          </div>
        ) : (
          <p className={s.note}>No broker usage reported.</p>
        )}
      </section>

      <section className={s.section}>
        <h3 className={s.sectionTitle}>Timeline</h3>
        <Timeline steps={grantSteps(g)} aria-label="Grant timeline" />
      </section>

      <GrantActions g={g} />
    </div>
  );
}

function GrantActions({ g }: { g: Grant }) {
  const rights = useRights();
  const [note, setNote] = useState('');
  const [approving, setApproving] = useState(false);
  const decide = g.state === GrantState.PENDING && canDecide(rights, g);
  const revoke = g.state === GrantState.ACTIVE && canRevokeGrant(rights, g);
  if (!decide && !revoke) return null;
  const noteField = <Input label="Note" value={note} onChange={(e) => setNote(e.target.value)} placeholder="optional" maxLength={1024} />;
  const approve = async () => {
    if (approving) return;
    setApproving(true);
    try {
      await act(() => getApi().rpc.grants.approveGrant({ id: g.id, note }), `Approved ${g.id}`);
    } catch {
      /* act toasted the failure */
    } finally {
      setApproving(false);
    }
  };
  return (
    <section className={s.section}>
      <h3 className={s.sectionTitle}>{decide ? 'Decide' : 'Manage'}</h3>
      {decide && noteField}
      <div className={s.actions}>
        {decide && (
          <>
            <Button variant="primary" size="sm" busy={approving} onClick={() => void approve()}>
              Approve {g.duration || '1h'}
            </Button>
            <ConfirmButton
              size="sm"
              variant="warn"
              tone="alert"
              describeError={errText}
              confirmTitle={`Deny ${g.id}?`}
              confirmBody={`${grantKind(g)} for ${agentShort(g.subject)} on ${g.node}.`}
              action="Deny"
              onConfirm={() => act(() => getApi().rpc.grants.denyGrant({ id: g.id, note }), `Denied ${g.id}`)}
            >
              Deny
            </ConfirmButton>
          </>
        )}
        {revoke && (
          <ConfirmButton
            size="sm"
            variant="warn"
            tone="alert"
            describeError={errText}
            confirmTitle={`Revoke ${g.id}?`}
            confirmBody={`The broker detaches ${grantKind(g)} from ${agentShort(g.subject)} on ${g.node} at once.`}
            extra={noteField}
            action="Revoke grant"
            onConfirm={() => act(() => getApi().rpc.grants.revokeGrant({ id: g.id, note }), `Revoked ${g.id}`)}
          >
            Revoke
          </ConfirmButton>
        )}
        {g.state === GrantState.PENDING && <Badge tone="alert">waiting</Badge>}
      </div>
    </section>
  );
}
