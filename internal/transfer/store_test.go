package transfer

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var ctx = context.Background()

func newStore(t *testing.T, l Limits) (*DirStore, string) {
	t.Helper()
	dir := t.TempDir()
	return NewDirStore(dir, l), dir
}

func put(t *testing.T, s Store, name, body string, overwrite bool) (Entry, error) {
	t.Helper()
	return s.Put(ctx, name, strings.NewReader(body), int64(len(body)), overwrite)
}

func get(t *testing.T, s Store, area Area, name string) (string, error) {
	t.Helper()
	var buf bytes.Buffer
	err := s.Get(ctx, area, name, func(size int64, _ time.Time) (io.Writer, error) {
		if size < 0 {
			t.Fatalf("negative size %d", size)
		}
		return &buf, nil
	})
	return buf.String(), err
}

func TestValidNameRejectsEveryTraversalAndControlForm(t *testing.T) {
	bad := []string{
		"", ".", "..", "../x", "a/b", "/etc/passwd", `a\b`, `..\x`, ".hidden", ".upload-1.part",
		"a\x00b", "a\nb", "a\tb", "a\x7fb", "x.crdownload",
		"\u202egpj.exe", // bidi override
		"bad\xffutf8",
		strings.Repeat("a", 256),
	}
	for _, n := range bad {
		if err := ValidName(n); !errors.Is(err, ErrInvalidName) {
			t.Errorf("ValidName(%q) = %v, want ErrInvalidName", n, err)
		}
	}
	good := []string{"report.pdf", "測試 檔案.txt", "a", strings.Repeat("a", 255), "with space", "-leading-dash", "emoji🙂.png"}
	for _, n := range good {
		if err := ValidName(n); err != nil {
			t.Errorf("ValidName(%q) = %v, want ok", n, err)
		}
	}
}

func TestPutGetListDeleteRoundTrip(t *testing.T) {
	s, dir := newStore(t, Limits{})
	e, err := put(t, s, "a.txt", "hello", false)
	if err != nil || e.Name != "a.txt" || e.Size != 5 {
		t.Fatalf("Put = %+v, %v", e, err)
	}
	if got, err := get(t, s, AreaUploads, "a.txt"); err != nil || got != "hello" {
		t.Fatalf("Get = %q, %v", got, err)
	}
	// A Chromium download lands in the top-level directory.
	if err := os.WriteFile(filepath.Join(dir, "saved.bin"), []byte("xyz"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Partial downloads and the store's own temp files are invisible.
	for _, n := range []string{"big.iso.crdownload", ".hidden"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("partial"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	l, err := s.List(ctx, AreaDownloads)
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Files) != 1 || l.Files[0].Name != "saved.bin" {
		t.Fatalf("downloads listing = %+v (directories, partial and dot files must be hidden)", l.Files)
	}
	if l.Usage.FileCount != 2 || l.Usage.UsedBytes != 5+3+7+7 {
		t.Fatalf("usage = %+v, want 2 visible files and every byte counted", l.Usage)
	}
	up, err := s.List(ctx, AreaUploads)
	if err != nil || len(up.Files) != 1 || up.Files[0].Name != "a.txt" || up.Area != AreaUploads {
		t.Fatalf("uploads listing = %+v, %v", up, err)
	}
	if err := s.Delete(ctx, AreaUploads, "a.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := get(t, s, AreaUploads, "a.txt"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after Delete = %v", err)
	}
	if err := s.Delete(ctx, AreaUploads, "a.txt"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second Delete = %v", err)
	}
}

func TestListOfAMissingUploadsDirectoryIsEmpty(t *testing.T) {
	s, _ := newStore(t, Limits{})
	l, err := s.List(ctx, AreaUploads)
	if err != nil || len(l.Files) != 0 || l.Files == nil {
		t.Fatalf("List = %+v, %v, want an empty non-nil list", l, err)
	}
	if _, err := s.List(ctx, Area("secrets")); !errors.Is(err, ErrInvalidArea) {
		t.Fatalf("List(unknown area) = %v", err)
	}
}

func TestPutNeverOverwritesUnlessAskedAndIsAtomic(t *testing.T) {
	s, dir := newStore(t, Limits{})
	if _, err := put(t, s, "f", "one", false); err != nil {
		t.Fatal(err)
	}
	if _, err := put(t, s, "f", "two", false); !errors.Is(err, ErrExists) {
		t.Fatalf("second Put = %v, want ErrExists", err)
	}
	if got, _ := get(t, s, AreaUploads, "f"); got != "one" {
		t.Fatalf("content after refused overwrite = %q", got)
	}
	if _, err := put(t, s, "f", "three!", true); err != nil {
		t.Fatalf("overwrite Put: %v", err)
	}
	if got, _ := get(t, s, AreaUploads, "f"); got != "three!" {
		t.Fatalf("content after overwrite = %q", got)
	}
	entries, err := os.ReadDir(filepath.Join(dir, "uploads"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), tempPrefix) {
			t.Fatalf("temporary upload file %q left behind", e.Name())
		}
	}
	fi, err := os.Stat(filepath.Join(dir, "uploads"))
	if err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("uploads dir mode = %v, %v, want 0700", fi.Mode().Perm(), err)
	}
}

func TestPutEnforcesPerFileQuotaAndCountLimits(t *testing.T) {
	s, dir := newStore(t, Limits{QuotaBytes: 100, MaxFileBytes: 60, MaxFiles: 2})
	if _, err := put(t, s, "big", strings.Repeat("x", 61), false); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("over per-file limit = %v, want ErrTooLarge", err)
	}
	if _, err := put(t, s, "a", strings.Repeat("x", 60), false); err != nil {
		t.Fatal(err)
	}
	if _, err := put(t, s, "b", strings.Repeat("x", 41), false); !errors.Is(err, ErrQuota) {
		t.Fatalf("over quota = %v, want ErrQuota", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "uploads", "b")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a refused upload left a file")
	}
	if _, err := put(t, s, "b", strings.Repeat("x", 40), false); err != nil {
		t.Fatalf("exactly at the quota must pass: %v", err)
	}
	if _, err := put(t, s, "c", "", false); !errors.Is(err, ErrTooMany) {
		t.Fatalf("third file = %v, want ErrTooMany", err)
	}
	// Overwriting reuses the old file's bytes in the budget.
	if _, err := put(t, s, "a", strings.Repeat("y", 60), true); err != nil {
		t.Fatalf("same-size overwrite at the quota: %v", err)
	}
	// Chromium's downloads count against the same quota.
	if err := os.WriteFile(filepath.Join(dir, "dl.bin"), []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := put(t, s, "a", strings.Repeat("y", 60), true); !errors.Is(err, ErrQuota) {
		t.Fatalf("overwrite with a download present = %v, want ErrQuota", err)
	}
}

func TestPutVerifiesTheDeclaredSize(t *testing.T) {
	s, dir := newStore(t, Limits{})
	if _, err := s.Put(ctx, "short", strings.NewReader("abc"), 10, false); !errors.Is(err, ErrSizeMismatch) {
		t.Fatalf("short body = %v, want ErrSizeMismatch", err)
	}
	if _, err := s.Put(ctx, "long", strings.NewReader("abcdefghij"), 3, false); !errors.Is(err, ErrSizeMismatch) {
		t.Fatalf("long body = %v, want ErrSizeMismatch", err)
	}
	if _, err := s.Put(ctx, "neg", strings.NewReader(""), -1, false); !errors.Is(err, ErrSizeMismatch) {
		t.Fatalf("negative size = %v", err)
	}
	if _, err := s.Put(ctx, "trunc", io.MultiReader(strings.NewReader("ab"), errReader{io.ErrUnexpectedEOF}), 5, false); !errors.Is(err, ErrSizeMismatch) {
		t.Fatalf("interrupted body = %v, want ErrSizeMismatch", err)
	}
	entries, _ := os.ReadDir(filepath.Join(dir, "uploads"))
	if len(entries) != 0 {
		t.Fatalf("rejected uploads left %d files behind", len(entries))
	}
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

func TestTraversalNamesNeverTouchTheFilesystem(t *testing.T) {
	s, dir := newStore(t, Limits{})
	secret := filepath.Join(filepath.Dir(dir), "outside-secret")
	if err := os.WriteFile(secret, []byte("top secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(secret) })

	for _, name := range []string{"../outside-secret", "../../etc/passwd", "uploads/../../outside-secret", "/etc/passwd", `..\outside-secret`} {
		if _, err := get(t, s, AreaDownloads, name); !errors.Is(err, ErrInvalidName) {
			t.Errorf("Get(%q) = %v, want ErrInvalidName", name, err)
		}
		if err := s.Delete(ctx, AreaDownloads, name); !errors.Is(err, ErrInvalidName) {
			t.Errorf("Delete(%q) = %v, want ErrInvalidName", name, err)
		}
		if _, err := put(t, s, name, "x", true); !errors.Is(err, ErrInvalidName) {
			t.Errorf("Put(%q) = %v, want ErrInvalidName", name, err)
		}
	}
	if b, err := os.ReadFile(secret); err != nil || string(b) != "top secret" {
		t.Fatalf("file outside the directory changed: %q, %v", b, err)
	}
}

func TestSymlinksAreNeverFollowedOrListed(t *testing.T) {
	s, dir := newStore(t, Limits{})
	secret := filepath.Join(filepath.Dir(dir), "outside-secret2")
	if err := os.WriteFile(secret, []byte("top secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(secret) })
	if err := os.Symlink(secret, filepath.Join(dir, "link-out")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "real"), []byte("r"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real", filepath.Join(dir, "link-in")); err != nil {
		t.Fatal(err)
	}

	l, err := s.List(ctx, AreaDownloads)
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Files) != 1 || l.Files[0].Name != "real" {
		t.Fatalf("listing = %+v, symlinks must not be listed", l.Files)
	}
	for _, n := range []string{"link-out", "link-in"} {
		if _, err := get(t, s, AreaDownloads, n); !errors.Is(err, ErrNotFound) {
			t.Errorf("Get(%s) = %v, want ErrNotFound", n, err)
		}
		if err := s.Delete(ctx, AreaDownloads, n); !errors.Is(err, ErrNotFound) {
			t.Errorf("Delete(%s) = %v, want ErrNotFound", n, err)
		}
	}
	if _, err := os.Stat(secret); err != nil {
		t.Fatal("Delete followed a symlink out of the directory")
	}

	// An uploads path replaced by a symlink to elsewhere is refused.
	if err := os.Symlink(filepath.Dir(dir), filepath.Join(dir, "uploads")); err != nil {
		t.Fatal(err)
	}
	if _, err := put(t, s, "x", "y", false); err == nil {
		t.Fatal("Put through a symlinked uploads directory succeeded")
	}
	// An upload over a symlink named like the target never writes through it.
	_ = os.Remove(filepath.Join(dir, "uploads"))
	if err := os.Mkdir(filepath.Join(dir, "uploads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(dir, "uploads", "trap")); err != nil {
		t.Fatal(err)
	}
	if _, err := put(t, s, "trap", "overwritten", true); !errors.Is(err, ErrExists) {
		t.Fatalf("Put over a symlink = %v, want ErrExists", err)
	}
	if b, _ := os.ReadFile(secret); string(b) != "top secret" {
		t.Fatalf("an upload wrote through a symlink: %q", b)
	}
}

func TestGetStreamsExactlyTheFileAndReportsSize(t *testing.T) {
	s, dir := newStore(t, Limits{})
	content := strings.Repeat("0123456789", 10000)
	if err := os.WriteFile(filepath.Join(dir, "big.bin"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	var gotSize int64
	var buf bytes.Buffer
	err := s.Get(ctx, AreaDownloads, "big.bin", func(size int64, mt time.Time) (io.Writer, error) {
		gotSize = size
		if mt.IsZero() || mt.Location() != time.UTC {
			t.Errorf("modTime = %v, want a UTC time", mt)
		}
		return &buf, nil
	})
	if err != nil || gotSize != int64(len(content)) || buf.String() != content {
		t.Fatalf("Get err=%v size=%d len=%d", err, gotSize, buf.Len())
	}
	wantErr := errors.New("client went away")
	if err := s.Get(ctx, AreaDownloads, "big.bin", func(int64, time.Time) (io.Writer, error) { return nil, wantErr }); !errors.Is(err, wantErr) {
		t.Fatalf("begin error = %v, want it returned", err)
	}
	if _, err := get(t, s, AreaDownloads, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get(missing) = %v", err)
	}
	if _, err := get(t, s, AreaUploads, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get(missing upload) = %v", err)
	}
}

func TestDirectoriesAreNotFiles(t *testing.T) {
	s, dir := newStore(t, Limits{})
	if err := os.Mkdir(filepath.Join(dir, "somedir"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := get(t, s, AreaDownloads, "somedir"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get(directory) = %v", err)
	}
	if err := s.Delete(ctx, AreaDownloads, "somedir"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete(directory) = %v", err)
	}
	if _, err := put(t, s, "ok", "1", false); err != nil {
		t.Fatal(err)
	}
	// The uploads directory itself is addressable only through its area.
	if _, err := get(t, s, AreaDownloads, "uploads"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get(downloads/uploads) = %v", err)
	}
}
