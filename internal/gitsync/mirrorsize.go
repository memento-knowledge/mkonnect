package gitsync

import (
	"errors"
	"io/fs"
	"path/filepath"
)

// mebibyte converts the MiB the platform's limits are expressed in to bytes.
const mebibyte = 1 << 20

// errOverCeiling stops a size walk as soon as the answer is known.
var errOverCeiling = errors.New("over ceiling")

// mirrorExceeds reports whether the files under dir add up to more than ceiling bytes. It sums the
// apparent size of regular files and follows no symlink, so a link inside the mirror cannot make it
// count (or walk) something elsewhere. It stops reading as soon as the ceiling is passed. A file that
// disappears during the walk is skipped: git may repack in the background, and what vanished is
// either counted elsewhere or gone. A missing dir is empty.
func mirrorExceeds(dir string, ceiling int64) (bool, error) {
	var total int64
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if info.Mode().IsRegular() {
			total += info.Size()
			if total > ceiling {
				return errOverCeiling
			}
		}
		return nil
	})
	switch {
	case errors.Is(err, errOverCeiling):
		return true, nil
	case err != nil:
		return false, err
	}
	return false, nil
}
