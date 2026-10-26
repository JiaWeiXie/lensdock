package rfb

import (
	"slices"
	"testing"
)

func TestParseChordResolvesModifiersAndKeys(t *testing.T) {
	tests := []struct {
		spec string
		want []uint32
	}{
		{"Return", []uint32{0xff0d}},
		{"ctrl+l", []uint32{0xffe3, 'l'}},
		{"CTRL+Shift+Tab", []uint32{0xffe3, 0xffe1, 0xff09}},
		{"alt+Left", []uint32{0xffe9, 0xff51}},
		{"F6", []uint32{0xffc3}},
		{"f24", []uint32{0xffd5}},
		{"ctrl+plus", []uint32{0xffe3, '+'}},
		{"ctrl++", []uint32{0xffe3, '+'}},
		{"+", []uint32{'+'}},
		{"ctrl+0x1008ff12", []uint32{0xffe3, 0x1008ff12}},
		{"\u7e41", []uint32{0x01000000 + 0x7e41}},
		{"\u00e9", []uint32{0xe9}},
	}
	for _, tt := range tests {
		got, err := ParseChord(tt.spec)
		if err != nil || !slices.Equal(got, tt.want) {
			t.Errorf("ParseChord(%q) = %#x, %v; want %#x", tt.spec, got, err, tt.want)
		}
	}
}

func TestParseChordRejectsAmbiguousInput(t *testing.T) {
	for _, spec := range []string{"", "ctrl+", "+l", "ctrl+nosuchkey", "F25", "F0", "0x0", "0xzz", "ab", "ctrl+\x07"} {
		if got, err := ParseChord(spec); err == nil {
			t.Errorf("ParseChord(%q) = %#x, want an error", spec, got)
		}
	}
}

func TestKeysymForRuneCoversLatin1UnicodeAndLineBreaks(t *testing.T) {
	tests := []struct {
		r    rune
		want uint32
		ok   bool
	}{
		{'a', 'a', true},
		{' ', ' ', true},
		{'~', '~', true},
		{0xe9, 0xe9, true},
		{'\n', KeyReturn, true},
		{'\r', KeyReturn, true},
		{'\t', 0xff09, true},
		{0x7e41, 0x01007e41, true},
		{0x1f600, 0x0101f600, true},
		{0x07, 0, false},
		{0x7f, 0, false},
		{0x85, 0, false},
	}
	for _, tt := range tests {
		if got, ok := KeysymForRune(tt.r); got != tt.want || ok != tt.ok {
			t.Errorf("KeysymForRune(%U) = %#x, %v; want %#x, %v", tt.r, got, ok, tt.want, tt.ok)
		}
	}
}

func TestParseButton(t *testing.T) {
	for name, want := range map[string]uint8{"left": ButtonLeft, "Middle": ButtonMiddle, "RIGHT": ButtonRight} {
		if got, err := ParseButton(name); err != nil || got != want {
			t.Errorf("ParseButton(%q) = %d, %v; want %d", name, got, err, want)
		}
	}
	if _, err := ParseButton("back"); err == nil {
		t.Error("an unknown button was accepted")
	}
}
