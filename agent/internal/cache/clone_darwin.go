package cache

import "golang.org/x/sys/unix"

// cloneDir clones a directory tree with clonefile(2): one call for the whole tree, which takes
// no data blocks of its own and keeps each file's times. Both must be on one APFS volume.
func cloneDir(src, dst string) error {
	return unix.Clonefile(src, dst, unix.CLONE_NOFOLLOW)
}
