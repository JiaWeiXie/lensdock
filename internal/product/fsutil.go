package product

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// ErrBusy means another process holds the exclusive lifecycle lock that
// serializes initialization or start of one deployment.
var ErrBusy = errors.New("product: another LensDock lifecycle operation holds the lock")

// lockFile takes an exclusive, non-blocking advisory lock on path (created
// when missing, mode 0600). The kernel releases it when the process dies. The
// returned function releases it.
func lockFile(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w (%s)", ErrBusy, path)
		}
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	return func() { _ = f.Close() }, nil
}

// syncDir flushes the directory entries of dir.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open directory for sync: %w", err)
	}
	serr := d.Sync()
	cerr := d.Close()
	return errors.Join(serr, cerr)
}

// syncTree flushes every directory below and including root, deepest first,
// so the whole tree is durable before it is renamed into place. Files are
// synced when they are written.
func syncTree(root string) error {
	var dirs []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			dirs = append(dirs, p)
		}
		return nil
	})
	if err != nil {
		return err
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := syncDir(dirs[i]); err != nil {
			return err
		}
	}
	return nil
}

// writeNew creates path with the exact mode, writes data and fsyncs. It
// refuses to overwrite an existing file or to follow a symlink, and removes a
// partially written file.
func writeNew(path string, data []byte, mode os.FileMode) (err error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			_ = os.Remove(path)
		}
	}()
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Chmod(mode); err != nil {
		return err
	}
	return f.Sync()
}

// writeFileAtomic replaces path with data through a same-directory rename and
// syncs the directory, so a crash leaves either the old or the new content.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-"+filepath.Base(path)+"-")
	if err != nil {
		return err
	}
	name := tmp.Name()
	_, werr := tmp.Write(data)
	cherr := tmp.Chmod(mode)
	serr := tmp.Sync()
	clerr := tmp.Close()
	if err := errors.Join(werr, cherr, serr, clerr); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return err
	}
	return syncDir(filepath.Dir(path))
}

// publishNew makes path appear atomically with its complete content. Unlike
// writeFileAtomic it never replaces an existing file: when path exists the
// error satisfies errors.Is(err, os.ErrExist).
func publishNew(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-"+filepath.Base(path)+"-")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	_, werr := tmp.Write(data)
	cherr := tmp.Chmod(mode)
	serr := tmp.Sync()
	clerr := tmp.Close()
	if err := errors.Join(werr, cherr, serr, clerr); err != nil {
		return err
	}
	if err := os.Link(name, path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}
