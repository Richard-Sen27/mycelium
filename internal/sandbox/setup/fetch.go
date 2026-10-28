package setup

import (
	"archive/tar"
	"bytes"
	"compress/bzip2"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

func newDigest(d string) (hash.Hash, []byte, error) {
	algo, hx, ok := strings.Cut(d, ":")
	if !ok {
		return nil, nil, fmt.Errorf("setup: malformed digest %q", d)
	}
	want, err := hex.DecodeString(hx)
	if err != nil {
		return nil, nil, fmt.Errorf("setup: malformed digest %q", d)
	}
	var h hash.Hash
	switch algo {
	case "sha256":
		h = sha256.New()
	case "sha512":
		h = sha512.New()
	default:
		return nil, nil, fmt.Errorf("setup: unsupported digest %q", algo)
	}
	if len(want) != h.Size() {
		return nil, nil, fmt.Errorf("setup: malformed digest %q", d)
	}
	return h, want, nil
}

func (o *Options) downloadPath(src Source) string {
	return filepath.Join(o.Dir, "downloads", src.File)
}

// cached reports whether the source is downloaded with the right digest.
func (o *Options) cached(src Source) bool {
	h, want, err := newDigest(src.Digest)
	if err != nil {
		return false
	}
	f, err := os.Open(o.downloadPath(src))
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	if _, err := io.Copy(h, f); err != nil {
		return false
	}
	return bytes.Equal(h.Sum(nil), want)
}

// uncached returns the sources not downloaded yet and their total size.
func (o *Options) uncached(srcs ...Source) (names []string, mb int64) {
	for _, s := range srcs {
		if !o.cached(s) {
			names = append(names, s.Name+" "+s.Version)
			mb += s.SizeMB
		}
	}
	return names, mb
}

// fetch makes sure the source is in <Dir>/downloads with the pinned digest
// and returns its path. A download is written to a .part file and renamed
// only when its digest matches; a mismatch removes it and returns
// ErrChecksum.
func (o *Options) fetch(ctx context.Context, src Source) (string, error) {
	dst := o.downloadPath(src)
	if o.cached(src) {
		o.logf("%s %s: already downloaded and verified", src.Name, src.Version)
		return dst, nil
	}
	h, want, err := newDigest(src.Digest)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return "", err
	}
	part := dst + ".part"
	f, err := os.OpenFile(part, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return "", err
	}
	o.logf("downloading %s %s (about %d MB) from %s", src.Name, src.Version, src.SizeMB, src.URL)
	start := time.Now()
	err = o.Downloader.Get(ctx, src.URL, maxBytes(src), io.MultiWriter(f, h))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(part)
		return "", fmt.Errorf("download %s: %w", src.URL, err)
	}
	if got := h.Sum(nil); !bytes.Equal(got, want) {
		_ = os.Remove(part)
		return "", fmt.Errorf("%w: %s: got %s, want %s", ErrChecksum, src.URL, hex.EncodeToString(got), src.Digest)
	}
	if err := os.Rename(part, dst); err != nil {
		_ = os.Remove(part)
		return "", err
	}
	o.logf("verified %s (%s) in %s", src.File, strings.SplitN(src.Digest, ":", 2)[0], time.Since(start).Round(time.Second))
	return dst, nil
}

// gvisorEntry is one file of the gVisor release that gets installed.
type gvisorEntry struct {
	Name string // "runsc", "gvisor-bin" (dir) or "gvisor-bin/<file>"
	Dir  bool
	Mode int64
	Size int64
	Body io.Reader
}

// walkGVisor reads a gvisor.tar.bz2 release and calls fn for runsc and the
// gvisor-bin/ directory and its regular files; everything else (the
// containerd shim, links, odd names) is skipped. It fails when runsc is
// missing.
func walkGVisor(r io.Reader, fn func(gvisorEntry) error) error {
	tr := tar.NewReader(bzip2.NewReader(r))
	sawRunsc := false
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("gvisor release: %w", err)
		}
		name := strings.TrimPrefix(path.Clean(strings.TrimPrefix(hdr.Name, "./")), "/")
		switch {
		case name == "runsc" && hdr.Typeflag == tar.TypeReg:
			sawRunsc = true
		case name == "gvisor-bin" && hdr.Typeflag == tar.TypeDir:
			if err := fn(gvisorEntry{Name: name, Dir: true, Mode: 0o755}); err != nil {
				return err
			}
			continue
		case strings.HasPrefix(name, "gvisor-bin/") && !strings.Contains(name[len("gvisor-bin/"):], "/") &&
			hdr.Typeflag == tar.TypeReg:
		default:
			continue
		}
		if err := fn(gvisorEntry{Name: name, Mode: 0o755, Size: hdr.Size, Body: tr}); err != nil {
			return err
		}
	}
	if !sawRunsc {
		return errors.New("gvisor release: no runsc in the archive")
	}
	return nil
}

// gvisorTar rewrites the verified release as a plain tar holding only runsc
// and gvisor-bin/, owned by root, for `tar -x` inside the WSL distro.
func gvisorTar(release io.Reader, w io.Writer) error {
	tw := tar.NewWriter(w)
	mtime := time.Unix(946684800, 0) // the release's own fixed date
	err := walkGVisor(release, func(e gvisorEntry) error {
		hdr := &tar.Header{Name: e.Name, Mode: e.Mode, ModTime: mtime, Format: tar.FormatPAX}
		if e.Dir {
			hdr.Typeflag, hdr.Name = tar.TypeDir, e.Name+"/"
			return tw.WriteHeader(hdr)
		}
		hdr.Typeflag, hdr.Size = tar.TypeReg, e.Size
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		_, err := io.Copy(tw, e.Body)
		return err
	})
	if err != nil {
		return err
	}
	return tw.Close()
}

// installGVisorLocal installs runsc and gvisor-bin/ from the release into
// dir, each file written to a temporary name and renamed into place.
func installGVisorLocal(release io.Reader, dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // a bin directory
		return err
	}
	return walkGVisor(release, func(e gvisorEntry) error {
		p := filepath.Join(dir, filepath.FromSlash(e.Name))
		if e.Dir {
			return os.MkdirAll(p, 0o755) //nolint:gosec // a bin directory
		}
		tmp := p + ".mycel-tmp"
		f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755) //nolint:gosec // executables
		if err != nil {
			return err
		}
		_, err = io.Copy(f, io.LimitReader(e.Body, e.Size))
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err == nil {
			err = os.Rename(tmp, p)
		}
		if err != nil {
			_ = os.Remove(tmp)
		}
		return err
	})
}
