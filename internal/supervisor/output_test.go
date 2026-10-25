package supervisor

import (
	"bytes"
	"testing"
)

func TestCappedBufferKeepsPrefixAndFlagsOverflow(t *testing.T) {
	for _, tt := range []struct {
		name     string
		writes   []string
		want     string
		overflow bool
	}{
		{"exactly at the limit", []string{"ab", "cd"}, "abcd", false},
		{"one byte over in a later write", []string{"abcd", "e"}, "abcd", true},
		{"one write straddles the limit", []string{"abc", "def"}, "abcd", true},
		{"empty output", nil, "", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			b := &cappedBuffer{max: 4}
			for _, w := range tt.writes {
				// Every write must be accepted in full so the child never
				// blocks on a pipe the reader stopped draining.
				if n, err := b.Write([]byte(w)); n != len(w) || err != nil {
					t.Fatalf("Write(%q) = %d, %v", w, n, err)
				}
			}
			if !bytes.Equal(b.buf.Bytes(), []byte(tt.want)) || b.overflow != tt.overflow {
				t.Fatalf("buf %q overflow %v, want %q %v", b.buf.Bytes(), b.overflow, tt.want, tt.overflow)
			}
		})
	}
}
