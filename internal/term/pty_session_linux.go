//go:build linux

package term

import (
	"os"
	"strconv"
	"strings"
	"syscall"
)

// killSession SIGKILLs every process in the session led by sid. A job-control
// shell gives each job its own process group, so killing the leader's group
// alone would orphan them; the session id never changes when processes are
// reparented, so /proc can still find the leftovers. Entries vanishing
// mid-scan are fine (ESRCH is ignored by syscall.Kill).
func killSession(sid int) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		// No /proc (unexpected on Linux): fall back to the leader's group.
		_ = syscall.Kill(-sid, syscall.SIGKILL)
		return
	}
	for _, e := range entries {
		pid, perr := strconv.Atoi(e.Name())
		if perr != nil || pid <= 0 {
			continue
		}
		if sessionIDOf(pid) != sid {
			continue
		}
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}

// sessionIDOf reads field 6 (sid) of /proc/<pid>/stat. comm (field 2) may
// contain spaces and parens, so parsing starts after the last ')'.
func sessionIDOf(pid int) int {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0
	}
	i := strings.LastIndexByte(string(b), ')')
	if i < 0 {
		return 0
	}
	fields := strings.Fields(string(b)[i+1:])
	// fields[0] is state (field 3), so sid (field 6) is index 3.
	if len(fields) < 4 {
		return 0
	}
	v, err := strconv.Atoi(fields[3])
	if err != nil {
		return 0
	}
	return v
}
