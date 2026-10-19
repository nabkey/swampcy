package desktop

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

// Launcher starts GUI apps on the session's display, detached from swampcy so they
// outlive it, with accessibility turned on. Their output goes to one log file.
type Launcher struct {
	Env     []string
	LogPath string
	Log     *slog.Logger
}

// Start runs command with sh -c and returns its pid.
func (l Launcher) Start(command string) (int, error) {
	cmd := exec.Command("/bin/sh", "-c", command)
	cmd.Env = l.Env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
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
	}()
	return pid, nil
}
