import { JobState, type Job, type JobSpec } from '../../gen/mycel/v1/job_pb';
import type { Tone } from '../../ds';
import type { Grant } from '../../gen/mycel/v1/grant_pb';

export const JOB_STATE: Record<JobState, string> = {
  [JobState.UNSPECIFIED]: '—',
  [JobState.QUEUED]: 'queued',
  [JobState.STARTING]: 'starting',
  [JobState.RUNNING]: 'running',
  [JobState.SUCCEEDED]: 'succeeded',
  [JobState.FAILED]: 'failed',
  [JobState.CANCELLED]: 'cancelled',
  [JobState.TIMED_OUT]: 'timed out',
};

export const JOB_TONE: Record<JobState, Tone> = {
  [JobState.UNSPECIFIED]: 'off',
  [JobState.QUEUED]: 'idle',
  [JobState.STARTING]: 'busy',
  [JobState.RUNNING]: 'ok',
  [JobState.SUCCEEDED]: 'idle',
  [JobState.FAILED]: 'alert',
  [JobState.CANCELLED]: 'off',
  [JobState.TIMED_OUT]: 'alert',
};

export function isFinished(j: Job): boolean {
  return j.state >= JobState.SUCCEEDED;
}

export function isActive(j: Job): boolean {
  return j.state === JobState.QUEUED || j.state === JobState.STARTING || j.state === JobState.RUNNING;
}

/** commandLine renders argv for display, quoting words with spaces. */
export function commandLine(argv: string[]): string {
  return argv.map((a) => (a === '' || /[\s"'\\$`]/.test(a) ? `'${a.replace(/'/g, `'\\''`)}'` : a)).join(' ');
}

/**
 * splitCommand turns a typed command line into argv: whitespace separates
 * words, single and double quotes group, a backslash escapes outside single
 * quotes. No expansion: the sandbox runs argv as given.
 */
export function splitCommand(line: string): string[] {
  const out: string[] = [];
  let cur = '';
  let has = false;
  let q: '"' | "'" | null = null;
  for (let i = 0; i < line.length; i++) {
    const c = line[i];
    if (q === "'") {
      if (c === "'") q = null;
      else cur += c;
    } else if (q === '"') {
      if (c === '"') q = null;
      else if (c === '\\' && i + 1 < line.length) cur += line[++i];
      else cur += c;
    } else if (c === "'" || c === '"') {
      q = c;
      has = true;
    } else if (c === '\\' && i + 1 < line.length) {
      cur += line[++i];
      has = true;
    } else if (/\s/.test(c)) {
      if (has || cur) out.push(cur);
      cur = '';
      has = false;
    } else {
      cur += c;
      has = true;
    }
  }
  if (has || cur) out.push(cur);
  return out;
}

/** parseEnv reads KEY=VALUE lines; returns the map or the first bad line. */
export function parseEnv(text: string): { env: Record<string, string>; error: string | null } {
  const env: Record<string, string> = {};
  for (const raw of text.split('\n')) {
    const line = raw.trim();
    if (!line || line.startsWith('#')) continue;
    const i = line.indexOf('=');
    const k = i > 0 ? line.slice(0, i).trim() : '';
    if (!/^[A-Za-z_][A-Za-z0-9_]*$/.test(k)) return { env, error: `Not KEY=VALUE: ${line.slice(0, 40)}` };
    if (k.startsWith('MYCEL_')) return { env, error: `${k}: MYCEL_* keys are reserved` };
    env[k] = line.slice(i + 1);
  }
  return { env, error: null };
}

export const splitList = (v: string) =>
  v
    .split(/[,\n]/)
    .map((x) => x.trim())
    .filter(Boolean);

/** jobTitle is the label or the command. */
export function jobTitle(j: Job): string {
  return j.spec?.name || commandLine(j.spec?.command ?? []) || j.id;
}

/** endpointKind says how a grant reaches the sandbox (spec: brokers behind virtual destinations). */
export function endpointKind(g: Grant): string {
  switch (g.spec?.kind.case) {
    case 'ollama':
      return 'broker · ollama.mycel.internal:80';
    case 'dockerHost':
      return 'broker · docker.mycel.internal:2375';
    case 'gpu':
      return `device · gpu ${g.spec.kind.value.device}`;
    case 'mount':
      return `mount · ${g.spec.kind.value.guestPath || g.spec.kind.value.hostPath}`;
    case 'egress':
      return 'egress proxy · allowlist';
    default:
      return '—';
  }
}

/** stripUserinfo drops credentials from a URL before it is shown ("https://user:token@host/…" → "https://host/…"). */
export function stripUserinfo(url: string): string {
  return url.replace(/^([a-z][a-z0-9+.-]*:\/\/)[^/@]*@/i, '$1');
}

/** A draft of the new-job form, prefilled from an earlier job for "run again". */
export interface JobDraft {
  name: string;
  profile: string;
  node: string;
  channel: string;
  command: string;
  timeout: string;
  inputs: string;
  outputs: string;
  env: string;
  git: string;
}

export function draftFrom(spec?: JobSpec): JobDraft {
  const ws = spec?.workspace?.source;
  return {
    name: spec?.name ?? '',
    profile: spec?.profile ?? 'default',
    node: spec?.node ?? '',
    channel: spec?.channel ?? '',
    command: commandLine(spec?.command ?? []),
    timeout: spec?.timeout ?? '',
    inputs: (spec?.inputs ?? []).map((i) => (i.path ? `${i.artifact}:${i.path}` : i.artifact)).join(', '),
    outputs: (spec?.outputs ?? []).join(', '),
    // Values are never shown again (they may be secrets): only the keys come back.
    env: Object.keys(spec?.env ?? {})
      .map((k) => `${k}=`)
      .join('\n'),
    git: ws?.case === 'git' ? `${stripUserinfo(ws.value.url)}${ws.value.ref ? `#${ws.value.ref}` : ''}` : '',
  };
}
