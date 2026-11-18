package kvm

import (
	"os"
	"regexp"
	"syscall"
)

// What the runner's listener says when it has read the configuration the entry command gave
// it and found no credentials in it.
var runnerOutput = regexp.MustCompile(`Could not find file '[^']*/actions-runner\.mcc/\.credentials'[\s\S]*Runner listener exit`)

func fileUID(st os.FileInfo) int {
	if s, ok := st.Sys().(*syscall.Stat_t); ok {
		return int(s.Uid)
	}
	return -1
}

// runnerStarted reports whether output is the Actions runner's own, rather than a shell's
// complaint that it could not be run.
func runnerStarted(out string) bool {
	return runnerOutput.MatchString(out)
}
