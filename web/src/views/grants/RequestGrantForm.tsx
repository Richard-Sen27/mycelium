import { useMemo, useState, type FormEvent } from 'react';
import { Button, Checkbox, Chips, Input, Select, TextArea, toast } from '../../ds';
import { GrantState } from '../../gen/mycel/v1/grant_pb';
import { getApi } from '../../app/api';
import { useEntityList } from '../../app/events/entities';
import { agentsStore, jobsStore } from '../../app/events/stores';
import { inspect, useInspector } from '../../app/shell/inspector';
import { useRights } from '../../app/session';
import { agentState } from '../agents/agents';
import { isActiveJob } from '../nodes/NodeDetail';
import { errText } from '../nodes/shared';
import s from './page.module.css';
import { inspectGrant } from './GrantDetail';
import { GRANT_KINDS, buildRequestGrant, emptyGrantForm, validateGrantForm, type GrantForm, type GrantKind } from './grants';

export function openRequestGrant(subject = ''): void {
  inspect({ key: 'grant:new', title: 'Request a grant', content: <RequestGrantForm subject={subject} /> });
}

const DURATIONS = ['30m', '1h', '4h', '8h', '24h'];

/** RequestGrantForm mirrors `mycel grants request` (kind-specific fields, subject the caller owns). */
export function RequestGrantForm({ subject = '' }: { subject?: string }) {
  const rights = useRights();
  const agents = useEntityList(agentsStore).items;
  const jobs = useEntityList(jobsStore).items;
  const close = useInspector((st) => st.close);
  const [f, setF] = useState<GrantForm>(() => emptyGrantForm('ollama', subject));
  const [tried, setTried] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const set = <K extends keyof GrantForm>(k: K, v: GrantForm[K]) => setF((cur) => ({ ...cur, [k]: v }));

  const subjects = useMemo(() => {
    const mine = (owner: string) => rights.isAdmin || owner === rights.userId;
    const a = agents
      .filter((x) => mine(x.ownerId) && agentState(x) !== 'stopped')
      .map((x) => ({ value: x.id, label: `${x.name}@${x.nodeName}` }))
      .sort((x, y) => x.label.localeCompare(y.label));
    const j = jobs
      .filter((x) => mine(x.ownerId) && isActiveJob(x))
      .map((x) => ({ value: `job:${x.id}`, label: `job ${x.spec?.name || x.id} on ${x.node || '…'}` }));
    return [...a, ...j];
  }, [agents, jobs, rights]);

  const errs = validateGrantForm(f);
  const shown = tried ? errs : {};

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setTried(true);
    if (Object.keys(errs).length) return;
    setBusy(true);
    setError(null);
    try {
      const res = await getApi().rpc.grants.requestGrant(buildRequestGrant(f));
      const g = res.grant;
      if (!g) {
        close();
        return;
      }
      toast({
        title:
          g.state === GrantState.ACTIVE
            ? `Grant ${g.id} is active`
            : g.state === GrantState.PENDING
              ? `Grant ${g.id} waits for approval`
              : `Grant ${g.id}`,
      });
      inspectGrant(g.id);
    } catch (err) {
      setError(errText(err));
      setBusy(false);
    }
  };

  return (
    <form className={s.form} onSubmit={(e) => void submit(e)} aria-label="Request a grant" noValidate>
      <Select<GrantKind>
        label="Kind"
        value={f.kind}
        onValueChange={(v) => set('kind', v)}
        options={GRANT_KINDS.map((k) => ({ value: k, label: k }))}
      />
      {subjects.length > 0 ? (
        <Select
          label="Subject"
          value={f.subject || null}
          onValueChange={(v) => set('subject', v)}
          options={subjects}
          placeholder="An agent or job you own"
        />
      ) : (
        <Input
          label="Subject"
          mono
          value={f.subject}
          onChange={(e) => set('subject', e.target.value)}
          placeholder="agent:<name>@<node> or job:<id>"
        />
      )}
      {shown.subject && <p className={s.formError}>{shown.subject}</p>}

      {f.kind === 'ollama' && (
        <>
          <Input
            label="Models"
            mono
            value={f.models}
            onChange={(e) => set('models', e.target.value)}
            placeholder="llama3.1:8b, qwen2.5 (empty = any present)"
          />
          <Checkbox label="Allow pulling models" checked={f.allowPull} onCheckedChange={(v) => set('allowPull', v)} />
        </>
      )}
      {f.kind === 'gpu' && (
        <>
          <Input
            label="Device"
            mono
            inputMode="numeric"
            value={f.device}
            onChange={(e) => set('device', e.target.value)}
            error={shown.device}
          />
          <Checkbox label="Exclusive (otherwise time-sliced)" checked={f.exclusive} onCheckedChange={(v) => set('exclusive', v)} />
        </>
      )}
      {f.kind === 'docker-host' && (
        <TextArea
          label="Images"
          mono
          rows={2}
          value={f.images}
          onChange={(e) => set('images', e.target.value)}
          placeholder="python:3.12, ghcr.io/acme/"
          error={shown.images}
        />
      )}
      {f.kind === 'mount' && (
        <>
          <div className={s.formRow}>
            <Input label="Host path" mono value={f.hostPath} onChange={(e) => set('hostPath', e.target.value)} error={shown.hostPath} />
            <Input label="Guest path" mono value={f.guestPath} onChange={(e) => set('guestPath', e.target.value)} error={shown.guestPath} />
          </div>
          <Checkbox label="Read-write (approval required)" checked={f.readWrite} onCheckedChange={(v) => set('readWrite', v)} />
        </>
      )}
      {f.kind === 'egress' && (
        <TextArea
          label="Allow"
          mono
          rows={2}
          value={f.allow}
          onChange={(e) => set('allow', e.target.value)}
          placeholder="pypi.org, *.github.com:443"
          error={shown.allow}
        />
      )}

      <div className={s.section}>
        <Input
          label="Duration"
          mono
          value={f.duration}
          onChange={(e) => set('duration', e.target.value)}
          error={shown.duration}
          hint="At most 24h."
        />
        <Chips
          aria-label="Duration presets"
          multiple={false}
          value={DURATIONS.includes(f.duration) ? [f.duration] : []}
          onChange={(v) => v[0] && set('duration', v[0])}
          options={DURATIONS.map((d) => ({ value: d, label: d }))}
        />
      </div>
      <Input
        label="Channel"
        mono
        value={f.channel}
        onChange={(e) => set('channel', e.target.value)}
        placeholder="#channel (optional, policy context)"
        error={shown.channel}
      />
      <TextArea label="Reason" rows={2} value={f.reason} onChange={(e) => set('reason', e.target.value)} error={shown.reason} />
      {error && <p className={s.formError}>{error}</p>}
      <div className={s.formActions}>
        <Button variant="ghost" onClick={close}>
          Cancel
        </Button>
        <Button type="submit" variant="primary" busy={busy}>
          Request grant
        </Button>
      </div>
    </form>
  );
}
