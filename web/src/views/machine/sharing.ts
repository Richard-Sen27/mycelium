import type { JsonValue } from '@bufbuild/protobuf';
import type { SharingSettings } from '../../gen/mycel/v1/grant_pb';

// The "This machine" sharing form's model. The daemon accepts a local
// ceiling only if it tightens the hub-side settings (internal/sharing
// Tightens): fewer offers, a subset of the active time, local_first kept,
// max_jobs not higher, idle_after not shorter. The form enforces the simple
// rules itself and leaves the window arithmetic to the daemon, whose 400
// message it shows.

export const CAPS = ['docker', 'ollama', 'gpu', 'sandbox'] as const;
export const DAYS = ['mon', 'tue', 'wed', 'thu', 'fri', 'sat', 'sun'] as const;
export const DEFAULT_IDLE_AFTER = 10;
export const DEFAULT_MAX_JOBS = 2;

export interface HubBase {
  offer: string[];
  when: SharingDraft['when'];
  localFirst: boolean;
  maxJobs: number;
  idleAfter: number;
}

/** hubBase is the hub-side settings with the daemon's defaults applied (zero means default there). */
export function hubBase(hub: SharingSettings): HubBase {
  return {
    offer: hub.offer ? [...hub.offer.names] : [...CAPS],
    when: whenOf(hub),
    localFirst: hub.localFirst,
    maxJobs: hub.maxJobs > 0 ? hub.maxJobs : DEFAULT_MAX_JOBS,
    idleAfter: hub.idleAfterMinutes > 0 ? hub.idleAfterMinutes : DEFAULT_IDLE_AFTER,
  };
}

export interface WindowDraft {
  days: string[];
  from: string;
  to: string;
}

export interface SharingDraft {
  offer: string[];
  when: 'always' | 'idle' | 'schedule';
  schedule: WindowDraft[];
  localFirst: boolean;
  maxJobs: number;
  idleAfter: number;
}

export function whenOf(s: SharingSettings | undefined): SharingDraft['when'] {
  const w = s?.when || 'always';
  return w === 'idle' || w === 'schedule' ? w : 'always';
}

export function draftOf(s: SharingSettings | undefined): SharingDraft {
  return {
    offer: [...(s?.offer?.names ?? [])],
    when: whenOf(s),
    schedule: (s?.schedule ?? []).map((w) => ({ days: [...w.days], from: w.from, to: w.to })),
    localFirst: !!s?.localFirst,
    maxJobs: s?.maxJobs ?? 0,
    idleAfter: s?.idleAfterMinutes ?? 0,
  };
}

/** toJson writes the draft as the daemon's SharingSettings JSON (proto field names). */
export function toJson(d: SharingDraft): JsonValue {
  const out: Record<string, JsonValue> = {
    offer: { names: [...d.offer].sort() },
    when: d.when,
    local_first: d.localFirst,
    max_jobs: d.maxJobs,
    idle_after_minutes: d.idleAfter,
  };
  if (d.when !== 'always' && d.schedule.length) out.schedule = d.schedule.map((w) => ({ days: w.days, from: w.from, to: w.to }));
  return out;
}

const HHMM = /^([01]\d|2[0-3]):[0-5]\d$|^24:00$/;

/**
 * problems lists why a draft would widen the hub-side settings or is
 * incomplete; an empty list means the form may submit. Window subsets are
 * checked by the daemon.
 */
export function problems(hub: SharingSettings, d: SharingDraft): string[] {
  const out: string[] = [];
  const base = hubBase(hub);
  const extra = d.offer.filter((o) => !base.offer.includes(o));
  if (extra.length) out.push(`The hub does not offer ${extra.join(', ')}; offers can only be removed here.`);
  const hw = base.when;
  if (hw === 'idle' && d.when !== 'idle') out.push('The hub shares only while you are idle; "idle" is the widest setting here.');
  if (hw === 'schedule' && d.when === 'always') out.push('The hub shares on a schedule; "always" would widen it.');
  if (hw === 'schedule' && d.schedule.length === 0) out.push('Add the windows to share in (inside the hub windows).');
  if (d.when === 'schedule' && d.schedule.length === 0) out.push('A schedule needs at least one window.');
  for (const [i, w] of d.schedule.entries()) {
    if (d.when === 'always') break;
    if (w.days.length === 0) out.push(`Window ${i + 1}: pick at least one day.`);
    if (!HHMM.test(w.from) || !HHMM.test(w.to)) out.push(`Window ${i + 1}: times are HH:MM.`);
  }
  if (base.localFirst && !d.localFirst) out.push('The hub keeps local-first on; it can only be switched off from the hub.');
  if (!Number.isInteger(d.maxJobs) || d.maxJobs < 1 || d.maxJobs > base.maxJobs)
    out.push(`Max jobs is 1 to ${base.maxJobs} (the hub's limit).`);
  if (!Number.isInteger(d.idleAfter) || d.idleAfter < base.idleAfter)
    out.push(`Idle after is at least ${base.idleAfter} minutes (the hub's setting).`);
  return out;
}

/** describe renders settings as one line for the hub/local/effective comparison. */
export function describe(s: SharingSettings | undefined): {
  offers: string;
  when: string;
  localFirst: string;
  maxJobs: string;
  idleAfter: string;
} {
  const w = whenOf(s);
  const windows = (s?.schedule ?? []).map((x) => `${x.days.join(',') || 'every day'} ${x.from}–${x.to}`).join('; ');
  return {
    offers: !s?.offer ? 'everything' : s.offer.names.length ? s.offer.names.join(', ') : 'nothing',
    when: windows ? `${w} · ${windows}` : w,
    localFirst: s?.localFirst ? 'on' : 'off',
    maxJobs: s?.maxJobs ? String(s.maxJobs) : `default (${DEFAULT_MAX_JOBS})`,
    idleAfter: `${s?.idleAfterMinutes || DEFAULT_IDLE_AFTER} min`,
  };
}
