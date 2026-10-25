// Package auth holds the credential primitives shared by the runtime API,
// the CDP proxy and the CLI: redacting secrets, digest comparison, bearer
// parsing and private file I/O.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
)

const (
	redacted = "[redacted]"

	// secretEntropyBytes is the number of random bytes behind every Secret.
	secretEntropyBytes = 32

	// maxBearerTokenLen is the longest bearer token BearerToken accepts.
	maxBearerTokenLen = 8192

	bearerPrefix = "Bearer "
)

// Secret is a credential that must never be printed or logged. Formatting it
// with any fmt verb or slog yields "[redacted]"; use Reveal for the raw value.
type Secret string

// NewSecret returns prefix followed by base64url(32 random bytes), unpadded.
func NewSecret(prefix string) (Secret, error) {
	var b [secretEntropyBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("auth: read random bytes: %w", err)
	}
	return Secret(prefix + base64.RawURLEncoding.EncodeToString(b[:])), nil
}

// RandomHex returns n random bytes from crypto/rand as 2n lowercase
// hexadecimal characters. It is for identifiers (lease IDs, request IDs,
// idempotency keys, URL prefixes); credentials use NewSecret.
func RandomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("auth: read random bytes: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// String returns "[redacted]".
func (Secret) String() string { return redacted }

// GoString returns "[redacted]" so %#v cannot leak the value.
func (Secret) GoString() string { return redacted }

// LogValue implements slog.LogValuer.
func (Secret) LogValue() slog.Value { return slog.StringValue(redacted) }

// Reveal returns the raw secret value.
func (s Secret) Reveal() string { return string(s) }

// Digest is a SHA-256 digest of a credential.
type Digest [sha256.Size]byte

// DigestOf returns the SHA-256 digest of s.
func DigestOf(s string) Digest { return sha256.Sum256([]byte(s)) }

// ParseDigestHex parses exactly 64 hexadecimal characters (either case).
func ParseDigestHex(s string) (Digest, error) {
	var d Digest
	if len(s) != hex.EncodedLen(len(d)) {
		return Digest{}, errors.New("auth: digest must be 64 hex characters")
	}
	if _, err := hex.Decode(d[:], []byte(s)); err != nil {
		return Digest{}, errors.New("auth: digest must be 64 hex characters")
	}
	return d, nil
}

// Hex returns the lowercase hexadecimal form of d.
func (d Digest) Hex() string { return hex.EncodeToString(d[:]) }

// Matches reports whether sha256(presented) equals d, in constant time.
func (d Digest) Matches(presented string) bool {
	p := DigestOf(presented)
	return subtle.ConstantTimeCompare(d[:], p[:]) == 1
}

// BearerToken extracts the token from a single "Authorization: Bearer <token>"
// header. The scheme is matched exactly, the token must be non-empty, at most
// 8192 bytes, and consist of printable ASCII without whitespace.
func BearerToken(r *http.Request) (string, bool) {
	values := r.Header.Values("Authorization")
	if len(values) != 1 {
		return "", false
	}
	v := values[0]
	if !strings.HasPrefix(v, bearerPrefix) {
		return "", false
	}
	tok := v[len(bearerPrefix):]
	if tok == "" || len(tok) > maxBearerTokenLen {
		return "", false
	}
	for i := range len(tok) {
		if c := tok[i]; c <= 0x20 || c >= 0x7f {
			return "", false
		}
	}
	return tok, true
}
