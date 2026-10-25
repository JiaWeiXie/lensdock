package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/JiaWeiXie/lensdock/internal/product"
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
func (c *cli) findBundle(explicit string) (*product.Bundle, error) {
	if explicit != "" {
		root, err := filepath.Abs(explicit)
		if err != nil {
			return nil, err
		}
		if root, err = filepath.EvalSymlinks(root); err != nil {
			return nil, fmt.Errorf("--bundle-dir: %w", err)
		}
		b, err := product.OpenBundle(root)
		if err != nil {
			return nil, usageErrorf("--bundle-dir: %v", err)
		}
		return b, nil
	}
	dir, err := c.executableDir()
	if err != nil {
		return nil, nil
	}
	b, err := product.OpenBundle(dir)
	if errors.Is(err, product.ErrNotBundle) {
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
		if _, err := os.Lstat(filepath.Join(dir, product.VersionFile)); err == nil {
			if h := c.homeDir(); h != "" {
				return filepath.Join(h, ".local", "share", "lensdock")
			}
		}
	}
	return ".lensdock"
}
