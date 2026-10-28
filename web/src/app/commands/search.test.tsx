import { create } from '@bufbuild/protobuf';
import { act, render, renderHook, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter } from 'react-router';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { AgentSchema } from '../../gen/mycel/v1/hub_pb';
import { EventKind, SearchHitSchema, SearchResponseSchema, type SearchHit } from '../../gen/mycel/v1/ui_pb';
import { getApi, makeHubApi, setApi } from '../api';
import { agentsStore } from '../events/stores';
import { useSession } from '../session';
import { CommandBar } from './CommandBar';
import { usePalette } from './registry';
import { useInspector } from '../shell/inspector';
import { SEARCH_DEBOUNCE_MS, groupHits, openHit, useServerSearch } from './search';

describe('openHit', () => {
  afterEach(() => {
    useInspector.getState().close();
    agentsStore.use.setState({ byId: {}, loaded: false });
  });

  it('routes channels, messages and jobs, and inspects nodes, agents and grants', async () => {
    const nav = vi.fn();
    await openHit(hit(EventKind.CHANNEL, '#backend'), nav);
    await openHit(create(SearchHitSchema, { kind: EventKind.MESSAGE, id: 'msg_1', channel: '#eval' }), nav);
    await openHit(hit(EventKind.JOB, 'job_1'), nav);
    expect(nav.mock.calls.map((c) => c[0])).toEqual(['/channels/%23backend', '/channels/%23eval', '/jobs/job_1']);
    await openHit(hit(EventKind.NODE, 'alpha'), nav);
    expect(useInspector.getState().key).toBe('node:alpha');
    const a = create(AgentSchema, { id: 'agent:coder@alpha', name: 'coder', nodeName: 'alpha' });
    agentsStore.use.setState({ byId: { [a.id]: a }, loaded: true });
    await openHit(hit(EventKind.AGENT, 'coder@alpha'), nav);
    expect(useInspector.getState().key).toBe('agent:agent:coder@alpha');
    await openHit(hit(EventKind.GRANT, 'grt_1'), nav);
    expect(useInspector.getState().key).toBe('grant:grt_1');
    expect(nav).toHaveBeenCalledTimes(3);
  });
});

const hit = (kind: EventKind, id: string, title = id): SearchHit => create(SearchHitSchema, { kind, id, title, snippet: `…${title}…` });

const HITS = [
  hit(EventKind.MESSAGE, 'msg_1', 'deploy alpha now'),
  hit(EventKind.AGENT, 'agent:coder@alpha', 'coder'),
  hit(EventKind.NODE, 'alpha'),
  hit(EventKind.GRANT, 'grt_1', 'gpu for coder'),
  hit(EventKind.AGENT, 'agent:lead@alpha', 'lead'),
];

function installApi() {
  setApi(makeHubApi({ getCsrf: () => 'c', fetch: vi.fn(async () => new Response('{}')) as unknown as typeof fetch }));
  const search = vi.fn(async () => create(SearchResponseSchema, { hits: HITS }));
  (getApi().rpc.events as unknown as { search: unknown }).search = search;
  return search;
}

describe('groupHits', () => {
  it('orders kinds: nodes, agents, channels, jobs, grants, messages', () => {
    const groups = groupHits(HITS);
    expect(groups.map((g) => g.label)).toEqual(['Nodes', 'Agents', 'Grants', 'Messages']);
    expect(groups[1].hits.map((h) => h.id)).toEqual(['agent:coder@alpha', 'agent:lead@alpha']);
  });
});

describe('useServerSearch', () => {
  beforeEach(() => vi.useFakeTimers());
  afterEach(() => vi.useRealTimers());

  it('debounces and only sends the last query', async () => {
    const search = installApi();
    const { result, rerender } = renderHook(({ q }) => useServerSearch(q), { initialProps: { q: 'a' } });
    expect(search).not.toHaveBeenCalled(); // one character does not search
    rerender({ q: 'al' });
    rerender({ q: 'alp' });
    rerender({ q: 'alpha' });
    await act(async () => {
      vi.advanceTimersByTime(SEARCH_DEBOUNCE_MS - 10);
    });
    expect(search).not.toHaveBeenCalled();
    await act(async () => {
      vi.advanceTimersByTime(20);
    });
    expect(search).toHaveBeenCalledTimes(1);
    expect((search.mock.calls[0] as unknown[])[0]).toEqual(expect.objectContaining({ query: 'alpha' }));
    await act(async () => {
      await Promise.resolve();
    });
    expect(result.current.query).toBe('alpha');
    expect(result.current.hits).toHaveLength(5);
  });
});

describe('useServerSearch stale answers', () => {
  beforeEach(() => vi.useFakeTimers());
  afterEach(() => vi.useRealTimers());

  it('drops an answer that arrives after the query moved on', async () => {
    installApi();
    let release: (v: unknown) => void = () => undefined;
    const slow = new Promise((r) => (release = r));
    const search = vi.fn(async (req: { query: string }) =>
      req.query === 'alpha'
        ? (await slow, create(SearchResponseSchema, { hits: [hit(EventKind.NODE, 'alpha')] }))
        : create(SearchResponseSchema, { hits: [hit(EventKind.NODE, 'beta')] }),
    );
    (getApi().rpc.events as unknown as { search: unknown }).search = search;
    const { result, rerender } = renderHook(({ q }) => useServerSearch(q), { initialProps: { q: 'alpha' } });
    await act(async () => {
      vi.advanceTimersByTime(SEARCH_DEBOUNCE_MS + 5);
    });
    rerender({ q: 'beta' });
    await act(async () => {
      vi.advanceTimersByTime(SEARCH_DEBOUNCE_MS + 5);
    });
    await act(async () => {
      release(undefined);
      await Promise.resolve();
      await Promise.resolve();
    });
    expect(search).toHaveBeenCalledTimes(2);
    expect(result.current.query).toBe('beta');
    expect(result.current.hits.map((h) => h.id)).toEqual(['beta']);
  });
});

describe('command bar search', () => {
  afterEach(() => {
    usePalette.setState({ open: false, query: '' });
    agentsStore.use.setState({ byId: {}, loaded: false });
  });

  it('lists hits grouped by kind, with the orchestrator entry last', async () => {
    installApi();
    useSession.setState({
      status: 'signed-in',
      rights: { mode: 'hub', isAdmin: false, readOnly: false, userId: 'u1', userName: 'ada' },
    });
    const orch = create(AgentSchema, { id: 'agent:boss@alpha', name: 'boss', nodeName: 'alpha', ownerId: 'u1', state: 'running' });
    agentsStore.use.setState({ byId: { [orch.id]: orch }, loaded: true });
    usePalette.setState({ open: true, query: '' });
    const user = userEvent.setup();
    render(
      <MemoryRouter>
        <CommandBar />
      </MemoryRouter>,
    );
    await user.type(await screen.findByRole('combobox'), 'deploy alpha now');
    const list = screen.getByRole('listbox', { name: 'Commands' });
    await within(list).findByText('Search · Nodes');
    const items = within(list)
      .getAllByRole('presentation')
      .map((li) => li.textContent);
    const groups = items.filter((t) => t?.startsWith('Search'));
    expect(groups).toEqual(['Search · Nodes', 'Search · Agents', 'Search · Grants', 'Search · Messages']);
    const options = within(list).getAllByRole('option');
    expect(options.at(-1)).toHaveTextContent('Send to boss');
  });
});
