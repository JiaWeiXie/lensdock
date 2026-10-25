package product

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenBundleValidatesEveryFileItCopies(t *testing.T) {
	root := writeBundle(t, "1.2.3")
	b, err := OpenBundle(root)
	if err != nil || b.Root != root || b.Version != "1.2.3" {
		t.Fatalf("OpenBundle = %+v, %v", b, err)
	}

	if _, err := OpenBundle(t.TempDir()); !errors.Is(err, ErrNotBundle) {
		t.Fatalf("a directory without VERSION: %v", err)
	}

	incomplete := writeBundle(t, "1.0.0")
	if err := os.Remove(filepath.Join(incomplete, "docker", "compose.yaml")); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenBundle(incomplete); err == nil || errors.Is(err, ErrNotBundle) {
		t.Fatalf("an incomplete bundle must be an error, not 'not a bundle': %v", err)
	}

	linked := writeBundle(t, "1.0.0")
	helper := filepath.Join(linked, "bin", "linux-amd64", "lensdock")
	if err := os.Remove(helper); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/bin/sh", helper); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenBundle(linked); err == nil {
		t.Fatal("a symlinked helper was accepted")
	}

	bad := writeBundle(t, "1.0.0")
	writeFile(t, filepath.Join(bad, VersionFile), "not a version!\n", 0o644)
	if _, err := OpenBundle(bad); err == nil {
		t.Fatal("an invalid version was accepted")
	}
}

func TestSnapshotCopiesPrivatelyAndNeverOverwrites(t *testing.T) {
	b, err := OpenBundle(writeBundle(t, "1.2.3"))
	if err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "release")
	if err := b.Snapshot(dest); err != nil {
		t.Fatal(err)
	}
	for _, f := range bundleFiles {
		fi, err := os.Stat(filepath.Join(dest, filepath.FromSlash(f.rel)))
		if err != nil || fi.Mode().Perm() != f.mode {
			t.Errorf("%s: %v %v", f.rel, err, fi)
		}
	}
	if err := b.Snapshot(dest); err == nil {
		t.Fatal("a second snapshot overwrote the first")
	}
}

func TestSnapshotRefusesAForgedBundle(t *testing.T) {
	root := writeBundle(t, "1.2.3")
	if err := (&Bundle{Root: root, Version: "6.6.6"}).Snapshot(filepath.Join(t.TempDir(), "r")); err == nil {
		t.Fatal("a bundle claiming another version was copied")
	}
	if err := os.Remove(filepath.Join(root, "docker", "compose.yaml")); err != nil {
		t.Fatal(err)
	}
	if err := (&Bundle{Root: root, Version: "1.2.3"}).Snapshot(filepath.Join(t.TempDir(), "r")); err == nil {
		t.Fatal("a bundle that is no longer complete was copied")
	}
}
