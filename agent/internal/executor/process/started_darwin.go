package process

import (
	"strconv"

	"golang.org/x/sys/unix"
)

// startedAt identifies when a process began, or is "" if there is no such process. A process
// id is used again in time, and what was written down about work before a crash must not be
// taken for whatever has the id now.
//
// It asks the kernel, not ps: ps is not always there to run. An agent started by a service
// manager may have a PATH without it, and a build sandbox does not let it run at all.
func startedAt(pid int) string {
	k, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || k == nil || int(k.Proc.P_pid) != pid {
		return ""
	}
	t := k.Proc.P_starttime
	return strconv.FormatInt(t.Sec, 10) + "." + strconv.FormatInt(int64(t.Usec), 10)
}
