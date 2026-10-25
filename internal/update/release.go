package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/JiaWeiXie/lensdock/internal/auth"
)

// DevManifestOptions are the inputs of GenerateDevManifest. It generates real
// local evidence from the inspected images; it never fabricates provenance,
// a vulnerability scan, a test pass or a signature. Therefore its manifest is
// on the dev channel and those fields are absent until actual tool reports are
// attached.
type DevManifestOptions struct {
	OutputDir      string
	ReleaseID      string
	RuntimeImage   string
	GatewayImage   string
	LinuxCLI       map[string]string
	GitCommit      string
	Dirty          bool
	AgentBrowser   string
	NoVNC          string
	GatewayVersion string
	CLIVersion     string
	GatewayDB      int
	MinGatewayDB   int
	Architectures  []string // defaults to the image architecture
	ObservedAt     time.Time
}

// SPDX structures emitted by the dev generator. This is an actual SPDX 2.3
// package SBOM of the dpkg database captured in the image and the CLI binary,
// not an attestation.
type spdxDocument struct {
	SPDXVersion       string        `json:"spdxVersion"`
	DataLicense       string        `json:"dataLicense"`
	SPDXID            string        `json:"SPDXID"`
	Name              string        `json:"name"`
	DocumentNamespace string        `json:"documentNamespace"`
	CreationInfo      spdxCreation  `json:"creationInfo"`
	Packages          []spdxPackage `json:"packages"`
	Relationships     []spdxRel     `json:"relationships"`
}

type spdxCreation struct {
	Created  string   `json:"created"`
	Creators []string `json:"creators"`
}

type spdxPackage struct {
	Name             string `json:"name"`
	SPDXID           string `json:"SPDXID"`
	VersionInfo      string `json:"versionInfo"`
	DownloadLocation string `json:"downloadLocation"`
	FilesAnalyzed    bool   `json:"filesAnalyzed"`
	LicenseConcluded string `json:"licenseConcluded"`
	LicenseDeclared  string `json:"licenseDeclared"`
	Supplier         string `json:"supplier,omitempty"`
	PrimaryPurpose   string `json:"primaryPackagePurpose,omitempty"`
}

type spdxRel struct {
	SPDXElementID      string `json:"spdxElementId"`
	RelationshipType   string `json:"relationshipType"`
	RelatedSPDXElement string `json:"relatedSpdxElement"`
}

func spdxID(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return "SPDXRef-" + strings.Trim(b.String(), "-")
}

func packagesFromTSV(data []byte) ([]Package, error) {
	var out []Package
	for n, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) != 3 || fields[0] == "" || fields[1] == "" || fields[2] == "" {
			return nil, fmt.Errorf("dpkg manifest line %d does not have package, version and architecture", n+1)
		}
		out = append(out, Package{Name: fields[0], Version: fields[1], Arch: fields[2]})
	}
	if len(out) == 0 {
		return nil, errors.New("dpkg manifest has no packages")
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Arch != out[j].Arch {
			return out[i].Arch < out[j].Arch
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

func spdxForImage(name, digest string, packages []Package, created time.Time) spdxDocument {
	doc := spdxDocument{
		SPDXVersion: "SPDX-2.3", DataLicense: "CC0-1.0", SPDXID: "SPDXRef-DOCUMENT", Name: name,
		DocumentNamespace: "https://lensdock.local/spdx/" + strings.TrimPrefix(digest, "sha256:"),
		CreationInfo:      spdxCreation{Created: created.UTC().Format(time.RFC3339), Creators: []string{"Tool: lensdock-release-dev"}},
	}
	for _, p := range packages {
		id := spdxID("Package-" + p.Name + "-" + p.Arch)
		doc.Packages = append(doc.Packages, spdxPackage{
			Name: p.Name, SPDXID: id, VersionInfo: p.Version, DownloadLocation: "NOASSERTION",
			FilesAnalyzed: false, LicenseConcluded: "NOASSERTION", LicenseDeclared: "NOASSERTION", Supplier: "Organization: Debian", PrimaryPurpose: "APPLICATION",
		})
		doc.Relationships = append(doc.Relationships, spdxRel{SPDXElementID: "SPDXRef-DOCUMENT", RelationshipType: "DESCRIBES", RelatedSPDXElement: id})
	}
	return doc
}

func imageCat(ctx context.Context, d Docker, image, path string) ([]byte, error) {
	if err := checkImageRef(image); err != nil {
		return nil, err
	}
	return d.out(ctx, "run", "--rm", "--network", "none", "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges:true", "--entrypoint", "cat", image, path)
}

func imageSources(ctx context.Context, d Docker, image string) []string {
	// Debian trixie uses deb822 sources. This evidence is informative for a
	// dev manifest; a release pipeline supplies an APT snapshot report.
	data, err := imageCat(ctx, d, image, "/etc/apt/sources.list.d/debian.sources")
	if err != nil {
		return []string{"unavailable-in-dev-image"}
	}
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "URIs:") {
			out = append(out, strings.Fields(strings.TrimPrefix(line, "URIs:"))...)
		}
	}
	if len(out) == 0 {
		out = []string{"unavailable-in-dev-image"}
	}
	return out
}

func writeAttachment(dir, rel string, v any) (Attachment, error) {
	path := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return Attachment{}, err
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return Attachment{}, err
	}
	data = append(data, '\n')
	if err := writeFileAtomic(path, data, 0o644); err != nil {
		return Attachment{}, err
	}
	return Attachment{Path: rel, SHA256: sha256Hex(data), Size: int64(len(data))}, nil
}

// GenerateDevManifest inspects local images and writes a complete dev manifest
// plus SPDX package SBOMs to a new output directory. It refuses an existing
// output directory, so evidence is never silently overwritten.
func GenerateDevManifest(ctx context.Context, d Docker, o DevManifestOptions) (*Manifest, error) {
	if !releaseIDPattern.MatchString(o.ReleaseID) {
		return nil, fmt.Errorf("release id %q must match %s", o.ReleaseID, releaseIDPattern)
	}
	if !commitPattern.MatchString(o.GitCommit) && !(o.Dirty && o.GitCommit == "") {
		return nil, errors.New("git commit must be exactly 40 lowercase hex characters, or omitted for explicitly uncommitted development source")
	}
	if o.OutputDir == "" {
		return nil, errors.New("output directory is required")
	}
	if err := os.Mkdir(o.OutputDir, 0o755); err != nil {
		return nil, fmt.Errorf("create release directory: %w", err)
	}
	removeOnError := true
	defer func() {
		if removeOnError {
			_ = os.RemoveAll(o.OutputDir)
		}
	}()
	if o.ObservedAt.IsZero() {
		o.ObservedAt = time.Now().UTC()
	}
	m := &Manifest{
		SchemaVersion: ManifestSchemaVersion, ReleaseID: o.ReleaseID, Channel: ChannelDev, CreatedAt: o.ObservedAt.UTC(),
		Source: Source{GitCommit: o.GitCommit, Dirty: o.Dirty}, Images: map[string]ArchImages{}, CLI: map[string]CLIBinary{},
		Components: Components{NoVNC: valueOrUnknown(o.NoVNC), AgentBrowser: valueOrUnknown(o.AgentBrowser), Gateway: valueOrUnknown(o.GatewayVersion), CLI: valueOrUnknown(o.CLIVersion)},
		Schema:     SchemaInfo{GatewayDB: o.GatewayDB, MinGatewayDB: o.MinGatewayDB},
		Rollback:   RollbackInfo{Mode: RollbackModePaired, NewerProfileReadableByOlder: false},
	}
	rtInfo, err := d.ImageInspect(ctx, o.RuntimeImage)
	if err != nil {
		return nil, err
	}
	rtRepo := repoOf(o.RuntimeImage)
	rtDigest, err := localDigest(rtInfo, rtRepo)
	if err != nil {
		return nil, fmt.Errorf("runtime image: %w", err)
	}
	baseDigest := rtInfo.Config.Labels["org.opencontainers.image.base.digest"]
	baseName := rtInfo.Config.Labels["org.opencontainers.image.base.name"]
	if !digestPattern.MatchString(baseDigest) || baseName == "" {
		return nil, errors.New("runtime image lacks org.opencontainers.image.base.name/base.digest labels")
	}
	m.Base = BaseImage{Name: baseName, Digest: baseDigest}
	arch := rtInfo.Arch
	if arch == "x86_64" {
		arch = "amd64"
	}
	if arch == "aarch64" {
		arch = "arm64"
	}
	arches := o.Architectures
	if len(arches) == 0 {
		arches = []string{arch}
	}
	if len(arches) != 1 || arches[0] != arch {
		return nil, errors.New("the dev generator accepts exactly the architecture of the inspected local image; use the release workflow for multi-platform manifests")
	}

	dpkg, err := imageCat(ctx, d, o.RuntimeImage, "/usr/share/lensdock/dpkg-manifest.tsv")
	if err != nil {
		return nil, fmt.Errorf("read runtime dpkg manifest: %w", err)
	}
	packages, err := packagesFromTSV(dpkg)
	if err != nil {
		return nil, err
	}
	for _, p := range packages {
		p.Arch = arch // the image's architecture is canonical for the release matrix
		m.APT.Packages = append(m.APT.Packages, p)
		if p.Name == "chromium" {
			m.Components.Chromium = p.Version
		}
	}
	if m.Components.Chromium == "" {
		return nil, errors.New("runtime dpkg manifest has no chromium package")
	}
	m.APT.ObservedAt, m.APT.Sources = o.ObservedAt.UTC(), imageSources(ctx, d, o.RuntimeImage)

	rtRef := ImageRef{Repository: rtRepo, Digest: rtDigest}
	rtSBOM := spdxForImage("lensdock-runtime-"+o.ReleaseID+"-"+arch, rtDigest, m.APT.Packages, o.ObservedAt)
	att, err := writeAttachment(o.OutputDir, "evidence/runtime-"+arch+".spdx.json", rtSBOM)
	if err != nil {
		return nil, err
	}
	att.SubjectDigest = rtDigest
	rtRef.SBOM = &SBOMEvidence{Attachment: att, Format: "spdx-json", Generator: GeneratorDpkgGo}
	imgs := ArchImages{Runtime: rtRef}

	if o.GatewayImage != "" {
		gwInfo, err := d.ImageInspect(ctx, o.GatewayImage)
		if err != nil {
			return nil, err
		}
		gwArch := gwInfo.Arch
		if gwArch == "x86_64" {
			gwArch = "amd64"
		}
		if gwArch == "aarch64" {
			gwArch = "arm64"
		}
		if gwArch != arch {
			return nil, fmt.Errorf("gateway image is %s, runtime is %s", gwArch, arch)
		}
		gwRepo := repoOf(o.GatewayImage)
		gwDigest, err := localDigest(gwInfo, gwRepo)
		if err != nil {
			return nil, fmt.Errorf("gateway image: %w", err)
		}
		// A dev manifest does not invent Gateway SBOM data. The promotion
		// workflow replaces this with the actual BuildKit SBOM exported from
		// the digest-pinned image.
		gwRef := ImageRef{Repository: gwRepo, Digest: gwDigest}
		imgs.Gateway = &gwRef
		if m.Schema.GatewayDB < 1 || m.Schema.MinGatewayDB < 1 {
			return nil, errors.New("gateway images require --gateway-db and --min-gateway-db")
		}
	} else {
		m.Schema = SchemaInfo{}
	}
	m.Images[arch] = imgs

	cliPath := o.LinuxCLI[arch]
	if cliPath == "" {
		return nil, fmt.Errorf("linux CLI path for %s is required", arch)
	}
	cliSHA, cliSize, err := sha256File(cliPath)
	if err != nil {
		return nil, err
	}
	m.CLI[arch] = CLIBinary{SHA256: cliSHA, Size: cliSize}

	if err := m.Validate(); err != nil {
		return nil, err
	}
	if err := writeJSONAtomic(filepath.Join(o.OutputDir, ManifestFileName), m, 0o644); err != nil {
		return nil, err
	}
	removeOnError = false
	return m, nil
}

func localDigest(info ImageInfo, repo string) (string, error) {
	if info.Descriptor != nil && digestPattern.MatchString(info.Descriptor.Digest) {
		return info.Descriptor.Digest, nil
	}
	for _, rd := range info.RepoDigests {
		if r, d, ok := strings.Cut(rd, "@"); ok && r == repo && digestPattern.MatchString(d) {
			return d, nil
		}
	}
	return "", errors.New("the local image has no immutable manifest digest; use Docker's containerd image store or push/load it with a digest")
}

func valueOrUnknown(s string) string {
	if strings.TrimSpace(s) == "" {
		return "unknown"
	}
	return s
}

// SignRelease signs the exact manifest.json in dir with a private key file
// (0600, a base64 Ed25519 seed) and writes manifest.sig without overwriting.
func SignRelease(dir, keyFile string) (SignatureFile, error) {
	raw, err := readFileLimit(filepath.Join(dir, ManifestFileName), maxManifestBytes)
	if err != nil {
		return SignatureFile{}, err
	}
	keyData, err := auth.ReadPrivateFile(keyFile, 4096)
	if err != nil {
		return SignatureFile{}, err
	}
	priv, err := DecodePrivateKey(string(keyData))
	if err != nil {
		return SignatureFile{}, err
	}
	sig := SignManifest(raw, priv)
	path := filepath.Join(dir, SignatureFileName)
	if _, err := os.Lstat(path); err == nil {
		return SignatureFile{}, fmt.Errorf("%s already exists", path)
	}
	return sig, writeJSONAtomic(path, sig, 0o644)
}

// VerifyReleaseDir is the public release-verification API of the CLI.
func VerifyReleaseDir(dir string, keys []TrustedKey) (*Release, error) {
	return LoadRelease(dir, VerifyPolicy{TrustedKeys: keys, Now: time.Now})
}
