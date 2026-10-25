package main

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

// version and commit are set by the release build:
//
//	-ldflags "-X main.version=<version> -X main.commit=<sha>"
var (
	version = "development"
	commit  = "unknown"
)

// versionText is what `lensdock version` prints.
func versionText() string { return fmt.Sprintf("lensdock %s (%s)\n", version, commit) }

// A release bundle is the extracted release archive: the host binary at the
// root next to VERSION, the product Compose file, the Chromium seccomp profile
// and the Linux helper binaries run inside update sidecars.
const bundleVersionFile = "VERSION"

type bundleFile struct {
	rel  string
	mode os.FileMode // mode of the private copy
	max  int64
}

const (
	maxBundleText = 1 << 20
	maxBundleCLI  = 256 << 20
)

// bundleFiles lists everything product-init needs from a bundle, in the order
// it is copied. Nothing else is read from the bundle at run time.
var bundleFiles = []bundleFile{
	{bundleVersionFile, 0o600, 256},
	{"docker/compose.yaml", 0o600, maxBundleText},
	{"docker/browser/seccomp/chromium.json", 0o600, maxBundleText},
	// The helpers are bind-mounted into containers that run as another
	// user, so they keep an executable mode.
	{"bin/linux-amd64/lensdock", 0o755, maxBundleCLI},
	{"bin/linux-arm64/lensdock", 0o755, maxBundleCLI},
}

var bundleVersionPattern = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z._+-]{0,63}$`)

// releaseBundle is a validated release bundle root.
type releaseBundle struct {
	Root    string
	Version string
}

// errNotBundle means a directory has no VERSION file and so is not a bundle
// (a source checkout, or a binary somewhere else).
var errNotBundle = errors.New("not a release bundle")

// openBundle validates root: VERSION and every file product-init copies must
// be regular files (never symlinks) within their size limits.
func openBundle(root string) (*releaseBundle, error) {
	if _, err := os.Lstat(filepath.Join(root, bundleVersionFile)); errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s has no %s file", errNotBundle, root, bundleVersionFile)
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
	raw, err := os.ReadFile(filepath.Join(root, bundleVersionFile))
	if err != nil {
		return nil, err
	}
	v := strings.TrimSpace(string(raw))
	if !bundleVersionPattern.MatchString(v) {
		return nil, fmt.Errorf("release bundle %s: %s does not hold a version", root, bundleVersionFile)
	}
	return &releaseBundle{Root: root, Version: v}, nil
}

// snapshot copies the bundle files into dest with private modes. dest need not
// exist; no file is overwritten. The copy is what a deployment points at, so
// removing or upgrading the installed bundle later cannot break it.
func (b *releaseBundle) snapshot(dest string) error {
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

// executableDir is the directory of the running executable with every symlink
// resolved, so a `~/.local/bin/lensdock` link or a mise shim path leads to the
// installed bundle.
func (c *cli) executableDir() (string, error) {
	exe := os.Executable
	if c.executable != nil {
		exe = c.executable
	}
	p, err := exe()
	if err != nil {
		return "", err
	}
	p, err = filepath.EvalSymlinks(p)
	if err != nil {
		return "", err
	}
	return filepath.Dir(p), nil
}

// findBundle returns the release bundle to initialize from: explicit when
// given, else the one the running executable sits in. It returns nil (and no
// error) when the executable is not part of a bundle, which is the source
// checkout case. A directory that has a VERSION file but is not a complete
// bundle is an error.
func (c *cli) findBundle(explicit string) (*releaseBundle, error) {
	if explicit != "" {
		root, err := filepath.Abs(explicit)
		if err != nil {
			return nil, err
		}
		if root, err = filepath.EvalSymlinks(root); err != nil {
			return nil, fmt.Errorf("--bundle-dir: %w", err)
		}
		b, err := openBundle(root)
		if err != nil {
			return nil, usageErrorf("--bundle-dir: %v", err)
		}
		return b, nil
	}
	dir, err := c.executableDir()
	if err != nil {
		return nil, nil
	}
	b, err := openBundle(dir)
	if errors.Is(err, errNotBundle) {
		return nil, nil
	}
	return b, err
}

// homeDir returns the user's home directory or "".
func (c *cli) homeDir() string {
	if h := c.getenv("HOME"); h != "" {
		return h
	}
	h, _ := os.UserHomeDir()
	return h
}

// defaultStateDir is the state directory used when --state-dir is not given:
// $LENSDOCK_STATE_DIR, else $HOME/.local/share/lensdock for a CLI that runs
// from a release bundle, else .lensdock (the source checkout).
func (c *cli) defaultStateDir() string {
	if v := c.getenv(envStateDir); v != "" {
		return v
	}
	if dir, err := c.executableDir(); err == nil {
		if _, err := os.Lstat(filepath.Join(dir, bundleVersionFile)); err == nil {
			if h := c.homeDir(); h != "" {
				return filepath.Join(h, ".local", "share", "lensdock")
			}
		}
	}
	return ".lensdock"
}
