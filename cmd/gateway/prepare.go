package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

// prepareConfig is a short-lived deployment step, never the serving process.
// Host UID mappings need not equal container root. Copies retain private modes
// and land in separate root-owned volumes; application containers mount them RO.
func prepareConfig() error {
	if os.Geteuid() != 0 {
		return errors.New("credential preparation must run as container root")
	}
	sets := map[string][]string{
		"/prepared/runtime": {"runtime.crt", "runtime.key", "ca.crt"},
		"/prepared/gateway": {"gateway.json", "gateway.crt", "gateway.key", "ca.crt", "operator-token"},
	}
	for dir, names := range sets {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return err
		}
		if err := os.Chmod(dir, 0700); err != nil {
			return err
		}
		for _, name := range names {
			source := filepath.Join("/source", name)
			f, err := os.OpenFile(source, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
			if err != nil {
				return err
			}
			st, err := f.Stat()
			if err != nil {
				f.Close()
				return err
			}
			if !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 || st.Size() > 1<<20 {
				f.Close()
				return fmt.Errorf("unsafe source credential %s", name)
			}
			data, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
			f.Close()
			if err != nil {
				return err
			}
			if len(data) > 1<<20 {
				return errors.New("credential exceeds limit")
			}
			tmp, err := os.CreateTemp(dir, ".prepare-")
			if err != nil {
				return err
			}
			tmpPath := tmp.Name()
			if err := tmp.Chmod(0600); err != nil {
				tmp.Close()
				os.Remove(tmpPath)
				return err
			}
			_, writeErr := tmp.Write(data)
			syncErr := tmp.Sync()
			closeErr := tmp.Close()
			if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
				os.Remove(tmpPath)
				return err
			}
			if err := os.Rename(tmpPath, filepath.Join(dir, name)); err != nil {
				os.Remove(tmpPath)
				return err
			}
		}
	}
	return nil
}
