package auth

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, path string, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatal(err)
	}
	// WriteFile is subject to umask and does not alter existing modes.
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func TestReadPrivateFile(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good")
	writeFile(t, good, []byte("secret"), 0o600)

	got, err := ReadPrivateFile(good, 6)
	if err != nil || !bytes.Equal(got, []byte("secret")) {
		t.Fatalf("ReadPrivateFile = %q, %v; want secret", got, err)
	}
	if _, err := ReadPrivateFile(good, 5); !errors.Is(err, ErrNotPrivate) {
		t.Fatalf("file one byte over the limit: err = %v, want ErrNotPrivate", err)
	}

	for _, mode := range []os.FileMode{0o640, 0o604, 0o644, 0o660, 0o606} {
		p := filepath.Join(dir, "mode-"+mode.String())
		writeFile(t, p, []byte("x"), mode)
		if _, err := ReadPrivateFile(p, 10); !errors.Is(err, ErrNotPrivate) {
			t.Errorf("mode %v: err = %v, want ErrNotPrivate", mode, err)
		}
	}

	link := filepath.Join(dir, "link")
	if err := os.Symlink(good, link); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPrivateFile(link, 10); !errors.Is(err, ErrNotPrivate) {
		t.Errorf("symlink: err = %v, want ErrNotPrivate", err)
	}

	if _, err := ReadPrivateFile(dir, 10); !errors.Is(err, ErrNotPrivate) {
		t.Errorf("directory: err = %v, want ErrNotPrivate", err)
	}
	if _, err := ReadPrivateFile(filepath.Join(dir, "missing"), 10); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing file: err = %v, want ErrNotExist", err)
	}
}

func TestWritePrivateFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	if err := WritePrivateFile(path, []byte("ldo_abc")); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", fi.Mode().Perm())
	}
	// What we wrote must pass our own reader.
	got, err := ReadPrivateFile(path, 64)
	if err != nil || string(got) != "ldo_abc" {
		t.Fatalf("round trip = %q, %v", got, err)
	}

	// Never overwrite.
	if err := WritePrivateFile(path, []byte("other")); !errors.Is(err, os.ErrExist) {
		t.Fatalf("overwrite: err = %v, want ErrExist", err)
	}
	if got, _ := os.ReadFile(path); string(got) != "ldo_abc" {
		t.Fatalf("failed overwrite changed the file to %q", got)
	}

	// Never follow a symlink, even a dangling one.
	target := filepath.Join(dir, "target")
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := WritePrivateFile(link, []byte("x")); err == nil {
		t.Fatal("write through symlink succeeded")
	}
	if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("symlink target was created: %v", err)
	}

	// Parent must exist; no implicit MkdirAll.
	if err := WritePrivateFile(filepath.Join(dir, "no", "such", "dir", "f"), []byte("x")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing parent: err = %v, want ErrNotExist", err)
	}
}
