//go:build darwin

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
	err := unix.RenamexNp(from, to, unix.RENAME_EXCL)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, unix.EEXIST):
		return fmt.Errorf("%s already exists: %w", to, os.ErrExist)
	case errors.Is(err, unix.ENOTSUP), errors.Is(err, unix.EINVAL):
		// The filesystem lacks RENAME_EXCL.
		return renameChecked(from, to)
	}
	return &os.LinkError{Op: "rename", Old: from, New: to, Err: err}
}
