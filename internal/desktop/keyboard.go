package desktop

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// The virtual keyboard's own keys, for the web viewer: evdev keycodes on the US
// keymap the keyboard was given, with the modifier state kept here and sent with
// every change, as the virtual-keyboard protocol expects of its clients.

// xkb modifier masks on the "pc+us" keymap.
const (
	modShift   = 1 << 0
	modLock    = 1 << 1
	modControl = 1 << 2
	modAlt     = 1 << 3 // Mod1
	modSuper   = 1 << 6 // Mod4
	modAltGr   = 1 << 7 // Mod5
)

// modifierKeys maps evdev keycodes to the modifier they hold.
var modifierKeys = map[uint32]uint32{
	42: modShift, 54: modShift, // LEFTSHIFT, RIGHTSHIFT
	29: modControl, 97: modControl, // LEFTCTRL, RIGHTCTRL
	56: modAlt, 100: modAlt, // LEFTALT, RIGHTALT
	125: modSuper, 126: modSuper, // LEFTMETA, RIGHTMETA
}

const keyCapsLock = 58

// MaxKeycode is the highest evdev keycode the keyboard accepts.
const MaxKeycode = 767

// Key presses or releases an evdev key, updating the modifiers it holds.
func (v *VirtualInput) Key(code uint32, down bool) error {
	if code == 0 || code > MaxKeycode {
		return fmt.Errorf("keycode %d out of range", code)
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.key(code, down)
}

func (v *VirtualInput) key(code uint32, down bool) error {
	state := uint32(0)
	if down {
		state = 1
	}
	// The protocol's keycodes are evdev's, not xkb's (no +8).
	if err := v.w.send(v.keyboard, vkKey, nil, v.now(), code, state); err != nil {
		return err
	}
	if down {
		v.keys[code] = true
	} else {
		delete(v.keys, code)
	}
	mods, locked := v.mods, v.locked
	if _, ok := modifierKeys[code]; ok {
		mods = 0
		for k := range v.keys {
			mods |= modifierKeys[k]
		}
	}
	if code == keyCapsLock && down {
		locked ^= modLock
	}
	if mods != v.mods || locked != v.locked {
		v.mods, v.locked = mods, locked
		return v.w.send(v.keyboard, vkModifiers, nil, mods, uint32(0), locked, uint32(0))
	}
	return nil
}

// ReleaseAll lets go of every key and button this connection holds.
func (v *VirtualInput) ReleaseAll() error {
	v.mu.Lock()
	defer v.mu.Unlock()
	var first error
	keep := func(err error) {
		if first == nil {
			first = err
		}
	}
	for code := range v.keys {
		keep(v.key(code, false))
	}
	for code, down := range v.buttons {
		if down {
			keep(v.w.send(v.pointer, vpButton, nil, v.now(), code, uint32(buttonReleased)))
			v.buttons[code] = false
		}
	}
	keep(v.w.send(v.pointer, vpFrame, nil))
	return first
}

// Typing goes through this keyboard too (the MCP server's type and key, the web
// viewer's text), so it lands on this keyboard's seat and nowhere else. A
// character on the US keymap is typed as its key. Anything else is typed by
// giving the keyboard, for the moment, a keymap holding exactly the characters
// needed, as wtype does, and then the US keymap back.

// TypeText types s, pausing delay after each character.
func (v *VirtualInput) TypeText(s string, delay time.Duration) error {
	if !utf8.ValidString(s) {
		return errors.New("text is not valid UTF-8")
	}
	syms := make([]string, 0, len(s))
	us := true
	for _, c := range s {
		if _, ok := usKeys[c]; !ok {
			us = false
		}
		sym, err := runeKeysym(c)
		if err != nil {
			return err
		}
		syms = append(syms, sym)
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if us && v.mods == 0 { // a held modifier would change what the keys mean
		for _, c := range s {
			if err := v.tapUS(usKeys[c], nil, delay); err != nil {
				return err
			}
		}
		return nil
	}
	return v.typeOnKeymap(syms, 0, delay)
}

// Keys presses each combination in turn: its modifiers held, the key pressed and
// released, the modifiers let go.
func (v *VirtualInput) Keys(combos []Combo, delay time.Duration) error {
	for _, c := range combos {
		if !keysymName.MatchString(c.Key) {
			return fmt.Errorf("%q is not a keysym name", c.Key)
		}
		for _, m := range c.Mods {
			if _, ok := modCodes[m]; !ok {
				return fmt.Errorf("unknown modifier %q", m)
			}
		}
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	for _, c := range combos {
		if k, ok := usKeysyms[c.Key]; ok {
			var mods []uint32
			for _, m := range c.Mods {
				mods = append(mods, modCodes[m].code)
			}
			if err := v.tapUS(k, mods, delay); err != nil {
				return err
			}
			continue
		}
		var mask uint32
		for _, m := range c.Mods {
			mask |= modCodes[m].mask
		}
		if err := v.typeOnKeymap([]string{c.Key}, mask, delay); err != nil {
			return err
		}
	}
	return nil
}

// tapUS presses and releases a key of the US keymap, with Shift if it needs it
// and the modifier keys mods held around it.
func (v *VirtualInput) tapUS(k usKey, mods []uint32, delay time.Duration) error {
	if k.shift && !slices.Contains(mods, 42) {
		mods = append(mods, 42)
	}
	for _, m := range mods {
		if err := v.key(m, true); err != nil {
			return err
		}
	}
	if err := v.key(k.code, true); err != nil {
		return err
	}
	if err := v.key(k.code, false); err != nil {
		return err
	}
	for i := len(mods) - 1; i >= 0; i-- {
		if err := v.key(mods[i], false); err != nil {
			return err
		}
	}
	if delay > 0 {
		time.Sleep(delay)
	}
	return nil
}

// keymapSlots is how many keysyms one extra keymap holds: xkb keycodes 9 to 255.
const keymapSlots = 247

// typeOnKeymap types syms, each a press and release, with the modifier mask mods
// set, on keymaps that hold them, then gives the keyboard the US keymap and the
// modifiers held back.
func (v *VirtualInput) typeOnKeymap(syms []string, mods uint32, delay time.Duration) (err error) {
	defer func() {
		if e := v.setKeymap(usKeymap); err == nil {
			err = e
		}
		if e := v.w.send(v.keyboard, vkModifiers, nil, v.mods, uint32(0), v.locked, uint32(0)); err == nil {
			err = e
		}
	}()
	for len(syms) > 0 {
		code := map[string]uint32{} // evdev keycode: xkb's less 8
		var order []string
		n := 0
		for ; n < len(syms); n++ {
			if _, ok := code[syms[n]]; !ok {
				if len(order) == keymapSlots {
					break
				}
				order = append(order, syms[n])
				code[syms[n]] = uint32(len(order))
			}
		}
		if err := v.setKeymap(extraKeymap(order)); err != nil {
			return err
		}
		if err := v.w.send(v.keyboard, vkModifiers, nil, mods, uint32(0), uint32(0), uint32(0)); err != nil {
			return err
		}
		for _, sym := range syms[:n] {
			for _, state := range []uint32{1, 0} {
				if err := v.w.send(v.keyboard, vkKey, nil, v.now(), code[sym], state); err != nil {
					return err
				}
			}
			if delay > 0 {
				time.Sleep(delay)
			}
		}
		syms = syms[n:]
	}
	return nil
}

// extraKeymap is a keymap holding syms, the first at xkb keycode 9 (evdev 1).
func extraKeymap(syms []string) string {
	var b strings.Builder
	b.WriteString("xkb_keymap {\n\txkb_keycodes \"swampcy\" {\n\t\tminimum = 8;\n\t\tmaximum = 255;\n")
	for i := range syms {
		fmt.Fprintf(&b, "\t\t<K%d> = %d;\n", i+1, i+9)
	}
	b.WriteString("\t};\n\txkb_types \"swampcy\" { include \"complete\" };\n\txkb_compat \"swampcy\" { include \"complete\" };\n\txkb_symbols \"swampcy\" {\n")
	for i, sym := range syms {
		fmt.Fprintf(&b, "\t\tkey <K%d> { [ %s ] };\n", i+1, sym)
	}
	b.WriteString("\t};\n};\n")
	return b.String()
}

// keysymName is what may go into a keymap as a keysym: a name like Return,
// XF86AudioPlay or U00E9, and nothing that could end the line it is written on.
var keysymName = regexp.MustCompile(`^[A-Za-z0-9_]{1,64}$`)

// runeKeysym names the keysym that types c.
func runeKeysym(c rune) (string, error) {
	switch c {
	case '\n', '\r':
		return "Return", nil
	case '\t':
		return "Tab", nil
	}
	if c < 0x20 || c == 0x7f || (c >= 0x80 && c < 0xa0) {
		return "", fmt.Errorf("cannot type the control character %U; use key", c)
	}
	return fmt.Sprintf("U%04X", c), nil
}

// modCodes are the modifiers a Combo names: the key held on the US keymap, and
// the mask set on an extra keymap.
var modCodes = map[string]struct{ code, mask uint32 }{
	"shift": {42, modShift},
	"ctrl":  {29, modControl},
	"alt":   {56, modAlt},
	"logo":  {125, modSuper},
	"altgr": {100, modAltGr},
}

type usKey struct {
	code  uint32
	shift bool
}

// usKeys maps the printable ASCII characters, newline and tab to their key on a
// US keyboard and whether Shift is needed.
var usKeys = func() map[rune]usKey {
	m := map[rune]usKey{}
	add := func(chars string, shifted string, codes ...uint32) {
		for i, c := range chars {
			m[c] = usKey{codes[i], false}
		}
		for i, c := range shifted {
			m[c] = usKey{codes[i], true}
		}
	}
	add("1234567890-=", "!@#$%^&*()_+", 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13)
	add("qwertyuiop[]", "QWERTYUIOP{}", 16, 17, 18, 19, 20, 21, 22, 23, 24, 25, 26, 27)
	add("asdfghjkl;'`", "ASDFGHJKL:\"~", 30, 31, 32, 33, 34, 35, 36, 37, 38, 39, 40, 41)
	add("\\zxcvbnm,./", "|ZXCVBNM<>?", 43, 44, 45, 46, 47, 48, 49, 50, 51, 52, 53)
	add(" \n\t", "", 57, 28, 15)
	return m
}()

// usKeysyms maps keysym names to their key on a US keyboard: the characters'
// names (a, A, comma, plus) and the keys that type none.
var usKeysyms = func() map[string]usKey {
	punct := map[rune]string{
		'-': "minus", '=': "equal", '[': "bracketleft", ']': "bracketright", ';': "semicolon",
		'\'': "apostrophe", '`': "grave", '\\': "backslash", ',': "comma", '.': "period", '/': "slash",
		' ': "space", '!': "exclam", '@': "at", '#': "numbersign", '$': "dollar", '%': "percent",
		'^': "asciicircum", '&': "ampersand", '*': "asterisk", '(': "parenleft", ')': "parenright",
		'_': "underscore", '+': "plus", '{': "braceleft", '}': "braceright", ':': "colon",
		'"': "quotedbl", '~': "asciitilde", '|': "bar", '<': "less", '>': "greater", '?': "question",
		'\n': "Return", '\t': "Tab",
	}
	m := map[string]usKey{}
	for c, k := range usKeys {
		if name, ok := punct[c]; ok {
			m[name] = k
		} else {
			m[string(c)] = k
		}
	}
	for name, code := range map[string]uint32{
		"Escape": 1, "BackSpace": 14, "Caps_Lock": 58, "Num_Lock": 69, "Scroll_Lock": 70,
		"KP_Enter": 96, "Print": 99, "Home": 102, "Up": 103, "Page_Up": 104, "Prior": 104,
		"Left": 105, "Right": 106, "End": 107, "Down": 108, "Page_Down": 109, "Next": 109,
		"Insert": 110, "Delete": 111, "Pause": 119, "Menu": 127,
		"F1": 59, "F2": 60, "F3": 61, "F4": 62, "F5": 63, "F6": 64, "F7": 65, "F8": 66, "F9": 67,
		"F10": 68, "F11": 87, "F12": 88,
	} {
		m[name] = usKey{code: code}
	}
	return m
}()
