package auth

import (
	"bytes"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"
)

func TestSecretNeverFormatsRawValue(t *testing.T) {
	s, err := NewSecret("ldo_")
	if err != nil {
		t.Fatal(err)
	}
	raw := s.Reveal()

	var logs bytes.Buffer
	slog.New(slog.NewTextHandler(&logs, nil)).Info("x", "token", s)
	slog.New(slog.NewJSONHandler(&logs, nil)).Info("x", "token", s)

	outputs := []string{
		fmt.Sprint(s), fmt.Sprintf("%s", s), fmt.Sprintf("%v", s),
		fmt.Sprintf("%+v", s), fmt.Sprintf("%#v", s), fmt.Sprintf("%q", s),
		fmt.Sprintf("%v", struct{ T Secret }{s}), logs.String(),
	}
	for _, out := range outputs {
		if strings.Contains(out, raw) {
			t.Fatalf("formatted output leaks the secret: %q", out)
		}
	}
	if !strings.Contains(logs.String(), "[redacted]") {
		t.Fatalf("slog output does not show the redaction marker: %q", logs.String())
	}
}

func TestNewSecretShapeAndUniqueness(t *testing.T) {
	a, err := NewSecret("ldl_")
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewSecret("ldl_")
	if err != nil {
		t.Fatal(err)
	}
	if a.Reveal() == b.Reveal() {
		t.Fatal("two secrets are identical")
	}
	// 32 bytes, unpadded base64url = 43 characters.
	if rest, ok := strings.CutPrefix(a.Reveal(), "ldl_"); !ok || len(rest) != 43 || strings.ContainsAny(rest, "=+/") {
		t.Fatalf("unexpected secret shape %q", a.Reveal())
	}
}

func TestDigestMatches(t *testing.T) {
	d := DigestOf("ldo_secret")
	if !d.Matches("ldo_secret") {
		t.Fatal("digest does not match its own preimage")
	}
	for _, other := range []string{"", "ldo_secret ", "ldo_Secret", "ldo_secre"} {
		if d.Matches(other) {
			t.Fatalf("digest matches %q", other)
		}
	}
}

func TestParseDigestHex(t *testing.T) {
	d := DigestOf("x")
	hex := d.Hex()
	if len(hex) != 64 || hex != strings.ToLower(hex) {
		t.Fatalf("Hex() = %q, want 64 lowercase characters", hex)
	}
	for name, in := range map[string]string{"lower": hex, "upper": strings.ToUpper(hex)} {
		got, err := ParseDigestHex(in)
		if err != nil || got != d {
			t.Fatalf("%s: got %v, %v; want round trip", name, got, err)
		}
	}
	bad := map[string]string{
		"empty":     "",
		"short":     hex[:63],
		"long":      hex + "0",
		"non-hex":   hex[:63] + "g",
		"padded":    " " + hex[:63],
		"0x prefix": "0x" + hex[:62],
	}
	for name, in := range bad {
		if _, err := ParseDigestHex(in); err == nil {
			t.Errorf("%s: ParseDigestHex(%q) succeeded", name, in)
		}
	}
}

func TestBearerToken(t *testing.T) {
	tests := []struct {
		name    string
		headers []string
		want    string
		ok      bool
	}{
		{"valid", []string{"Bearer abc.DEF-123_~"}, "abc.DEF-123_~", true},
		{"max length", []string{"Bearer " + strings.Repeat("a", 8192)}, strings.Repeat("a", 8192), true},
		{"too long", []string{"Bearer " + strings.Repeat("a", 8193)}, "", false},
		{"missing header", nil, "", false},
		{"empty token", []string{"Bearer "}, "", false},
		{"scheme only", []string{"Bearer"}, "", false},
		{"lowercase scheme", []string{"bearer abc"}, "", false},
		{"other scheme", []string{"Basic abc"}, "", false},
		{"double space", []string{"Bearer  abc"}, "", false},
		{"trailing space", []string{"Bearer abc "}, "", false},
		{"embedded space", []string{"Bearer abc def"}, "", false},
		{"embedded tab", []string{"Bearer abc\tdef"}, "", false},
		{"non-ascii", []string{"Bearer abc\u00e9"}, "", false},
		{"duplicate headers", []string{"Bearer abc", "Bearer abc"}, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, err := http.NewRequest(http.MethodGet, "http://example.invalid/", nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, h := range tt.headers {
				r.Header.Add("Authorization", h)
			}
			got, ok := BearerToken(r)
			if got != tt.want || ok != tt.ok {
				t.Fatalf("BearerToken = %q, %v; want %q, %v", got, ok, tt.want, tt.ok)
			}
		})
	}
}
