import { create } from '@bufbuild/protobuf';
import { Code, ConnectError } from '@connectrpc/connect';
import { describe, expect, it, vi } from 'vitest';
import { EventKind, EventSchema, WatchEventsResponseSchema, type WatchEventsResponse } from '../../gen/mycel/v1/ui_pb';
import { EventStream, type WatchClient } from './stream';

const ev = (seq: number) =>
  create(WatchEventsResponseSchema, { event: create(EventSchema, { seq: BigInt(seq), kind: EventKind.NODE, id: `n${seq}` }) });
const heartbeat = () => create(WatchEventsResponseSchema, {});
/** The hub's first message on a stream: kind UNSPECIFIED, no body, seq = latest at subscription. */
const firstHeartbeat = (seq: number) =>
  create(WatchEventsResponseSchema, { event: create(EventSchema, { seq: BigInt(seq), kind: EventKind.UNSPECIFIED }) });

type Script = Array<WatchEventsResponse | Error>;

/** A fake EventService: each call plays the next script; an Error item is thrown. */
function fakeClient(scripts: Script[]) {
  const requests: bigint[] = [];
  let call = 0;
  const client: WatchClient = {
    watchEvents(req, opts) {
      requests.push(req.afterSeq);
      const script = (call < scripts.length ? scripts[call] : 'hang') as Script | 'hang';
      call++;
      return (async function* () {
        // A call that fails before any message never got response headers.
        if (script === 'hang' || !(script[0] instanceof Error)) opts.onHeader?.(new Headers());
        if (script === 'hang') {
          await new Promise<void>((resolve) => opts.signal.addEventListener('abort', () => resolve(), { once: true }));
          return;
        }
        for (const item of script) {
          if (item instanceof Error) throw item;
          yield item;
        }
      })();
    },
  };
  return { client, requests, calls: () => call };
}

const until = async (cond: () => boolean) => {
  for (let i = 0; i < 200 && !cond(); i++) await new Promise((r) => setTimeout(r, 1));
  expect(cond()).toBe(true);
};

const noSleep = () => Promise.resolve();

describe('event stream', () => {
  it('starts from now and resumes with the last seen seq after a drop', async () => {
    const { client, requests, calls } = fakeClient([[ev(5), heartbeat(), ev(6), new ConnectError('reset', Code.Unavailable)], [ev(7)]]);
    const seen: bigint[] = [];
    const s = new EventStream(client, { onEvent: (e) => seen.push(e.seq), onTooOld: vi.fn() }, { sleep: noSleep });
    s.start();
    await until(() => calls() >= 3);
    await s.stop();
    expect(requests.slice(0, 3)).toEqual([0n, 6n, 7n]);
    expect(seen).toEqual([5n, 6n, 7n]);
  });

  it('drops events replayed at or below the last seq', async () => {
    const { client, calls } = fakeClient([
      [ev(3), ev(4)],
      [ev(4), ev(5)],
    ]);
    const seen: bigint[] = [];
    const s = new EventStream(client, { onEvent: (e) => seen.push(e.seq), onTooOld: vi.fn() }, { sleep: noSleep });
    s.start();
    await until(() => calls() >= 3);
    await s.stop();
    expect(seen).toEqual([3n, 4n, 5n]);
  });

  it('on "too old" reloads the lists and restarts from now', async () => {
    const onTooOld = vi.fn();
    const { client, requests, calls } = fakeClient([[ev(10), new ConnectError('too old', Code.FailedPrecondition)], [ev(500)]]);
    const s = new EventStream(client, { onEvent: vi.fn(), onTooOld }, { sleep: noSleep });
    s.start();
    await until(() => calls() >= 3);
    await s.stop();
    expect(onTooOld).toHaveBeenCalledTimes(1);
    expect(requests.slice(0, 3)).toEqual([0n, 0n, 500n]);
  });

  it('reloads the lists when a live call drops before any position is known', async () => {
    const onTooOld = vi.fn();
    const { client, requests, calls } = fakeClient([[firstHeartbeat(0), new ConnectError('reset', Code.Unavailable)]]);
    const s = new EventStream(client, { onEvent: vi.fn(), onTooOld }, { sleep: noSleep });
    s.start();
    await until(() => calls() >= 2 && onTooOld.mock.calls.length > 0);
    await s.stop();
    expect(requests.slice(0, 2)).toEqual([0n, 0n]);
    expect(onTooOld).toHaveBeenCalledTimes(1);
  });

  it('resumes from the first heartbeat seq without a reload', async () => {
    const onTooOld = vi.fn();
    const seen: bigint[] = [];
    const { client, requests, calls } = fakeClient([[firstHeartbeat(42), new ConnectError('reset', Code.Unavailable)], [ev(43)]]);
    const s = new EventStream(client, { onEvent: (e) => seen.push(e.seq), onTooOld }, { sleep: noSleep });
    s.start();
    await until(() => calls() >= 3);
    await s.stop();
    expect(requests.slice(0, 3)).toEqual([0n, 42n, 43n]);
    expect(seen).toEqual([43n]); // the heartbeat itself is not dispatched
    expect(onTooOld).not.toHaveBeenCalled();
  });

  it('stops for good when the session is rejected', async () => {
    const onFatal = vi.fn();
    const states: string[] = [];
    const { client, calls } = fakeClient([[new ConnectError('no session', Code.Unauthenticated)]]);
    const s = new EventStream(client, { onEvent: vi.fn(), onTooOld: vi.fn(), onFatal, onState: (x) => states.push(x) }, { sleep: noSleep });
    s.start();
    await until(() => onFatal.mock.calls.length === 1);
    await new Promise((r) => setTimeout(r, 5));
    expect(calls()).toBe(1);
    expect(states.at(-1)).toBe('stopped');
  });

  it('backs off between failed attempts', async () => {
    const waits: number[] = [];
    const err = () => new ConnectError('down', Code.Unavailable);
    const { client, calls } = fakeClient([[err()], [err()], [err()]]);
    const s = new EventStream(
      client,
      { onEvent: vi.fn(), onTooOld: vi.fn() },
      {
        backoff: [10, 20, 40],
        sleep: async (ms) => {
          waits.push(ms);
        },
      },
    );
    s.start();
    await until(() => calls() >= 4);
    await s.stop();
    expect(waits.slice(0, 3)).toEqual([10, 20, 40]);
  });
});
