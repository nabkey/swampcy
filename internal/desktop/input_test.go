package desktop

import (
	"reflect"
	"testing"
)

func TestParseKeys(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want []Combo
	}{
		{"Return", []Combo{{Key: "Return"}}},
		{"enter", []Combo{{Key: "Return"}}},
		{"ctrl+s", []Combo{{Mods: []string{"ctrl"}, Key: "s"}}},
		{"Ctrl+Shift+T", []Combo{{Mods: []string{"ctrl", "shift"}, Key: "t"}}},
		{"super+Return", []Combo{{Mods: []string{"logo"}, Key: "Return"}}},
		{"alt+F4", []Combo{{Mods: []string{"alt"}, Key: "F4"}}},
		{"f12", []Combo{{Key: "F12"}}},
		{"Down Down Return", []Combo{{Key: "Down"}, {Key: "Down"}, {Key: "Return"}}},
		{"ctrl+-", []Combo{{Mods: []string{"ctrl"}, Key: "minus"}}},
		{"ctrl++", []Combo{{Mods: []string{"ctrl"}, Key: "plus"}}},
		{"+", []Combo{{Key: "plus"}}},
		{"XF86AudioPlay", []Combo{{Key: "XF86AudioPlay"}}},
		{"pagedown", []Combo{{Key: "Page_Down"}}},
	} {
		got, err := ParseKeys(tc.in)
		if err != nil {
			t.Errorf("ParseKeys(%q): %v", tc.in, err)
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("ParseKeys(%q) = %+v, want %+v", tc.in, got, tc.want)
		}
	}
	for _, bad := range []string{"", "   ", "hyper+a", "ctrl+", "ctrl++a+"} {
		if got, err := ParseKeys(bad); err == nil {
			t.Errorf("ParseKeys(%q) = %+v, want an error", bad, got)
		}
	}
}
