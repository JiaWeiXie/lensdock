package update

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// signatureDomain separates release manifest signatures from any other use of
// the same key.
const signatureDomain = "lensdock-release-manifest-v1\n"

// SignatureFile is the content of manifest.sig: an Ed25519 signature over the
// domain string followed by the exact bytes of manifest.json.
type SignatureFile struct {
	Version   int    `json:"version"`
	Algorithm string `json:"algorithm"`
	KeyID     string `json:"keyId"`
	Signature string `json:"signature"`
}

// keyIDOfPublic is the first 8 bytes of SHA-256(public key), hex encoded.
func keyIDOfPublic(pub []byte) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:8])
}

// KeyIDOf returns the key id of an Ed25519 public key.
func KeyIDOf(pub ed25519.PublicKey) string { return keyIDOfPublic(pub) }

// GenerateSigningKey creates a release signing key pair.
func GenerateSigningKey() (ed25519.PublicKey, ed25519.PrivateKey, error) {
	return ed25519.GenerateKey(rand.Reader)
}

// EncodePrivateKey encodes the 32-byte seed of priv as standard base64.
func EncodePrivateKey(priv ed25519.PrivateKey) string {
	return base64.StdEncoding.EncodeToString(priv.Seed())
}

// DecodePrivateKey parses the output of EncodePrivateKey.
func DecodePrivateKey(s string) (ed25519.PrivateKey, error) {
	seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, errors.New("signing key must be a base64 encoded 32-byte Ed25519 seed")
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// SignManifest signs the raw manifest.json bytes.
func SignManifest(raw []byte, priv ed25519.PrivateKey) SignatureFile {
	pub := priv.Public().(ed25519.PublicKey)
	sig := ed25519.Sign(priv, signedMessage(raw))
	return SignatureFile{
		Version:   1,
		Algorithm: "ed25519",
		KeyID:     keyIDOfPublic(pub),
		Signature: base64.StdEncoding.EncodeToString(sig),
	}
}

func signedMessage(raw []byte) []byte {
	msg := make([]byte, 0, len(signatureDomain)+len(raw))
	msg = append(msg, signatureDomain...)
	return append(msg, raw...)
}

// VerifyManifestSignature checks sig against raw using the trusted keys. It
// returns the id of the key that verified the signature.
func VerifyManifestSignature(raw []byte, sig SignatureFile, trusted []TrustedKey) (string, error) {
	if sig.Version != 1 || sig.Algorithm != "ed25519" {
		return "", fmt.Errorf("unsupported signature (version %d, algorithm %q)", sig.Version, sig.Algorithm)
	}
	value, err := base64.StdEncoding.DecodeString(sig.Signature)
	if err != nil || len(value) != ed25519.SignatureSize {
		return "", errors.New("malformed signature value")
	}
	for _, k := range trusted {
		if k.KeyID != sig.KeyID {
			continue
		}
		pub, err := base64.StdEncoding.DecodeString(k.PublicKey)
		if err != nil || len(pub) != ed25519.PublicKeySize {
			return "", fmt.Errorf("trusted key %s is malformed", k.KeyID)
		}
		if !ed25519.Verify(ed25519.PublicKey(pub), signedMessage(raw), value) {
			return "", fmt.Errorf("signature by key %s does not match the manifest", sig.KeyID)
		}
		return k.KeyID, nil
	}
	return "", fmt.Errorf("signature key %s is not in the trusted keys of this deployment", sig.KeyID)
}
