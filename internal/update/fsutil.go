package update

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// writeFileAtomic writes data to path through a temporary file in the same
// directory, fsyncs it and renames it over path, so readers never observe a
// partial file. The parent directory must exist.
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

// writeJSONAtomic marshals v with indentation and a trailing newline.
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

func ensureJSONEOF(dec *json.Decoder) error {
	var extra any
	err := dec.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return errors.New("trailing JSON value")
	}
	return fmt.Errorf("trailing data: %w", err)
}

// readFileLimit reads a regular file of at most max bytes. Symlinks are not
// followed.
func readFileLimit(path string, max int64) ([]byte, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", filepath.Base(path))
	}
	if fi.Size() > max {
		return nil, fmt.Errorf("%s is larger than %d bytes", filepath.Base(path), max)
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, fmt.Errorf("%s is larger than %d bytes", filepath.Base(path), max)
	}
	return data, nil
}

// sha256File returns the lowercase hex SHA-256 and the size of a regular file.
func sha256File(path string) (string, int64, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", 0, err
	}
	if !fi.Mode().IsRegular() {
		return "", 0, fmt.Errorf("%s is not a regular file", filepath.Base(path))
	}
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// nearestExisting returns the deepest existing ancestor of path (path itself
// when it exists).
func nearestExisting(path string) string {
	p := filepath.Clean(path)
	for {
		if _, err := os.Lstat(p); err == nil {
			return p
		}
		parent := filepath.Dir(p)
		if parent == p {
			return p
		}
		p = parent
	}
}

// realPath resolves symlinks in the existing part of path and appends the
// remaining, not yet existing components.
func realPath(path string) string {
	p := filepath.Clean(path)
	base := nearestExisting(p)
	resolved, err := filepath.EvalSymlinks(base)
	if err != nil {
		resolved = base
	}
	rest, err := filepath.Rel(base, p)
	if err != nil || rest == "." {
		return resolved
	}
	return filepath.Join(resolved, rest)
}

// pathWithin reports whether child equals parent or lies below it, after
// resolving symlinks in the existing part of both paths.
func pathWithin(parent, child string) bool {
	rel, err := filepath.Rel(realPath(parent), realPath(child))
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// freeBytes returns the bytes available to unprivileged users on the file
// system holding path (or its nearest existing ancestor).
func freeBytes(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(nearestExisting(path), &st); err != nil {
		return 0, fmt.Errorf("statfs: %w", err)
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}

// dirSize sums the sizes of the regular files below dir.
func dirSize(dir string) (int64, error) {
	var total int64
	err := filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			info, err := d.Info()
			if err != nil {
				return err
			}
			total += info.Size()
		}
		return nil
	})
	return total, err
}

// lockFile takes an exclusive, non-blocking advisory lock on path (created
// when missing, mode 0600). The returned function releases it.
func lockFile(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrOperationInProgress
		}
		return nil, fmt.Errorf("lock %s: %w", filepath.Base(path), err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

// ErrOperationInProgress is returned when another update or rollback holds
// the deployment lock.
var ErrOperationInProgress = errors.New("update: another update or rollback is running for this deployment")
