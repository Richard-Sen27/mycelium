import { create } from '@bufbuild/protobuf';
import { render, screen } from '@testing-library/react';
import { afterEach, beforeAll, describe, expect, it, vi } from 'vitest';
import { App } from '../../app/App';
import { agentsStore, channelsStore, grantsStore, jobsStore, nodesStore, useFreeze } from '../../app/events/stores';
import { anonymousRights, useSession } from '../../app/session';
import { ChannelSchema, ChannelType } from '../../gen/mycel/v1/channel_pb';
import { FreezeStatusResponseSchema, GrantSchema, GrantState } from '../../gen/mycel/v1/grant_pb';
import { AgentSchema } from '../../gen/mycel/v1/hub_pb';
import { JobSchema, JobState } from '../../gen/mycel/v1/job_pb';
import { fakeServer, json, resetAll, seed, signIn } from '../test-utils/fakeServer';

// Every route of this package renders inside the shell from seeded stores.
vi.setConfig({ testTimeout: 20_000 });

function seedAll() {
  seed(nodesStore, []);
  seed(channelsStore, [
    create(ChannelSchema, {
      name: '#backend',
      type: ChannelType.TOPIC,
      members: [{ id: 'agent:coder@mac', role: 'agent' }],
      policy: { budget: { usd: 5 } },
      lastSeq: 9n,
    }),
  ]);
  seed(agentsStore, [
    create(AgentSchema, {
      id: 'agent:coder@mac',
      name: 'coder',
      nodeName: 'mac',
      state: 'running',
      used: { tokens: 12000n, usd: 1.2 },
      budget: { usd: 4 },
    }),
  ]);
  seed(jobsStore, [
    create(JobSchema, { id: 'job_9', state: JobState.RUNNING, node: 'mac', spec: { profile: 'dev', command: ['make', 'test'] } }),
  ]);
  seed(grantsStore, [
    create(GrantSchema, {
      id: 'grt_9',
      state: GrantState.ACTIVE,
      subject: 'job:job_9',
      node: 'mac',
      spec: { kind: { case: 'gpu', value: { device: 0 } } },
      usage: { gpuSeconds: 3600n },
    }),
  ]);
  useFreeze.setState({ status: create(FreezeStatusResponseSchema, {}), loaded: true });
}

describe('views-b routes', () => {
  // Load the lazy route modules up front so the first render under load does not time out.
  beforeAll(async () => {
    await Promise.all([import('../channels'), import('../approvals'), import('../jobs'), import('./index'), import('../machine')]);
  }, 20_000);

  afterEach(() => {
    resetAll();
    vi.unstubAllGlobals();
    useSession.setState({ status: 'booting', me: null, config: null, rights: anonymousRights });
    window.history.replaceState(null, '', '/');
  });

  it.each([
    ['/channels', 'Channels'],
    ['/channels/%23backend', 'Channel #backend'],
    ['/approvals', 'Pending approvals'],
    ['/jobs', 'Jobs'],
    ['/jobs/job_9', 'Job job_9'],
  ])('%s renders', async (path, region) => {
    signIn(fakeServer({ 'ChannelService/ListMessages': () => ({ messages: [] }) }), { admin: true });
    seedAll();
    window.history.replaceState(null, '', path);
    render(<App />);
    expect(await screen.findByRole('region', { name: region }, { timeout: 5000 })).toBeInTheDocument();
  });

  it('/traces renders the cost tables', async () => {
    signIn(
      fakeServer({ 'AgentService/GetChannelUsage': () => ({ usage: { channel: '#backend', used: { usd: 2.5 }, budget: { usd: 5 } } }) }),
      { admin: true },
    );
    seedAll();
    window.history.replaceState(null, '', '/traces');
    render(<App />);
    expect(await screen.findByRole('heading', { name: 'Traces and cost' }, { timeout: 5000 })).toBeInTheDocument();
    expect(await screen.findByRole('table', { name: 'Cost per agent' }, { timeout: 5000 })).toHaveTextContent('coder@mac');
    expect(screen.getByRole('table', { name: 'Broker usage per grant' })).toHaveTextContent('3,600');
    expect(await screen.findByText('$2.50 left', {}, { timeout: 5000 })).toBeInTheDocument();
  });

  it('/machine renders in node mode', async () => {
    const hub = { offer: { names: ['docker', 'gpu'] }, when: 'always', local_first: true, max_jobs: 2, idle_after_minutes: 10 };
    const server = fakeServer({
      'GET /api/status': () =>
        json({
          node: 'mac',
          hub_url: 'https://hub.example:8443',
          state: 'connected',
          connected: true,
          last_heartbeat_at: new Date().toISOString(),
          cert_not_after: '',
          caps: ['docker'],
          local_agents: 1,
          workers: 0,
          jobs: 1,
          sandbox_backends_available: ['applevf'],
          egress_proxy: '127.0.0.1:7781',
          grants: 1,
          frozen: false,
          sharing_active: true,
          idle: '3m0s',
        }),
      'GET /api/local/sharing': () => json({ hub, local: {}, effective: hub, idle: '3m0s' }),
      'GET /api/local/freeze': () => json({ frozen: false, scope: '', reason: '' }),
      'GET /api/local/grants': () => json({ grants: [], endpoints: {} }),
      'GET /api/local/workers': () => json({ workers: [] }),
    });
    // GET /api/status is the daemon's unauthenticated route, fetched directly.
    vi.stubGlobal('fetch', server.fetch);
    signIn(server, { mode: 'node' });
    seedAll();
    window.history.replaceState(null, '', '/machine');
    render(<App />);
    expect(await screen.findByRole('heading', { name: 'This machine' }, { timeout: 5000 })).toBeInTheDocument();
    expect(await screen.findByText('hub connected', {}, { timeout: 5000 })).toBeInTheDocument();
    expect(await screen.findByRole('form', { name: 'Local sharing ceiling' }, { timeout: 5000 })).toBeInTheDocument();
    expect(screen.getByText('applevf')).toBeInTheDocument();
  });
});
