package cache

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Lend gives a job a copy of its cache at into, which must not exist yet, instead of the
// cache itself. A VM that was booted before anyone knew whose job it would run has only an
// empty directory shared into it, and a VM's shares cannot change once it has booted; so the
// job's cache is put into that directory when the job arrives.
//
// The copy is a clone (clonefile on APFS): it takes no time and no space, whatever the size of
// the cache, and only what the job writes takes room on the disk. The function returned puts
// back what the job added or changed, then deletes the copy, and must be called once the VM
// is gone. Until then the cache is held as Acquire holds it.
func (m *Manager) Lend(githubURL, into string) (func(), error) {
	dir, release, err := m.Acquire(githubURL)
	if err != nil {
		return nil, err
	}
	if err := cloneDir(dir, into); err != nil {
		release()
		return nil, fmt.Errorf("cloning the cache: %w", err)
	}
	return func() {
		if n, err := merge(into, dir); err != nil {
			m.Log.Warn("keeping what a job added to its cache failed", "err", err)
		} else if n > 0 {
			m.Log.Debug("kept what a job added to its cache", "files", n)
		}
		_ = os.RemoveAll(into)
		release()
	}, nil
}

// merge moves into dst each file of src that dst lacks, and each that is newer in src than
// in dst: one the job changed, or only touched, which is how a job marks a path in a Nix cache
// as used. It says how many files it moved.
//
// What a job deleted is not deleted from dst: two jobs of a repository may run at once, each
// with its own copy, and what one of them no longer needed the other may have read. The caches
// are trimmed by time and size instead (TrimNix, Prune).
//
// A job writes src and may have put anything there. Only directories and plain files are taken:
// a link could point anywhere on the host. Nothing in dst is written through a link either.
func merge(src, dst string) (int, error) {
	if st, err := os.Lstat(dst); err != nil || !st.IsDir() {
		return 0, fmt.Errorf("%s is not a directory", dst)
	}
	moved := 0
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// What cannot be read is left out; the rest is still kept.
			if d != nil && d.IsDir() && path != src {
				return fs.SkipDir
			}
			return nil
		}
		if path == src {
			return nil
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return nil
		}
		target := filepath.Join(dst, rel)
		have, haveErr := os.Lstat(target)
		switch {
		case d.IsDir():
			if errors.Is(haveErr, fs.ErrNotExist) {
				// Made, not moved whole: what is in it is looked at like everything else.
				if os.Mkdir(target, 0o755) != nil {
					return fs.SkipDir
				}
				return nil
			}
			if haveErr != nil || !have.IsDir() {
				return fs.SkipDir
			}
			return nil
		case !d.Type().IsRegular():
			return nil
		case partial(rel):
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		switch {
		case errors.Is(haveErr, fs.ErrNotExist):
		case haveErr != nil || !have.Mode().IsRegular():
			return nil
		case !info.ModTime().After(have.ModTime()):
			// As it was cloned: not touched.
			return nil
		}
		// New, changed, or only marked as used: in each case the job's copy is the one to
		// keep, and moving it costs no more than changing the time of the other.
		if os.Rename(path, target) == nil {
			moved++
		}
		return nil
	})
	return moved, err
}

// partial is a file a job was still writing when it ended, under the name the Nix hook gives
// what it writes before renaming it (nix.go): kept, it would never be read or deleted.
func partial(rel string) bool {
	dir, name := filepath.Split(rel)
	dir = filepath.Clean(dir)
	return strings.HasPrefix(name, ".") && (dir == "nix" || dir == filepath.Join("nix", "nar"))
}
