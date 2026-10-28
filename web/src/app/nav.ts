import type { Mode } from './config';

export type RouteId =
  'constellation' | 'channels' | 'approvals' | 'jobs' | 'grants' | 'nodes' | 'agents' | 'traces' | 'machine' | 'settings';

export interface NavItem {
  id: RouteId;
  path: string;
  label: string;
  /** Label in the phone bottom bar. */
  short: string;
  /** Extra palette keywords. */
  keywords: string[];
  /** Only shown in node mode ("This machine"). */
  nodeOnly?: boolean;
  /** Shown in the phone bottom bar (the rest go under "More"). */
  phone?: boolean;
}

/** NAV is the rail order. Paths are the contract with the views packages. */
export const NAV: NavItem[] = [
  { id: 'constellation', path: '/constellation', label: 'Constellation', short: 'Map', keywords: ['network', 'map', '3d', 'star'] },
  { id: 'channels', path: '/channels', label: 'Channels', short: 'Chat', keywords: ['chat', 'messages', 'threads', 'post'], phone: true },
  {
    id: 'approvals',
    path: '/approvals',
    label: 'Approvals',
    short: 'Approve',
    keywords: ['pending', 'approve', 'deny', 'queue'],
    phone: true,
  },
  { id: 'jobs', path: '/jobs', label: 'Jobs', short: 'Jobs', keywords: ['run', 'sandbox', 'logs'], phone: true },
  { id: 'grants', path: '/grants', label: 'Grants', short: 'Grants', keywords: ['gpu', 'ollama', 'docker', 'lease'] },
  { id: 'nodes', path: '/nodes', label: 'Nodes', short: 'Nodes', keywords: ['machines', 'inventory', 'enrol', 'tokens'] },
  { id: 'agents', path: '/agents', label: 'Agents', short: 'Agents', keywords: ['harness', 'workers', 'orchestrator', 'budget'] },
  { id: 'traces', path: '/traces', label: 'Traces', short: 'Traces', keywords: ['cost', 'tokens', 'audit', 'latency'] },
  {
    id: 'machine',
    path: '/machine',
    label: 'This machine',
    short: 'Local',
    keywords: ['local', 'sharing', 'ceiling', 'restrict'],
    nodeOnly: true,
  },
  {
    id: 'settings',
    path: '/settings',
    label: 'Settings',
    short: 'Settings',
    keywords: ['passkeys', 'push', 'notifications', 'motion', 'preferences'],
  },
];

export function navFor(mode: Mode): NavItem[] {
  return NAV.filter((n) => !n.nodeOnly || mode === 'node');
}

/** isPhone is the breakpoint where the rail becomes a bottom bar. */
export const PHONE_QUERY = '(max-width: 640px)';

export function isPhone(): boolean {
  return typeof matchMedia === 'function' && matchMedia(PHONE_QUERY).matches;
}

/** startPath: Approvals on a phone (the PWA's job), the constellation otherwise. */
export function startPath(): string {
  return isPhone() ? '/approvals' : '/constellation';
}
