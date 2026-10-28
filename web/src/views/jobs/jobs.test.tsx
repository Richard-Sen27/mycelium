import { create } from '@bufbuild/protobuf';
import { render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { createMemoryRouter, RouterProvider } from 'react-router';
import { afterEach, describe, expect, it } from 'vitest';
import { jobsStore } from '../../app/events/stores';
import { JobSchema, JobState } from '../../gen/mycel/v1/job_pb';
import { b64, fakeServer, resetAll, seed, signIn, stream } from '../test-utils/fakeServer';
import { getApi } from '../../app/api';
import { artifactUrl, isArtifactId } from './artifacts';
import { Component } from './index';
import { parseEnv, splitCommand, stripUserinfo } from './model';

function seedJob(state = JobState.RUNNING) {
  seed(jobsStore, [
    create(JobSchema, {
      id: 'job_01',
      state,
      node: 'gpu-box',
      submittedBy: 'user:u1',
      createdAt: new Date(Date.now() - 60_000).toISOString(),
      spec: { profile: 'dev', command: ['pytest', '-q'], env: { TOKEN: 'secret-value' }, outputs: ['out/*.xml'] },
      stats: { startedAt: new Date(Date.now() - 50_000).toISOString() },
      artifacts: ['art_1'],
    }),
  ]);
}

function renderAt(path: string) {
  const router = createMemoryRouter([{ path: '/jobs/*', Component }], { initialEntries: [path] });
  return render(<RouterProvider router={router} />);
}

describe('job helpers', () => {
  it('split a command line into argv', () => {
    expect(splitCommand(`python -c 'print("a b")' --x="1 2" plain\\ word`)).toEqual([
      'python',
      '-c',
      'print("a b")',
      '--x=1 2',
      'plain word',
    ]);
  });
  it('hide credentials in git URLs', () => {
    expect(stripUserinfo('https://bot:ghp_secret@github.com/acme/x.git')).toBe('https://github.com/acme/x.git');
    expect(stripUserinfo('https://github.com/acme/x.git')).toBe('https://github.com/acme/x.git');
  });
  it('refuse reserved env keys', () => {
    expect(parseEnv('A=1\nMYCEL_X=2').error).toMatch(/MYCEL_X/);
    expect(parseEnv('A=1\n# c\nB=x=y').env).toEqual({ A: '1', B: 'x=y' });
  });
  it("download through the api layer with the mode's auth", async () => {
    let auth: string | null = null;
    const server = fakeServer({
      'GET /api/hub/api/artifacts/art_1': (_b, _u, init) => {
        auth = new Headers(init?.headers).get('Authorization');
        return new Response('bytes');
      },
    });
    signIn(server, { mode: 'node' });
    expect(await (await getApi().download('/api/artifacts/art_1')).text()).toBe('bytes');
    expect(auth).toBe('Bearer t');
    await expect(getApi().upload('/api/artifacts?name=x', new Uint8Array([1]))).rejects.toThrow(/hub UI/);
  });
  it('only link well-formed artifact ids', () => {
    expect(isArtifactId('art_01JBX9')).toBe(true);
    expect(isArtifactId('art_../../api/auth/logout')).toBe(false);
    expect(isArtifactId('javascript:alert(1)')).toBe(false);
  });
  it('build artifact URLs through the hub or the node proxy', () => {
    expect(artifactUrl('hub', 'art_1')).toBe('/api/artifacts/art_1');
    expect(artifactUrl('node', 'art_1')).toBe('/api/hub/api/artifacts/art_1');
  });
});

describe('jobs view', () => {
  afterEach(() => resetAll());

  it('lists jobs and opens the inspector', async () => {
    const user = userEvent.setup();
    signIn(fakeServer());
    seedJob();
    renderAt('/jobs');
    const table = await screen.findByRole('table', { name: 'Jobs' });
    expect(within(table).getByText('pytest -q')).toBeInTheDocument();
    await user.click(within(table).getByText('pytest -q'));
    expect(await screen.findByRole('region', { name: 'Job job_01' })).toBeInTheDocument();
    // Env keys are shown without values.
    expect(screen.getByText('TOKEN')).toBeInTheDocument();
    expect(screen.queryByText(/secret-value/)).toBeNull();
  });

  it('appends streamed log lines', async () => {
    const server = fakeServer({
      'JobService/StreamLogs': () =>
        stream([
          { data: b64('collecting tests\nok 1\nok '), offset: '0' },
          { data: b64('2\nFAILED test_auth\n'), offset: '26' },
          { data: b64(''), offset: '44', eof: true },
        ]),
    });
    signIn(server);
    seedJob(JobState.FAILED);
    renderAt('/jobs/job_01?tab=logs');
    const log = await screen.findByRole('log', { name: 'Log of job_01' });
    expect(await within(log).findByText('FAILED test_auth')).toBeInTheDocument();
    expect(within(log).getByText('ok 2')).toBeInTheDocument();
    expect(within(log).getByText('collecting tests')).toBeInTheDocument();
    expect(screen.getByText(/4 lines/)).toBeInTheDocument();
    const req = server.bodies('JobService/StreamLogs')[0] as { id: string; follow?: boolean };
    expect(req.id).toBe('job_01');
    expect(req.follow).toBeFalsy();
  });

  it('links artifacts to the artifact route', async () => {
    signIn(
      fakeServer({
        'ArtifactService/ListArtifacts': () => ({
          artifacts: [{ id: 'art_1', name: 'report.xml', size: '2048', sha256: 'abcdef0123456789' }],
        }),
      }),
    );
    seedJob(JobState.SUCCEEDED);
    renderAt('/jobs/job_01?tab=artifacts');
    const link = await screen.findByRole('link', { name: 'report.xml' });
    expect(link).toHaveAttribute('href', '/api/artifacts/art_1');
    expect(screen.getByText('2.0 KiB')).toBeInTheDocument();
  });

  it('shows the terminal as not available in v1', async () => {
    signIn(fakeServer());
    seedJob();
    renderAt('/jobs/job_01?tab=terminal');
    expect(await screen.findByText('No live terminal in v1')).toBeInTheDocument();
  });

  it('hides cancel in the read-only node UI', async () => {
    signIn(fakeServer(), { mode: 'node' });
    seedJob();
    renderAt('/jobs/job_01');
    await screen.findByRole('region', { name: 'Job job_01' });
    expect(screen.queryByRole('button', { name: 'Cancel' })).toBeNull();
    expect(screen.queryByRole('button', { name: 'Run again' })).toBeNull();
  });
});
