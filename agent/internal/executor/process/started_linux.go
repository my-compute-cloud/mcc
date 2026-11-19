package process

import (
	"os"
	"strconv"
	"strings"
)

// startedAt identifies when a process began, or is "" if there is no such process. A process
// id is used again in time, and what was written down about work before a crash must not be
// taken for whatever has the id now.
func startedAt(pid int) string {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return ""
	}
	// The command name, in parentheses, may itself hold spaces and parentheses; the fields
	// after its last ")" are fixed. Start time, in clock ticks since boot, is the 22nd field.
	stat := string(b)
	fields := strings.Fields(stat[strings.LastIndex(stat, ")")+1:])
	if len(fields) < 20 {
		return ""
	}
	return fields[19]
}
