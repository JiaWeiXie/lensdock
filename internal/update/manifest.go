package update

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Manifest constants.
const (
	ManifestSchemaVersion = 1
	ChannelRelease        = "release"
	ChannelDev            = "dev"
	ManifestFileName      = "manifest.json"
	SignatureFileName     = "manifest.sig"
	RollbackModePaired    = "paired-snapshot"

	maxManifestBytes = 4 << 20
	maxEvidenceBytes = 128 << 20
)

// Generator names recorded in evidence.
const (
	GeneratorBuildKit = "buildkit-attestation"
	GeneratorDpkgGo   = "lensdock-dpkg-gobuildinfo"
)

// ErrIncompleteRelease wraps every manifest rejection: an image tag or a
// partial manifest is not a release (ACCEPTANCE UP-07).
var ErrIncompleteRelease = errors.New("release manifest is incomplete or invalid")

var (
	releaseIDPattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	digestPattern     = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	hex64Pattern      = regexp.MustCompile(`^[0-9a-f]{64}$`)
	commitPattern     = regexp.MustCompile(`^[0-9a-f]{40}$`)
	repositoryPattern = regexp.MustCompile(`^(?:[a-z0-9]+(?:[.-][a-z0-9]+)*(?::[0-9]{1,5})?/)?[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*(?:/[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*)*$`)
	epochPattern      = regexp.MustCompile(`^[0-9]+:`)
	leadingInt        = regexp.MustCompile(`^[0-9]+`)
	placeholderValues = []string{"", "unknown", "unspecified", "n/a", "none", "dev", "latest", "todo"}
)

// Manifest is the immutable record of one release (SPEC 15.2). It lives at
// <releasesDir>/<releaseId>/manifest.json together with its evidence files.
type Manifest struct {
	SchemaVersion int                   `json:"schemaVersion"`
	ReleaseID     string                `json:"releaseId"`
	Channel       string                `json:"channel"`
	CreatedAt     time.Time             `json:"createdAt"`
	Source        Source                `json:"source"`
	Base          BaseImage             `json:"base"`
	Images        map[string]ArchImages `json:"images"`
	APT           APT                   `json:"apt"`
	Components    Components            `json:"components"`
	Schema        SchemaInfo            `json:"schema"`
	CLI           map[string]CLIBinary  `json:"cli"`
	Rollback      RollbackInfo          `json:"rollback"`
	Tests         *TestsEvidence        `json:"tests,omitempty"`
}

// Source identifies the code the release was built from.
type Source struct {
	GitCommit string `json:"gitCommit"`
	// Dirty is only accepted on the dev channel.
	Dirty bool `json:"dirty,omitempty"`
}

// BaseImage is the Debian base the runtime image was built from.
type BaseImage struct {
	Name   string `json:"name"`
	Digest string `json:"digest"`
}

// ArchImages are the images run on one Docker engine architecture.
type ArchImages struct {
	Runtime ImageRef  `json:"runtime"`
	Gateway *ImageRef `json:"gateway,omitempty"`
}

// ImageRef pins one image by exact digest. A tag is never part of a release.
type ImageRef struct {
	Repository string `json:"repository"`
	// Digest is the digest of the manifest or index a pull by digest
	// resolves. It is what the compose override pins.
	Digest string `json:"digest"`
	// ManifestDigest is the digest of the platform image manifest when
	// Digest names an index (attestations are attached to that manifest).
	ManifestDigest string              `json:"manifestDigest,omitempty"`
	SBOM           *SBOMEvidence       `json:"sbom,omitempty"`
	Provenance     *ProvenanceEvidence `json:"provenance,omitempty"`
	Scan           *ScanEvidence       `json:"scan,omitempty"`
}

// Ref returns repository@digest.
func (r ImageRef) Ref() string { return r.Repository + "@" + r.Digest }

// subjectDigest is the digest evidence about this image must be bound to.
func (r ImageRef) subjectDigests() []string {
	out := []string{r.Digest}
	if r.ManifestDigest != "" {
		out = append(out, r.ManifestDigest)
	}
	return out
}

// Attachment is an evidence file stored next to the manifest.
type Attachment struct {
	// Path is relative to the release directory, slash separated.
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	// SubjectDigest is the image digest the evidence describes.
	SubjectDigest string `json:"subjectDigest,omitempty"`
}

// SBOMEvidence is a software bill of materials document.
type SBOMEvidence struct {
	Attachment
	// Format is spdx-json or cyclonedx-json.
	Format    string `json:"format"`
	Generator string `json:"generator"`
}

// ProvenanceEvidence is an in-toto statement with a SLSA provenance
// predicate as produced by BuildKit. It is not signed by LensDock; the
// manifest signature covers its SHA-256.
type ProvenanceEvidence struct {
	Attachment
	PredicateType string `json:"predicateType"`
	Generator     string `json:"generator"`
}

// VulnSummary counts findings by severity.
type VulnSummary struct {
	Critical int `json:"critical"`
	High     int `json:"high"`
	Medium   int `json:"medium"`
	Low      int `json:"low"`
	Unknown  int `json:"unknown"`
}

// ScanEvidence is the raw report of an image vulnerability scan.
type ScanEvidence struct {
	Attachment
	Tool        string      `json:"tool"`
	ToolVersion string      `json:"toolVersion"`
	ScannedAt   time.Time   `json:"scannedAt"`
	Summary     VulnSummary `json:"summary"`
}

// TestsEvidence is the report of the acceptance run against the release.
type TestsEvidence struct {
	Attachment
	Summary string `json:"summary"`
	Passed  bool   `json:"passed"`
}

// APT records the package set of the runtime images and where it came from.
type APT struct {
	// ObservedAt is when the package index the build used was fetched (the
	// image build time: the Dockerfile runs apt-get update during the build).
	ObservedAt time.Time `json:"observedAt"`
	// Sources are the repository URIs from the image's apt configuration.
	Sources  []string  `json:"sources"`
	Packages []Package `json:"packages"`
}

// Package is one dpkg package of an image.
type Package struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Arch    string `json:"arch"`
}

// Components are the user-visible component versions.
type Components struct {
	Chromium     string `json:"chromium"`
	NoVNC        string `json:"novnc"`
	AgentBrowser string `json:"agentBrowser"`
	Gateway      string `json:"gateway"`
	CLI          string `json:"cli"`
}

// SchemaInfo is the Gateway database schema contract of the release. Both are
// 0 for a release without a Gateway image.
type SchemaInfo struct {
	// GatewayDB is the schema version the release's Gateway migrates the
	// database to.
	GatewayDB int `json:"gatewayDb"`
	// MinGatewayDB is the oldest schema version it can migrate from.
	MinGatewayDB int `json:"minGatewayDb"`
}

// CLIBinary is the Linux lensdock binary the candidate verifier runs.
type CLIBinary struct {
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// RollbackInfo states how this release is rolled back. Only the paired mode
// exists: older images never open a Profile a newer Chromium has written.
type RollbackInfo struct {
	Mode string `json:"mode"`
	// NewerProfileReadableByOlder must be false.
	NewerProfileReadableByOlder bool `json:"newerProfileReadableByOlder"`
}

// HasGateway reports whether the release ships Gateway images.
func (m *Manifest) HasGateway() bool {
	for _, a := range m.Images {
		if a.Gateway != nil {
			return true
		}
	}
	return false
}

// ChromiumMajor returns the major version of Components.Chromium, or 0.
func (m *Manifest) ChromiumMajor() int { return chromiumMajor(m.Components.Chromium) }

func chromiumMajor(version string) int {
	v := epochPattern.ReplaceAllString(version, "")
	n, err := strconv.Atoi(leadingInt.FindString(v))
	if err != nil {
		return 0
	}
	return n
}

func isPlaceholder(s string) bool {
	return contains(placeholderValues, strings.ToLower(strings.TrimSpace(s)))
}

// validateImageRef checks the syntactic form of an image pin.
func validateImageRef(where string, r ImageRef, add func(string, ...any)) {
	if !repositoryPattern.MatchString(r.Repository) {
		add("%s.repository %q is not a plain repository name (no tag, no digest)", where, r.Repository)
	}
	if !digestPattern.MatchString(r.Digest) {
		add("%s.digest must be sha256:<64 hex>, got %q", where, r.Digest)
	}
	if r.ManifestDigest != "" && !digestPattern.MatchString(r.ManifestDigest) {
		add("%s.manifestDigest must be sha256:<64 hex>", where)
	}
}

func validateAttachment(where string, a Attachment, add func(string, ...any)) {
	if err := validateRelPath(a.Path); err != nil {
		add("%s.path: %v", where, err)
	}
	if !hex64Pattern.MatchString(a.SHA256) {
		add("%s.sha256 must be 64 lowercase hex characters", where)
	}
	if a.Size <= 0 {
		add("%s.size must be positive", where)
	}
}

// validateRelPath accepts only clean, relative, slash separated names without
// parent references.
func validateRelPath(p string) error {
	switch {
	case p == "":
		return errors.New("is empty")
	case strings.ContainsAny(p, "\\\x00"):
		return errors.New("contains a backslash or NUL")
	case strings.HasPrefix(p, "/"):
		return errors.New("must be relative")
	}
	for _, part := range strings.Split(p, "/") {
		if part == "" || part == "." || part == ".." {
			return errors.New("must be a clean relative path")
		}
	}
	return nil
}

// Validate checks everything that does not need the evidence files. All
// problems are reported in one error wrapping ErrIncompleteRelease.
func (m *Manifest) Validate() error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	if m.SchemaVersion != ManifestSchemaVersion {
		add("schemaVersion must be %d", ManifestSchemaVersion)
	}
	if !releaseIDPattern.MatchString(m.ReleaseID) {
		add("releaseId %q must match %s", m.ReleaseID, releaseIDPattern)
	}
	if m.Channel != ChannelRelease && m.Channel != ChannelDev {
		add("channel must be %q or %q", ChannelRelease, ChannelDev)
	}
	release := m.Channel == ChannelRelease
	if m.CreatedAt.IsZero() {
		add("createdAt is required")
	}
	if !commitPattern.MatchString(m.Source.GitCommit) && !(m.Channel == ChannelDev && m.Source.Dirty && m.Source.GitCommit == "") {
		add("source.gitCommit must be a 40 hex character commit; only dirty dev source may omit it")
	}
	if m.Source.Dirty && release {
		add("source.dirty is not allowed on the %s channel", ChannelRelease)
	}
	if strings.TrimSpace(m.Base.Name) == "" || !digestPattern.MatchString(m.Base.Digest) {
		add("base.name and base.digest (sha256:<64 hex>) are required")
	}

	if len(m.Images) == 0 {
		add("images must list at least one architecture")
	}
	gateways := 0
	arches := make([]string, 0, len(m.Images))
	for arch := range m.Images {
		arches = append(arches, arch)
	}
	sort.Strings(arches)
	for _, arch := range arches {
		imgs := m.Images[arch]
		if !contains(validArchNames, arch) {
			add("images: unsupported architecture %q", arch)
			continue
		}
		validateImageRef("images."+arch+".runtime", imgs.Runtime, add)
		validateImageEvidence("images."+arch+".runtime", imgs.Runtime, release, add)
		if imgs.Gateway != nil {
			gateways++
			validateImageRef("images."+arch+".gateway", *imgs.Gateway, add)
			validateImageEvidence("images."+arch+".gateway", *imgs.Gateway, release, add)
		}
		bin, ok := m.CLI[arch]
		switch {
		case !ok:
			add("cli.%s is required: the linux lensdock binary must be pinned for every image architecture", arch)
		case !hex64Pattern.MatchString(bin.SHA256) || bin.Size <= 0:
			add("cli.%s needs a 64 hex sha256 and a positive size", arch)
		}
		m.validateChromium(arch, add)
	}
	for arch := range m.CLI {
		if _, ok := m.Images[arch]; !ok {
			add("cli.%s has no matching images entry", arch)
		}
	}
	if gateways != 0 && gateways != len(m.Images) {
		add("either every architecture or none must ship a gateway image")
	}

	if m.APT.ObservedAt.IsZero() {
		add("apt.observedAt is required")
	}
	if len(m.APT.Sources) == 0 {
		add("apt.sources must list the package repositories")
	}
	if len(m.APT.Packages) == 0 {
		add("apt.packages must list the installed package versions")
	}
	for i, p := range m.APT.Packages {
		if p.Name == "" || p.Version == "" || p.Arch == "" {
			add("apt.packages[%d] needs name, version and arch", i)
			break
		}
	}

	for name, v := range map[string]string{
		"chromium": m.Components.Chromium, "novnc": m.Components.NoVNC, "agentBrowser": m.Components.AgentBrowser,
		"gateway": m.Components.Gateway, "cli": m.Components.CLI,
	} {
		switch {
		case strings.TrimSpace(v) == "":
			add("components.%s is required", name)
		case release && isPlaceholder(v):
			add("components.%s %q is a placeholder, not allowed on the %s channel", name, v, ChannelRelease)
		}
	}

	if gateways > 0 {
		if m.Schema.GatewayDB < 1 || m.Schema.MinGatewayDB < 1 || m.Schema.MinGatewayDB > m.Schema.GatewayDB {
			add("schema needs 1 <= minGatewayDb <= gatewayDb for a release with a gateway image")
		}
	} else if m.Schema.GatewayDB != 0 || m.Schema.MinGatewayDB != 0 {
		add("schema.gatewayDb and schema.minGatewayDb must be 0 without a gateway image")
	}

	if m.Rollback.Mode != RollbackModePaired {
		add("rollback.mode must be %q", RollbackModePaired)
	}
	if m.Rollback.NewerProfileReadableByOlder {
		add("rollback.newerProfileReadableByOlder must be false: an older Chromium must never open a newer Profile")
	}

	if m.Tests != nil {
		validateAttachment("tests", m.Tests.Attachment, add)
		if strings.TrimSpace(m.Tests.Summary) == "" {
			add("tests.summary is required")
		}
	}
	if release {
		if m.Tests == nil {
			add("tests: a release needs the test report of its acceptance run")
		} else if !m.Tests.Passed {
			add("tests.passed must be true on the %s channel", ChannelRelease)
		}
	}

	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("%w: %w", ErrIncompleteRelease, errors.Join(errs...))
}

func (m *Manifest) validateChromium(arch string, add func(string, ...any)) {
	found := false
	for _, p := range m.APT.Packages {
		if p.Name == "chromium" && p.Arch == arch {
			found = true
			if p.Version != m.Components.Chromium {
				add("components.chromium %q does not match the %s package chromium %q", m.Components.Chromium, arch, p.Version)
			}
		}
	}
	if !found {
		add("apt.packages has no chromium package for %s", arch)
	}
}

func validateImageEvidence(where string, r ImageRef, release bool, add func(string, ...any)) {
	subjects := r.subjectDigests()
	checkSubject := func(name string, a Attachment) {
		if a.SubjectDigest == "" || !contains(subjects, a.SubjectDigest) {
			add("%s.%s.subjectDigest %q is not the digest of this image", where, name, a.SubjectDigest)
		}
	}
	if r.SBOM != nil {
		validateAttachment(where+".sbom", r.SBOM.Attachment, add)
		checkSubject("sbom", r.SBOM.Attachment)
		if r.SBOM.Format != "spdx-json" && r.SBOM.Format != "cyclonedx-json" {
			add("%s.sbom.format must be spdx-json or cyclonedx-json", where)
		}
		if r.SBOM.Generator != GeneratorBuildKit && r.SBOM.Generator != GeneratorDpkgGo {
			add("%s.sbom.generator %q is unknown", where, r.SBOM.Generator)
		}
	} else if release {
		add("%s.sbom is required on the %s channel", where, ChannelRelease)
	}
	if r.Provenance != nil {
		validateAttachment(where+".provenance", r.Provenance.Attachment, add)
		checkSubject("provenance", r.Provenance.Attachment)
		if r.Provenance.Generator != GeneratorBuildKit {
			add("%s.provenance.generator must be %s: LensDock does not fabricate provenance", where, GeneratorBuildKit)
		}
		if !strings.HasPrefix(r.Provenance.PredicateType, "https://slsa.dev/provenance/") {
			add("%s.provenance.predicateType must be a SLSA provenance type", where)
		}
	} else if release {
		add("%s.provenance is required on the %s channel", where, ChannelRelease)
	}
	if r.Scan != nil {
		validateAttachment(where+".scan", r.Scan.Attachment, add)
		if r.Scan.SubjectDigest != r.Digest {
			add("%s.scan.subjectDigest must equal the image digest", where)
		}
		if r.Scan.Tool == "" || r.Scan.ToolVersion == "" || r.Scan.ScannedAt.IsZero() {
			add("%s.scan needs tool, toolVersion and scannedAt", where)
		}
		if release && r.Scan.Summary.Critical > 0 {
			add("%s.scan reports %d critical vulnerabilities; a release is blocked", where, r.Scan.Summary.Critical)
		}
	} else if release {
		add("%s.scan is required on the %s channel", where, ChannelRelease)
	}
}

// Release is a loaded and verified release directory.
type Release struct {
	Dir      string
	Manifest Manifest
	// SHA256 is the digest of the exact manifest.json bytes.
	SHA256         string
	Signed         bool
	SignatureKeyID string
	// Warnings are non-fatal findings (dev release, unsigned, high findings).
	Warnings   []string
	VerifiedAt time.Time
}

// VerifyPolicy controls LoadRelease.
type VerifyPolicy struct {
	TrustedKeys []TrustedKey
	Now         func() time.Time
}

// ResolveRelease maps a --release argument to a release directory. A release
// id is looked up in releasesDir; a path may name the directory or its
// manifest.json. Anything that looks like an image reference is refused.
func ResolveRelease(releasesDir, ref string) (string, error) {
	if ref == "" {
		return "", errors.New("--release is required")
	}
	if strings.ContainsAny(ref, "@:") && !filepath.IsAbs(ref) && !strings.HasPrefix(ref, ".") {
		return "", fmt.Errorf("%q looks like an image reference: an image tag is not a release; pass a release id or the path of a release directory with a manifest.json", ref)
	}
	if strings.ContainsRune(ref, os.PathSeparator) || strings.HasSuffix(ref, ".json") {
		fi, err := os.Stat(ref)
		if err != nil {
			return "", fmt.Errorf("release path: %w", err)
		}
		if fi.IsDir() {
			return filepath.Abs(ref)
		}
		if filepath.Base(ref) != ManifestFileName {
			return "", fmt.Errorf("a release file must be named %s", ManifestFileName)
		}
		return filepath.Abs(filepath.Dir(ref))
	}
	if !releaseIDPattern.MatchString(ref) {
		return "", fmt.Errorf("release id %q must match %s", ref, releaseIDPattern)
	}
	dir := filepath.Join(releasesDir, ref)
	if _, err := os.Stat(filepath.Join(dir, ManifestFileName)); err != nil {
		return "", fmt.Errorf("no release manifest for %q in %s (an image tag alone is not a release): %w", ref, filepath.Base(releasesDir), err)
	}
	return dir, nil
}

// DecodeManifest strictly decodes manifest.json bytes.
func DecodeManifest(raw []byte) (Manifest, error) {
	var m Manifest
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return Manifest{}, fmt.Errorf("%w: decode: %v", ErrIncompleteRelease, err)
	}
	if err := ensureJSONEOF(dec); err != nil {
		return Manifest{}, fmt.Errorf("%w: %v", ErrIncompleteRelease, err)
	}
	return m, nil
}

// LoadRelease reads dir/manifest.json, validates it, verifies every evidence
// file (existence, size, SHA-256, document form, subject digest) and the
// signature policy: the release channel needs a valid signature by a trusted
// key; a dev release may be unsigned but a present signature must verify.
func LoadRelease(dir string, pol VerifyPolicy) (*Release, error) {
	raw, err := readFileLimit(filepath.Join(dir, ManifestFileName), maxManifestBytes)
	if err != nil {
		return nil, fmt.Errorf("%w: read manifest: %v", ErrIncompleteRelease, err)
	}
	m, err := DecodeManifest(raw)
	if err != nil {
		return nil, err
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	rel := &Release{Dir: dir, Manifest: m, SHA256: sha256Hex(raw)}
	if err := verifyEvidenceFiles(dir, &m); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrIncompleteRelease, err)
	}

	sigRaw, sigErr := readFileLimit(filepath.Join(dir, SignatureFileName), 64<<10)
	switch {
	case sigErr == nil:
		var sig SignatureFile
		if err := json.Unmarshal(sigRaw, &sig); err != nil {
			return nil, fmt.Errorf("%w: %s is not valid: %v", ErrIncompleteRelease, SignatureFileName, err)
		}
		keyID, err := VerifyManifestSignature(raw, sig, pol.TrustedKeys)
		if err != nil {
			return nil, fmt.Errorf("%w: signature: %v", ErrIncompleteRelease, err)
		}
		rel.Signed, rel.SignatureKeyID = true, keyID
	case errors.Is(sigErr, os.ErrNotExist):
		if m.Channel == ChannelRelease {
			return nil, fmt.Errorf("%w: the %s channel requires %s signed by a trusted key", ErrIncompleteRelease, ChannelRelease, SignatureFileName)
		}
		rel.Warnings = append(rel.Warnings, "unsigned dev release: no "+SignatureFileName)
	default:
		return nil, fmt.Errorf("%w: read %s: %v", ErrIncompleteRelease, SignatureFileName, sigErr)
	}

	if m.Channel == ChannelDev {
		rel.Warnings = append(rel.Warnings, "dev channel release: not a supply-chain verified release")
	}
	for _, arch := range sortedArches(m.Images) {
		imgs := m.Images[arch]
		appendWarnings := func(name string, ref ImageRef) {
			where := arch + " " + name
			if ref.SBOM == nil {
				rel.Warnings = append(rel.Warnings, where+": no SBOM")
			}
			if ref.Provenance == nil {
				rel.Warnings = append(rel.Warnings, where+": no provenance attestation")
			}
			if ref.Scan == nil {
				rel.Warnings = append(rel.Warnings, where+": no vulnerability scan")
			} else if ref.Scan.Summary.High > 0 {
				rel.Warnings = append(rel.Warnings, fmt.Sprintf("%s: scan reports %d high findings", where, ref.Scan.Summary.High))
			}
		}
		appendWarnings("runtime", imgs.Runtime)
		if imgs.Gateway != nil {
			appendWarnings("gateway", *imgs.Gateway)
		}
	}
	if m.Tests == nil {
		rel.Warnings = append(rel.Warnings, "no test report")
	}
	now := time.Now
	if pol.Now != nil {
		now = pol.Now
	}
	rel.VerifiedAt = now().UTC()
	return rel, nil
}

func sortedArches(m map[string]ArchImages) []string {
	out := make([]string, 0, len(m))
	for a := range m {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

// verifyEvidenceFiles checks every attachment named by m.
func verifyEvidenceFiles(dir string, m *Manifest) error {
	var errs []error
	check := func(where string, a Attachment, extra func(path string) error) {
		path := filepath.Join(dir, filepath.FromSlash(a.Path))
		fi, err := os.Lstat(path)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %v", where, err))
			return
		}
		if !fi.Mode().IsRegular() {
			errs = append(errs, fmt.Errorf("%s: %s is not a regular file", where, a.Path))
			return
		}
		if fi.Size() > maxEvidenceBytes {
			errs = append(errs, fmt.Errorf("%s: %s is larger than %d bytes", where, a.Path, int64(maxEvidenceBytes)))
			return
		}
		sum, size, err := sha256File(path)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %v", where, err))
			return
		}
		if size != a.Size || sum != a.SHA256 {
			errs = append(errs, fmt.Errorf("%s: %s does not match the manifest (size %d sha256 %s)", where, a.Path, size, sum))
			return
		}
		if extra != nil {
			if err := extra(path); err != nil {
				errs = append(errs, fmt.Errorf("%s: %s: %v", where, a.Path, err))
			}
		}
	}
	for _, arch := range sortedArches(m.Images) {
		imgs := m.Images[arch]
		list := []struct {
			name string
			ref  *ImageRef
		}{{"runtime", &imgs.Runtime}, {"gateway", imgs.Gateway}}
		for _, it := range list {
			if it.ref == nil {
				continue
			}
			where := "images." + arch + "." + it.name
			if s := it.ref.SBOM; s != nil {
				format := s.Format
				check(where+".sbom", s.Attachment, func(p string) error { return checkSBOMDocument(p, format) })
			}
			if p := it.ref.Provenance; p != nil {
				pt, subj := p.PredicateType, p.SubjectDigest
				check(where+".provenance", p.Attachment, func(path string) error { return checkProvenanceStatement(path, pt, subj) })
			}
			if s := it.ref.Scan; s != nil {
				check(where+".scan", s.Attachment, func(p string) error { return checkJSONObject(p) })
			}
		}
	}
	if t := m.Tests; t != nil {
		check("tests", t.Attachment, nil)
	}
	return errors.Join(errs...)
}

func checkJSONObject(path string) error {
	data, err := readFileLimit(path, maxEvidenceBytes)
	if err != nil {
		return err
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err != nil {
		return fmt.Errorf("not a JSON object: %v", err)
	}
	return nil
}

func checkSBOMDocument(path, format string) error {
	data, err := readFileLimit(path, maxEvidenceBytes)
	if err != nil {
		return err
	}
	switch format {
	case "spdx-json":
		var doc struct {
			SPDXVersion string            `json:"spdxVersion"`
			Packages    []json.RawMessage `json:"packages"`
		}
		if err := json.Unmarshal(data, &doc); err != nil {
			return fmt.Errorf("not an SPDX JSON document: %v", err)
		}
		if !strings.HasPrefix(doc.SPDXVersion, "SPDX-") || len(doc.Packages) == 0 {
			return errors.New("SPDX document has no spdxVersion or no packages")
		}
	case "cyclonedx-json":
		var doc struct {
			BOMFormat  string            `json:"bomFormat"`
			Components []json.RawMessage `json:"components"`
		}
		if err := json.Unmarshal(data, &doc); err != nil {
			return fmt.Errorf("not a CycloneDX JSON document: %v", err)
		}
		if doc.BOMFormat != "CycloneDX" || len(doc.Components) == 0 {
			return errors.New("CycloneDX document has no bomFormat or no components")
		}
	}
	return nil
}

// checkProvenanceStatement requires an in-toto statement of the recorded
// predicate type whose subjects include the evidence's subject digest.
func checkProvenanceStatement(path, predicateType, subject string) error {
	data, err := readFileLimit(path, maxEvidenceBytes)
	if err != nil {
		return err
	}
	var st struct {
		Type          string `json:"_type"`
		PredicateType string `json:"predicateType"`
		Subject       []struct {
			Digest map[string]string `json:"digest"`
		} `json:"subject"`
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return fmt.Errorf("not an in-toto statement: %v", err)
	}
	if !strings.HasPrefix(st.Type, "https://in-toto.io/Statement/") {
		return errors.New("not an in-toto statement")
	}
	if st.PredicateType != predicateType {
		return fmt.Errorf("predicateType %q differs from the manifest (%q)", st.PredicateType, predicateType)
	}
	want := strings.TrimPrefix(subject, "sha256:")
	for _, s := range st.Subject {
		if s.Digest["sha256"] == want {
			return nil
		}
	}
	return fmt.Errorf("no statement subject has digest %s", subject)
}
