package service

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// Log rotation defaults: daemon.log plus daemon.log.1 and daemon.log.2, each
// up to 10 MiB.
const (
	DefaultMaxLogBytes = 10 << 20
	DefaultLogFiles    = 3
)

// RotatingWriter appends to a log file and rotates it when it reaches
// maxBytes, keeping files-1 older copies (path.1 is the newest).
//
// Rotation copies the file and truncates it in place rather than renaming
// it, because launchd and systemd also append the daemon's stdout and
// stderr to the same file through descriptors of their own; every writer
// opens with O_APPEND, so after the truncation they all continue at the
// start of the same file. Lines the service manager appends between the
// copy and the truncation (stderr output only; the daemon's own log goes
// through this writer) are lost.
type RotatingWriter struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	files    int
	f        *os.File
	size     int64
}

// NewRotatingWriter opens (creating) path for appending.
func NewRotatingWriter(path string, maxBytes int64, files int) (*RotatingWriter, error) {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxLogBytes
	}
	if files < 1 {
		files = 1
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return &RotatingWriter{path: path, maxBytes: maxBytes, files: files, f: f, size: st.Size()}, nil
}

// Write appends p, rotating first when the file is full.
func (w *RotatingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return 0, os.ErrClosed
	}
	// Others append to the file too: take its real size.
	if st, err := w.f.Stat(); err == nil {
		w.size = st.Size()
	}
	if w.size > 0 && w.size+int64(len(p)) > w.maxBytes {
		if err := w.rotate(); err != nil {
			return 0, fmt.Errorf("rotate %s: %w", w.path, err)
		}
	}
	n, err := w.f.Write(p)
	w.size += int64(n)
	return n, err
}

func (w *RotatingWriter) rotate() error {
	if w.files > 1 {
		for i := w.files - 1; i > 1; i-- {
			older, newer := fmt.Sprintf("%s.%d", w.path, i), fmt.Sprintf("%s.%d", w.path, i-1)
			if _, err := os.Stat(newer); err == nil {
				if err := os.Rename(newer, older); err != nil {
					return err
				}
			}
		}
		if err := copyFile(w.path, w.path+".1"); err != nil {
			return err
		}
	}
	// By path: Windows strips write access from O_APPEND handles, so
	// (*os.File).Truncate would fail there.
	if err := os.Truncate(w.path, 0); err != nil {
		return err
	}
	w.size = 0
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// Close closes the file.
func (w *RotatingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return nil
	}
	err := w.f.Close()
	w.f = nil
	return err
}
