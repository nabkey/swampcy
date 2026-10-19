package desktop

import (
	"reflect"
	"strings"
	"testing"
)

func TestVirtualKeyboardModifiers(t *testing.T) {
	f := &fakeCompositor{}
	v, err := OpenVirtualInput(startFakeCompositor(t, f), InputPlace{})
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	for _, k := range []struct {
		code uint32
		down bool
	}{{29, true}, {46, true}, {46, false}, {29, false}} { // ctrl+c
		if err := v.Key(k.code, k.down); err != nil {
			t.Fatal(err)
		}
		if k.code == 46 && k.down {
			// wtype ran mid-shortcut: reclaiming the seat keeps ctrl held
			if err := v.Activate(); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := v.Key(0, true); err == nil {
		t.Error("keycode 0 accepted")
	}
	if err := v.Key(MaxKeycode+1, true); err == nil {
		t.Error("keycode above the maximum accepted")
	}
	if err := v.Sync(); err != nil {
		t.Fatal(err)
	}
	var keys [][2]uint32
	for _, r := range f.find(v.keyboard, vkKey) {
		keys = append(keys, [2]uint32{r.args[1], r.args[2]})
	}
	if want := [][2]uint32{{29, 1}, {46, 1}, {46, 0}, {29, 0}}; !reflect.DeepEqual(keys, want) {
		t.Errorf("keys = %v, want %v", keys, want)
	}
	var mods []uint32
	for _, r := range f.find(v.keyboard, vkModifiers) {
		mods = append(mods, r.args[0])
	}
	// create's Shift flip, ctrl, Activate's flip ending on ctrl, then none
	if want := []uint32{shiftMask, 0, modControl, modControl | shiftMask, modControl, 0}; !reflect.DeepEqual(mods, want) {
		t.Errorf("modifier updates = %v, want %v", mods, want)
	}
}

func keysSent(f *fakeCompositor, keyboard uint32) [][2]uint32 {
	var keys [][2]uint32
	for _, r := range f.find(keyboard, vkKey) {
		keys = append(keys, [2]uint32{r.args[1], r.args[2]})
	}
	return keys
}

func modsSent(f *fakeCompositor, keyboard uint32) []uint32 {
	var mods []uint32
	for _, r := range f.find(keyboard, vkModifiers) {
		mods = append(mods, r.args[0])
	}
	return mods
}

func TestTypeTextOnTheUSKeymap(t *testing.T) {
	f := &fakeCompositor{}
	v, err := OpenVirtualInput(startFakeCompositor(t, f), InputPlace{})
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	if err := v.TypeText("aZ!\n", 0); err != nil {
		t.Fatal(err)
	}
	_ = v.Key(29, true) // ctrl held
	_ = v.Press("left")
	if err := v.ReleaseAll(); err != nil {
		t.Fatal(err)
	}
	_ = v.Sync()
	want := [][2]uint32{
		{30, 1}, {30, 0}, // a
		{42, 1}, {44, 1}, {44, 0}, {42, 0}, // Z
		{42, 1}, {2, 1}, {2, 0}, {42, 0}, // !
		{28, 1}, {28, 0}, // newline: Return
		{29, 1}, {29, 0}, // ctrl, then released by ReleaseAll
	}
	if got := keysSent(f, v.keyboard); !reflect.DeepEqual(got, want) {
		t.Errorf("keys =\n%v\nwant\n%v", got, want)
	}
	if len(f.gotKeymaps()) != 1 {
		t.Errorf("%d keymaps sent, want only the US one", len(f.gotKeymaps()))
	}
	buttons := f.find(v.pointer, vpButton)
	if len(buttons) != 2 || buttons[1].args[2] != buttonReleased {
		t.Errorf("buttons = %+v, want a press then ReleaseAll's release", buttons)
	}
}

func TestTypeTextOffTheUSKeymap(t *testing.T) {
	f := &fakeCompositor{}
	v, err := OpenVirtualInput(startFakeCompositor(t, f), InputPlace{})
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	if err := v.TypeText("é ée\n", 0); err != nil {
		t.Fatal(err)
	}
	_ = v.Sync()
	if len(f.gotKeymaps()) != 3 {
		t.Fatalf("%d keymaps, want US, one for the text, US again", len(f.gotKeymaps()))
	}
	extra := f.gotKeymaps()[1]
	for _, want := range []string{"<K1> = 9;", "<K4> = 12;", "key <K1> { [ U00E9 ] };", "key <K2> { [ U0020 ] };", "key <K3> { [ U0065 ] };", "key <K4> { [ Return ] };"} {
		if !strings.Contains(extra, want) {
			t.Errorf("extra keymap lacks %q:\n%s", want, extra)
		}
	}
	if f.gotKeymaps()[2] != f.gotKeymaps()[0] {
		t.Error("the US keymap was not given back")
	}
	// é, space, é, e, Return: each character's evdev code is its slot.
	want := [][2]uint32{{1, 1}, {1, 0}, {2, 1}, {2, 0}, {1, 1}, {1, 0}, {3, 1}, {3, 0}, {4, 1}, {4, 0}}
	if got := keysSent(f, v.keyboard); !reflect.DeepEqual(got, want) {
		t.Errorf("keys = %v, want %v", got, want)
	}

	// With ctrl held, even ASCII goes through a keymap, typed with no modifiers,
	// and ctrl is held again after.
	_ = v.Key(29, true)
	if err := v.TypeText("a", 0); err != nil {
		t.Fatal(err)
	}
	_ = v.Sync()
	mods := modsSent(f, v.keyboard)
	if tail := mods[len(mods)-3:]; !reflect.DeepEqual(tail, []uint32{modControl, 0, modControl}) {
		t.Errorf("modifiers around text typed with ctrl held = %v, want ctrl, none, ctrl", tail)
	}

	if err := v.TypeText("a\x07", 0); err == nil {
		t.Error("typed a control character")
	}
}

func TestTypeTextSplitsLongAlphabets(t *testing.T) {
	f := &fakeCompositor{}
	v, err := OpenVirtualInput(startFakeCompositor(t, f), InputPlace{})
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	var b strings.Builder
	for r := rune(0x4e00); r < 0x4e00+300; r++ { // 300 distinct characters
		b.WriteRune(r)
	}
	if err := v.TypeText(b.String(), 0); err != nil {
		t.Fatal(err)
	}
	_ = v.Sync()
	keymaps := f.gotKeymaps()
	if len(keymaps) != 4 {
		t.Fatalf("%d keymaps, want US, 247 characters, 53, US", len(keymaps))
	}
	if !strings.Contains(keymaps[1], "<K247> = 255;") || strings.Contains(keymaps[1], "<K248>") {
		t.Error("the first extra keymap does not hold exactly 247")
	}
	if len(keysSent(f, v.keyboard)) != 600 {
		t.Errorf("%d key events, want 600", len(keysSent(f, v.keyboard)))
	}
}

func TestKeys(t *testing.T) {
	f := &fakeCompositor{}
	v, err := OpenVirtualInput(startFakeCompositor(t, f), InputPlace{})
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	combos, err := ParseKeys("ctrl+s Return ctrl+shift+t ctrl+XF86AudioPlay")
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Keys(combos, 0); err != nil {
		t.Fatal(err)
	}
	_ = v.Sync()
	want := [][2]uint32{
		{29, 1}, {31, 1}, {31, 0}, {29, 0}, // ctrl+s
		{28, 1}, {28, 0}, // Return
		{29, 1}, {42, 1}, {20, 1}, {20, 0}, {42, 0}, {29, 0}, // ctrl+shift+t
		{1, 1}, {1, 0}, // XF86AudioPlay, on a keymap of its own
	}
	if got := keysSent(f, v.keyboard); !reflect.DeepEqual(got, want) {
		t.Errorf("keys =\n%v\nwant\n%v", got, want)
	}
	if len(f.gotKeymaps()) != 3 || !strings.Contains(f.gotKeymaps()[1], "key <K1> { [ XF86AudioPlay ] };") {
		t.Errorf("keymaps = %q", f.gotKeymaps())
	}
	mods := modsSent(f, v.keyboard)
	if tail := mods[len(mods)-2:]; !reflect.DeepEqual(tail, []uint32{modControl, 0}) {
		t.Errorf("modifiers for ctrl+XF86AudioPlay = %v, want ctrl, then none", tail)
	}

	sent := len(f.requests)
	for _, bad := range []Combo{{Key: "a b"}, {Key: "x;"}, {Key: "a", Mods: []string{"hyper"}}} {
		if err := v.Keys([]Combo{{Key: "a"}, bad}, 0); err == nil {
			t.Errorf("Keys accepted %+v", bad)
		}
	}
	_ = v.Sync()
	if f.mu.Lock(); len(f.requests) != sent+0 && f.requests[len(f.requests)-1].opcode != wlDisplaySync {
		f.mu.Unlock()
		t.Error("an invalid combination sent something")
	} else {
		f.mu.Unlock()
	}
}

func TestInputPlace(t *testing.T) {
	f := &fakeCompositor{seats: []string{"seat0", "owner"}, outputs: []string{"HEADLESS-1", "HEADLESS-2"}}
	sess := startFakeCompositor(t, f)
	v, err := OpenVirtualInput(sess, InputPlace{Seat: "owner", Output: "HEADLESS-2"})
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	_ = v.Sync()
	creates := 0
	f.mu.Lock()
	for _, r := range f.requests {
		switch f.objects[r.obj] {
		case "zwlr_virtual_pointer_manager_v1":
			if r.opcode != vpManagerCreateOn || f.names[r.args[0]] != "owner" || f.names[r.args[1]] != "HEADLESS-2" {
				t.Errorf("pointer created with %d on seat %q output %q, want create_virtual_pointer_with_output on owner, HEADLESS-2",
					r.opcode, f.names[r.args[0]], f.names[r.args[1]])
			}
			creates++
		case "zwp_virtual_keyboard_manager_v1":
			if f.names[r.args[0]] != "owner" {
				t.Errorf("keyboard created on seat %q, want owner", f.names[r.args[0]])
			}
			creates++
		}
	}
	f.mu.Unlock()
	if creates != 2 {
		t.Errorf("%d devices created, want 2", creates)
	}

	if _, err := OpenVirtualInput(sess, InputPlace{Seat: "nobody"}); err == nil || !strings.Contains(err.Error(), "seat0, owner") {
		t.Errorf("unknown seat: err = %v, want one naming the seats", err)
	}
	if _, err := OpenVirtualInput(sess, InputPlace{Output: "HEADLESS-9"}); err == nil || !strings.Contains(err.Error(), "HEADLESS-1, HEADLESS-2") {
		t.Errorf("unknown output: err = %v, want one naming the outputs", err)
	}
	// With no seat named, the first is taken, as before there were several.
	if v2, err := OpenVirtualInput(sess, InputPlace{}); err != nil {
		t.Errorf("no seat named: %v", err)
	} else {
		v2.Close()
	}
}
