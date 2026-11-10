package desktop

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// Launcher starts GUI apps on the session's display, detached from swampcy so they
// outlive it, with accessibility turned on. Their output goes to one log file.
type Launcher struct {
	Env     []string
	LogPath string
	Log     *slog.Logger
	// Exited, when set, is told when the command's shell exits, with what it wrote
	// to the log (its tail), so a command that failed at once can be reported.
	Exited func(pid int, err error, output string)
}

// Start runs command with sh -c and returns its pid.
func (l Launcher) Start(command string) (int, error) {
	cmd := exec.Command("/bin/sh", "-c", command)
	cmd.Env = l.Env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	var offset int64
	if l.LogPath != "" {
		if err := os.MkdirAll(filepath.Dir(l.LogPath), 0o700); err != nil {
			return 0, err
		}
		f, err := os.OpenFile(l.LogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return 0, err
		}
		defer f.Close() // the child keeps its own copy
		fmt.Fprintf(f, "--- %s\n", command)
		if fi, err := f.Stat(); err == nil {
			offset = fi.Size()
		}
		cmd.Stdout, cmd.Stderr = f, f
	}
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	pid := cmd.Process.Pid
	go func() {
		err := cmd.Wait() // reap it; the shell often exits at once when the app forks
		if l.Log != nil {
			l.Log.Info("launched app exited", "pid", pid, "err", err)
		}
		if l.Exited != nil {
			l.Exited(pid, err, logTail(l.LogPath, offset))
		}
	}()
	return pid, nil
}

// logTail is what was appended to the log after offset, at most its last 1500
// bytes. Other apps write to the same log, so it may hold their lines too.
func logTail(path string, offset int64) string {
	if path == "" {
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || fi.Size() <= offset {
		return ""
	}
	const most = 1500
	if fi.Size()-offset > most {
		offset = fi.Size() - most
	}
	buf := make([]byte, fi.Size()-offset)
	n, _ := f.ReadAt(buf, offset)
	return strings.TrimSpace(string(buf[:n]))
}
