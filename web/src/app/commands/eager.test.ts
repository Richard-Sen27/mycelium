import { create } from '@bufbuild/protobuf';
import { afterEach, describe, expect, it } from 'vitest';
import { channelsStore, jobsStore } from '../events/stores';
import { ChannelSchema, ChannelType } from '../../gen/mycel/v1/channel_pb';
import { JobSchema, JobState } from '../../gen/mycel/v1/job_pb';
import { fakeServer, resetAll, seed, signIn } from '../../views/test-utils/fakeServer';
import { registerViewCommands } from './eager';
import { availableCommands } from './registry';

describe('eager view commands', () => {
  afterEach(() => resetAll());

  it('offer channel and job actions before their views load', () => {
    signIn(fakeServer(), { admin: true });
    seed(channelsStore, [create(ChannelSchema, { name: '#backend', type: ChannelType.TOPIC, ownerId: 'u1' })]);
    seed(jobsStore, [create(JobSchema, { id: 'job_1', state: JobState.RUNNING, spec: { command: ['make'] } })]);
    registerViewCommands();
    const ctx = {
      rights: { mode: 'hub' as const, isAdmin: true, readOnly: false, userId: 'u1', userName: 'a' },
      navigate: () => undefined,
    };
    const ids = availableCommands(ctx).map((c) => c.id);
    expect(ids).toEqual(expect.arrayContaining(['channel.freeze.#backend', 'channel.archive.#backend', 'job.run', 'job.cancel.job_1']));
    // The node UI is read-only: none of them.
    const ro = availableCommands({ ...ctx, rights: { ...ctx.rights, mode: 'node', readOnly: true, isAdmin: false } }).map((c) => c.id);
    expect(ro.some((i) => i.startsWith('channel.') || i.startsWith('job.'))).toBe(false);
  });
});
