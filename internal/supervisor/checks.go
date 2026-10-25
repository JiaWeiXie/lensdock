package supervisor

import (
	"fmt"
	"io/fs"
	"os"
	"syscall"
)

// VerifyDataDir verifies a persistent data directory: a real directory (not a
// symlink) owned by the browser user and inaccessible to group and others.
// It never changes ownership or permissions.
func VerifyDataDir(path string) error {
	return verifyDir(path, browserUID)
}

func verifyDir(path string, wantUID uint32) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("data dir %s: %w", path, err)
	}
	if fi.Mode()&fs.ModeSymlink != 0 {
		return fmt.Errorf("data dir %s: is a symlink", path)
	}
	if !fi.IsDir() {
		return fmt.Errorf("data dir %s: not a directory", path)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("data dir %s: cannot read ownership", path)
	}
	if st.Uid != wantUID {
		return fmt.Errorf("data dir %s: owner uid %d, want %d", path, st.Uid, wantUID)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("data dir %s: mode %04o grants group/other access", path, perm)
	}
	return nil
}
