package update

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/JiaWeiXie/lensdock/internal/auth"
)

// Sealed stream format ("LDSEAL1"):
//
//	header : magic(8) keyId(8) noncePrefix(4) chunkSize(4, big endian)
//	frame* : final(1) length(4, big endian) ciphertext(length)
//
// Every frame is AES-256-GCM sealed with nonce = noncePrefix || counter(8) and
// additional data = header || final. The last frame has final=1 (it may be
// empty), so a truncated or reordered stream fails authentication.
const (
	sealMagic       = "LDSEAL1\n"
	sealHeaderSize  = 8 + 8 + 4 + 4
	sealChunkSize   = 1 << 20
	sealMaxChunk    = 16 << 20
	sealKeyBytes    = 32
	sealFrameHeader = 5
)

// ErrSealCorrupt is returned for any authentication or framing failure.
var ErrSealCorrupt = errors.New("sealed stream is corrupt, truncated or sealed with another key")

// KeyID is the identifier recorded with sealed data: the first 8 bytes of
// SHA-256(key), hex encoded.
func KeyID(key []byte) string {
	sum := sha256.Sum256(key)
	return hex.EncodeToString(sum[:8])
}

// GenerateBackupKey writes a new 32-byte random key as 64 hex characters to
// path with mode 0600. It refuses to overwrite an existing file.
func GenerateBackupKey(path string) (string, error) {
	key := make([]byte, sealKeyBytes)
	if _, err := rand.Read(key); err != nil {
		return "", fmt.Errorf("generate backup key: %w", err)
	}
	if err := auth.WritePrivateFile(path, []byte(hex.EncodeToString(key)+"\n")); err != nil {
		return "", err
	}
	return KeyID(key), nil
}

// LoadBackupKey reads a key written by GenerateBackupKey. The file must be a
// private regular file.
func LoadBackupKey(path string) ([]byte, error) {
	data, err := auth.ReadPrivateFile(path, 1024)
	if err != nil {
		return nil, fmt.Errorf("read backup key: %w", err)
	}
	key, err := hex.DecodeString(strings.TrimSpace(string(data)))
	if err != nil || len(key) != sealKeyBytes {
		return nil, errors.New("backup key must be 64 hexadecimal characters")
	}
	return key, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	if len(key) != sealKeyBytes {
		return nil, errors.New("backup key must be 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func sealNonce(prefix []byte, counter uint64) []byte {
	nonce := make([]byte, 12)
	copy(nonce, prefix)
	binary.BigEndian.PutUint64(nonce[4:], counter)
	return nonce
}

// SealWriter encrypts everything written to it. Close writes the final frame
// and must be called; it does not close the destination.
type SealWriter struct {
	w       io.Writer
	aead    cipher.AEAD
	header  []byte
	prefix  []byte
	counter uint64
	buf     []byte
	closed  bool
}

// NewSealWriter writes the header to w and returns the encrypting writer.
func NewSealWriter(w io.Writer, key []byte) (*SealWriter, error) {
	aead, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	header := make([]byte, sealHeaderSize)
	copy(header, sealMagic)
	id, _ := hex.DecodeString(KeyID(key))
	copy(header[8:], id)
	if _, err := rand.Read(header[16:20]); err != nil {
		return nil, fmt.Errorf("seal: random nonce prefix: %w", err)
	}
	binary.BigEndian.PutUint32(header[20:], sealChunkSize)
	if _, err := w.Write(header); err != nil {
		return nil, err
	}
	return &SealWriter{w: w, aead: aead, header: header, prefix: header[16:20], buf: make([]byte, 0, sealChunkSize)}, nil
}

func (s *SealWriter) flush(final bool) error {
	flag := byte(0)
	if final {
		flag = 1
	}
	aad := append(append([]byte{}, s.header...), flag)
	ct := s.aead.Seal(nil, sealNonce(s.prefix, s.counter), s.buf, aad)
	s.counter++
	frame := make([]byte, sealFrameHeader, sealFrameHeader+len(ct))
	frame[0] = flag
	binary.BigEndian.PutUint32(frame[1:], uint32(len(ct)))
	frame = append(frame, ct...)
	if _, err := s.w.Write(frame); err != nil {
		return err
	}
	s.buf = s.buf[:0]
	return nil
}

// Write implements io.Writer.
func (s *SealWriter) Write(p []byte) (int, error) {
	if s.closed {
		return 0, errors.New("seal: write after close")
	}
	written := 0
	for len(p) > 0 {
		n := min(sealChunkSize-len(s.buf), len(p))
		s.buf = append(s.buf, p[:n]...)
		p = p[n:]
		written += n
		if len(s.buf) == sealChunkSize {
			if err := s.flush(false); err != nil {
				return written, err
			}
		}
	}
	return written, nil
}

// Close writes the final frame.
func (s *SealWriter) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	return s.flush(true)
}

// SealReader decrypts a sealed stream and verifies every frame.
type SealReader struct {
	r       io.Reader
	aead    cipher.AEAD
	header  []byte
	prefix  []byte
	chunk   uint32
	counter uint64
	plain   []byte
	done    bool
}

// NewSealReader reads and checks the header. A stream sealed with a different
// key fails here with a key id mismatch.
func NewSealReader(r io.Reader, key []byte) (*SealReader, error) {
	aead, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	header := make([]byte, sealHeaderSize)
	if _, err := io.ReadFull(r, header); err != nil {
		return nil, fmt.Errorf("%w: header: %v", ErrSealCorrupt, err)
	}
	if !bytes.HasPrefix(header, []byte(sealMagic)) {
		return nil, fmt.Errorf("%w: not a LensDock sealed stream", ErrSealCorrupt)
	}
	if hex.EncodeToString(header[8:16]) != KeyID(key) {
		return nil, fmt.Errorf("%w: key id %s does not match the configured backup key %s", ErrSealCorrupt, hex.EncodeToString(header[8:16]), KeyID(key))
	}
	chunk := binary.BigEndian.Uint32(header[20:])
	if chunk == 0 || chunk > sealMaxChunk {
		return nil, fmt.Errorf("%w: chunk size", ErrSealCorrupt)
	}
	return &SealReader{r: r, aead: aead, header: header, prefix: header[16:20], chunk: chunk}, nil
}

func (s *SealReader) next() error {
	var fh [sealFrameHeader]byte
	if _, err := io.ReadFull(s.r, fh[:]); err != nil {
		return fmt.Errorf("%w: frame header: %v", ErrSealCorrupt, err)
	}
	if fh[0] > 1 {
		return fmt.Errorf("%w: frame flag", ErrSealCorrupt)
	}
	n := binary.BigEndian.Uint32(fh[1:])
	if n < uint32(s.aead.Overhead()) || n > s.chunk+uint32(s.aead.Overhead()) {
		return fmt.Errorf("%w: frame length", ErrSealCorrupt)
	}
	ct := make([]byte, n)
	if _, err := io.ReadFull(s.r, ct); err != nil {
		return fmt.Errorf("%w: frame body: %v", ErrSealCorrupt, err)
	}
	aad := append(append([]byte{}, s.header...), fh[0])
	plain, err := s.aead.Open(nil, sealNonce(s.prefix, s.counter), ct, aad)
	if err != nil {
		return ErrSealCorrupt
	}
	s.counter++
	s.plain = plain
	if fh[0] == 1 {
		s.done = true
		var one [1]byte
		if n, _ := s.r.Read(one[:]); n != 0 {
			return fmt.Errorf("%w: data after the final frame", ErrSealCorrupt)
		}
	}
	return nil
}

// Read implements io.Reader. A clean io.EOF is returned only after the final
// frame authenticated.
func (s *SealReader) Read(p []byte) (int, error) {
	for len(s.plain) == 0 {
		if s.done {
			return 0, io.EOF
		}
		if err := s.next(); err != nil {
			return 0, err
		}
	}
	n := copy(p, s.plain)
	s.plain = s.plain[n:]
	return n, nil
}
