package desktop

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
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

// Stroke is one key of paced typing: a character, or a key or shortcut, pressed
// a while after the one before and held for a while, as a finger does.
type Stroke struct {
	Char   rune          // the character to type; 0 when Key is the stroke
	Key    Combo         // a key or shortcut, when Char is 0
	Before time.Duration // from the stroke before going down to this one going down
	Hold   time.Duration // how long it stays down
	// Lead is how long its modifiers (Shift, for a capital) are down before it, and
	// Lag how long they stay after it is up; 0 for a short default.
	Lead, Lag time.Duration
}

// Strokes types strokes at their own pace. A plain letter may go down before the
// one before it is up, as fast fingers overlap; anything with a modifier waits
// for every key to be up. Shift goes down a moment before the key that needs it
// and comes up a moment after, and stays down across capitals typed in a row. A
// cancelled ctx stops it, with nothing left held.
func (v *VirtualInput) Strokes(ctx context.Context, strokes []Stroke) error {
	for _, s := range strokes {
		if s.Char != 0 {
			if _, err := runeKeysym(s.Char); err != nil {
				return err
			}
			continue
		}
		if !keysymName.MatchString(s.Key.Key) {
			return fmt.Errorf("%q is not a keysym name", s.Key.Key)
		}
		for _, m := range s.Key.Mods {
			if _, ok := modCodes[m]; !ok {
				return fmt.Errorf("unknown modifier %q", m)
			}
		}
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	plain := v.mods == 0 // a held modifier would change what the keys mean
	const shift = 42
	shifted := func(s Stroke) bool {
		k, ok := usKeys[s.Char]
		return s.Char != 0 && plain && ok && k.shift
	}
	lead := func(s Stroke) time.Duration {
		if s.Lead > 0 {
			return s.Lead
		}
		return min(max(s.Hold/2, 15*time.Millisecond), 45*time.Millisecond)
	}
	lag := func(s Stroke) time.Duration {
		if s.Lag > 0 {
			return s.Lag
		}
		return lead(s) / 2
	}
	// down are the letters pressed and not yet due up, in the order they are due.
	type held struct {
		code  uint32
		until time.Time
	}
	var down []held
	carried := false // Shift left down by the stroke before
	defer func() {
		for _, h := range down {
			_ = v.key(h.code, false)
		}
		if carried {
			_ = v.key(shift, false)
		}
	}()
	// wait sleeps until t, letting go of each held letter as it falls due.
	wait := func(t time.Time) error {
		for {
			next := t
			if len(down) > 0 && down[0].until.Before(t) {
				next = down[0].until
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Until(next)):
			}
			if len(down) == 0 || down[0].until.After(next) {
				return nil
			}
			h := down[0]
			down = down[1:]
			if err := v.key(h.code, false); err != nil {
				return err
			}
		}
	}
	// flush waits for every held letter to come up, and says when the last did.
	flush := func() (time.Time, error) {
		if len(down) == 0 {
			return time.Time{}, nil
		}
		err := wait(down[len(down)-1].until)
		return time.Now(), err
	}
	const gap = 20 * time.Millisecond // the least time between one key up and the next down
	lastDown, lastUp := time.Now(), time.Time{}
	for i, s := range strokes {
		at := lastDown.Add(s.Before)
		k, us := usKeys[s.Char]
		if s.Char != 0 && us && plain && !k.shift {
			// A letter: it may overlap the ones before, but not itself.
			for _, h := range down {
				if h.code == k.code && at.Before(h.until.Add(gap)) {
					at = h.until.Add(gap)
				}
			}
			if at.Before(lastUp.Add(gap)) {
				at = lastUp.Add(gap)
			}
			if err := wait(at); err != nil {
				return err
			}
			if err := v.key(k.code, true); err != nil {
				return err
			}
			lastDown = time.Now()
			h := held{k.code, lastDown.Add(s.Hold)}
			j := len(down)
			for j > 0 && down[j-1].until.After(h.until) {
				j--
			}
			down = slices.Insert(down, j, h)
			continue
		}
		// Anything else is pressed with every other key up.
		if up, err := flush(); err != nil {
			return err
		} else if up.After(lastUp) {
			lastUp = up
		}
		if at.Before(lastUp.Add(gap)) {
			at = lastUp.Add(gap)
		}
		if err := wait(at); err != nil {
			return err
		}
		if s.Char != 0 {
			if !us || !plain {
				sym, _ := runeKeysym(s.Char)
				lastDown = time.Now()
				if err := v.typeSolo(sym, soloCode(s.Char), 0, s.Hold); err != nil {
					return err
				}
				lastUp = time.Now()
				continue
			}
			if !carried {
				if err := v.key(shift, true); err != nil {
					return err
				}
				carried = true
				time.Sleep(lead(s))
			}
			lastDown = time.Now()
			if err := v.tap(k.code, s.Hold); err != nil {
				return err
			}
			if !(i+1 < len(strokes) && shifted(strokes[i+1]) && strokes[i+1].Before < 400*time.Millisecond) {
				time.Sleep(lag(s))
				carried = false
				if err := v.key(shift, false); err != nil {
					return err
				}
			}
			lastUp = time.Now()
			continue
		}
		uk, ok := usKeysyms[s.Key.Key]
		if !ok {
			var mask uint32
			for _, m := range s.Key.Mods {
				mask |= modCodes[m].mask
			}
			lastDown = time.Now()
			if err := v.typeSolo(s.Key.Key, keySpare, mask, s.Hold); err != nil {
				return err
			}
			lastUp = time.Now()
			continue
		}
		var mods []uint32
		for _, m := range s.Key.Mods {
			mods = append(mods, modCodes[m].code)
		}
		if uk.shift && !slices.Contains(mods, shift) {
			mods = append(mods, shift)
		}
		err := func() (err error) {
			var pressed []uint32
			defer func() {
				if len(pressed) > 0 {
					time.Sleep(lag(s))
				}
				for j := len(pressed) - 1; j >= 0; j-- {
					if e := v.key(pressed[j], false); err == nil {
						err = e
					}
				}
			}()
			for _, m := range mods {
				if err := v.key(m, true); err != nil {
					return err
				}
				pressed = append(pressed, m)
				time.Sleep(lead(s))
			}
			lastDown = time.Now()
			return v.tap(uk.code, s.Hold)
		}()
		if err != nil {
			return err
		}
		lastUp = time.Now()
	}
	_, err := flush()
	return err
}

// keySpare is the key a character with no key of its own is typed on: evdev's
// KEY_102ND, the extra key beside left Shift on keyboards outside the US, which
// types a character there and is no app's shortcut. Not the first keycode, as
// bulk typing uses: that is Escape, and a page that watches for the Escape key
// would see it go down with the character. Not KEY_UNKNOWN either: Chrome
// drops a character typed on that.
const keySpare = 86

// baseLetters are the accented Latin letters by the unaccented one they are
// typed with on keyboards that have them.
var baseLetters = func() map[rune]rune {
	m := map[rune]rune{}
	for base, accented := range map[rune]string{
		'a': "àáâãäåāăą", 'c': "çćĉċč", 'd': "ďđ", 'e': "èéêëēĕėęě", 'g': "ĝğġģ", 'h': "ĥħ",
		'i': "ìíîïĩīĭįı", 'j': "ĵ", 'k': "ķ", 'l': "ĺļľŀł", 'n': "ñńņň", 'o': "òóôõöøōŏő",
		'r': "ŕŗř", 's': "śŝşšß", 't': "ţťŧ", 'u': "ùúûüũūŭůűų", 'w': "ŵ", 'y': "ýÿŷ", 'z': "źżž",
	} {
		for _, c := range accented {
			m[c] = base
			m[unicode.ToUpper(c)] = base
		}
	}
	return m
}()

// soloCode is the key a character off the US keymap is typed on: its
// unaccented letter's, as on a keyboard that has it, or else the spare key's.
func soloCode(c rune) uint32 {
	if base, ok := baseLetters[c]; ok {
		return usKeys[base].code
	}
	return keySpare
}

// typeSolo types one keysym, held for hold with the modifier mask mods set, on a
// keymap that has it alone on the evdev key code, then gives the keyboard the US
// keymap and the modifiers held back.
func (v *VirtualInput) typeSolo(sym string, code, mods uint32, hold time.Duration) (err error) {
	defer func() {
		if e := v.setKeymap(usKeymap); err == nil {
			err = e
		}
		if e := v.w.send(v.keyboard, vkModifiers, nil, v.mods, uint32(0), v.locked, uint32(0)); err == nil {
			err = e
		}
	}()
	km := fmt.Sprintf("xkb_keymap {\n\txkb_keycodes \"swampcy\" {\n\t\tminimum = 8;\n\t\tmaximum = 255;\n\t\t<K1> = %d;\n\t};\n"+
		"\txkb_types \"swampcy\" { include \"complete\" };\n\txkb_compat \"swampcy\" { include \"complete\" };\n"+
		"\txkb_symbols \"swampcy\" {\n\t\tkey <K1> { [ %s ] };\n\t};\n};\n", code+8, sym)
	if err := v.setKeymap(km); err != nil {
		return err
	}
	if err := v.w.send(v.keyboard, vkModifiers, nil, mods, uint32(0), uint32(0), uint32(0)); err != nil {
		return err
	}
	if err := v.w.send(v.keyboard, vkKey, nil, v.now(), code, uint32(1)); err != nil {
		return err
	}
	time.Sleep(hold)
	return v.w.send(v.keyboard, vkKey, nil, v.now(), code, uint32(0))
}

// tap presses a key, holds it and lets it go.
func (v *VirtualInput) tap(code uint32, hold time.Duration) error {
	if err := v.key(code, true); err != nil {
		return err
	}
	time.Sleep(hold)
	return v.key(code, false)
}

// HoldMods presses (or releases) modifier keys and leaves them so, for a click or
// a scroll made while they are held.
func (v *VirtualInput) HoldMods(mods []string, down bool) error {
	for _, m := range mods {
		if _, ok := modCodes[m]; !ok {
			return fmt.Errorf("unknown modifier %q", m)
		}
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	for _, m := range mods {
		if err := v.key(modCodes[m].code, down); err != nil {
			return err
		}
	}
	return nil
}

// HoldKey holds a combination down for d, so the app repeats it or sees it held.
// Only keys of the US keymap can be held: any other is typed on a keymap of its
// own, which cannot stay while other things happen.
func (v *VirtualInput) HoldKey(c Combo, d time.Duration) error {
	k, ok := usKeysyms[c.Key]
	if !ok {
		return fmt.Errorf("%q cannot be held: only keys of the US keymap can", c.Key)
	}
	codes := []uint32{}
	for _, m := range c.Mods {
		mc, ok := modCodes[m]
		if !ok {
			return fmt.Errorf("unknown modifier %q", m)
		}
		codes = append(codes, mc.code)
	}
	if k.shift && !slices.Contains(codes, 42) {
		codes = append(codes, 42)
	}
	codes = append(codes, k.code)
	v.mu.Lock()
	defer v.mu.Unlock()
	var first error
	for _, code := range codes {
		if first = v.key(code, true); first != nil {
			break
		}
	}
	if first == nil {
		time.Sleep(d)
	}
	for i := len(codes) - 1; i >= 0; i-- {
		if v.keys[codes[i]] {
			if err := v.key(codes[i], false); first == nil {
				first = err
			}
		}
	}
	return first
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
		"Shift_L": 42, "Shift_R": 54, "Control_L": 29, "Control_R": 97, "Alt_L": 56, "Super_L": 125,
		"F1": 59, "F2": 60, "F3": 61, "F4": 62, "F5": 63, "F6": 64, "F7": 65, "F8": 66, "F9": 67,
		"F10": 68, "F11": 87, "F12": 88,
	} {
		m[name] = usKey{code: code}
	}
	return m
}()
