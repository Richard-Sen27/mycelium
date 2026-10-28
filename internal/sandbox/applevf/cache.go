//go:build darwin

package applevf

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Lima 2.x has no command that only downloads an image (`limactl create`
// does not fetch it; `limactl start` boots a VM), so Prepare downloads the
// pinned image itself into Lima's download cache, in the layout Lima's
// downloader uses:
//
//	<cache>/download/by-url-sha256/<sha256(url) hex>/data           the image
//	<cache>/download/by-url-sha256/<sha256(url) hex>/url            the URL
//	<cache>/download/by-url-sha256/<sha256(url) hex>/<algo>.digest  "algo:hex"
//
// <cache> is $LIMA_CACHE_HOME or ~/Library/Caches/lima. When Lima later
// starts an instance with the same URL and digest it finds the cached data
// and the matching digest file and skips the download. The digest is
// verified while downloading; the file is renamed into place only if it
// matches.

// cacheEntryDir is Lima's cache directory for one URL.
func cacheEntryDir(cacheDir, url string) string {
	sum := sha256.Sum256([]byte(url))
	return filepath.Join(cacheDir, "download", "by-url-sha256", hex.EncodeToString(sum[:]))
}

// digestHash parses "sha256:<hex>" or "sha512:<hex>".
func digestHash(d string) (algo string, want []byte, h hash.Hash, err error) {
	algo, hexsum, ok := strings.Cut(d, ":")
	if !ok {
		return "", nil, nil, fmt.Errorf("applevf: malformed digest %q", d)
	}
	want, err = hex.DecodeString(hexsum)
	if err != nil {
		return "", nil, nil, fmt.Errorf("applevf: malformed digest %q", d)
	}
	switch algo {
	case "sha256":
		h = sha256.New()
	case "sha512":
		h = sha512.New()
	default:
		return "", nil, nil, fmt.Errorf("applevf: unsupported digest algorithm %q", algo)
	}
	if len(want) != h.Size() {
		return "", nil, nil, fmt.Errorf("applevf: malformed digest %q", d)
	}
	return algo, want, h, nil
}

func (b *Backend) imageLock(url string) *sync.Mutex {
	b.prepMu.Lock()
	defer b.prepMu.Unlock()
	m, ok := b.prepLock[url]
	if !ok {
		m = &sync.Mutex{}
		b.prepLock[url] = m
	}
	return m
}

// warmCache makes sure the image is in Lima's cache with a verified digest.
func (b *Backend) warmCache(ctx context.Context, img limaImage, progress func(string)) error {
	lock := b.imageLock(img.Location)
	lock.Lock()
	defer lock.Unlock()

	cacheDir, err := b.cacheDir()
	if err != nil {
		return fmt.Errorf("applevf: lima cache dir: %w", err)
	}
	algo, want, h, err := digestHash(img.Digest)
	if err != nil {
		return err
	}
	dir := cacheEntryDir(cacheDir, img.Location)
	data := filepath.Join(dir, "data")
	digestFile := filepath.Join(dir, algo+".digest")

	if _, err := os.Stat(data); err == nil {
		if got, err := os.ReadFile(digestFile); err == nil && strings.TrimSpace(string(got)) == img.Digest {
			progress("image cached: " + img.Location)
			return nil
		}
		// Data without our digest record: verify it once.
		progress("verifying cached image " + img.Location)
		if ok, err := fileHasDigest(data, h, want); err != nil {
			return err
		} else if ok {
			return writeCacheMeta(dir, img, digestFile)
		}
		b.log.Warn("applevf: cached image has a wrong digest, downloading again", "url", img.Location)
		h.Reset()
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("applevf: lima cache: %w", err)
	}
	tmp, err := os.CreateTemp(dir, "data.mycel-*")
	if err != nil {
		return fmt.Errorf("applevf: lima cache: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, img.Location, nil)
	if err != nil {
		_ = tmp.Close()
		return fmt.Errorf("applevf: download %s: %w", img.Location, err)
	}
	resp, err := b.http.Do(req)
	if err != nil {
		_ = tmp.Close()
		return fmt.Errorf("applevf: download %s: %w", img.Location, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		_ = tmp.Close()
		return fmt.Errorf("applevf: download %s: HTTP %s", img.Location, resp.Status)
	}
	progress("downloading " + img.Location)
	pw := &progressWriter{total: resp.ContentLength, report: progress, every: 10 * time.Second}
	_, err = io.Copy(io.MultiWriter(tmp, h, pw), resp.Body)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("applevf: download %s: %w", img.Location, err)
	}
	if got := h.Sum(nil); !bytes.Equal(got, want) {
		return fmt.Errorf("applevf: download %s: digest mismatch: got %s:%s, want %s", img.Location, algo, hex.EncodeToString(got), img.Digest)
	}
	if err := os.Chmod(tmpName, 0o644); err != nil { //nolint:gosec // Lima's cached images are 0644
		return fmt.Errorf("applevf: lima cache: %w", err)
	}
	if err := os.Rename(tmpName, data); err != nil {
		return fmt.Errorf("applevf: lima cache: %w", err)
	}
	if err := writeCacheMeta(dir, img, digestFile); err != nil {
		return err
	}
	progress("image ready: " + img.Location)
	b.log.Info("applevf: image cached", "url", img.Location, "bytes", pw.n)
	return nil
}

func writeCacheMeta(dir string, img limaImage, digestFile string) error {
	if err := os.WriteFile(filepath.Join(dir, "url"), []byte(img.Location), 0o644); err != nil { //nolint:gosec // Lima's own cache files are 0644
		return fmt.Errorf("applevf: lima cache: %w", err)
	}
	if err := os.WriteFile(digestFile, []byte(img.Digest), 0o644); err != nil { //nolint:gosec // as above
		return fmt.Errorf("applevf: lima cache: %w", err)
	}
	return nil
}

func fileHasDigest(path string, h hash.Hash, want []byte) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, fmt.Errorf("applevf: lima cache: %w", err)
	}
	defer func() { _ = f.Close() }()
	if _, err := io.Copy(h, f); err != nil {
		return false, fmt.Errorf("applevf: lima cache: %w", err)
	}
	return bytes.Equal(h.Sum(nil), want), nil
}

// progressWriter reports download progress at most every interval.
type progressWriter struct {
	n, total int64
	report   func(string)
	every    time.Duration
	last     time.Time
}

func (p *progressWriter) Write(b []byte) (int, error) {
	p.n += int64(len(b))
	if now := time.Now(); now.Sub(p.last) >= p.every {
		p.last = now
		if p.total > 0 {
			p.report(fmt.Sprintf("downloaded %d%% (%d/%d MiB)", p.n*100/p.total, p.n>>20, p.total>>20))
		} else {
			p.report(fmt.Sprintf("downloaded %d MiB", p.n>>20))
		}
	}
	return len(b), nil
}
