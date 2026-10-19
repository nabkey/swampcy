package desktop

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Pointer moves the seat's pointer and presses its buttons, in layout coordinates.
type Pointer interface {
	MoveTo(x, y int) error
	Press(button string) error
	Release(button string) error
	Scroll(direction string, clicks int) error
}

// Keyboard types text and presses key combinations.
type Keyboard interface {
	Type(ctx context.Context, text string) error
	Keys(ctx context.Context, combos []Combo) error
}

// Combo is one key with the modifiers held while it is pressed: modifiers ctrl,
// shift, alt, logo, altgr; the key an XKB keysym name.
type Combo struct {
	Mods []string
	Key  string
}

var modNames = map[string]string{
	"ctrl": "ctrl", "control": "ctrl", "shift": "shift", "alt": "alt", "option": "alt",
	"super": "logo", "meta": "logo", "logo": "logo", "win": "logo", "cmd": "logo", "altgr": "altgr",
}

var keyNames = map[string]string{
	"enter": "Return", "return": "Return", "esc": "Escape", "escape": "Escape", "tab": "Tab",
	"backspace": "BackSpace", "delete": "Delete", "del": "Delete", "insert": "Insert", "space": "space",
	"up": "Up", "down": "Down", "left": "Left", "right": "Right", "home": "Home", "end": "End",
	"pageup": "Page_Up", "page_up": "Page_Up", "pgup": "Page_Up",
	"pagedown": "Page_Down", "page_down": "Page_Down", "pgdn": "Page_Down",
	"menu": "Menu", "print": "Print",
	"-": "minus", "+": "plus", "=": "equal", "/": "slash", "\\": "backslash", ",": "comma",
	".": "period", ";": "semicolon", "'": "apostrophe", "[": "bracketleft", "]": "bracketright",
	"`": "grave",
}

// ParseKeys reads combinations like "ctrl+shift+t", "Return" or "alt+F4",
// separated by spaces. Names are case-insensitive except unknown ones, which pass
// through as keysym names (XF86AudioPlay, KP_Enter).
func ParseKeys(s string) ([]Combo, error) {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return nil, fmt.Errorf("no keys given")
	}
	var out []Combo
	for _, f := range fields {
		parts := strings.Split(f, "+")
		switch {
		case f == "+": // the plus key itself
			parts = []string{"+"}
		case strings.HasSuffix(f, "++"): // "ctrl++"
			parts = append(strings.Split(strings.TrimSuffix(f, "++"), "+"), "+")
		}
		var c Combo
		for i, p := range parts {
			if p == "" {
				return nil, fmt.Errorf("bad key combination %q", f)
			}
			if i < len(parts)-1 {
				m, ok := modNames[strings.ToLower(p)]
				if !ok {
					return nil, fmt.Errorf("unknown modifier %q in %q", p, f)
				}
				c.Mods = append(c.Mods, m)
				continue
			}
			c.Key = keysym(p)
		}
		out = append(out, c)
	}
	return out, nil
}

func keysym(k string) string {
	if n, ok := keyNames[strings.ToLower(k)]; ok {
		return n
	}
	if utf8.RuneCountInString(k) == 1 {
		return strings.ToLower(k) // "A" with ctrl means ctrl+a; shift is explicit
	}
	if len(k) >= 2 && (k[0] == 'f' || k[0] == 'F') {
		if n, err := strconv.Atoi(k[1:]); err == nil && n >= 1 && n <= 35 {
			return "F" + k[1:]
		}
	}
	return k
}
