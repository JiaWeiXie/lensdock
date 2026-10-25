package main

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/JiaWeiXie/lensdock/internal/auth"
	"github.com/JiaWeiXie/lensdock/internal/update"
)

// releaseUsageText is appended to usageText by the CLI dispatcher integration.
const releaseUsageText = `  lensdock release manifest --dev --id ID --output DIR --runtime-image IMAGE --linux-cli FILE [component flags]
  lensdock release keygen --output PRIVATE_KEY_FILE
  lensdock release sign --release DIR --key-file PRIVATE_KEY_FILE
  lensdock release promote --dev-release DIR --evidence-index FILE --output DIR
  lensdock release merge --output DIR DEV_RELEASE_DIR DEV_RELEASE_DIR [...]
  lensdock release verify --release DIR [--deployment FILE] [--key-file PRIVATE_KEY_FILE]
`

// releaseSubcommands is the CLI dispatch seam owned by cmd/lensdock/main.go.
func (c *cli) releaseSubcommands(ctx context.Context) map[string]func([]string) error {
	return map[string]func([]string) error{
		"manifest": func(a []string) error { return c.releaseManifest(ctx, a) },
		"keygen":   func(a []string) error { return c.releaseKeygen(a) },
		"sign":     func(a []string) error { return c.releaseSign(a) },
		"promote":  func(a []string) error { return c.releasePromote(a) },
		"merge":    func(a []string) error { return c.releaseMerge(a) },
		"verify":   func(a []string) error { return c.releaseVerify(a) },
	}
}

func gitValue(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Stderr = io.Discard
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func gitSource(ctx context.Context) (commit string, dirty bool, err error) {
	commit, err = gitValue(ctx, "rev-parse", "HEAD")
	if err != nil {
		return "", false, errors.New("--git-commit is required outside a Git worktree")
	}
	status, err := gitValue(ctx, "status", "--porcelain", "--untracked-files=normal")
	if err != nil {
		return "", false, err
	}
	return commit, status != "", nil
}

func (c *cli) releaseManifest(ctx context.Context, args []string) error {
	fs := c.newFlagSet("release manifest")
	dev := fs.Bool("dev", false, "generate honest local dev evidence (not a publishable release)")
	id := fs.String("id", "", "release id")
	out := fs.String("output", "", "new release directory")
	runtimeImage := fs.String("runtime-image", "", "local Runtime image reference")
	gatewayImage := fs.String("gateway-image", "", "local Gateway image reference")
	linuxCLI := fs.String("linux-cli", "", "Linux lensdock binary matching the image architecture")
	gitCommit := fs.String("git-commit", "", "40 hex Git commit (default current HEAD)")
	sourceClean := fs.Bool("source-clean", false, "record source clean after the release workflow verified Git status before generating outputs")
	uncommitted := fs.Bool("uncommitted", false, "explicitly record uncommitted development source; never eligible for release promotion")
	noVNC := fs.String("novnc-version", "", "noVNC version (empty records unknown on dev channel)")
	agentBrowser := fs.String("agent-browser-version", "", "agent-browser version")
	gatewayVersion := fs.String("gateway-version", "", "Gateway version")
	cliVersion := fs.String("cli-version", "development", "CLI version")
	gatewayDB := fs.Int("gateway-db", 0, "Gateway database schema version")
	minGatewayDB := fs.Int("min-gateway-db", 0, "oldest Gateway database schema this release can migrate")
	if _, err := parse(fs, args, 0, 0); err != nil {
		return err
	}
	if !*dev {
		return usageErrorf("release manifest: only --dev local generation is implemented; publishable release manifests are promoted by tools/release/platform-evidence.sh and assemble-release.sh from actual BuildKit, scanner and test reports")
	}
	for name, value := range map[string]string{"--id": *id, "--output": *out, "--runtime-image": *runtimeImage, "--linux-cli": *linuxCLI} {
		if value == "" {
			return usageErrorf("release manifest: %s is required", name)
		}
	}
	dirty := false
	if *uncommitted {
		if *gitCommit != "" || *sourceClean {
			return usageErrorf("--uncommitted cannot be combined with --git-commit or --source-clean")
		}
		dirty = true
	} else if *gitCommit == "" {
		var err error
		*gitCommit, dirty, err = gitSource(ctx)
		if err != nil {
			return err
		}
	} else if !*sourceClean {
		if status, err := gitValue(ctx, "status", "--porcelain", "--untracked-files=normal"); err == nil {
			dirty = status != ""
		}
	}
	manifest, err := update.GenerateDevManifest(ctx, update.Docker{Run: update.ExecRunner{}}, update.DevManifestOptions{
		OutputDir: *out, ReleaseID: *id, RuntimeImage: *runtimeImage, GatewayImage: *gatewayImage,
		LinuxCLI: map[string]string{"amd64": *linuxCLI, "arm64": *linuxCLI}, GitCommit: *gitCommit, Dirty: dirty,
		AgentBrowser: *agentBrowser, NoVNC: *noVNC, GatewayVersion: *gatewayVersion, CLIVersion: *cliVersion,
		GatewayDB: *gatewayDB, MinGatewayDB: *minGatewayDB,
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(c.stderr, "wrote dev release %s to %s (real local SPDX package SBOM; no provenance, scan, test pass or signature was fabricated)\n", manifest.ReleaseID, *out)
	return c.writeJSONLine(manifest)
}

func (c *cli) releaseKeygen(args []string) error {
	fs := c.newFlagSet("release keygen")
	out := fs.String("output", "", "new private key file (0600)")
	if _, err := parse(fs, args, 0, 0); err != nil {
		return err
	}
	if *out == "" {
		return usageErrorf("release keygen: --output is required")
	}
	if err := os.MkdirAll(filepath.Dir(*out), 0o700); err != nil {
		return err
	}
	pub, priv, err := update.GenerateSigningKey()
	if err != nil {
		return err
	}
	if err := auth.WritePrivateFile(*out, []byte(update.EncodePrivateKey(priv)+"\n")); err != nil {
		return err
	}
	key := update.TrustedKey{KeyID: update.KeyIDOf(pub), PublicKey: base64.StdEncoding.EncodeToString(pub)}
	fmt.Fprintf(c.stderr, "wrote private signing key %s (mode 0600); protect and back it up offline\n", *out)
	return c.writeJSONLine(key)
}

func (c *cli) releaseSign(args []string) error {
	fs := c.newFlagSet("release sign")
	dir := fs.String("release", "", "release directory")
	key := fs.String("key-file", "", "private Ed25519 seed file (0600)")
	if _, err := parse(fs, args, 0, 0); err != nil {
		return err
	}
	if *dir == "" || *key == "" {
		return usageErrorf("release sign: --release and --key-file are required")
	}
	sig, err := update.SignRelease(*dir, *key)
	if err != nil {
		return err
	}
	return c.writeJSONLine(sig)
}

func (c *cli) releasePromote(args []string) error {
	fs := c.newFlagSet("release promote")
	devDir := fs.String("dev-release", "", "dev release directory with exact image and CLI pins")
	index := fs.String("evidence-index", "", "promotion index naming real BuildKit, Trivy and acceptance reports")
	output := fs.String("output", "", "new release-channel directory")
	if _, err := parse(fs, args, 0, 0); err != nil {
		return err
	}
	if *devDir == "" || *index == "" || *output == "" {
		return usageErrorf("release promote: --dev-release, --evidence-index and --output are required")
	}
	m, err := update.PromoteRelease(*devDir, *output, *index)
	if err != nil {
		return err
	}
	fmt.Fprintf(c.stderr, "promoted %s: real BuildKit SBOM/provenance, Trivy scan and passed test report attached; sign it before use\n", m.ReleaseID)
	return c.writeJSONLine(m)
}

func (c *cli) releaseMerge(args []string) error {
	fs := c.newFlagSet("release merge")
	output := fs.String("output", "", "new merged dev release directory")
	inputs, err := parse(fs, args, 2, 64)
	if err != nil {
		return err
	}
	if *output == "" {
		return usageErrorf("release merge: --output is required")
	}
	m, err := update.MergeDevManifests(*output, inputs)
	if err != nil {
		return err
	}
	return c.writeJSONLine(m)
}

func (c *cli) releaseVerify(args []string) error {
	fs := c.newFlagSet("release verify")
	dir := fs.String("release", "", "release directory")
	df := c.addDeploymentFlags(fs)
	keyFile := fs.String("key-file", "", "private signing key file used to verify its public key (optional)")
	if _, err := parse(fs, args, 0, 0); err != nil {
		return err
	}
	if *dir == "" {
		return usageErrorf("release verify: --release is required")
	}
	var keys []update.TrustedKey
	if p, err := df.path(); err == nil {
		if dep, err := update.LoadDeployment(p); err == nil {
			keys = dep.TrustedKeys
		}
	}
	if *keyFile != "" {
		data, err := auth.ReadPrivateFile(*keyFile, 4096)
		if err != nil {
			return err
		}
		priv, err := update.DecodePrivateKey(string(data))
		if err != nil {
			return err
		}
		pub := priv.Public().(ed25519.PublicKey)
		keys = append(keys, update.TrustedKey{KeyID: update.KeyIDOf(pub), PublicKey: base64.StdEncoding.EncodeToString(pub)})
	}
	rel, err := update.VerifyReleaseDir(*dir, keys)
	if err != nil {
		return err
	}
	return c.writeJSONLine(map[string]any{
		"releaseId": rel.Manifest.ReleaseID, "manifestSha256": rel.SHA256,
		"signed": rel.Signed, "signatureKeyId": rel.SignatureKeyID,
		"verifiedAt": rel.VerifiedAt, "warnings": rel.Warnings,
	})
}
