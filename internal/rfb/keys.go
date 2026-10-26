package rfb

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

// namedKey returns the X11 keysym of a case-insensitive key name.
func namedKey(name string) (uint32, bool) {
	switch name {
	case "ctrl", "control":
		return 0xffe3, true
	case "shift":
		return 0xffe1, true
	case "alt", "option":
		return 0xffe9, true
	case "altgr":
		return 0xfe03, true
	case "super", "meta", "win", "cmd":
		return 0xffeb, true
	case "return", "enter":
		return KeyReturn, true
	case "tab":
		return 0xff09, true
	case "escape", "esc":
		return 0xff1b, true
	case "backspace":
		return KeyBackSpace, true
	case "delete", "del":
		return 0xffff, true
	case "insert":
		return 0xff63, true
	case "home":
		return 0xff50, true
	case "end":
		return 0xff57, true
	case "left":
		return 0xff51, true
	case "up":
		return 0xff52, true
	case "right":
		return 0xff53, true
	case "down":
		return 0xff54, true
	case "pageup":
		return 0xff55, true
	case "pagedown":
		return 0xff56, true
	case "menu":
		return 0xff67, true
	case "space":
		return 0x0020, true
	case "plus":
		return '+', true
	case "minus":
		return '-', true
	}
	return 0, false
}

// KeysymForRune returns the X11 keysym that types r. Latin-1 characters use
// their own code point; every other Unicode character uses the 0x01000000
// plus code point form that x11vnc maps to a spare keycode on demand. Line
// breaks type Return and a tab types Tab. The other control characters have
// no keysym.
func KeysymForRune(r rune) (uint32, bool) {
	switch {
	case r == '\n' || r == '\r':
		return KeyReturn, true
	case r == '\t':
		return 0xff09, true
	case r >= 0x20 && r <= 0x7e, r >= 0xa0 && r <= 0xff:
		return uint32(r), true
	case r > 0xff && r <= unicode.MaxRune && !unicode.IsControl(r):
		return 0x01000000 + uint32(r), true
	}
	return 0, false
}

// ParseChord parses a key combination such as "Return", "ctrl+l",
// "ctrl+shift+Tab", "alt+Left", "F6" or "plus". Parts are joined by "+"; the
// last one is the key and the others are held while it is pressed. A part is
// a key name (any case), F1 to F24, a single character, or a hexadecimal
// keysym written 0x.... "+" itself is written "plus" or as a trailing "++".
func ParseChord(spec string) ([]uint32, error) {
	if spec == "" {
		return nil, fmt.Errorf("rfb: empty key chord")
	}
	parts := strings.Split(spec, "+")
	switch {
	case spec == "+":
		parts = []string{"+"}
	case strings.HasSuffix(spec, "++"):
		parts = append(strings.Split(strings.TrimSuffix(spec, "++"), "+"), "+")
	}
	out := make([]uint32, 0, len(parts))
	for _, p := range parts {
		k, err := parseKey(p)
		if err != nil {
			return nil, fmt.Errorf("rfb: key chord %q: %w", spec, err)
		}
		out = append(out, k)
	}
	return out, nil
}

func parseKey(p string) (uint32, error) {
	if p == "" {
		return 0, fmt.Errorf("empty key name")
	}
	lower := strings.ToLower(p)
	if k, ok := namedKey(lower); ok {
		return k, nil
	}
	if rest, ok := strings.CutPrefix(lower, "0x"); ok {
		n, err := strconv.ParseUint(rest, 16, 32)
		if err != nil || n == 0 {
			return 0, fmt.Errorf("invalid keysym %q", p)
		}
		return uint32(n), nil
	}
	if len(lower) >= 2 && lower[0] == 'f' {
		if n, err := strconv.Atoi(lower[1:]); err == nil {
			if n < 1 || n > 24 {
				return 0, fmt.Errorf("function key %q is outside F1-F24", p)
			}
			return 0xffbe + uint32(n-1), nil
		}
	}
	if runes := []rune(p); len(runes) == 1 {
		if k, ok := KeysymForRune(runes[0]); ok {
			return k, nil
		}
	}
	return 0, fmt.Errorf("unknown key %q", p)
}

// ParseButton turns "left", "middle" or "right" into a pointer button mask.
func ParseButton(name string) (uint8, error) {
	switch strings.ToLower(name) {
	case "left":
		return ButtonLeft, nil
	case "middle":
		return ButtonMiddle, nil
	case "right":
		return ButtonRight, nil
	}
	return 0, fmt.Errorf("rfb: unknown button %q (left, middle or right)", name)
}
