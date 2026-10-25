package supervisor

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

type xauthEntry struct {
	family  uint16
	address []byte
	number  string
	name    string
	data    []byte
}

// decodeXauthority is an independent reader of the .Xauthority format
// (libXau XauReadAuth).
func decodeXauthority(t *testing.T, b []byte) []xauthEntry {
	t.Helper()
	var entries []xauthEntry
	for len(b) > 0 {
		if len(b) < 2 {
			t.Fatal("truncated family")
		}
		e := xauthEntry{family: binary.BigEndian.Uint16(b)}
		b = b[2:]
		fields := make([][]byte, 4)
		for i := range fields {
			if len(b) < 2 {
				t.Fatal("truncated length")
			}
			n := int(binary.BigEndian.Uint16(b))
			b = b[2:]
			if len(b) < n {
				t.Fatal("truncated field")
			}
			fields[i] = b[:n]
			b = b[n:]
		}
		e.address, e.number, e.name, e.data = fields[0], string(fields[1]), string(fields[2]), fields[3]
		entries = append(entries, e)
	}
	return entries
}

func TestEncodeXauthorityGolden(t *testing.T) {
	cookie, _ := hex.DecodeString("000102030405060708090a0b0c0d0e0f")
	got := encodeXauthority(99, cookie)
	want, _ := hex.DecodeString(
		"ffff" + // family wild
			"0000" + // empty address
			"0002" + "3939" + // display "99"
			"0012" + hex.EncodeToString([]byte("MIT-MAGIC-COOKIE-1")) +
			"0010" + "000102030405060708090a0b0c0d0e0f")
	if !bytes.Equal(got, want) {
		t.Fatalf("encoding mismatch\n got %x\nwant %x", got, want)
	}

	entries := decodeXauthority(t, got)
	if len(entries) != 1 {
		t.Fatalf("decoded %d entries, want 1", len(entries))
	}
	e := entries[0]
	if e.family != 0xffff || len(e.address) != 0 || e.number != "99" || e.name != "MIT-MAGIC-COOKIE-1" || !bytes.Equal(e.data, cookie) {
		t.Fatalf("decoded entry = %+v", e)
	}
}

func TestWriteXauthority(t *testing.T) {
	path := filepath.Join(t.TempDir(), "Xauthority")
	if err := WriteXauthority(path, -1, -1); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", fi.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	entries := decodeXauthority(t, data)
	if len(entries) != 1 || entries[0].number != "99" || len(entries[0].data) != 16 {
		t.Fatalf("entries = %+v", entries)
	}
	if bytes.Equal(entries[0].data, make([]byte, 16)) {
		t.Error("cookie is all zero")
	}

	if err := WriteXauthority(path, -1, -1); !errors.Is(err, os.ErrExist) {
		t.Fatalf("second write = %v, want ErrExist (must not overwrite)", err)
	}

	other := filepath.Join(t.TempDir(), "Xauthority")
	if err := WriteXauthority(other, -1, -1); err != nil {
		t.Fatal(err)
	}
	otherData, _ := os.ReadFile(other)
	if bytes.Equal(data, otherData) {
		t.Error("two cookies are identical")
	}
}
