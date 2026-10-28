package hub

import (
	"bufio"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"golang.org/x/crypto/argon2"
)

// Export file format MYCELHUB2 (moving.go): a header, then the tar.gz in
// AES-256-GCM segments, so neither export nor import holds the archive in
// memory.
//
//	header  = "MYCELHUB2" | argon2id salt (16) | base nonce (12)
//	segment = AES-256-GCM(1 MiB of plaintext; the last one may be shorter, even empty)
//	nonce   = base nonce XOR big-endian segment counter (in its last 8 bytes)
//	AAD     = header | 0x01 for the last segment, 0x00 otherwise
//
// Binding the header into every segment protects the salt and nonce;
// numbering the nonces stops reordering; the last-segment flag makes a
// file cut at a segment boundary fail instead of decrypting to a prefix.
const (
	exportMagic   = "MYCELHUB2"
	exportSaltLen = 16
	exportNonce   = 12
	exportHdrLen  = len(exportMagic) + exportSaltLen + exportNonce
	exportSegment = 1 << 20
	exportTagLen  = 16
	argonTime     = 3
	argonMemory   = 64 * 1024 // KiB
	argonThreads  = 4
	exportKeyLen  = 32

	// MinPassphrase is the shortest export passphrase accepted.
	MinPassphrase = 12
)

// ErrExportDecrypt is returned for a wrong passphrase or a damaged file.
var ErrExportDecrypt = errors.New("hub: cannot decrypt the export (wrong passphrase or damaged file)")

func exportKey(pass, salt []byte) []byte {
	return argon2.IDKey(pass, salt, argonTime, argonMemory, argonThreads, exportKeyLen)
}

type segmentCipher struct {
	gcm  cipher.AEAD
	hdr  []byte
	base [exportNonce]byte
	ctr  uint64
}

func newSegmentCipher(pass, hdr []byte) (*segmentCipher, error) {
	block, err := aes.NewCipher(exportKey(pass, hdr[len(exportMagic):len(exportMagic)+exportSaltLen]))
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	c := &segmentCipher{gcm: gcm, hdr: hdr}
	copy(c.base[:], hdr[len(exportMagic)+exportSaltLen:])
	return c, nil
}

// next returns the nonce and additional data of the next segment.
func (c *segmentCipher) next(last bool) (nonce, aad []byte) {
	n := c.base
	var ctr [8]byte
	binary.BigEndian.PutUint64(ctr[:], c.ctr)
	for i := range ctr {
		n[exportNonce-8+i] ^= ctr[i]
	}
	c.ctr++
	flag := byte(0)
	if last {
		flag = 1
	}
	return n[:], append(append([]byte{}, c.hdr...), flag)
}

// sealWriter encrypts what is written to it into w; Close writes the last
// segment and must be called.
type sealWriter struct {
	w   io.Writer
	c   *segmentCipher
	buf []byte
}

func newSealWriter(w io.Writer, pass []byte) (*sealWriter, error) {
	if len(pass) < MinPassphrase {
		return nil, fmt.Errorf("hub: the passphrase must have at least %d characters", MinPassphrase)
	}
	hdr := make([]byte, exportHdrLen)
	copy(hdr, exportMagic)
	if _, err := rand.Read(hdr[len(exportMagic):]); err != nil {
		return nil, err
	}
	c, err := newSegmentCipher(pass, hdr)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(hdr); err != nil {
		return nil, err
	}
	return &sealWriter{w: w, c: c, buf: make([]byte, 0, exportSegment)}, nil
}

func (s *sealWriter) flush(last bool) error {
	nonce, aad := s.c.next(last)
	_, err := s.w.Write(s.c.gcm.Seal(nil, nonce, s.buf, aad))
	s.buf = s.buf[:0]
	return err
}

// Write buffers p; a full segment is sealed only when more data follows,
// so the final segment is always the one Close seals.
func (s *sealWriter) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		if len(s.buf) == exportSegment {
			if err := s.flush(false); err != nil {
				return n - len(p), err
			}
		}
		k := min(len(p), exportSegment-len(s.buf))
		s.buf = append(s.buf, p[:k]...)
		p = p[k:]
	}
	return n, nil
}

func (s *sealWriter) Close() error { return s.flush(true) }

// openReader decrypts an export as it is read.
type openReader struct {
	r    *bufio.Reader
	c    *segmentCipher
	seg  []byte
	buf  []byte
	done bool
}

func newOpenReader(r io.Reader, pass []byte) (*openReader, error) {
	hdr := make([]byte, exportHdrLen)
	if _, err := io.ReadFull(r, hdr); err != nil || string(hdr[:len(exportMagic)]) != exportMagic {
		return nil, errors.New("hub: not a mycel hub export (bad magic)")
	}
	c, err := newSegmentCipher(pass, hdr)
	if err != nil {
		return nil, err
	}
	o := &openReader{r: bufio.NewReaderSize(r, exportSegment+exportTagLen), c: c, seg: make([]byte, exportSegment+exportTagLen)}
	// Decrypt the first segment now: a wrong passphrase fails here.
	if err := o.fill(); err != nil {
		return nil, err
	}
	return o, nil
}

func (o *openReader) fill() error {
	n, err := io.ReadFull(o.r, o.seg)
	last := false
	switch {
	case errors.Is(err, io.EOF):
		return ErrExportDecrypt // cut before the last segment
	case errors.Is(err, io.ErrUnexpectedEOF):
		last = true
	case err != nil:
		return err
	default:
		if _, perr := o.r.Peek(1); errors.Is(perr, io.EOF) {
			last = true
		} else if perr != nil {
			return perr
		}
	}
	nonce, aad := o.c.next(last)
	plain, oerr := o.c.gcm.Open(o.seg[:0], nonce, o.seg[:n], aad)
	if oerr != nil {
		return ErrExportDecrypt
	}
	o.buf, o.done = plain, last
	return nil
}

func (o *openReader) Read(p []byte) (int, error) {
	for len(o.buf) == 0 {
		if o.done {
			return 0, io.EOF
		}
		if err := o.fill(); err != nil {
			return 0, err
		}
	}
	n := copy(p, o.buf)
	o.buf = o.buf[n:]
	return n, nil
}
