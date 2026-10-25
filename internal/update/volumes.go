package update

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// VolumeHelper runs short-lived, locked-down containers that read or write one
// named volume. The container has no network, a read-only root file system, no
// capabilities beyond the few each operation needs, and never sees the Docker
// socket. Image must contain GNU tar, findutils and coreutils (every Debian
// based LensDock image does); the runtime image of the current generation is
// used because it is guaranteed to be present locally.
type VolumeHelper struct {
	D     Docker
	Image string
}

const (
	helperSrc = "/src"
	helperDst = "/dst"
)

func (h VolumeHelper) base(interactive bool, caps ...string) []string {
	a := []string{"run", "--rm"}
	if interactive {
		a = append(a, "-i")
	}
	a = append(a,
		"--network", "none",
		"--read-only",
		"--cap-drop", "ALL",
	)
	for _, c := range caps {
		a = append(a, "--cap-add", c)
	}
	return append(a,
		"--security-opt", "no-new-privileges:true",
		"--pids-limit", "128",
		"--memory", "512m",
		"--user", "0:0",
	)
}

func mountArg(volume, target string, readOnly bool) string {
	m := "type=volume,src=" + volume + ",dst=" + target + ",volume-nocopy"
	if readOnly {
		m += ",readonly"
	}
	return m
}

func (h VolumeHelper) check(volume string) error {
	if err := checkDockerName("volume", volume); err != nil {
		return err
	}
	return checkImageRef(h.Image)
}

// run executes entrypoint with args against volume mounted at target.
func (h VolumeHelper) run(ctx context.Context, c Cmd, volume, target string, readOnly bool, caps []string, entry string, args ...string) error {
	if err := h.check(volume); err != nil {
		return err
	}
	a := h.base(c.Stdin != nil, caps...)
	a = append(a, "--mount", mountArg(volume, target, readOnly), "--entrypoint", entry, h.Image)
	a = append(a, args...)
	c.Name, c.Args = h.D.bin(), a
	return h.D.Run.Run(ctx, c)
}

func (h VolumeHelper) capture(ctx context.Context, volume string, caps []string, entry string, args ...string) ([]byte, error) {
	var buf bytes.Buffer
	c := Cmd{Stdout: &limitWriter{w: &buf, max: maxCommandOutput}}
	if err := h.run(ctx, c, volume, helperSrc, true, caps, entry, args...); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// TarOut streams the volume as a tar archive (numeric owners, one file
// system) to w.
func (h VolumeHelper) TarOut(ctx context.Context, volume string, w io.Writer) error {
	c := Cmd{Stdout: w}
	return h.run(ctx, c, volume, helperSrc, true, []string{"DAC_READ_SEARCH"},
		"tar", "--create", "--file", "-", "--sparse", "--numeric-owner", "--one-file-system", "--directory", helperSrc, ".")
}

// TarIn extracts a tar archive from r into the (empty) volume, keeping
// numeric owners, modes and times.
func (h VolumeHelper) TarIn(ctx context.Context, volume string, r io.Reader) error {
	c := Cmd{Stdin: r}
	return h.run(ctx, c, volume, helperDst, false, []string{"CHOWN", "DAC_OVERRIDE", "FOWNER"},
		"tar", "--extract", "--file", "-", "--numeric-owner", "--same-owner", "--preserve-permissions", "--directory", helperDst)
}

// UsedBytes returns the disk usage of the volume.
func (h VolumeHelper) UsedBytes(ctx context.Context, volume string) (int64, error) {
	out, err := h.capture(ctx, volume, []string{"DAC_READ_SEARCH"}, "du", "--summarize", "--one-file-system", "--block-size=1", helperSrc)
	if err != nil {
		return 0, err
	}
	field, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\t")
	n, err := strconv.ParseInt(field, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse du output %q: %w", strings.TrimSpace(string(out)), err)
	}
	return n, nil
}

// FreeBytes returns the free bytes of the file system holding the volume
// (the Docker data root).
func (h VolumeHelper) FreeBytes(ctx context.Context, volume string) (int64, error) {
	out, err := h.capture(ctx, volume, nil, "df", "--block-size=1", "--output=avail", helperSrc)
	if err != nil {
		return 0, err
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) < 2 {
		return 0, fmt.Errorf("unexpected df output %q", strings.TrimSpace(string(out)))
	}
	n, err := strconv.ParseInt(strings.TrimSpace(lines[len(lines)-1]), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse df output: %w", err)
	}
	return n, nil
}

// ReadFile returns a file of the volume (path relative to the volume root),
// at most limit bytes. A missing file yields (nil, false, nil).
func (h VolumeHelper) ReadFile(ctx context.Context, volume, rel string, limit int64) ([]byte, bool, error) {
	if err := validateRelPath(rel); err != nil {
		return nil, false, fmt.Errorf("volume path %q: %w", rel, err)
	}
	probe, err := h.capture(ctx, volume, []string{"DAC_READ_SEARCH"}, "find", helperSrc+"/"+rel, "-maxdepth", "0", "-type", "f", "-printf", "%s")
	if err != nil {
		// find exits 1 when the file does not exist.
		var ee *ExitError
		if errors.As(err, &ee) && ee.Code == 1 {
			return nil, false, nil
		}
		return nil, false, err
	}
	if len(bytes.TrimSpace(probe)) == 0 {
		return nil, false, nil
	}
	if size, err := strconv.ParseInt(string(probe), 10, 64); err == nil && size > limit {
		return nil, false, fmt.Errorf("%s is larger than %d bytes", rel, limit)
	}
	data, err := h.capture(ctx, volume, []string{"DAC_READ_SEARCH"}, "cat", helperSrc+"/"+rel)
	if err != nil {
		return nil, false, err
	}
	return data, true, nil
}

// TopLevelNames lists the entries of the volume root whose name matches the
// find -name glob (for example "Singleton*").
func (h VolumeHelper) TopLevelNames(ctx context.Context, volume, glob string) ([]string, error) {
	out, err := h.capture(ctx, volume, []string{"DAC_READ_SEARCH"}, "find", helperSrc, "-mindepth", "1", "-maxdepth", "1", "-name", glob, "-printf", "%f\\0")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, n := range bytes.Split(out, []byte{0}) {
		if len(n) > 0 {
			names = append(names, string(n))
		}
	}
	sort.Strings(names)
	return names, nil
}

// Fingerprint is a content and metadata digest of a volume tree. Two volumes
// with equal fingerprints have the same entries, modes, owners, link targets
// and file contents (directory sizes and times are ignored).
type Fingerprint struct {
	SHA256   string `json:"sha256"`
	Files    int    `json:"files"`
	Dirs     int    `json:"dirs"`
	Symlinks int    `json:"symlinks"`
	Bytes    int64  `json:"bytes"`
	// JournalFiles lists nonempty SQLite sidecars included in the fingerprint.
	// Persistent journals and WAL files can remain after a clean close.
	JournalFiles []string `json:"journalFiles,omitempty"`
}

// Fingerprint computes the fingerprint of volume.
func (h VolumeHelper) Fingerprint(ctx context.Context, volume string) (Fingerprint, error) {
	caps := []string{"DAC_READ_SEARCH"}
	meta, err := h.capture(ctx, volume, caps, "find", helperSrc, "-xdev", "-printf", `%y\t%m\t%U\t%G\t%s\t%l\t%P\0`)
	if err != nil {
		return Fingerprint{}, fmt.Errorf("fingerprint metadata: %w", err)
	}
	sums, err := h.capture(ctx, volume, caps, "find", helperSrc, "-xdev", "-type", "f", "-exec", "sha256sum", "--zero", "{}", "+")
	if err != nil {
		return Fingerprint{}, fmt.Errorf("fingerprint contents: %w", err)
	}
	return ComputeFingerprint(meta, sums)
}

// ComputeFingerprint derives a Fingerprint from the NUL separated output of
// `find -printf '%y\t%m\t%U\t%G\t%s\t%l\t%P\0'` and `sha256sum --zero`. It is
// separate from the helper so it is testable.
func ComputeFingerprint(meta, sums []byte) (Fingerprint, error) {
	var fp Fingerprint
	var lines []string
	for _, rec := range bytes.Split(meta, []byte{0}) {
		if len(rec) == 0 {
			continue
		}
		fields := strings.SplitN(string(rec), "\t", 7)
		if len(fields) != 7 {
			return Fingerprint{}, fmt.Errorf("malformed find record %q", string(rec))
		}
		typ, size, path := fields[0], fields[4], fields[6]
		switch typ {
		case "f":
			fp.Files++
			n, err := strconv.ParseInt(size, 10, 64)
			if err != nil {
				return Fingerprint{}, fmt.Errorf("malformed size in %q", string(rec))
			}
			fp.Bytes += n
			if (strings.HasSuffix(path, "-wal") || strings.HasSuffix(path, "-journal")) && n > 0 {
				fp.JournalFiles = append(fp.JournalFiles, path)
			}
		case "d":
			fp.Dirs++
			fields[4] = "0"
		case "l":
			fp.Symlinks++
			fields[4] = "0"
		default:
			fields[4] = "0"
		}
		lines = append(lines, "m\t"+strings.Join(fields, "\t"))
	}
	for _, rec := range bytes.Split(sums, []byte{0}) {
		if len(rec) == 0 {
			continue
		}
		sum, name, ok := strings.Cut(string(rec), "  ")
		if !ok || len(sum) != 64 {
			return Fingerprint{}, fmt.Errorf("malformed sha256sum record %q", string(rec))
		}
		lines = append(lines, "h\t"+sum+"\t"+strings.TrimPrefix(name, helperSrc+"/"))
	}
	sort.Strings(lines)
	sort.Strings(fp.JournalFiles)
	h := sha256.New()
	for _, l := range lines {
		h.Write([]byte(l))
		h.Write([]byte{'\n'})
	}
	fp.SHA256 = hex.EncodeToString(h.Sum(nil))
	return fp, nil
}

// Fingerprint comparison ignores JournalFiles, already covered by the digest.
func (f Fingerprint) Equal(o Fingerprint) bool {
	return f.SHA256 == o.SHA256 && f.Files == o.Files && f.Dirs == o.Dirs && f.Symlinks == o.Symlinks && f.Bytes == o.Bytes
}

// uncleanJournals examines a stopped dataset without changing it. Chromium can
// retain WAL files after normal teardown; the complete cold snapshot keeps them
// with their databases. The Gateway explicitly truncates its WAL on close.
func (h VolumeHelper) uncleanJournals(ctx context.Context, volume, kind string, fp Fingerprint) ([]string, error) {
	var hot []string
	for _, path := range fp.JournalFiles {
		var header []byte
		if strings.HasSuffix(path, "-journal") {
			if err := validateRelPath(path); err != nil {
				return nil, err
			}
			var err error
			header, err = h.capture(ctx, volume, []string{"DAC_READ_SEARCH"}, "dd",
				"if="+helperSrc+"/"+path, "bs=8", "count=1", "status=none")
			if err != nil {
				return nil, fmt.Errorf("read journal header %s: %w", path, err)
			}
		}
		if journalNeedsRecovery(kind, path, header) {
			hot = append(hot, path)
		}
	}
	return hot, nil
}

func journalNeedsRecovery(kind, path string, header []byte) bool {
	if strings.HasSuffix(path, "-wal") {
		return kind != KindProfile
	}
	// PERSIST commits by zeroing the header, without truncating the file.
	// Reject a nonzero or incomplete header conservatively.
	if len(header) != 8 {
		return true
	}
	for _, b := range header {
		if b != 0 {
			return true
		}
	}
	return false
}
