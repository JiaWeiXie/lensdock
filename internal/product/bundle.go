package product

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
)

// VersionFile marks a directory as a release bundle. A release bundle is the
// extracted release archive: the host binary at the root next to VERSION, the
// product Compose file, the Chromium seccomp profile and the Linux helper
// binaries run inside update sidecars.
const VersionFile = "VERSION"

type bundleFile struct {
	rel  string
	mode os.FileMode // mode of the private copy
	max  int64
}

const (
	maxBundleText = 1 << 20
	maxBundleCLI  = 256 << 20
)

// bundleFiles lists everything Initialize needs from a bundle, in the order it
// is copied. Nothing else is read from the bundle at run time.
var bundleFiles = []bundleFile{
	{VersionFile, 0o600, 256},
	{"docker/compose.yaml", 0o600, maxBundleText},
	{"docker/browser/seccomp/chromium.json", 0o600, maxBundleText},
	// The helpers are bind-mounted into containers that run as another
	// user, so they keep an executable mode.
	{"bin/linux-amd64/lensdock", 0o755, maxBundleCLI},
	{"bin/linux-arm64/lensdock", 0o755, maxBundleCLI},
}

var bundleVersionPattern = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z._+-]{0,63}$`)

// Bundle is a validated release bundle root.
type Bundle struct {
	Root    string
	Version string
}

// ErrNotBundle means a directory has no VERSION file and so is not a bundle
// (a source checkout, or a binary somewhere else).
var ErrNotBundle = errors.New("not a release bundle")

// OpenBundle validates root: VERSION and every file Initialize copies must be
// regular files (never symlinks) within their size limits.
func OpenBundle(root string) (*Bundle, error) {
	if _, err := os.Lstat(filepath.Join(root, VersionFile)); errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s has no %s file", ErrNotBundle, root, VersionFile)
	}
	for _, f := range bundleFiles {
		p := filepath.Join(root, filepath.FromSlash(f.rel))
		fi, err := os.Lstat(p)
		if err != nil {
			return nil, fmt.Errorf("release bundle %s is incomplete: %w", root, err)
		}
		if !fi.Mode().IsRegular() {
			return nil, fmt.Errorf("release bundle %s: %s must be a regular file", root, f.rel)
		}
		if fi.Size() > f.max {
			return nil, fmt.Errorf("release bundle %s: %s is larger than %d bytes", root, f.rel, f.max)
		}
	}
	raw, err := os.ReadFile(filepath.Join(root, VersionFile))
	if err != nil {
		return nil, err
	}
	v := strings.TrimSpace(string(raw))
	if !bundleVersionPattern.MatchString(v) {
		return nil, fmt.Errorf("release bundle %s: %s does not hold a version", root, VersionFile)
	}
	return &Bundle{Root: root, Version: v}, nil
}

// Snapshot copies the bundle files into dest with private modes. dest need not
// exist; no file is overwritten. The copy is what a deployment points at, so
// removing or upgrading the installed bundle later cannot break it. The bundle
// is validated again first, so a hand-built Bundle value cannot copy anything
// OpenBundle would refuse or claim another version.
func (b *Bundle) Snapshot(dest string) error {
	fresh, err := OpenBundle(b.Root)
	if err != nil {
		return err
	}
	if fresh.Version != b.Version {
		return fmt.Errorf("release bundle %s holds version %s, not %s", b.Root, fresh.Version, b.Version)
	}
	for _, f := range bundleFiles {
		src := filepath.Join(b.Root, filepath.FromSlash(f.rel))
		dst := filepath.Join(dest, filepath.FromSlash(f.rel))
		if err := copyBundleFile(src, dst, f); err != nil {
			return fmt.Errorf("copy %s from the release bundle: %w", f.rel, err)
		}
	}
	return nil
}

func copyBundleFile(src, dst string, f bundleFile) (err error) {
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	in, err := os.OpenFile(src, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer in.Close()
	if fi, err := in.Stat(); err != nil {
		return err
	} else if !fi.Mode().IsRegular() {
		return errors.New("not a regular file")
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			_ = os.Remove(dst)
		}
	}()
	n, err := io.Copy(out, io.LimitReader(in, f.max+1))
	if err != nil {
		return err
	}
	if n > f.max {
		return fmt.Errorf("larger than %d bytes", f.max)
	}
	if err := out.Chmod(f.mode); err != nil {
		return err
	}
	return out.Sync()
}
