import { Code, ConnectError } from '@connectrpc/connect';
import { useEffect, useState } from 'react';
import { Badge, Button, Input, LogView, splitLines } from '../../ds';
import { getApi } from '../../app/api';
import type { Job } from '../../gen/mycel/v1/job_pb';
import { errText } from '../channels/model';
import { isFinished } from './model';
import s from './Jobs.module.css';

const MAX_LINES = 200_000;

type Status = 'connecting' | 'streaming' | 'ended' | 'error';

export interface JobLog {
  lines: string[];
  /** Line number of lines[0] (lines before it were trimmed to bound memory). */
  first: number;
  status: Status;
  error: string | null;
}

const nextFrame = (fn: () => void): (() => void) => {
  if (typeof requestAnimationFrame === 'function') {
    const h = requestAnimationFrame(fn);
    return () => cancelAnimationFrame(h);
  }
  const h = setTimeout(fn, 16);
  return () => clearTimeout(h);
};

/**
 * useJobLog streams a job's log with JobService.StreamLogs (following while
 * the job runs) and returns its lines. New lines are batched per animation
 * frame; after a stream error it reconnects once from the last byte it has.
 * The stream is per job and closes when the view unmounts.
 */
export function useJobLog(id: string, follow: boolean): JobLog {
  const [log, setLog] = useState<{ lines: string[]; first: number }>({ lines: [], first: 1 });
  const [status, setStatus] = useState<Status>('connecting');
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    const ctl = new AbortController();
    const decoder = new TextDecoder();
    let carry = '';
    let nextByte = 0n;
    let pending: string[] = [];
    let cancelFrame: (() => void) | null = null;
    setLog({ lines: [], first: 1 });
    setStatus('connecting');
    setError(null);

    const flush = () => {
      cancelFrame = null;
      if (pending.length === 0) return;
      const add = pending;
      pending = [];
      setLog((prev) => {
        const all = prev.lines.concat(add);
        const drop = Math.max(0, all.length - MAX_LINES);
        return drop ? { lines: all.slice(drop), first: prev.first + drop } : { lines: all, first: prev.first };
      });
    };
    const push = (lines: string[]) => {
      if (lines.length === 0) return;
      pending.push(...lines);
      cancelFrame ??= nextFrame(flush);
    };

    const run = async (fromByte: bigint) => {
      for await (const r of getApi().rpc.jobs.streamLogs({ id, fromByte, follow }, { signal: ctl.signal })) {
        setStatus('streaming');
        nextByte = r.offset + BigInt(r.data.length);
        const out = splitLines(carry, decoder.decode(r.data, { stream: !r.eof }));
        carry = out.carry;
        if (r.eof && carry) {
          out.lines.push(carry);
          carry = '';
        }
        push(out.lines);
        if (r.eof) return;
      }
    };

    (async () => {
      let retried = false;
      for (;;) {
        try {
          await run(nextByte);
          if (carry) push([carry]);
          carry = '';
          if (!ctl.signal.aborted) setStatus('ended');
          return;
        } catch (err) {
          if (ctl.signal.aborted || ConnectError.from(err).code === Code.Canceled) return;
          if (!retried) {
            retried = true;
            setStatus('connecting');
            continue;
          }
          setError(errText(err));
          setStatus('error');
          return;
        }
      }
    })();
    return () => {
      ctl.abort();
      cancelFrame?.();
    };
  }, [id, follow]);

  return { lines: log.lines, first: log.first, status, error };
}

/** JobLogs: the live log with a follow toggle and search within. */
export function JobLogs({ job }: { job: Job }) {
  const finished = isFinished(job);
  // Follow the stream only if the job was still running when the tab opened;
  // the server ends a followed stream with eof once the job finishes.
  const [streamFollow] = useState(!finished);
  const { lines, first, status, error } = useJobLog(job.id, streamFollow);
  const [follow, setFollow] = useState(true);
  const [query, setQuery] = useState('');
  const matches = query.trim() ? lines.filter((l) => l.toLowerCase().includes(query.trim().toLowerCase())).length : null;
  return (
    <div className={s.logs}>
      <div className={s.logTools}>
        <Input
          aria-label="Search the log"
          placeholder="Search"
          mono
          value={query}
          onChange={(e) => setQuery(e.target.value)}
          className={s.logSearch}
        />
        <Button size="sm" variant={follow ? 'primary' : 'default'} aria-pressed={follow} onClick={() => setFollow(!follow)}>
          Follow
        </Button>
        <span className={s.logCount}>{matches != null ? `${matches} of ${lines.length}` : lines.length} lines</span>
        <span className={s.spacer} />
        {status === 'streaming' && !finished && (
          <Badge tone="ok" pulse>
            live
          </Badge>
        )}
        {status === 'connecting' && <Badge tone="idle">connecting</Badge>}
        {(status === 'ended' || (finished && status === 'streaming')) && <Badge tone="off">end of log</Badge>}
        {status === 'error' && <Badge tone="alert">error</Badge>}
      </div>
      {error && (
        <p className={s.error} role="alert">
          {error}
        </p>
      )}
      <LogView
        firstLine={first}
        lines={lines}
        follow={follow}
        onFollowChange={setFollow}
        query={query}
        aria-label={`Log of ${job.id}`}
        className={s.logView}
      />
    </div>
  );
}
