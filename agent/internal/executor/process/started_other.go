//go:build !linux && !darwin

package process

import (
	"os/exec"
	"strconv"
	"strings"
)

// startedAt identifies when a process began, as ps prints it, or is "" if there is no such
// process.
func startedAt(pid int) string {
	out, err := exec.Command("ps", "-o", "lstart=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
