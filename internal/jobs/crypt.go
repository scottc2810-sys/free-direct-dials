package jobs

import (
	"bufio"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Backup encryption: AES-256-GCM in the STREAM construction. The file is a
// header (magic + random 7-byte nonce prefix) followed by chunks of
// [4-byte length][sealed chunk]. Each nonce is prefix ‖ 4-byte counter ‖
// last-flag, so chunks can't be reordered, dropped or truncated undetected.

const (
	encMagic     = "FDDENC1"
	encChunk     = 1 << 20
	noncePrefixN = 7
)

var ErrBadBackupKey = errors.New("BACKUP_KEY must be 32 bytes, as 64 hex characters or base64")

// ParseKey accepts a 32-byte key encoded as hex or base64.
func ParseKey(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	if b, err := hex.DecodeString(s); err == nil && len(b) == 32 {
		return b, nil
	}
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil && len(b) == 32 {
			return b, nil
		}
	}
	return nil, ErrBadBackupKey
}

type encWriter struct {
	w      io.Writer
	aead   cipher.AEAD
	prefix [noncePrefixN]byte
	ctr    uint32
	buf    []byte
	closed bool
}

// NewEncryptWriter wraps w; Close must be called to write the final chunk.
func NewEncryptWriter(w io.Writer, key []byte) (io.WriteCloser, error) {
	blk, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(blk)
	if err != nil {
		return nil, err
	}
	e := &encWriter{w: w, aead: aead, buf: make([]byte, 0, encChunk)}
	if _, err := io.ReadFull(rand.Reader, e.prefix[:]); err != nil {
		return nil, err
	}
	if _, err := w.Write(append([]byte(encMagic), e.prefix[:]...)); err != nil {
		return nil, err
	}
	return e, nil
}

func (e *encWriter) nonce(last bool) []byte {
	n := make([]byte, 12)
	copy(n, e.prefix[:])
	binary.BigEndian.PutUint32(n[noncePrefixN:], e.ctr)
	if last {
		n[11] = 1
	}
	return n
}

func (e *encWriter) seal(last bool) error {
	if e.ctr == ^uint32(0) {
		return errors.New("backup too large for one stream")
	}
	ct := e.aead.Seal(nil, e.nonce(last), e.buf, nil)
	var l [4]byte
	binary.BigEndian.PutUint32(l[:], uint32(len(ct)))
	if _, err := e.w.Write(l[:]); err != nil {
		return err
	}
	if _, err := e.w.Write(ct); err != nil {
		return err
	}
	e.ctr++
	e.buf = e.buf[:0]
	return nil
}

func (e *encWriter) Write(p []byte) (int, error) {
	if e.closed {
		return 0, errors.New("write after close")
	}
	n := 0
	for len(p) > 0 {
		room := encChunk - len(e.buf)
		k := min(room, len(p))
		e.buf = append(e.buf, p[:k]...)
		p, n = p[k:], n+k
		if len(e.buf) == encChunk {
			if err := e.seal(false); err != nil {
				return n, err
			}
		}
	}
	return n, nil
}

func (e *encWriter) Close() error {
	if e.closed {
		return nil
	}
	e.closed = true
	return e.seal(true)
}

type decReader struct {
	r      *bufio.Reader
	aead   cipher.AEAD
	prefix [noncePrefixN]byte
	ctr    uint32
	plain  []byte
	done   bool
}

// IsEncrypted reports whether a stream starts with the backup encryption magic.
func IsEncrypted(head []byte) bool { return strings.HasPrefix(string(head), encMagic) }

// NewDecryptReader authenticates and decrypts a stream from NewEncryptWriter.
func NewDecryptReader(r io.Reader, key []byte) (io.Reader, error) {
	if len(key) != 32 {
		return nil, errors.New("this backup is encrypted: set BACKUP_KEY to restore it")
	}
	blk, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(blk)
	if err != nil {
		return nil, err
	}
	d := &decReader{r: bufio.NewReaderSize(r, encChunk+64), aead: aead}
	head := make([]byte, len(encMagic)+noncePrefixN)
	if _, err := io.ReadFull(d.r, head); err != nil || string(head[:len(encMagic)]) != encMagic {
		return nil, errors.New("not an encrypted backup")
	}
	copy(d.prefix[:], head[len(encMagic):])
	return d, nil
}

func (d *decReader) Read(p []byte) (int, error) {
	for len(d.plain) == 0 {
		if d.done {
			return 0, io.EOF
		}
		var l [4]byte
		if _, err := io.ReadFull(d.r, l[:]); err != nil {
			return 0, fmt.Errorf("backup is truncated: %w", io.ErrUnexpectedEOF)
		}
		n := binary.BigEndian.Uint32(l[:])
		if n < 16 || n > encChunk+16 {
			return 0, errors.New("backup is corrupt (bad chunk length)")
		}
		ct := make([]byte, n)
		if _, err := io.ReadFull(d.r, ct); err != nil {
			return 0, fmt.Errorf("backup is truncated: %w", io.ErrUnexpectedEOF)
		}
		nonce := make([]byte, 12)
		copy(nonce, d.prefix[:])
		binary.BigEndian.PutUint32(nonce[noncePrefixN:], d.ctr)
		pt, err := d.aead.Open(nil, nonce, ct, nil)
		if err != nil {
			nonce[11] = 1
			if pt, err = d.aead.Open(nil, nonce, ct, nil); err != nil {
				return 0, errors.New("backup failed authentication: wrong BACKUP_KEY or tampered file")
			}
			d.done = true
			if _, err := d.r.Peek(1); err != io.EOF {
				return 0, errors.New("backup has data after its final chunk")
			}
		}
		d.ctr++
		d.plain = pt
	}
	n := copy(p, d.plain)
	d.plain = d.plain[n:]
	return n, nil
}
