package daemon

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
)

// Job log streaming limits.
const (
	jobLogChunkMax = 64 << 10 // bytes per job.log chunk
	// jobLogLowAckBackoff: after this many acks in a row below the chunk
	// sent, the sender backs off before resending.
	jobLogLowAckBackoff = 3
	// jobLogMaxLowAcks ends the stream when the hub acks below the chunk
	// sent this many times in a row without the stream advancing (a gap
	// that resending does not close, e.g. a hub that always acks seq-1 or
	// lost chunks the node no longer holds).
	jobLogMaxLowAcks = 8
	// jobLogSendsPerFlush bounds one regular flush, so the sender returns
	// to its loop and notices close.
	jobLogSendsPerFlush = 32
)

// jobLogDroppedMarker is written once per drop episode: when a write first
// finds the unacknowledged buffer full. Further writes are dropped silently
// until one fits again, and the next overflow writes the marker again.
const jobLogDroppedMarker = "\n[mycel] log dropped: hub behind\n"

type logChunk struct {
	seq  uint64
	data []byte
}

// jobLog is the Runner's log writer: it batches output into job.log chunks
// (≤ 64 KiB, flushed every flushEvery or when a chunk is full), numbers them
// from 1, resends from the hub's ack + 1 when the hub acks less than it was
// sent, keeps at most maxBuf bytes unacknowledged and drops output beyond
// that with a marker line.
type jobLog struct {
	send       func(ctx context.Context, c *mycelv1.JobLogChunk) (*mycelv1.JobLogAck, error)
	jobID      string
	flushEvery time.Duration
	maxBuf     int
	warn       func(msg string, args ...any)
	// onFinished is called (once) when the hub says the job is finished.
	onFinished func()

	mu       sync.Mutex
	pending  []byte
	buffered int // pending plus unacked bytes
	dropping bool
	dead     bool // the hub refused the stream; everything is discarded
	closing  bool

	kick    chan struct{}
	closeCh chan time.Time // the drain deadline
	done    chan struct{}

	// Sender-owned.
	unacked []logChunk
	nextSeq uint64
	backoff time.Duration // after a transient failure
	retryAt time.Time     // regular flushes wait until then
	lowAcks int           // acks in a row below the chunk sent
}

func newJobLog(jobID string, flushEvery time.Duration, maxBuf int,
	send func(context.Context, *mycelv1.JobLogChunk) (*mycelv1.JobLogAck, error),
	warn func(string, ...any), onFinished func()) *jobLog {
	l := &jobLog{
		send: send, jobID: jobID, flushEvery: flushEvery, maxBuf: maxBuf, warn: warn, onFinished: onFinished,
		kick: make(chan struct{}, 1), closeCh: make(chan time.Time, 1), done: make(chan struct{}),
		nextSeq: 1,
	}
	go l.loop()
	return l
}

// Write never fails, so the command's pipes keep draining.
func (l *jobLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.dead || l.closing || len(p) == 0 {
		return len(p), nil
	}
	if l.buffered+len(p) > l.maxBuf {
		if !l.dropping {
			l.dropping = true
			// The marker may exceed the cap by its own length.
			l.pending = append(l.pending, jobLogDroppedMarker...)
			l.buffered += len(jobLogDroppedMarker)
			l.wake()
		}
		return len(p), nil
	}
	l.dropping = false
	l.pending = append(l.pending, p...)
	l.buffered += len(p)
	if len(l.pending) >= jobLogChunkMax {
		l.wake()
	}
	return len(p), nil
}

func (l *jobLog) wake() {
	select {
	case l.kick <- struct{}{}:
	default:
	}
}

// close flushes what is buffered, trying until deadline, and stops the
// sender. It returns once the sender stopped.
func (l *jobLog) close(deadline time.Time) {
	l.mu.Lock()
	already := l.closing
	l.closing = true
	l.mu.Unlock()
	if !already {
		l.closeCh <- deadline
	}
	<-l.done
}

func (l *jobLog) loop() {
	defer close(l.done)
	t := time.NewTicker(l.flushEvery)
	defer t.Stop()
	for {
		var deadline time.Time
		select {
		case <-t.C:
		case <-l.kick:
		case deadline = <-l.closeCh:
		}
		l.flush(deadline)
		if !deadline.IsZero() {
			return
		}
	}
}

// cut moves pending bytes into numbered chunks.
func (l *jobLog) cut() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for len(l.pending) > 0 {
		n := min(jobLogChunkMax, len(l.pending))
		data := append([]byte(nil), l.pending[:n]...)
		l.unacked = append(l.unacked, logChunk{seq: l.nextSeq, data: data})
		l.nextSeq++
		l.pending = l.pending[n:]
	}
	if len(l.pending) == 0 {
		l.pending = nil
	}
}

// ackUpTo drops chunks the hub has.
func (l *jobLog) ackUpTo(seq uint64) {
	i := 0
	freed := 0
	for i < len(l.unacked) && l.unacked[i].seq <= seq {
		freed += len(l.unacked[i].data)
		i++
	}
	l.unacked = l.unacked[i:]
	l.mu.Lock()
	l.buffered -= freed
	l.mu.Unlock()
}

func (l *jobLog) kill() {
	l.mu.Lock()
	l.dead = true
	l.pending = nil
	l.buffered = 0
	l.mu.Unlock()
	l.unacked = nil
}

// flush sends what is buffered. A zero deadline means a regular flush: it
// sends at most jobLogSendsPerFlush chunks and never sleeps (a failure sets
// retryAt and a later tick retries). A non-zero deadline drains until then.
func (l *jobLog) flush(deadline time.Time) {
	regular := deadline.IsZero()
	if regular && time.Now().Before(l.retryAt) {
		return
	}
	// wait backs off: a regular flush returns and retries on a later tick,
	// a drain sleeps (it reports false when the deadline passed).
	wait := func(d time.Duration) bool {
		if regular {
			l.retryAt = time.Now().Add(d)
			return false
		}
		return sleepUntil(d, deadline)
	}
	for sends := 0; ; sends++ {
		l.mu.Lock()
		dead := l.dead
		l.mu.Unlock()
		if dead {
			return
		}
		if regular && sends >= jobLogSendsPerFlush {
			l.wake() // more to send: come back after checking for close
			return
		}
		if !regular && !time.Now().Before(deadline) {
			return
		}
		l.cut()
		if len(l.unacked) == 0 {
			return
		}
		c := l.unacked[0]
		ack, err := l.send(context.Background(), &mycelv1.JobLogChunk{JobId: l.jobID, Seq: c.seq, Data: c.data})
		if err != nil {
			var he *hubError
			if errors.As(err, &he) {
				if hubSaysJobFinished(he.msg) {
					l.kill()
					if l.onFinished != nil {
						l.onFinished()
					}
					return
				}
				if !retryableHubCode(errCode(he.msg)) {
					l.warn("job log refused by hub; dropping the rest", "job", l.jobID, "err", err)
					l.kill()
					return
				}
			}
			l.backoff = min(max(l.backoff*2, 200*time.Millisecond), 5*time.Second)
			if !wait(l.backoff) {
				return
			}
			continue
		}
		l.backoff, l.retryAt = 0, time.Time{}
		as := ack.GetSeq()
		if as >= c.seq {
			l.ackUpTo(c.seq)
			l.lowAcks = 0
			continue
		}
		// The hub is missing chunks: drop what it has and resend from its
		// ack + 1. Every such answer counts until the stream advances.
		l.ackUpTo(as)
		l.lowAcks++
		if l.lowAcks >= jobLogMaxLowAcks {
			l.warn("job log: the hub keeps acknowledging less than was sent; dropping the rest", "job", l.jobID, "ack", as, "sent", c.seq)
			l.kill()
			return
		}
		if l.lowAcks >= jobLogLowAckBackoff && !wait(time.Duration(l.lowAcks)*100*time.Millisecond) {
			return
		}
	}
}

// sleepUntil sleeps d, or until deadline when that is sooner; it reports
// false when the deadline has passed.
func sleepUntil(d time.Duration, deadline time.Time) bool {
	if !deadline.IsZero() {
		left := time.Until(deadline)
		if left <= 0 {
			return false
		}
		d = min(d, left)
	}
	time.Sleep(d)
	return deadline.IsZero() || time.Now().Before(deadline)
}

// hubSaysJobFinished: the hub already ended the job (finished, cancelled or
// timed out while the node did not hear about it).
func hubSaysJobFinished(msg string) bool {
	return errCode(msg) == "failed_precondition" && strings.Contains(strings.ToLower(msg), "job finished")
}

// retryableHubCode: hub refusals worth retrying.
func retryableHubCode(code string) bool {
	switch code {
	case "", "unavailable", "internal", "deadline_exceeded", "resource_exhausted", "aborted", "unknown":
		return true
	}
	return false
}
