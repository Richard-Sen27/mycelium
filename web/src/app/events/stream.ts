import { Code, ConnectError } from '@connectrpc/connect';
import { EventKind, type Event, type WatchEventsResponse } from '../../gen/mycel/v1/ui_pb';
import type { StreamState } from '../session';

/** The slice of EventService the stream needs (makes it testable without a server). */
export interface WatchClient {
  watchEvents(
    req: { afterSeq: bigint; kinds?: EventKind[] },
    opts: { signal: AbortSignal; onHeader?: (h: Headers) => void },
  ): AsyncIterable<WatchEventsResponse>;
}

export interface StreamHandlers {
  onEvent: (e: Event) => void;
  /** The hub's ring no longer holds after_seq: every list must be reloaded. */
  onTooOld: () => void;
  onState?: (s: StreamState) => void;
  /** Unauthenticated / PermissionDenied: the stream stops for good. */
  onFatal?: (err: ConnectError) => void;
}

export interface StreamOptions {
  /** Backoff steps in ms; the last one repeats. */
  backoff?: number[];
  sleep?: (ms: number, signal: AbortSignal) => Promise<void>;
}

const defaultSleep = (ms: number, signal: AbortSignal) =>
  new Promise<void>((resolve) => {
    if (signal.aborted) return resolve();
    const t = setTimeout(resolve, ms);
    signal.addEventListener(
      'abort',
      () => {
        clearTimeout(t);
        resolve();
      },
      { once: true },
    );
  });

/**
 * EventStream keeps one EventService.WatchEvents call open and resumes it
 * after a drop with after_seq = the last sequence seen, so no event is lost
 * or applied twice. The hub's first message on every stream is a heartbeat
 * (kind UNSPECIFIED, no body) whose seq is the latest sequence at
 * subscription time; that becomes the resume point even before any event.
 * When the hub answers FailedPrecondition ("too old": the ring buffer moved
 * past after_seq), or a call drops before any position is known, the next
 * call starts from now (after_seq 0) and, once it is up, the owner is asked
 * to reload every list.
 */
export class EventStream {
  lastSeq = 0n;
  private ctrl: AbortController | null = null;
  private attempt = 0;
  private running: Promise<void> | null = null;
  /** A call has reached the server before: a later call from seq 0 may have missed events. */
  private wasLive = false;

  constructor(
    private client: WatchClient,
    private h: StreamHandlers,
    private opts: StreamOptions = {},
  ) {}

  start(): void {
    if (this.ctrl) return;
    this.ctrl = new AbortController();
    this.running = this.run(this.ctrl.signal);
  }

  stop(): Promise<void> {
    this.ctrl?.abort();
    this.ctrl = null;
    this.h.onState?.('stopped');
    return this.running ?? Promise.resolve();
  }

  private async run(signal: AbortSignal): Promise<void> {
    const backoff = this.opts.backoff ?? [500, 1000, 2000, 5000, 10000];
    const sleep = this.opts.sleep ?? defaultSleep;
    while (!signal.aborted) {
      this.h.onState?.(this.attempt === 0 ? 'connecting' : 'retrying');
      try {
        const it = this.client.watchEvents(
          { afterSeq: this.lastSeq },
          {
            signal,
            onHeader: () => {
              this.attempt = 0;
              this.h.onState?.('live');
              // Reconnected without a position (dropped before the first
              // heartbeat, or after "too old"): whatever happened in between
              // is unknown, so reload every list now that the stream is up.
              if (this.wasLive && this.lastSeq === 0n) this.h.onTooOld();
              this.wasLive = true;
            },
          },
        );
        for await (const res of it) {
          if (signal.aborted) break;
          this.attempt = 0;
          this.h.onState?.('live');
          const e = res.event;
          if (!e) continue; // heartbeat
          if (e.kind === EventKind.UNSPECIFIED && e.body.case === undefined) {
            // The stream's first message: a heartbeat carrying the hub's
            // latest seq at subscription time. Resuming from it closes the
            // gap after a drop without a reload.
            if (this.lastSeq === 0n && e.seq > 0n) this.lastSeq = e.seq;
            continue;
          }
          if (this.lastSeq !== 0n && e.seq <= this.lastSeq) continue; // replayed twice
          this.lastSeq = e.seq;
          this.h.onEvent(e);
        }
        // The server closed the stream cleanly: reconnect from where we are.
      } catch (err) {
        if (signal.aborted) break;
        const ce = ConnectError.from(err);
        if (ce.code === Code.FailedPrecondition) {
          // Restart from now; the lists reload when the new call is up (onHeader).
          this.lastSeq = 0n;
          this.attempt = 0;
          this.wasLive = true;
          continue;
        }
        if (ce.code === Code.Unauthenticated || ce.code === Code.PermissionDenied) {
          this.h.onState?.('stopped');
          this.h.onFatal?.(ce);
          this.ctrl = null;
          return;
        }
      }
      if (signal.aborted) break;
      const wait = backoff[Math.min(this.attempt, backoff.length - 1)];
      this.attempt++;
      this.h.onState?.('retrying');
      await sleep(wait, signal);
    }
  }
}
