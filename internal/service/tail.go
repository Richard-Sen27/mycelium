package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"time"
)

// Tail writes the last n lines of path to w; with follow it keeps writing
// what is appended until ctx ends, starting over when the file is truncated
// by a rotation.
func Tail(ctx context.Context, path string, n int, follow bool, w io.Writer) error {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return errors.New("no log yet at " + path + " (is the service installed and started?)")
		}
		return err
	}
	defer func() { _ = f.Close() }()
	off, err := writeLastLines(f, n, w)
	if err != nil || !follow {
		return err
	}
	buf := make([]byte, 32<<10)
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		st, err := f.Stat()
		if err != nil {
			return err
		}
		if st.Size() < off {
			off = 0 // rotated (truncated in place)
		}
		for st.Size() > off {
			k, err := f.ReadAt(buf, off)
			if k > 0 {
				if _, werr := w.Write(buf[:k]); werr != nil {
					return werr
				}
				off += int64(k)
			}
			if err != nil {
				break
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}

// writeLastLines writes the last n lines of f and returns the file size it
// read up to.
func writeLastLines(f *os.File, n int, w io.Writer) (int64, error) {
	st, err := f.Stat()
	if err != nil {
		return 0, err
	}
	size := st.Size()
	if n <= 0 {
		return size, nil
	}
	const chunk = 64 << 10
	var data []byte
	start := size
	for start > 0 && bytes.Count(data, []byte("\n")) <= n {
		k := min(int64(chunk), start)
		start -= k
		b := make([]byte, k)
		if _, err := f.ReadAt(b, start); err != nil && !errors.Is(err, io.EOF) {
			return 0, err
		}
		data = append(b, data...)
	}
	// Drop everything before the last n lines (a trailing newline ends the
	// last line, it does not start another).
	trimmed := bytes.TrimSuffix(data, []byte("\n"))
	idx := len(trimmed)
	for i := 0; i < n; i++ {
		j := bytes.LastIndexByte(trimmed[:idx], '\n')
		if j < 0 {
			idx = -1
			break
		}
		idx = j
	}
	if idx >= 0 {
		data = data[idx+1:]
	}
	_, err = w.Write(data)
	return size, err
}
