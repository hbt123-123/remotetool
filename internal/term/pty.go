//go:build !windows

// Package term provides remote terminal sessions backed by real PTYs.
// Unix backend: fork/exec with a pty (creack/pty).
package term

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"

	"github.com/creack/pty"
)

// PTY is one shell session pseudoterminal.
type PTY interface {
	io.ReadWriteCloser
	Resize(cols, rows uint16) error
	Kill() error
	Wait() (int, error)
}

// DefaultShell picks the login shell.
func DefaultShell() string {
	if s := os.Getenv("SHELL"); s != "" {
		return s
	}
	if p, err := exec.LookPath("bash"); err == nil {
		return p
	}
	return "/bin/sh"
}

type unixPTY struct {
	f    *os.File
	cmd  *exec.Cmd
	once sync.Once
	code int
}

// StartPTY launches a shell on a fresh pty. creack/pty starts the child with
// Setsid+Setctty, so the shell leads its own session (pgid == pid) — which is
// what killSession sweeps below.
func StartPTY(cols, rows uint16, shell, cwd string) (PTY, error) {
	if shell == "" {
		shell = DefaultShell()
	}
	cmd := exec.Command(shell)
	cmd.Env = append(os.Environ(), "TERM=xterm-256color", "COLORTERM=truecolor")
	if cwd != "" {
		cmd.Dir = cwd
	}
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: cols, Rows: rows})
	if err != nil {
		return nil, err
	}
	return &unixPTY{f: f, cmd: cmd}, nil
}

func (u *unixPTY) Read(p []byte) (int, error)  { return u.f.Read(p) }
func (u *unixPTY) Write(p []byte) (int, error) { return u.f.Write(p) }

func (u *unixPTY) Resize(cols, rows uint16) error {
	if cols == 0 || rows == 0 {
		return nil
	}
	return pty.Setsize(u.f, &pty.Winsize{Cols: cols, Rows: rows})
}

func (u *unixPTY) Kill() error {
	if u.cmd.Process == nil {
		return nil
	}
	killSession(u.cmd.Process.Pid)
	return nil
}

func (u *unixPTY) Wait() (int, error) {
	u.once.Do(func() {
		err := u.cmd.Wait()
		u.code = 0
		if err != nil {
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				u.code = ee.ExitCode()
			}
		}
		// The shell exited but grandchildren may still hold the slave open,
		// which would keep the master readable forever. Sweep the session and
		// close the master fd: the master is pollable, so closing it wakes a
		// readLoop still blocked in Read instead of leaking the fd and the
		// goroutine until daemon exit.
		killSession(u.cmd.Process.Pid)
		_ = u.f.Close()
	})
	return u.code, nil
}

func (u *unixPTY) Close() error {
	_ = u.Kill()
	_ = u.f.Close()
	return nil
}
