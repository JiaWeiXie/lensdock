package hostmanager

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// writeFileAtomic writes data to path through a temporary file in the same
// directory, fsyncs it, renames it over path and syncs the directory, so a
// crash leaves either the old or the new content and never a partial file.
func writeFileAtomic(path string, data []byte, mode os.FileMode) (err error) {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temporary file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}
	}()
	if err = tmp.Chmod(mode); err != nil {
		return fmt.Errorf("chmod temporary file: %w", err)
	}
	if _, err = tmp.Write(data); err != nil {
		return fmt.Errorf("write temporary file: %w", err)
	}
	if err = tmp.Sync(); err != nil {
		return fmt.Errorf("sync temporary file: %w", err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("close temporary file: %w", err)
	}
	if err = os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename temporary file: %w", err)
	}
	return syncDir(dir)
}

func writeJSONAtomic(path string, v any, mode os.FileMode) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("encode %s: %w", filepath.Base(path), err)
	}
	return writeFileAtomic(path, append(data, '\n'), mode)
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open directory for sync: %w", err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil && !errors.Is(err, syscall.EINVAL) {
		return fmt.Errorf("sync directory: %w", err)
	}
	return nil
}

func ownedByUs(fi os.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Geteuid()
}

// identityOf returns the device and inode of fi.
func identityOf(fi os.FileInfo) (DirectoryIdentity, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return DirectoryIdentity{}, false
	}
	return DirectoryIdentity{Device: uint64(st.Dev), Inode: uint64(st.Ino)}, true //nolint:unconvert // width differs per platform
}

// checkPrivateDir verifies a directory owned by the current user without
// group or other access. Symlinks are not followed.
func checkPrivateDir(path string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", path)
	}
	if !ownedByUs(fi) {
		return fmt.Errorf("%s is not owned by the current user", path)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%s must not be accessible by group or others (mode %o)", path, fi.Mode().Perm())
	}
	return nil
}

// ensurePrivateDir creates a missing directory with mode 0700 and verifies it.
func ensurePrivateDir(path string) error {
	if err := os.Mkdir(path, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	return checkPrivateDir(path)
}

// readPrivateFile reads a regular file of at most max bytes that only the
// current user can access. Symlinks are not followed.
func readPrivateFile(path string, max int64) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", filepath.Base(path))
	}
	if !ownedByUs(fi) || fi.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s must be private to the current user", filepath.Base(path))
	}
	if fi.Size() > max {
		return nil, fmt.Errorf("%s is larger than %d bytes", filepath.Base(path), max)
	}
	data, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, fmt.Errorf("%s is larger than %d bytes", filepath.Base(path), max)
	}
	return data, nil
}

func readPrivateJSON(path string, max int64, v any) error {
	data, err := readPrivateFile(path, max)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("decode %s: %w", filepath.Base(path), err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return fmt.Errorf("decode %s: trailing data", filepath.Base(path))
	}
	return nil
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("random: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// tryFlock takes an exclusive advisory lock without waiting. The returned
// file keeps the lock for as long as it, or any inherited duplicate of its
// descriptor, stays open. busy is true when another holder owns the lock.
func tryFlock(path string) (f *os.File, busy bool, err error) {
	f, err = os.OpenFile(path, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, false, fmt.Errorf("open lock %s: %w", filepath.Base(path), err)
	}
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() || !ownedByUs(fi) {
		_ = f.Close()
		return nil, false, fmt.Errorf("lock %s is not a regular file owned by the current user", filepath.Base(path))
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, true, nil
		}
		return nil, false, fmt.Errorf("lock %s: %w", filepath.Base(path), err)
	}
	return f, false, nil
}

// waitFlock polls tryFlock until the lock is acquired, ctx ends or wait
// elapses.
func waitFlock(ctx context.Context, path string, wait time.Duration) (*os.File, error) {
	deadline := time.Now().Add(wait)
	for {
		f, busy, err := tryFlock(path)
		if err != nil {
			return nil, err
		}
		if !busy {
			return f, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("lock %s is busy", filepath.Base(path))
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}
