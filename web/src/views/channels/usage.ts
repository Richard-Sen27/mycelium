import { useEffect } from 'react';
import { create } from 'zustand';
import type { ChannelUsage } from '../../gen/mycel/v1/hub_pb';
import { getApi } from '../../app/api';
import { onEvent } from '../../app/events/stores';
import { EventKind } from '../../gen/mycel/v1/ui_pb';
import { errText } from './model';

// Channel budgets: GetChannelUsage per channel, cached for the session and
// refreshed when an agent row changes (usage arrives with agent events).

interface UsageState {
  byChannel: Record<string, ChannelUsage | null>;
  errors: Record<string, string>;
}

const useUsage = create<UsageState>(() => ({ byChannel: {}, errors: {} }));
const inflight = new Map<string, Promise<void>>();
const loadedAt = new Map<string, number>();
const REFRESH_MS = 10_000;

export function loadChannelUsage(channel: string, force = false): Promise<void> {
  const cur = inflight.get(channel);
  if (cur) return cur;
  if (!force && Date.now() - (loadedAt.get(channel) ?? 0) < REFRESH_MS) return Promise.resolve();
  const p = getApi()
    .rpc.agents.getChannelUsage({ channel })
    .then((r) => {
      useUsage.setState((st) => ({ byChannel: { ...st.byChannel, [channel]: r.usage ?? null } }));
    })
    .catch((err: unknown) => {
      useUsage.setState((st) => ({ errors: { ...st.errors, [channel]: errText(err) } }));
    })
    .finally(() => {
      loadedAt.set(channel, Date.now());
      inflight.delete(channel);
    });
  inflight.set(channel, p);
  return p;
}

let watching = false;
function watchAgents() {
  if (watching) return;
  watching = true;
  onEvent((e) => {
    if (e.kind !== EventKind.AGENT) return;
    const home = e.body.case === 'agent' ? e.body.value.homeChannel : '';
    if (home && home in useUsage.getState().byChannel) void loadChannelUsage(home);
  });
}

/** useChannelUsage loads a channel's budget usage when `enabled`. */
export function useChannelUsage(channel: string, enabled = true): ChannelUsage | null | undefined {
  useEffect(() => {
    if (!enabled || !channel) return;
    watchAgents();
    void loadChannelUsage(channel);
  }, [channel, enabled]);
  return useUsage((s) => s.byChannel[channel]);
}

export function resetChannelUsageCache(): void {
  inflight.clear();
  loadedAt.clear();
  useUsage.setState({ byChannel: {}, errors: {} });
}
