import { create } from '@bufbuild/protobuf';
import { parseGoDuration, type Tone } from '../../ds';
import {
  GrantSpecSchema,
  GrantState,
  RequestGrantRequestSchema,
  type Grant,
  type GrantSpec,
  type RequestGrantRequest,
} from '../../gen/mycel/v1/grant_pb';

// Pure grant helpers shared by the grants table, the inspector, the request
// form and the constellation. No React, no API.

export type GrantKind = 'ollama' | 'gpu' | 'docker-host' | 'mount' | 'egress';
export const GRANT_KINDS: GrantKind[] = ['ollama', 'gpu', 'docker-host', 'mount', 'egress'];

export { grantKind } from '../approvals/model';

export function stateName(s: GrantState): string {
  switch (s) {
    case GrantState.PENDING:
      return 'pending';
    case GrantState.ACTIVE:
      return 'active';
    case GrantState.EXPIRED:
      return 'expired';
    case GrantState.REVOKED:
      return 'revoked';
    case GrantState.DENIED:
      return 'denied';
    default:
      return 'unknown';
  }
}

export function stateTone(s: GrantState): Tone {
  switch (s) {
    case GrantState.PENDING:
      return 'alert';
    case GrantState.ACTIVE:
      return 'ok';
    case GrantState.DENIED:
    case GrantState.REVOKED:
      return 'off';
    default:
      return 'idle';
  }
}

/** isLive: pending or active (the grants that still matter). */
export function isLive(g: Pick<Grant, 'state'>): boolean {
  return g.state === GrantState.PENDING || g.state === GrantState.ACTIVE;
}

/** specSummary is a one-line description of what a grant gives. */
export function specSummary(spec: GrantSpec | undefined): string {
  const k = spec?.kind;
  switch (k?.case) {
    case 'ollama': {
      const m = k.value.models.length ? k.value.models.join(', ') : 'any local model';
      return `ollama: ${m}${k.value.allowPull ? ', may pull' : ''}`;
    }
    case 'gpu':
      return `gpu ${k.value.device}${k.value.migSlice ? ` (${k.value.migSlice})` : ''}${k.value.exclusive ? ', exclusive' : ', time-sliced'}`;
    case 'dockerHost':
      return `docker: ${k.value.images.join(', ') || 'no images'}`;
    case 'mount':
      return `mount ${k.value.hostPath} → ${k.value.guestPath} ${k.value.readOnly ? '(ro)' : '(rw)'}`;
    case 'egress':
      return `egress: ${k.value.allow.join(', ')}`;
    default:
      return 'unknown grant';
  }
}

/** byCreatedDesc: newest first (module-level, so useEntityList's compare is stable). */
export function byCreatedDesc(a: Grant, b: Grant): number {
  return b.createdAt.localeCompare(a.createdAt) || a.id.localeCompare(b.id);
}

// ---- Request form ----

export interface GrantForm {
  kind: GrantKind;
  subject: string;
  duration: string;
  reason: string;
  channel: string;
  // ollama
  models: string;
  allowPull: boolean;
  // gpu
  device: string;
  exclusive: boolean;
  // docker-host
  images: string;
  // mount
  hostPath: string;
  guestPath: string;
  readWrite: boolean;
  // egress
  allow: string;
}

export const emptyGrantForm = (kind: GrantKind = 'ollama', subject = ''): GrantForm => ({
  kind,
  subject,
  duration: '1h',
  reason: '',
  channel: '',
  models: '',
  allowPull: false,
  device: '0',
  exclusive: false,
  images: '',
  hostPath: '',
  guestPath: '',
  readWrite: false,
  allow: '',
});

/** splitList splits on commas and newlines and drops blanks (like the CLI's repeated flags). */
export function splitList(s: string): string[] {
  return s
    .split(/[,\n]/)
    .map((x) => x.trim())
    .filter(Boolean);
}

export type FormErrors = Partial<Record<keyof GrantForm, string>>;

/** validateGrantForm mirrors the CLI's checks (`mycel grants request`). */
export function validateGrantForm(f: GrantForm): FormErrors {
  const e: FormErrors = {};
  const subject = f.subject.trim();
  if (!subject) e.subject = 'Pick an agent or job you own.';
  else if (!/^(agent:|job:)/.test(subject)) e.subject = 'A subject is agent:<name>@<node> or job:<id>.';
  if (f.duration.trim()) {
    const ms = parseGoDuration(f.duration.trim());
    if (Number.isNaN(ms) || ms <= 0) e.duration = 'A duration such as 30m or 2h.';
    else if (ms > 24 * 3.6e6) e.duration = 'A grant may last at most 24h.';
  }
  if (new TextEncoder().encode(f.reason).length > 1024) e.reason = 'At most 1 KiB.';
  const ch = f.channel.trim();
  if (ch && !/^#?[^\s#][^\s]*$/.test(ch)) e.channel = 'A #channel name.';
  switch (f.kind) {
    case 'gpu': {
      const d = Number(f.device);
      if (!/^\d+$/.test(f.device.trim()) || d < 0) e.device = 'A device index from the node inventory.';
      break;
    }
    case 'docker-host':
      if (splitList(f.images).length === 0) e.images = 'At least one image or registry prefix.';
      break;
    case 'mount':
      if (!f.hostPath.trim()) e.hostPath = 'Required.';
      if (!f.guestPath.trim()) e.guestPath = 'Required.';
      break;
    case 'egress':
      if (splitList(f.allow).length === 0) e.allow = 'At least one host rule.';
      break;
  }
  return e;
}

/** buildGrantSpec turns the form's kind-specific fields into a GrantSpec. */
export function buildGrantSpec(f: GrantForm): GrantSpec {
  switch (f.kind) {
    case 'ollama':
      return create(GrantSpecSchema, { kind: { case: 'ollama', value: { models: splitList(f.models), allowPull: f.allowPull } } });
    case 'gpu':
      return create(GrantSpecSchema, { kind: { case: 'gpu', value: { device: Number(f.device) || 0, exclusive: f.exclusive } } });
    case 'docker-host':
      return create(GrantSpecSchema, { kind: { case: 'dockerHost', value: { images: splitList(f.images) } } });
    case 'mount':
      return create(GrantSpecSchema, {
        kind: { case: 'mount', value: { hostPath: f.hostPath.trim(), guestPath: f.guestPath.trim(), readOnly: !f.readWrite } },
      });
    case 'egress':
      return create(GrantSpecSchema, { kind: { case: 'egress', value: { allow: splitList(f.allow) } } });
  }
}

/** buildRequestGrant is the RequestGrantRequest the form sends (same shape as the CLI's). */
export function buildRequestGrant(f: GrantForm): RequestGrantRequest {
  const ch = f.channel.trim();
  return create(RequestGrantRequestSchema, {
    spec: buildGrantSpec(f),
    subject: f.subject.trim(),
    duration: f.duration.trim(),
    reason: f.reason,
    channel: ch && !ch.startsWith('#') ? `#${ch}` : ch,
  });
}
