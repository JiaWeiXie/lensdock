// Command bundle packs and verifies the LensDock GitHub release assets: four
// host archives, SHA256SUMS and install.sh.
//
//	bundle pack   --version V --source-root DIR --build-dir DIR --out DIR --installer FILE --epoch SECONDS
//	bundle verify --version V DIR
//
// pack never builds anything: --build-dir holds <goos>-<goarch>/lensdock for
// darwin and linux on amd64 and arm64, compiled by tools/release/package.sh.
// verify re-reads the finished assets and checks the shared archive contract
// (README.md#develop-and-release): the host CLI as lensdock, both Linux helper
// CLIs under bin/linux-<arch>/, VERSION, docker/compose.yaml and the Chromium
// seccomp profile, all genuine binaries for the platform they claim.
package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"debug/buildinfo"
	"debug/elf"
	"debug/macho"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	binaryName    = "lensdock"
	mainPackage   = "github.com/JiaWeiXie/lensdock/cmd/lensdock"
	installerName = "install.sh"
	sumsName      = "SHA256SUMS"
)

var versionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z][0-9A-Za-z.-]*)?$`)

type target struct{ os, arch string }

// targets get one archive each. Their Linux builds are also the helper CLIs
// that run inside containers, so every archive carries both.
var targets = []target{{"darwin", "amd64"}, {"darwin", "arm64"}, {"linux", "amd64"}, {"linux", "arm64"}}

func (t target) archiveName(version string) string {
	return fmt.Sprintf("lensdock_%s_%s_%s.tar.gz", version, t.os, t.arch)
}

func helperPath(arch string) string { return path.Join("bin", "linux-"+arch, binaryName) }

// Files copied from the source tree at packaging time: the Compose bundle plus every
// document the READMEs link to, so that the relative links also work offline.
var requiredFiles = []string{
	"README.md",
	"README.zh-TW.md",
	"apps/web/README.md",
	"docker/browser/README.md",
	"docker/browser/seccomp/chromium.json",
	"docker/compose.yaml",
	"docs/AGENT-INTEGRATION.md",
	"docs/REFERENCES.md",
	"docs/UPDATE-IMPLEMENTATION.md",
	"docs/UPDATE-RUNBOOK.md",
}

func main() {
	if len(os.Args) < 2 || (os.Args[1] != "pack" && os.Args[1] != "verify") {
		fmt.Fprintln(os.Stderr, "usage: bundle pack|verify [flags]")
		os.Exit(2)
	}
	fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	version := fs.String("version", "", "release version without a leading v")
	var o packOptions
	var epoch int64
	if os.Args[1] == "pack" {
		fs.StringVar(&o.sourceRoot, "source-root", ".", "repository root")
		fs.StringVar(&o.buildDir, "build-dir", "", "directory holding <goos>-<goarch>/lensdock")
		fs.StringVar(&o.out, "out", "", "output directory; must be empty or absent")
		fs.StringVar(&o.installer, "installer", "", "install.sh to publish")
		fs.Int64Var(&epoch, "epoch", 0, "modification time of every archive entry, Unix seconds")
	}
	_ = fs.Parse(os.Args[2:])
	if !versionPattern.MatchString(*version) {
		fatal(fmt.Errorf("--version %q must look like 1.2.3", *version))
	}
	var err error
	if os.Args[1] == "pack" {
		o.version, o.epoch = *version, time.Unix(epoch, 0).UTC()
		err = pack(o)
	} else if fs.NArg() != 1 {
		err = errors.New("verify takes exactly one release directory")
	} else {
		err = verify(fs.Arg(0), *version)
	}
	if err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "bundle: %v\n", err)
	os.Exit(1)
}

type packOptions struct {
	version, sourceRoot, buildDir, out, installer string

	epoch time.Time
}

type entry struct {
	name string
	mode int64
	data []byte
}

func pack(o packOptions) error {
	var extras []entry
	for _, rel := range requiredFiles {
		data, err := os.ReadFile(filepath.Join(o.sourceRoot, filepath.FromSlash(rel)))
		if err != nil {
			return err
		}
		extras = append(extras, entry{rel, 0o644, data})
	}
	binaries := map[target][]byte{}
	for _, t := range targets {
		data, err := os.ReadFile(filepath.Join(o.buildDir, t.os+"-"+t.arch, binaryName))
		if err != nil {
			return err
		}
		binaries[t] = data
	}
	installer, err := os.ReadFile(o.installer)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(o.out, 0o755); err != nil {
		return err
	}
	if existing, err := os.ReadDir(o.out); err != nil || len(existing) != 0 {
		return fmt.Errorf("output directory %s must be empty (%v)", o.out, err)
	}
	if err := os.WriteFile(filepath.Join(o.out, installerName), installer, 0o755); err != nil {
		return err
	}
	for _, t := range targets {
		entries := append([]entry{
			{"VERSION", 0o644, []byte(o.version + "\n")},
			{binaryName, 0o755, binaries[t]},
			{helperPath("amd64"), 0o755, binaries[target{"linux", "amd64"}]},
			{helperPath("arm64"), 0o755, binaries[target{"linux", "arm64"}]},
		}, extras...)
		if err := writeArchive(filepath.Join(o.out, t.archiveName(o.version)), entries, o.epoch); err != nil {
			return err
		}
	}
	return writeSums(o.out)
}

// writeArchive writes a reproducible tar.gz: sorted entries, fixed mtime,
// root ownership and no gzip file name or timestamp.
func writeArchive(name string, entries []entry, epoch time.Time) error {
	sort.Slice(entries, func(i, j int) bool { return entries[i].name < entries[j].name })
	f, err := os.Create(name)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewWriterLevel(f, gzip.BestCompression)
	if err != nil {
		return err
	}
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Mode: e.mode, Size: int64(len(e.data)), ModTime: epoch, Format: tar.FormatUSTAR}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if _, err := tw.Write(e.data); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}
	return f.Close()
}

// writeSums lists every file of dir in GNU sha256sum format.
func writeSums(dir string) error {
	listing, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var sums strings.Builder
	for _, e := range listing { // ReadDir is sorted by name
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return err
		}
		fmt.Fprintf(&sums, "%x  %s\n", sha256.Sum256(data), e.Name())
	}
	fmt.Print(sums.String())
	return os.WriteFile(filepath.Join(dir, sumsName), []byte(sums.String()), 0o644)
}

func verify(dir, version string) error {
	want := []string{installerName}
	for _, t := range targets {
		want = append(want, t.archiveName(version))
	}
	sums, err := readSums(dir)
	if err != nil {
		return err
	}
	if len(sums) != len(want) {
		return fmt.Errorf("%s lists %d files, want %d", sumsName, len(sums), len(want))
	}
	for _, name := range want {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return err
		}
		if got := fmt.Sprintf("%x", sha256.Sum256(data)); got != sums[name] {
			return fmt.Errorf("%s: SHA-256 %s does not match %s (%s)", name, got, sumsName, sums[name])
		}
	}

	helpers := map[string][]byte{}
	for _, t := range targets {
		name := t.archiveName(version)
		files, err := readArchive(filepath.Join(dir, name))
		if err == nil {
			err = checkArchive(files, t, version, helpers)
		}
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	fmt.Printf("verified %d archives, %s and %s\n", len(targets), installerName, sumsName)
	return nil
}

var sumLine = regexp.MustCompile(`^([0-9a-f]{64})  (\S+)$`)

func readSums(dir string) (map[string]string, error) {
	b, err := os.ReadFile(filepath.Join(dir, sumsName))
	if err != nil {
		return nil, err
	}
	sums := map[string]string{}
	for i, line := range strings.Split(strings.TrimSuffix(string(b), "\n"), "\n") {
		m := sumLine.FindStringSubmatch(line)
		if m == nil {
			return nil, fmt.Errorf("%s line %d is not '<sha256>  <file>'", sumsName, i+1)
		}
		sums[m[2]] = m[1]
	}
	return sums, nil
}

type file struct {
	mode int64
	data []byte
}

func readArchive(name string) (map[string]file, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	files := map[string]file{}
	for tr := tar.NewReader(gz); ; {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return files, nil
		}
		if err != nil {
			return nil, err
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return nil, err
		}
		files[h.Name] = file{h.Mode, data}
	}
}

// checkArchive verifies one archive. helpers accumulates the Linux helper
// binaries so all four archives are proven to ship identical ones.
func checkArchive(files map[string]file, t target, version string, helpers map[string][]byte) error {
	for _, name := range append([]string{"VERSION", binaryName, helperPath("amd64"), helperPath("arm64")}, requiredFiles...) {
		if _, ok := files[name]; !ok {
			return fmt.Errorf("missing %s", name)
		}
	}
	if got := string(files["VERSION"].data); got != version+"\n" {
		return fmt.Errorf("VERSION is %q, want %q", got, version+"\n")
	}
	binaries := map[string]target{binaryName: t, helperPath("amd64"): {"linux", "amd64"}, helperPath("arm64"): {"linux", "arm64"}}
	for name, bt := range binaries {
		if files[name].mode != 0o755 {
			return fmt.Errorf("%s has mode %o, want 755", name, files[name].mode)
		}
		if err := checkBinary(files[name].data, bt); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	for _, arch := range []string{"amd64", "arm64"} {
		data := files[helperPath(arch)].data
		if prev, ok := helpers[arch]; ok && !bytes.Equal(prev, data) {
			return fmt.Errorf("%s differs from the other archives", helperPath(arch))
		}
		helpers[arch] = data
		if t == (target{"linux", arch}) && !bytes.Equal(files[binaryName].data, data) {
			return fmt.Errorf("%s differs from its own %s", binaryName, helperPath(arch))
		}
	}
	return nil
}

// checkBinary checks the CLI package, executable format and CPU. Linux helpers
// must be static because they run in minimal containers. package.sh executes
// the native CLI to verify version stamping; trimpath omits ldflags metadata.
func checkBinary(data []byte, t target) error {
	switch t.os {
	case "linux":
		f, err := elf.NewFile(bytes.NewReader(data))
		if err != nil {
			return fmt.Errorf("not an ELF file: %w", err)
		}
		defer f.Close()
		machine := map[string]elf.Machine{"amd64": elf.EM_X86_64, "arm64": elf.EM_AARCH64}[t.arch]
		if f.Class != elf.ELFCLASS64 || f.Machine != machine {
			return fmt.Errorf("ELF %v %v, want linux/%s", f.Class, f.Machine, t.arch)
		}
		for _, p := range f.Progs {
			if p.Type == elf.PT_INTERP {
				return errors.New("dynamically linked; the Linux CLI must be static")
			}
		}
	case "darwin":
		f, err := macho.NewFile(bytes.NewReader(data))
		if err != nil {
			return fmt.Errorf("not a Mach-O file: %w", err)
		}
		defer f.Close()
		cpu := map[string]macho.Cpu{"amd64": macho.CpuAmd64, "arm64": macho.CpuArm64}[t.arch]
		if f.Cpu != cpu {
			return fmt.Errorf("Mach-O %v, want darwin/%s", f.Cpu, t.arch)
		}
	}
	info, err := buildinfo.Read(bytes.NewReader(data))
	if err != nil {
		return err
	}
	if info.Path != mainPackage {
		return fmt.Errorf("main package %q, want %q", info.Path, mainPackage)
	}
	return nil
}
