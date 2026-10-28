package setup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Download limits.
const (
	maxRedirects       = 5
	defaultIdleTimeout = 60 * time.Second
)

// Errors of HTTPDownloader.
var (
	ErrTooLarge = errors.New("setup: download larger than expected")
	ErrStalled  = errors.New("setup: download stalled")
)

// HTTPDownloader fetches https URLs. Redirects must stay on https and are
// limited to five hops; the body is limited to the caller's maximum (checked
// against Content-Length first); a body that delivers nothing for
// IdleTimeout is aborted.
type HTTPDownloader struct {
	Client      *http.Client  // a client with 30 s TLS and header timeouts when nil
	IdleTimeout time.Duration // 60 s when zero
}

func checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) > maxRedirects {
		return fmt.Errorf("setup: more than %d redirects", maxRedirects)
	}
	if req.URL.Scheme != "https" {
		return fmt.Errorf("setup: refusing redirect to non-https URL %s", req.URL.Redacted())
	}
	return nil
}

// Get implements Downloader.
func (d HTTPDownloader) Get(ctx context.Context, url string, max int64, w io.Writer) error {
	if !strings.HasPrefix(url, "https://") {
		return fmt.Errorf("setup: refusing non-https URL %s", url)
	}
	var c http.Client
	if d.Client != nil {
		c = *d.Client
	} else {
		c.Transport = &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			TLSHandshakeTimeout:   30 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
		}
	}
	c.CheckRedirect = checkRedirect
	idle := d.IdleTimeout
	if idle <= 0 {
		idle = defaultIdleTimeout
	}

	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	timer := time.AfterFunc(idle, func() { cancel(fmt.Errorf("%w: no data for %s", ErrStalled, idle)) })
	defer timer.Stop()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := c.Do(req)
	if err != nil {
		return causeOr(ctx, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: HTTP %s", url, resp.Status)
	}
	if max > 0 && resp.ContentLength > max {
		return fmt.Errorf("%w: %s is %d bytes, at most %d expected", ErrTooLarge, url, resp.ContentLength, max)
	}
	var body io.Reader = &idleReader{r: resp.Body, timer: timer, idle: idle}
	if max > 0 {
		body = io.LimitReader(body, max+1)
	}
	n, err := io.Copy(w, body)
	if err != nil {
		return causeOr(ctx, err)
	}
	if max > 0 && n > max {
		return fmt.Errorf("%w: %s exceeds %d bytes", ErrTooLarge, url, max)
	}
	return nil
}

// causeOr returns the reason the request context was cancelled (a stall)
// in place of err, when there is one.
func causeOr(ctx context.Context, err error) error {
	if c := context.Cause(ctx); c != nil && errors.Is(c, ErrStalled) {
		return c
	}
	return err
}

// idleReader re-arms the stall timer whenever data arrives.
type idleReader struct {
	mu    sync.Mutex
	r     io.Reader
	timer *time.Timer
	idle  time.Duration
}

func (i *idleReader) Read(p []byte) (int, error) {
	n, err := i.r.Read(p)
	if n > 0 {
		i.mu.Lock()
		i.timer.Reset(i.idle)
		i.mu.Unlock()
	}
	return n, err
}

// maxBytes is the download limit for a source: twice its approximate size.
func maxBytes(s Source) int64 {
	mb := s.SizeMB
	if mb < 1 {
		mb = 1
	}
	return 2 * mb << 20
}
