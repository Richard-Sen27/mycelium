package bundle

import (
	"io"
	"sync"
)

// bufPipe is an in-memory pipe with a bounded buffer, so a caller that reads
// stdout to EOF before touching stderr does not deadlock the process as long
// as stderr stays under the bound.
type bufPipe struct {
	mu     sync.Mutex
	cond   *sync.Cond
	buf    []byte
	max    int
	closed bool  // writer side closed
	err    error // reported after the buffer drains
	rdone  bool  // reader side closed: writes are discarded
}

func newBufPipe(max int) *bufPipe {
	p := &bufPipe{max: max}
	p.cond = sync.NewCond(&p.mu)
	return p
}

func (p *bufPipe) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for len(b) > 0 {
		for len(p.buf) >= p.max && !p.rdone && !p.closed {
			p.cond.Wait()
		}
		if p.rdone {
			return n + len(b), nil
		}
		if p.closed {
			return n, io.ErrClosedPipe
		}
		k := p.max - len(p.buf)
		if k > len(b) {
			k = len(b)
		}
		p.buf = append(p.buf, b[:k]...)
		b = b[k:]
		n += k
		p.cond.Broadcast()
	}
	return n, nil
}

func (p *bufPipe) Read(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for len(p.buf) == 0 && !p.closed {
		p.cond.Wait()
	}
	if len(p.buf) == 0 {
		if p.err != nil {
			return 0, p.err
		}
		return 0, io.EOF
	}
	n := copy(b, p.buf)
	p.buf = p.buf[n:]
	p.cond.Broadcast()
	return n, nil
}

// CloseWrite ends the stream; readers see err (or EOF) after the buffer.
func (p *bufPipe) CloseWrite(err error) {
	p.mu.Lock()
	p.closed = true
	p.err = err
	p.cond.Broadcast()
	p.mu.Unlock()
}

// Close is the reader giving up: later writes are discarded.
func (p *bufPipe) Close() error {
	p.mu.Lock()
	p.rdone = true
	p.buf = nil
	p.cond.Broadcast()
	p.mu.Unlock()
	return nil
}
