// Package transfer is the runtime's controlled file transfer area: a flat
// downloads directory (written by Chromium) and a flat uploads subdirectory,
// both inside one quota. Names are single path segments, every filesystem
// access goes through os.Root so no name can leave the directory, and writes
// are bounded by per-file, total-size and file-count limits.
//
// DirStore does the work in-process. The runtime's data directories belong to
// the browser user, so the API process reaches them through HelperStore,
// which runs the same code in a short-lived `__files` helper process as that
// user.
package transfer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/JiaWeiXie/lensdock/internal/auth"
)

// Area names one of the two transfer directories.
type Area string

const (
	// AreaDownloads is the top level of the data directory, where Chromium
	// saves downloads.
	AreaDownloads Area = "downloads"
	// AreaUploads is the uploads subdirectory, where operators place files for
	// the browser to pick.
	AreaUploads Area = "uploads"

	uploadsDir    = "uploads"
	maxNameBytes  = 255
	partialSuffix = ".crdownload"
	tempPrefix    = ".upload-"
	tempSuffix    = ".part"
	dirMode       = 0o700
	fileMode      = 0o600
)

// Limits optionally bound what the transfer area may hold. The zero value of
// every field means no limit.
type Limits struct {
	// QuotaBytes caps the bytes of every regular file in both areas.
	QuotaBytes int64 `json:"quotaBytes"`
	// MaxFileBytes caps one uploaded file.
	MaxFileBytes int64 `json:"maxFileBytes"`
	// MaxFiles caps the number of visible files in both areas.
	MaxFiles int `json:"maxFiles"`
}

// Entry is one visible file.
type Entry struct {
	Name       string    `json:"name"`
	Size       int64     `json:"size"`
	ModifiedAt time.Time `json:"modifiedAt"`
}

// Usage is the transfer area's consumption next to its configured limits;
// a zero QuotaBytes or MaxFiles means no limit is configured.
type Usage struct {
	UsedBytes  int64 `json:"usedBytes"`
	QuotaBytes int64 `json:"quotaBytes"`
	FileCount  int   `json:"fileCount"`
	MaxFiles   int   `json:"maxFiles"`
}

// Listing is the visible files of one area and the overall usage.
type Listing struct {
	Area  Area    `json:"area"`
	Files []Entry `json:"files"`
	Usage Usage   `json:"usage"`
}

// Errors returned by Store implementations.
var (
	ErrInvalidName  = errors.New("transfer: invalid file name")
	ErrInvalidArea  = errors.New("transfer: invalid area")
	ErrNotFound     = errors.New("transfer: file not found")
	ErrExists       = errors.New("transfer: file already exists")
	ErrQuota        = errors.New("transfer: quota exceeded")
	ErrTooLarge     = errors.New("transfer: file larger than the per-file limit")
	ErrTooMany      = errors.New("transfer: too many files")
	ErrSizeMismatch = errors.New("transfer: body length differs from the declared size")
)

// Store is the transfer operations the runtime API needs.
type Store interface {
	// List returns the visible files of area and the overall usage.
	List(ctx context.Context, area Area) (Listing, error)
	// Get streams a file. begin is called once with the size and
	// modification time before any byte is written and returns the sink.
	Get(ctx context.Context, area Area, name string, begin func(size int64, modTime time.Time) (io.Writer, error)) error
	// Put stores exactly size bytes from body as uploads/name. Without
	// overwrite an existing name is ErrExists.
	Put(ctx context.Context, name string, body io.Reader, size int64, overwrite bool) (Entry, error)
	// Delete removes a file.
	Delete(ctx context.Context, area Area, name string) error
}

// ValidArea reports whether a is a known area.
func ValidArea(a Area) bool { return a == AreaDownloads || a == AreaUploads }

// ValidName checks that name is a safe single path segment: 1 to 255 bytes of
// valid UTF-8, no separators, no control or format characters, not "." or ".."
// and no leading dot (dot files are the store's own temporary files).
func ValidName(name string) error {
	switch {
	case name == "" || len(name) > maxNameBytes:
		return ErrInvalidName
	case !utf8.ValidString(name):
		return ErrInvalidName
	case name == "." || name == ".." || name[0] == '.':
		return ErrInvalidName
	case strings.HasSuffix(name, partialSuffix):
		return ErrInvalidName
	}
	for _, r := range name {
		if r == '/' || r == '\\' || r == 0 || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return ErrInvalidName
		}
	}
	return nil
}

func visible(name string) bool {
	return name != "" && name[0] != '.' && !strings.HasSuffix(name, partialSuffix)
}

// DirStore implements Store on a directory.
type DirStore struct {
	dir    string
	limits Limits
}

// NewDirStore returns a Store over dir. A zero limit field means no limit.
func NewDirStore(dir string, limits Limits) *DirStore {
	return &DirStore{dir: dir, limits: limits}
}

// areaRoot opens the root of area under base. The uploads directory is
// created (private to its owner) on first use and must be a real directory.
func areaRoot(base *os.Root, area Area, create bool) (*os.Root, error) {
	switch area {
	case AreaDownloads:
		return base.OpenRoot(".")
	case AreaUploads:
		if create {
			if err := base.Mkdir(uploadsDir, dirMode); err != nil && !errors.Is(err, fs.ErrExist) {
				return nil, err
			}
		}
		fi, err := base.Lstat(uploadsDir)
		if err != nil {
			return nil, err
		}
		if !fi.IsDir() {
			return nil, fmt.Errorf("transfer: %s is not a directory", uploadsDir)
		}
		return base.OpenRoot(uploadsDir)
	}
	return nil, ErrInvalidArea
}

// scan lists the regular files of r.
func scan(r *os.Root) ([]fs.FileInfo, error) {
	d, err := r.Open(".")
	if err != nil {
		return nil, err
	}
	defer d.Close()
	entries, err := d.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	out := make([]fs.FileInfo, 0, len(entries))
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue // vanished between ReadDir and Info
		}
		out = append(out, info)
	}
	return out, nil
}

func (s *DirStore) usage(base *os.Root) (Usage, error) {
	u := Usage{QuotaBytes: s.limits.QuotaBytes, MaxFiles: s.limits.MaxFiles}
	add := func(infos []fs.FileInfo) {
		for _, fi := range infos {
			u.UsedBytes += fi.Size()
			if visible(fi.Name()) {
				u.FileCount++
			}
		}
	}
	top, err := scan(base)
	if err != nil {
		return Usage{}, err
	}
	add(top)
	up, err := areaRoot(base, AreaUploads, false)
	switch {
	case err == nil:
		defer up.Close()
		infos, err := scan(up)
		if err != nil {
			return Usage{}, err
		}
		add(infos)
	case errors.Is(err, fs.ErrNotExist):
	default:
		return Usage{}, err
	}
	return u, nil
}

// List implements Store.
func (s *DirStore) List(_ context.Context, area Area) (Listing, error) {
	if !ValidArea(area) {
		return Listing{}, ErrInvalidArea
	}
	base, err := os.OpenRoot(s.dir)
	if err != nil {
		return Listing{}, err
	}
	defer base.Close()
	out := Listing{Area: area, Files: []Entry{}}
	if out.Usage, err = s.usage(base); err != nil {
		return Listing{}, err
	}
	r, err := areaRoot(base, area, false)
	if errors.Is(err, fs.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return Listing{}, err
	}
	defer r.Close()
	infos, err := scan(r)
	if err != nil {
		return Listing{}, err
	}
	for _, fi := range infos {
		if !visible(fi.Name()) {
			continue
		}
		out.Files = append(out.Files, Entry{Name: fi.Name(), Size: fi.Size(), ModifiedAt: fi.ModTime().UTC()})
	}
	return out, nil
}

// Get implements Store.
func (s *DirStore) Get(_ context.Context, area Area, name string, begin func(int64, time.Time) (io.Writer, error)) error {
	if !ValidArea(area) {
		return ErrInvalidArea
	}
	if err := ValidName(name); err != nil {
		return err
	}
	base, err := os.OpenRoot(s.dir)
	if err != nil {
		return err
	}
	defer base.Close()
	r, err := areaRoot(base, area, false)
	if errors.Is(err, fs.ErrNotExist) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	defer r.Close()
	li, err := r.Lstat(name)
	if err != nil {
		return notFound(err)
	}
	if !li.Mode().IsRegular() {
		return ErrNotFound
	}
	f, err := r.Open(name)
	if err != nil {
		return notFound(err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() || !os.SameFile(li, fi) {
		return ErrNotFound
	}
	w, err := begin(fi.Size(), fi.ModTime().UTC())
	if err != nil {
		return err
	}
	n, err := io.Copy(w, io.LimitReader(f, fi.Size()))
	if err != nil {
		return err
	}
	if n != fi.Size() {
		return fmt.Errorf("transfer: file shrank while streaming (%d of %d bytes)", n, fi.Size())
	}
	return nil
}

func notFound(err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return ErrNotFound
	}
	return err
}

// Put implements Store.
func (s *DirStore) Put(_ context.Context, name string, body io.Reader, size int64, overwrite bool) (Entry, error) {
	if err := ValidName(name); err != nil {
		return Entry{}, err
	}
	if size < 0 {
		return Entry{}, ErrSizeMismatch
	}
	if s.limits.MaxFileBytes > 0 && size > s.limits.MaxFileBytes {
		return Entry{}, ErrTooLarge
	}
	base, err := os.OpenRoot(s.dir)
	if err != nil {
		return Entry{}, err
	}
	defer base.Close()
	up, err := areaRoot(base, AreaUploads, true)
	if err != nil {
		return Entry{}, err
	}
	defer up.Close()

	u, err := s.usage(base)
	if err != nil {
		return Entry{}, err
	}
	used, count := u.UsedBytes, u.FileCount
	existing, err := up.Lstat(name)
	exists := err == nil
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return Entry{}, err
	}
	if exists {
		if !overwrite || !existing.Mode().IsRegular() {
			return Entry{}, ErrExists
		}
		used -= existing.Size()
	} else {
		count++
	}
	if s.limits.QuotaBytes > 0 && used+size > s.limits.QuotaBytes {
		return Entry{}, ErrQuota
	}
	if s.limits.MaxFiles > 0 && count > s.limits.MaxFiles {
		return Entry{}, ErrTooMany
	}

	suffix, err := auth.RandomHex(8)
	if err != nil {
		return Entry{}, err
	}
	tmp := tempPrefix + suffix + tempSuffix
	f, err := up.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fileMode)
	if err != nil {
		return Entry{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = up.Remove(tmp)
		}
	}()
	n, err := io.Copy(f, io.LimitReader(body, size+1))
	if err != nil {
		_ = f.Close()
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return Entry{}, ErrSizeMismatch
		}
		return Entry{}, err
	}
	if n != size {
		_ = f.Close()
		return Entry{}, ErrSizeMismatch
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return Entry{}, err
	}
	if err := f.Close(); err != nil {
		return Entry{}, err
	}
	if overwrite && exists {
		if err := up.Rename(tmp, name); err != nil {
			return Entry{}, err
		}
		committed = true
	} else {
		// Link fails with EEXIST instead of replacing a file another
		// upload created in the meantime.
		if err := up.Link(tmp, name); err != nil {
			if errors.Is(err, fs.ErrExist) {
				return Entry{}, ErrExists
			}
			return Entry{}, err
		}
		_ = up.Remove(tmp)
		committed = true
	}
	fi, err := up.Lstat(name)
	if err != nil {
		return Entry{}, err
	}
	return Entry{Name: name, Size: fi.Size(), ModifiedAt: fi.ModTime().UTC()}, nil
}

// Delete implements Store.
func (s *DirStore) Delete(_ context.Context, area Area, name string) error {
	if !ValidArea(area) {
		return ErrInvalidArea
	}
	if err := ValidName(name); err != nil {
		return err
	}
	base, err := os.OpenRoot(s.dir)
	if err != nil {
		return err
	}
	defer base.Close()
	r, err := areaRoot(base, area, false)
	if errors.Is(err, fs.ErrNotExist) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	defer r.Close()
	fi, err := r.Lstat(name)
	if err != nil {
		return notFound(err)
	}
	if !fi.Mode().IsRegular() {
		return ErrNotFound
	}
	return notFound(r.Remove(name))
}
