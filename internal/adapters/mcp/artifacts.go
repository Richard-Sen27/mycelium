package mcp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
)

// MaxArtifactBytes caps what export_artifact uploads and fetch_artifact
// downloads through the local API (localapi.go: ≤ 256 MiB).
const MaxArtifactBytes = 256 << 20

// artifactCap is MaxArtifactBytes; tests lower it.
var artifactCap int64 = MaxArtifactBytes

// transferTimeout bounds one artifact upload or download over loopback (the
// daemon relays it to the hub).
const transferTimeout = 15 * time.Minute

// ExportArtifact uploads body (size bytes) for session as an artifact named
// name, optionally attached to channel. The daemon relays it to the hub.
func (c *Client) ExportArtifact(ctx context.Context, session, name, channel string, body io.Reader, size int64) (*mycelv1.Artifact, error) {
	q := url.Values{"name": {name}}
	if channel != "" {
		q.Set("channel", channel)
	}
	path := "/api/local/agents/" + url.PathEscape(session) + "/artifacts?" + q.Encode()
	res, err := c.stream(ctx, http.MethodPost, path, body, size)
	if err != nil {
		return nil, err
	}
	defer func() { _ = res.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read local API reply: %w", err)
	}
	var a mycelv1.Artifact
	if err := unmarshal.Unmarshal(b, &a); err != nil {
		return nil, fmt.Errorf("decode local API reply: %w", err)
	}
	if a.GetId() == "" {
		return nil, errors.New("local API returned no artifact id")
	}
	return &a, nil
}

// FetchArtifact downloads artifact id for session into w, at most limit
// bytes; a larger artifact is an error. It returns the number of bytes written.
func (c *Client) FetchArtifact(ctx context.Context, session, id string, w io.Writer, limit int64) (int64, error) {
	path := "/api/local/agents/" + url.PathEscape(session) + "/artifacts/" + url.PathEscape(id)
	res, err := c.stream(ctx, http.MethodGet, path, nil, 0)
	if err != nil {
		return 0, err
	}
	defer func() { _ = res.Body.Close() }()
	if res.ContentLength > limit {
		return 0, fmt.Errorf("artifact %s is %d bytes, more than the %s allowed", id, res.ContentLength, sizeText(limit))
	}
	n, err := io.Copy(w, io.LimitReader(res.Body, limit+1))
	if err != nil {
		return n, fmt.Errorf("download artifact %s: %w", id, err)
	}
	if n > limit {
		return n, fmt.Errorf("artifact %s is more than the %s allowed", id, sizeText(limit))
	}
	return n, nil
}

// stream sends a request with a raw body (nil for none) and returns the 2xx
// response with its body unread; any other status becomes an *APIError.
func (c *Client) stream(ctx context.Context, method, path string, body io.Reader, size int64) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.ContentLength = size
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	res, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("local API at %s: %w", c.base, ctx.Err())
		}
		return nil, fmt.Errorf("mycel node daemon not reachable at %s: %w", c.base, err)
	}
	if res.StatusCode >= 200 && res.StatusCode <= 299 {
		return res, nil
	}
	defer func() { _ = res.Body.Close() }()
	b, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	var e struct {
		Error string `json:"error"`
	}
	msg := ""
	if json.Unmarshal(b, &e) == nil {
		msg = e.Error
	}
	if msg == "" {
		msg = strings.TrimSpace(string(b))
	}
	if msg == "" {
		msg = res.Status
	}
	return nil, &APIError{Status: res.StatusCode, Message: msg}
}

// sizeText renders a byte count like "256 MiB" when it is a whole number of
// MiB, otherwise "<n> bytes".
func sizeText(n int64) string {
	if n > 0 && n%(1<<20) == 0 {
		return fmt.Sprintf("%d MiB", n>>20)
	}
	return fmt.Sprintf("%d bytes", n)
}

// ErrOutsideWorkdir is returned for a path that leaves the working directory.
var ErrOutsideWorkdir = errors.New("path is outside the working directory")

// WorkdirPath turns p (relative to dir, or absolute) into a slash-free
// relative path inside dir. It rejects "", any ".." element and absolute
// paths outside dir. Symlinks are not resolved here: callers open the result
// through an os.Root on dir, which refuses links that lead out.
func WorkdirPath(dir, p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", errors.New("path is required")
	}
	for _, el := range strings.FieldsFunc(p, func(r rune) bool { return r == '/' || r == '\\' }) {
		if el == ".." {
			return "", fmt.Errorf("%w: %q contains \"..\"", ErrOutsideWorkdir, p)
		}
	}
	if !filepath.IsAbs(p) {
		if filepath.VolumeName(p) != "" || strings.HasPrefix(p, "/") || strings.HasPrefix(p, `\`) {
			return "", fmt.Errorf("%w: %q", ErrOutsideWorkdir, p)
		}
		rel := filepath.Clean(p)
		if rel == "." {
			return "", fmt.Errorf("%q is the working directory itself, not a file", p)
		}
		return rel, nil
	}
	// Compare both as given and with symlinks in the directory parts
	// resolved (e.g. /var -> /private/var on macOS); the last element is
	// never resolved, so a link there is judged by os.Root when opened.
	bases := []string{dir}
	if real, err := filepath.EvalSymlinks(dir); err == nil && real != dir {
		bases = append(bases, real)
	}
	p = filepath.Clean(p)
	targets := []string{p}
	if realDir, err := filepath.EvalSymlinks(filepath.Dir(p)); err == nil {
		if rp := filepath.Join(realDir, filepath.Base(p)); rp != p {
			targets = append(targets, rp)
		}
	}
	for _, b := range bases {
		for _, tp := range targets {
			rel, err := filepath.Rel(b, tp)
			if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
				continue
			}
			if rel == "." {
				return "", fmt.Errorf("%q is the working directory itself, not a file", p)
			}
			return rel, nil
		}
	}
	return "", fmt.Errorf("%w: %q is not under %s", ErrOutsideWorkdir, p, dir)
}

// rootError rewords os.Root's escape error.
func rootError(p string, err error) error {
	if err != nil && strings.Contains(err.Error(), "escapes from parent") {
		return fmt.Errorf("%w: %q leads out of it through a symbolic link", ErrOutsideWorkdir, p)
	}
	return err
}

// openExport opens rel under dir for reading: a regular file of at most
// artifactCap bytes, reached without leaving dir.
func openExport(dir, rel string) (*os.File, int64, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, 0, fmt.Errorf("open working directory: %w", err)
	}
	defer func() { _ = root.Close() }()
	f, err := root.Open(rel)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, 0, fmt.Errorf("%s does not exist in the working directory", rel)
		}
		return nil, 0, rootError(rel, err)
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, 0, err
	}
	if !st.Mode().IsRegular() {
		_ = f.Close()
		return nil, 0, fmt.Errorf("%s is not a regular file", rel)
	}
	if st.Size() > artifactCap {
		_ = f.Close()
		return nil, 0, fmt.Errorf("%s is %d bytes, more than the %s an agent may export", rel, st.Size(), sizeText(artifactCap))
	}
	return f, st.Size(), nil
}

// writeFetched writes what fill produces to rel under dir: into a new
// temporary file next to it, renamed over rel on success. Missing parent
// directories are created; nothing is written outside dir.
func writeFetched(dir, rel string, fill func(io.Writer) (int64, error)) (int64, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return 0, fmt.Errorf("open working directory: %w", err)
	}
	defer func() { _ = root.Close() }()
	if parent := filepath.Dir(rel); parent != "." {
		if err := root.MkdirAll(parent, 0o755); err != nil {
			return 0, rootError(rel, err)
		}
	}
	if st, err := root.Lstat(rel); err == nil && st.IsDir() {
		return 0, fmt.Errorf("%s is a directory", rel)
	}
	var rnd [6]byte
	_, _ = rand.Read(rnd[:])
	tmp := filepath.Join(filepath.Dir(rel), ".mycel-fetch-"+hex.EncodeToString(rnd[:]))
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return 0, rootError(rel, err)
	}
	n, err := fill(f)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = rootError(rel, root.Rename(tmp, rel))
	}
	if err != nil {
		_ = root.Remove(tmp)
		return n, err
	}
	return n, nil
}
