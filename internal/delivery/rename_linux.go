//go:build linux

package delivery

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// renameNoReplace moves the directory from to the absent path to. An existing
// to is never replaced, even by an empty directory.
func renameNoReplace(from, to string) error {
	err := unix.Renameat2(unix.AT_FDCWD, from, unix.AT_FDCWD, to, unix.RENAME_NOREPLACE)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, unix.EEXIST):
		return fmt.Errorf("%s already exists: %w", to, os.ErrExist)
	case errors.Is(err, unix.EINVAL), errors.Is(err, unix.ENOSYS), errors.Is(err, unix.ENOTSUP):
		// The filesystem or kernel lacks RENAME_NOREPLACE.
		return renameChecked(from, to)
	}
	return &os.LinkError{Op: "rename", Old: from, New: to, Err: err}
}
