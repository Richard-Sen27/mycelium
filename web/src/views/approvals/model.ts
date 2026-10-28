import { GrantState, type Grant } from '../../gen/mycel/v1/grant_pb';
import { goDuration, parseGoDuration } from '../../ds';

export const GRANT_KIND: Record<string, string> = {
  ollama: 'ollama',
  gpu: 'gpu',
  dockerHost: 'docker-host',
  mount: 'mount',
  egress: 'egress',
};

export function grantKind(g: Grant): string {
  const c = g.spec?.kind.case;
  return c ? (GRANT_KIND[c] ?? c) : 'grant';
}

/** grantSpecLine summarises what the grant gives, in the decider's terms. */
export function grantSpecLine(g: Grant): string {
  const k = g.spec?.kind;
  switch (k?.case) {
    case 'gpu':
      return `device ${k.value.device}${k.value.migSlice ? ` (${k.value.migSlice})` : ''}, ${k.value.exclusive ? 'exclusive' : 'time-sliced'}`;
    case 'ollama': {
      const v = k.value;
      const parts = [v.models.length ? `models ${v.models.join(', ')}` : 'models already present'];
      if (v.allowPull) parts.push('may pull');
      if (v.allowDelete) parts.push('may delete');
      if (Number(v.maxTokens) > 0) parts.push(`≤ ${Number(v.maxTokens).toLocaleString('en-US')} tokens`);
      return parts.join(', ');
    }
    case 'dockerHost': {
      const v = k.value;
      return `images ${v.images.join(', ') || 'none'}${v.gpu ? ', with GPU' : ''}${v.cpus ? `, ${v.cpus} cpu` : ''}`;
    }
    case 'mount':
      return `${k.value.hostPath} → ${k.value.guestPath || '(same path)'} ${k.value.readOnly ? 'read-only' : 'read-write'}`;
    case 'egress':
      return `allow ${k.value.allow.join(', ')}`;
    default:
      return '';
  }
}

/** Presets offered when approving; only those shorter than the request, plus the request itself. */
export const DURATION_PRESETS = ['15m', '30m', '1h', '2h', '4h', '8h', '12h', '24h'];

export interface DurationOption {
  value: string;
  label: string;
  requested: boolean;
}

/**
 * durationOptions lists what an approver may choose: the requested duration
 * and every preset strictly shorter. An approval can only shorten.
 */
export function durationOptions(requested: string): DurationOption[] {
  const req = parseGoDuration(requested);
  if (!Number.isFinite(req) || req <= 0) {
    // No readable request: the hub applies its default (1h); offer shorter presets only.
    return [
      { value: '', label: 'default', requested: true },
      ...DURATION_PRESETS.filter((p) => parseGoDuration(p) < 3_600_000).map((p) => ({ value: p, label: p, requested: false })),
    ];
  }
  const shorter = DURATION_PRESETS.filter((p) => parseGoDuration(p) < req).map((p) => ({ value: p, label: p, requested: false }));
  return [...shorter, { value: requested, label: goDuration(req), requested: true }];
}

export const STATE_LABEL: Record<GrantState, string> = {
  [GrantState.UNSPECIFIED]: '—',
  [GrantState.PENDING]: 'pending',
  [GrantState.ACTIVE]: 'active',
  [GrantState.EXPIRED]: 'expired',
  [GrantState.REVOKED]: 'revoked',
  [GrantState.DENIED]: 'denied',
};

export const STATE_TONE: Record<GrantState, 'ok' | 'busy' | 'idle' | 'off' | 'alert'> = {
  [GrantState.UNSPECIFIED]: 'off',
  [GrantState.PENDING]: 'busy',
  [GrantState.ACTIVE]: 'ok',
  [GrantState.EXPIRED]: 'off',
  [GrantState.REVOKED]: 'alert',
  [GrantState.DENIED]: 'alert',
};
