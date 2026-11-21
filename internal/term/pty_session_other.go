//go:build !windows && !linux

package term

import "syscall"

// killSession falls back to the session leader's process group: without
// /proc there is no portable way to enumerate session members, so processes
// the shell put into separate job groups are only covered on Linux.
func killSession(sid int) {
	_ = syscall.Kill(-sid, syscall.SIGKILL)
}
