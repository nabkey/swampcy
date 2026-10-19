package web

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/nabkey/swampcy/internal/desktop"
)

// The owner's launcher: the apps installed on the machine, from their
// desktop entries, started by sway itself so that they get its environment (the
// Wayland and X displays, the session bus), then moved to the owner's desktop.
// The page names an entry; it never sends a command, and only an entry the
// server finds afresh is started.

// appInfo is one app, for the page.
type appInfo struct {
	ID   string `json:"id"` // the desktop file ID, e.g. org.gnome.Calculator.desktop
	Name string `json:"name"`

	wmClass string // StartupWMClass: what its windows' class or app ID will be
}

// owns says whether window app (an app ID or X11 class) is likely the app's: its
// StartupWMClass, or its desktop file ID without .desktop, ignoring case.
func (a appInfo) owns(app string) bool {
	app = strings.ToLower(app)
	return app != "" && (app == strings.ToLower(a.wmClass) || app == strings.ToLower(strings.TrimSuffix(a.ID, ".desktop")))
}

// appID is what a desktop file ID must look like to be started: it goes into a
// sway command.
var appID = regexp.MustCompile(`^[A-Za-z0-9._+-]{1,120}\.desktop$`)

// appDirs are where desktop entries live, in order of precedence: the user's own,
// then the system's (XDG base directories).
func appDirs(getenv func(string) string) []string {
	home := getenv("XDG_DATA_HOME")
	if home == "" {
		home = filepath.Join(getenv("HOME"), ".local", "share")
	}
	dirs := []string{home}
	sys := getenv("XDG_DATA_DIRS")
	if sys == "" {
		sys = "/usr/local/share:/usr/share"
	}
	dirs = append(dirs, filepath.SplitList(sys)...)
	for i, d := range dirs {
		dirs[i] = filepath.Join(d, "applications")
	}
	return dirs
}

// scanApps lists the apps the dirs' desktop entries offer, sorted by name: type
// Application, and not hidden, not NoDisplay, not run in a terminal. An ID found
// in an earlier dir hides the same ID in a later one, displayed or not.
func scanApps(dirs []string) []appInfo {
	seen := map[string]bool{}
	var apps []appInfo
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			id := e.Name()
			if e.IsDir() || !appID.MatchString(id) || seen[id] {
				continue
			}
			seen[id] = true
			if name, class, ok := readEntry(filepath.Join(dir, id)); ok {
				apps = append(apps, appInfo{ID: id, Name: name, wmClass: class})
			}
		}
	}
	slices.SortFunc(apps, func(a, b appInfo) int {
		return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
	})
	return apps
}

// readEntry reads a desktop entry's [Desktop Entry] group, and says whether it is
// an app to offer, by what name, and with what StartupWMClass.
func readEntry(path string) (name, class string, ok bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", "", false
	}
	defer f.Close()
	keys := map[string]string{}
	in := false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "[") {
			if in {
				break // the actions' groups come after
			}
			in = line == "[Desktop Entry]"
			continue
		}
		if k, v, ok := strings.Cut(line, "="); in && ok {
			k = strings.TrimSpace(k)
			if _, dup := keys[k]; !dup {
				keys[k] = strings.TrimSpace(v)
			}
		}
	}
	if keys["Type"] != "Application" || keys["Name"] == "" || keys["Exec"] == "" ||
		keys["NoDisplay"] == "true" || keys["Hidden"] == "true" || keys["Terminal"] == "true" {
		return "", "", false
	}
	return keys["Name"], keys["StartupWMClass"], true
}

// A launch follows the app's windows for appFollow, moving each to the owner's
// desktop as it opens: an app may open more than one, and the first may go (Steam
// shows an updater, then its own window a minute later). When none of the new
// windows looks like the app's after appWait, the first new window is taken.
const (
	appWait   = 45 * time.Second
	appFollow = 3 * time.Minute
)

// launchApp starts the app with ID id through sway, and moves its first new
// window to the owner's desktop, keeping the agents' focus where it was. It
// returns once the app is started; done is called with the window's title, or an
// error, when the window has been moved or the wait is over.
func (se *session) launchApp(id string, done func(title string, err error)) error {
	var app *appInfo
	for _, a := range scanApps(appDirs(os.Getenv)) {
		if a.ID == id {
			app = &a
			break
		}
	}
	if app == nil || !appID.MatchString(id) {
		return fmt.Errorf("no app %q", id)
	}
	tree, err := se.s.live.Tree()
	if err != nil {
		return err
	}
	before := map[int64]bool{}
	for _, w := range tree.Windows() {
		before[w.ID] = true
	}
	// sway's exec runs the command in sway's environment, through /bin/sh.
	if err := se.s.live.Command("exec gtk-launch " + id); err != nil {
		return err
	}
	se.s.log().Info("launch", "app", id)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), appFollow)
		defer cancel()
		began := time.Now()
		moved := map[int64]bool{}
		reported := false
		report := func(title string, err error) {
			if !reported {
				reported = true
				done(title, err)
			}
		}
		take := func(w desktop.Window) {
			moved[w.ID] = true
			err := errors.New("there is no desktop of the owner's to move it to")
			if _, ok := se.desks["own"]; ok {
				err = se.move(w.ID, "own")
			}
			se.s.log().Info("launch", "app", id, "window", w.ID, "title", w.Name, "err", err)
			report(w.Name, err)
		}
		for {
			select {
			case <-ctx.Done():
				// An app that runs once may only raise the window it has open.
				report("", fmt.Errorf("%s opened no new window within %s; if it was open already, ▣ finds it", app.Name, appFollow))
				return
			case <-time.After(250 * time.Millisecond):
			}
			tree, err := se.s.live.Tree()
			if err != nil {
				continue
			}
			var fresh []desktop.Window
			for _, w := range tree.Windows() {
				if !before[w.ID] && !moved[w.ID] {
					fresh = append(fresh, w)
				}
			}
			for _, w := range fresh {
				if app.owns(w.App()) {
					take(w)
				}
			}
			if len(moved) == 0 && len(fresh) > 0 && time.Since(began) > appWait {
				take(fresh[0]) // nothing looked like the app's: the first new window
			}
		}
	}()
	return nil
}
