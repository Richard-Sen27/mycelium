import { create } from '@bufbuild/protobuf';
import { useEffect, useState, type FormEvent } from 'react';
import { Button, Checkbox, Chips, EmptyState, Input, Select, Skeleton } from '../../ds';
import { SharingSettingsSchema, type SharingSettings } from '../../gen/mycel/v1/grant_pb';
import { getApi } from '../../app/api';
import { useEntity } from '../../app/events/entities';
import { nodesStore } from '../../app/events/stores';
import { inspectNode } from './NodeDetail';
import { useNodeSharing, useSharing } from './sharing';
import { act, errText } from './shared';
import s from './page.module.css';

export const CAPS = ['docker', 'ollama', 'gpu', 'sandbox'] as const;

function Back({ name }: { name: string }) {
  return (
    <Button variant="ghost" onClick={() => inspectNode(name)}>
      Back
    </Button>
  );
}

/** CapsForm: switch capabilities on or off within the join token's ceiling (`mycel nodes caps`). */
export function CapsForm({ name }: { name: string }) {
  const node = useEntity(nodesStore, name);
  const ceiling = node?.capsCeiling?.names ?? [];
  const [on, setOn] = useState<string[]>(() => node?.capsEnabled?.names ?? []);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  if (!node) return <EmptyState title="This node is gone" />;

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError(null);
    try {
      await act(() => getApi().rpc.nodes.setNodeCaps({ name, capsEnabled: { names: on } }), `Caps set on ${name}`, on.join(', ') || 'none');
      inspectNode(name);
    } catch (err) {
      setError(errText(err));
      setBusy(false);
    }
  };

  return (
    <form className={s.form} onSubmit={(e) => void submit(e)} aria-label={`Capabilities of ${name}`}>
      <p className={s.note}>Capabilities the node offers to the network. Only those in the join token's ceiling can be switched on.</p>
      <div className={s.checks}>
        {CAPS.map((c) => {
          const allowed = ceiling.includes(c);
          return (
            <Checkbox
              key={c}
              label={c}
              checked={on.includes(c)}
              disabled={!allowed}
              hint={allowed ? undefined : 'not in ceiling'}
              onCheckedChange={(v) => setOn((cur) => (v ? [...cur, c] : cur.filter((x) => x !== c)))}
            />
          );
        })}
      </div>
      {error && <p className={s.formError}>{error}</p>}
      <div className={s.formActions}>
        <Back name={name} />
        <Button type="submit" variant="primary" busy={busy}>
          Set caps
        </Button>
      </div>
    </form>
  );
}

const DAYS = ['mon', 'tue', 'wed', 'thu', 'fri', 'sat', 'sun'] as const;

interface SchedWindow {
  days: string[];
  from: string;
  to: string;
}

const HHMM = /^([01]\d|2[0-3]):[0-5]\d$/;

/** buildSharing turns the form state into SharingSettings (exported for tests). */
export function buildSharing(f: {
  offer: string[];
  when: string;
  schedule: SchedWindow[];
  localFirst: boolean;
  maxJobs: string;
  idleAfter: string;
}): SharingSettings {
  return create(SharingSettingsSchema, {
    offer: { names: f.offer },
    when: f.when,
    // "idle" with windows means idle AND inside a window (see SharingSettings).
    schedule: f.when === 'always' ? [] : f.schedule.map((w) => ({ days: w.days, from: w.from, to: w.to })),
    localFirst: f.localFirst,
    maxJobs: Number(f.maxJobs) || 0,
    idleAfterMinutes: f.when === 'idle' ? Number(f.idleAfter) || 0 : 0,
  });
}

/** SharingForm edits the owner's hub-side sharing settings (`mycel nodes share`). */
export function SharingForm({ name }: { name: string }) {
  const node = useEntity(nodesStore, name);
  const sharing = useNodeSharing(name);
  const hub = sharing?.value?.hub;
  if (!node) return <EmptyState title="This node is gone" />;
  if (!hub) return sharing?.error ? <p className={s.formError}>{sharing.error}</p> : <Skeleton rows={4} />;
  return <SharingFields name={name} ceiling={node.capsCeiling?.names ?? []} initial={hub} />;
}

function SharingFields({ name, ceiling, initial }: { name: string; ceiling: string[]; initial: SharingSettings }) {
  const [offer, setOffer] = useState<string[]>(initial.offer?.names ?? []);
  const [when, setWhen] = useState(initial.when || 'always');
  const [schedule, setSchedule] = useState<SchedWindow[]>(() =>
    initial.schedule.map((w) => ({ days: [...w.days], from: w.from, to: w.to })),
  );
  const [localFirst, setLocalFirst] = useState(initial.localFirst);
  const [maxJobs, setMaxJobs] = useState(initial.maxJobs ? String(initial.maxJobs) : '');
  const [idleAfter, setIdleAfter] = useState(initial.idleAfterMinutes ? String(initial.idleAfterMinutes) : '10');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => setError(null), [offer, when, schedule, maxJobs, idleAfter]);

  const invalid =
    (maxJobs !== '' && !/^\d+$/.test(maxJobs)) ||
    (when === 'idle' && !/^\d+$/.test(idleAfter)) ||
    (when === 'schedule' && schedule.length === 0) ||
    (when !== 'always' && schedule.some((w) => w.days.length === 0 || !HHMM.test(w.from) || !HHMM.test(w.to)));

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    if (invalid) {
      setError('Check the highlighted fields.');
      return;
    }
    setBusy(true);
    try {
      const settings = buildSharing({ offer, when, schedule, localFirst, maxJobs, idleAfter });
      await act(async () => {
        const res = await getApi().rpc.control.setNodeSharing({ name, settings });
        if (res.sharing) useSharing.getState().put(name, res.sharing);
      }, `Sharing set on ${name}`);
      inspectNode(name);
    } catch (err) {
      setError(errText(err));
      setBusy(false);
    }
  };

  const setWin = (i: number, w: Partial<SchedWindow>) => setSchedule((cur) => cur.map((x, j) => (j === i ? { ...x, ...w } : x)));

  return (
    <form className={s.form} onSubmit={(e) => void submit(e)} aria-label={`Sharing of ${name}`}>
      <p className={s.note}>What this node offers to other members, and when. The node's local ceiling can only tighten this.</p>
      <div className={s.section}>
        <span className={s.legendLabel}>Offer</span>
        <div className={s.checks}>
          {CAPS.map((c) => (
            <Checkbox
              key={c}
              label={c}
              checked={offer.includes(c)}
              disabled={!ceiling.includes(c)}
              hint={ceiling.includes(c) ? undefined : 'not in ceiling'}
              onCheckedChange={(v) => setOffer((cur) => (v ? [...cur, c] : cur.filter((x) => x !== c)))}
            />
          ))}
        </div>
      </div>
      <Select
        label="When"
        value={when}
        onValueChange={(v) => {
          setWhen(v);
          if (v === 'schedule' && schedule.length === 0)
            setSchedule([{ days: ['mon', 'tue', 'wed', 'thu', 'fri'], from: '22:00', to: '07:00' }]);
        }}
        options={[
          { value: 'always', label: 'Always' },
          { value: 'idle', label: 'When the owner is idle' },
          { value: 'schedule', label: 'On a schedule' },
        ]}
      />
      {when === 'idle' && (
        <Input
          label="Idle after (minutes)"
          mono
          inputMode="numeric"
          value={idleAfter}
          onChange={(e) => setIdleAfter(e.target.value)}
          error={!/^\d+$/.test(idleAfter) ? 'Minutes.' : undefined}
        />
      )}
      {when === 'idle' && <p className={s.note}>Optionally, only inside these windows:</p>}
      {when !== 'always' &&
        schedule.map((w, i) => (
          <div key={i} className={s.section}>
            <span className={s.legendLabel}>Window {i + 1}</span>
            <Chips
              aria-label={`Days of window ${i + 1}`}
              options={DAYS.map((d) => ({ value: d, label: d }))}
              value={w.days}
              onChange={(days) => setWin(i, { days })}
            />
            <div className={s.formRow}>
              <Input
                label="From"
                mono
                value={w.from}
                onChange={(e) => setWin(i, { from: e.target.value })}
                error={HHMM.test(w.from) ? undefined : 'HH:MM'}
              />
              <Input
                label="To"
                mono
                value={w.to}
                onChange={(e) => setWin(i, { to: e.target.value })}
                error={HHMM.test(w.to) ? undefined : 'HH:MM'}
              />
            </div>
            {(schedule.length > 1 || when === 'idle') && (
              <div>
                <Button size="sm" variant="ghost" onClick={() => setSchedule((cur) => cur.filter((_, j) => j !== i))}>
                  Remove window
                </Button>
              </div>
            )}
          </div>
        ))}
      {when !== 'always' && (
        <div>
          <Button size="sm" onClick={() => setSchedule((cur) => [...cur, { days: ['sat', 'sun'], from: '00:00', to: '23:59' }])}>
            Add window
          </Button>
        </div>
      )}
      <div className={s.formRow}>
        <Input
          label="Max remote jobs"
          mono
          inputMode="numeric"
          placeholder="default"
          value={maxJobs}
          onChange={(e) => setMaxJobs(e.target.value)}
          error={maxJobs !== '' && !/^\d+$/.test(maxJobs) ? 'A number.' : undefined}
        />
      </div>
      <Checkbox label="Local first: throttle remote work while the owner is active" checked={localFirst} onCheckedChange={setLocalFirst} />
      {error && <p className={s.formError}>{error}</p>}
      <div className={s.formActions}>
        <Back name={name} />
        <Button type="submit" variant="primary" busy={busy}>
          Set sharing
        </Button>
      </div>
    </form>
  );
}
