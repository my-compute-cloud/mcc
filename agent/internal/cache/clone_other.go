//go:build !darwin

package cache

import "errors"

// cloneDir needs APFS. Hosts without it share a job its cache directly, at boot.
func cloneDir(src, dst string) error {
	return errors.New("cloning a directory needs APFS, on macOS")
}
