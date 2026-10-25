package auth

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"syscall"
)

// ErrNotPrivate is wrapped by ReadPrivateFile when the file is not a regular,
// owner-only file owned by the current user.
var ErrNotPrivate = errors.New("auth: file is not private")

// ReadPrivateFile reads path after verifying with Lstat that it is a regular
// file (not a symlink), owned by the effective uid, with no group/other
// permission bits and a size of at most max bytes. The opened descriptor is
// re-checked against the Lstat result so the path cannot be swapped between
// the check and the read.
func ReadPrivateFile(path string, max int64) ([]byte, error) {
	if max < 0 {
		return nil, errors.New("auth: negative size limit")
	}
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("auth: stat private file: %w", err)
	}
	if err := checkPrivate(fi, max); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("auth: open private file: %w", err)
	}
	defer f.Close()
	fi2, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("auth: stat private file: %w", err)
	}
	if !os.SameFile(fi, fi2) {
		return nil, fmt.Errorf("%w: file changed while opening", ErrNotPrivate)
	}
	if err := checkPrivate(fi2, max); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, fmt.Errorf("auth: read private file: %w", err)
	}
	if int64(len(data)) > max {
		return nil, fmt.Errorf("%w: larger than %d bytes", ErrNotPrivate, max)
	}
	return data, nil
}

func checkPrivate(fi fs.FileInfo, max int64) error {
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%w: not a regular file", ErrNotPrivate)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%w: accessible by group or others", ErrNotPrivate)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("%w: owner cannot be determined", ErrNotPrivate)
	}
	if int64(st.Uid) != int64(os.Geteuid()) {
		return fmt.Errorf("%w: owned by another user", ErrNotPrivate)
	}
	if fi.Size() > max {
		return fmt.Errorf("%w: larger than %d bytes", ErrNotPrivate, max)
	}
	return nil
}

// WritePrivateFile creates path with mode 0600 and writes data to it. It
// refuses to overwrite an existing file or to follow a symlink, requires the
// parent directory to exist, and fsyncs before returning. A partially written
// file is removed.
func WritePrivateFile(path string, data []byte) (err error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return fmt.Errorf("auth: create private file: %w", err)
	}
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = os.Remove(path)
		}
	}()
	if _, err = f.Write(data); err != nil {
		return fmt.Errorf("auth: write private file: %w", err)
	}
	if err = f.Sync(); err != nil {
		return fmt.Errorf("auth: sync private file: %w", err)
	}
	if err = f.Close(); err != nil {
		return fmt.Errorf("auth: close private file: %w", err)
	}
	return nil
}
