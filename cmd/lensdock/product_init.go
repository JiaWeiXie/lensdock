package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/JiaWeiXie/lensdock/internal/auth"
	"github.com/JiaWeiXie/lensdock/internal/gateway"
	"github.com/JiaWeiXie/lensdock/internal/update"
)

const (
	defaultProject = "lensdock-product"
	defaultOrigin  = "http://127.0.0.1:18081"

	envPublicOrigin   = "LENSDOCK_PUBLIC_ORIGIN"
	envComposeProject = "LENSDOCK_COMPOSE_PROJECT"
	envRuntimeImage   = "LENSDOCK_RUNTIME_IMAGE"
	envGatewayImage   = "LENSDOCK_GATEWAY_IMAGE"

	// A release bundle starts on the published images; a source checkout on
	// the images `mise run image:build` and `gateway:image` build locally.
	releaseRuntimeImage = "ghcr.io/jiaweixie/lensdock/runtime:latest"
	releaseGatewayImage = "ghcr.io/jiaweixie/lensdock/gateway:latest"
	sourceRuntimeImage  = "lensdock/runtime:dev"
	sourceGatewayImage  = "lensdock/gateway:dev"

	// releaseDir holds the private copy of the bundle files and pinFile the
	// Compose override that product-start fills with the exact images of the
	// first start. Both live in the product directory.
	releaseDir = "release"
	pinFile    = "images.pin.yaml"
	// pinPlaceholder is the pin file until the first start. It changes nothing.
	pinPlaceholder = "# LensDock pins the exact Runtime and Gateway images in this file at the first start\n# (lensdock admin product-start). Until then it changes nothing.\nservices: {}\n"
)

func (c *cli) productInit(args []string) error {
	fs := c.newFlagSet("admin product-init")
	state := fs.String("state-dir", c.defaultStateDir(), "private deployment state directory (default $LENSDOCK_STATE_DIR, $HOME/.local/share/lensdock for a release, else .lensdock)")
	originDefault := c.getenv(envPublicOrigin)
	if originDefault == "" {
		originDefault = defaultOrigin
	}
	origin := fs.String("origin", originDefault, "exact public Gateway origin (default $"+envPublicOrigin+" or "+defaultOrigin+")")
	ws := fs.String("workspace", "default", "workspace ID")
	mode := fs.String("mode", c.getenv("LENSDOCK_MODE"), "initial usage mode: NOVNC or AGENT (default AGENT)")
	project := fs.String("project", c.getenv(envComposeProject), "Compose project name; its Docker volumes hold the data (default $"+envComposeProject+" or "+defaultProject+")")
	runtimeImage := fs.String("runtime-image", c.getenv(envRuntimeImage), "Runtime image (default $"+envRuntimeImage+", else "+releaseRuntimeImage+" for a release)")
	gatewayImage := fs.String("gateway-image", c.getenv(envGatewayImage), "Gateway image (default $"+envGatewayImage+", else "+releaseGatewayImage+" for a release)")
	bundleDir := fs.String("bundle-dir", "", "release bundle to copy the Compose file and helpers from (default: the bundle of the running executable)")
	if _, err := parse(fs, args, 0, 0); err != nil {
		return err
	}
	initialMode := strings.ToUpper(strings.TrimSpace(*mode))
	if initialMode == "" {
		initialMode = "AGENT"
	}
	if initialMode != "AGENT" && initialMode != "NOVNC" {
		return usageErrorf("--mode / LENSDOCK_MODE must be NOVNC or AGENT")
	}
	u, err := url.Parse(*origin)
	if err != nil || u.Host == "" || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.Path != "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return usageErrorf("product-init supports loopback HTTP origin; configure TLS explicitly before remote exposure")
	}
	port := u.Port()
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 || portNumber == 80 || strconv.Itoa(portNumber) != port {
		return usageErrorf("origin requires an explicit canonical non-default HTTP port between 1 and 65535")
	}
	if len(*ws) == 0 || len(*ws) > 64 {
		return usageErrorf("workspace must contain 1-64 ASCII letters, digits, underscores or hyphens")
	}
	if (*ws)[0] == '_' || (*ws)[0] == '-' {
		return usageErrorf("workspace must start with a letter or digit")
	}
	for _, ch := range *ws {
		if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '_' || ch == '-') {
			return usageErrorf("invalid workspace ID")
		}
	}
	if *project == "" {
		*project = defaultProject
	}
	if err := update.CheckComposeProject(*project); err != nil {
		return usageErrorf("--project: %v", err)
	}
	bundle, err := c.findBundle(*bundleDir)
	if err != nil {
		return err
	}
	// Without a bundle this is a source checkout: the deployment points at the
	// checkout's own files and local :dev images, as before.
	var cwd string
	if bundle == nil {
		if cwd, err = os.Getwd(); err != nil {
			return err
		}
		if fi, err := os.Stat(filepath.Join(cwd, "docker", "compose.yaml")); err != nil || !fi.Mode().IsRegular() {
			return usageErrorf("no release bundle found for this executable and %s does not exist: run product-init from a source checkout, from an installed release, or pass --bundle-dir", filepath.Join(cwd, "docker", "compose.yaml"))
		}
	}
	if *runtimeImage == "" {
		*runtimeImage = sourceRuntimeImage
		if bundle != nil {
			*runtimeImage = releaseRuntimeImage
		}
	}
	if *gatewayImage == "" {
		*gatewayImage = sourceGatewayImage
		if bundle != nil {
			*gatewayImage = releaseGatewayImage
		}
	}
	for flagName, ref := range map[string]string{"--runtime-image": *runtimeImage, "--gateway-image": *gatewayImage} {
		if err := update.CheckImageRef(ref); err != nil {
			return usageErrorf("%s: %v", flagName, err)
		}
	}
	root, err := filepath.Abs(*state)
	if err != nil {
		return err
	}
	for _, name := range []string{"deployment.json", "product"} {
		if _, err := os.Lstat(filepath.Join(root, name)); err == nil {
			return fmt.Errorf("%s already exists: product-init never overwrites or upgrades a deployment (operate it with `lensdock update compose`, change images with `lensdock update`)", filepath.Join(root, name))
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if _, err := os.Stat(filepath.Join(root, operatorTokenFile)); errors.Is(err, os.ErrNotExist) {
		if err := c.adminInit([]string{"--state-dir", root}); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	op, err := auth.ReadPrivateFile(filepath.Join(root, operatorTokenFile), 4096)
	if err != nil {
		return err
	}
	dir := filepath.Join(root, "product")
	if err := os.Mkdir(dir, 0700); err != nil {
		return fmt.Errorf("create product configuration (existing configuration is never overwritten): %w", err)
	}
	complete := false
	defer func() {
		if !complete {
			_ = os.RemoveAll(dir)
		}
	}()
	if err := auth.WritePrivateFile(filepath.Join(dir, "operator-token"), op); err != nil {
		return err
	}
	var identities []gateway.Identity
	for _, role := range []string{"admin", "human", "viewer", "agent"} {
		tok, err := auth.NewSecret("ld_" + role + "_")
		if err != nil {
			return err
		}
		if err := auth.WritePrivateFile(filepath.Join(dir, role+"-token"), []byte(tok.Reveal()+"\n")); err != nil {
			return err
		}
		identities = append(identities, gateway.Identity{ID: role, Role: role, WorkspaceID: *ws, TokenSHA256: auth.DigestOf(tok.Reveal()).Hex()})
	}
	if err := writeRuntimePKI(dir); err != nil {
		return err
	}
	if err := os.Mkdir(filepath.Join(dir, "public"), 0755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "public", "deployment-status.json"), []byte("{\"release\":{\"id\":\"development\",\"verified\":false},\"update\":{\"phase\":\"not-configured\",\"lastBackupAt\":null}}\n"), 0644); err != nil {
		return err
	}
	cfg := gateway.Config{Listen: ":8080", Origin: *origin, DataDir: "/data", WebDir: "/app/web", Identities: identities, Runtimes: []gateway.RuntimeConfig{{WorkspaceID: *ws, URL: "https://runtime:8080", OperatorTokenFile: "/run/lensdock/operator-token", CAFile: "/run/lensdock/ca.crt", ClientCertFile: "/run/lensdock/gateway.crt", ClientKeyFile: "/run/lensdock/gateway.key"}}}
	cfg.StatusFile = "/run/lensdock-status/deployment-status.json"
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := auth.WritePrivateFile(filepath.Join(dir, "gateway.json"), append(b, '\n')); err != nil {
		return err
	}
	// The host-run Gateway of `mise run gateway:dev` exists only in a source checkout.
	if bundle == nil {
		cfg.Listen = u.Host
		cfg.DataDir = filepath.Join(root, "gateway-data")
		cfg.WebDir = filepath.Join(cwd, "apps/web/dist")
		cfg.StatusFile = filepath.Join(dir, "public", "deployment-status.json")
		cfg.Runtimes = []gateway.RuntimeConfig{{WorkspaceID: *ws, URL: "http://127.0.0.1:18080", OperatorTokenFile: filepath.Join(root, operatorTokenFile)}}
		b, err = json.MarshalIndent(cfg, "", "  ")
		if err != nil {
			return err
		}
		if err := auth.WritePrivateFile(filepath.Join(dir, "gateway-host.json"), append(b, '\n')); err != nil {
			return err
		}
	}
	env := fmt.Sprintf("LENSDOCK_WORKSPACE_ID=%s\nLENSDOCK_MODE=%s\nLENSDOCK_STATE_DIR=%s\nLENSDOCK_HOST_PORT=%s\nLENSDOCK_PUBLIC_ORIGIN=%s\n%s=%s\n%s=%s\n", *ws, initialMode, composeEnvValue(root), port, composeEnvValue(*origin), envRuntimeImage, composeEnvValue(*runtimeImage), envGatewayImage, composeEnvValue(*gatewayImage))
	if err := auth.WritePrivateFile(filepath.Join(dir, "compose.env"), []byte(env)); err != nil {
		return err
	}
	composeFiles := []string{filepath.Join(cwd, "docker", "compose.yaml")}
	linuxCLI := map[string]string{"arm64": filepath.Join(cwd, "bin", "linux-arm64", "lensdock"), "amd64": filepath.Join(cwd, "bin", "linux-amd64", "lensdock")}
	if bundle != nil {
		// A private copy keeps the deployment independent of the bundle's
		// location, so reinstalling or upgrading the CLI cannot break it.
		rel := filepath.Join(dir, releaseDir)
		if err := bundle.snapshot(rel); err != nil {
			return err
		}
		if err := auth.WritePrivateFile(filepath.Join(dir, pinFile), []byte(pinPlaceholder)); err != nil {
			return err
		}
		composeFiles = []string{filepath.Join(rel, "docker", "compose.yaml"), filepath.Join(dir, pinFile)}
		linuxCLI = map[string]string{"arm64": filepath.Join(rel, "bin", "linux-arm64", "lensdock"), "amd64": filepath.Join(rel, "bin", "linux-amd64", "lensdock")}
	}
	dep := update.Deployment{
		Version: 1, StateDir: dir, PublicStatusFile: filepath.Join(dir, "public", "deployment-status.json"),
		Compose:  update.ComposeConfig{Project: *project, Files: composeFiles, EnvFiles: []string{filepath.Join(dir, "compose.env")}},
		LinuxCLI: linuxCLI,
		Workspaces: map[string]update.WorkspaceConfig{*ws: {
			RuntimeService: "runtime", OperatorTokenFile: filepath.Join(dir, "operator-token"),
			Probe: update.ProbeConfig{URL: "https://127.0.0.1:8080", CAFile: filepath.Join(dir, "ca.crt"), CertFile: filepath.Join(dir, "gateway.crt"), KeyFile: filepath.Join(dir, "gateway.key")},
			Datasets: []update.Dataset{
				{Key: "lensdock-profile", Kind: "profile", Role: "snapshot", MountPath: "/data/profile"},
				{Key: "lensdock-downloads", Kind: "downloads", Role: "retained", MountPath: "/data/downloads"},
				{Key: "lensdock-runtime-policy", Kind: "other", Role: "retained", CandidateAccess: "scratch", MountPath: "/data/runtime"},
				{Key: "runtime-credentials", Kind: "other", Role: "retained", MountPath: "/run/lensdock-credentials"},
			},
		}},
		Gateway: &update.GatewayConfig{Service: "gateway", MigrateArgv: []string{"migrate", "--data-dir", "/data"}, ReadyURL: *origin + "/readyz", Datasets: []update.Dataset{{Key: "gateway-db", Kind: "db", Role: "snapshot", MountPath: "/data/db"}, {Key: "gateway-private", Kind: "private", Role: "retained", MountPath: "/data/private"}, {Key: "gateway-artifacts", Kind: "artifacts", Role: "retained", MountPath: "/data/artifacts"}, {Key: "gateway-credentials", Kind: "other", Role: "retained", MountPath: "/run/lensdock"}}},
	}
	b, err = json.MarshalIndent(dep, "", "  ")
	if err != nil {
		return err
	}
	if err := auth.WritePrivateFile(filepath.Join(root, "deployment.json"), append(b, '\n')); err != nil {
		return err
	}
	complete = true
	if _, err = fmt.Fprintf(c.stdout, "Product configuration: %s\nPairing credentials: admin-token, human-token, viewer-token (private files; never put tokens in URLs).\nAgent credential: agent-token.\nGateway origin: %s\nRuntime mutual-TLS certificates expire in 365 days.\n", dir, *origin); err != nil {
		return err
	}
	if bundle != nil {
		_, err = fmt.Fprintf(c.stdout, "Release %s: Compose file and helpers copied to %s.\nImages (resolved and pinned at the first start): runtime %s, gateway %s.\nNext: lensdock admin product-start --state-dir %s\n", bundle.Version, filepath.Join(dir, releaseDir), *runtimeImage, *gatewayImage, root)
	}
	return err
}

// Single-quoted Compose env values do not interpolate dollar signs.
func composeEnvValue(value string) string { return "'" + strings.ReplaceAll(value, "'", "\\'") + "'" }
func writeRuntimePKI(dir string) error {
	serial := func() (*big.Int, error) { return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128)) }
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	n, err := serial()
	if err != nil {
		return err
	}
	now := time.Now()
	ca := &x509.Certificate{SerialNumber: n, Subject: pkix.Name{CommonName: "LensDock deployment CA"}, NotBefore: now.Add(-5 * time.Minute), NotAfter: now.AddDate(1, 0, 0), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
	der, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		return err
	}
	if err := auth.WritePrivateFile(filepath.Join(dir, "ca.crt"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})); err != nil {
		return err
	}
	for _, name := range []string{"runtime", "gateway"} {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return err
		}
		n, err := serial()
		if err != nil {
			return err
		}
		cert := &x509.Certificate{SerialNumber: n, Subject: pkix.Name{CommonName: "lensdock-" + name}, DNSNames: []string{name, "localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}, NotBefore: now.Add(-5 * time.Minute), NotAfter: now.AddDate(1, 0, 0), KeyUsage: x509.KeyUsageDigitalSignature}
		if name == "runtime" {
			cert.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		} else {
			cert.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
		}
		der, err := x509.CreateCertificate(rand.Reader, cert, ca, &key.PublicKey, caKey)
		if err != nil {
			return err
		}
		if err := auth.WritePrivateFile(filepath.Join(dir, name+".crt"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})); err != nil {
			return err
		}
		keyDER, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			return err
		}
		if err := auth.WritePrivateFile(filepath.Join(dir, name+".key"), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})); err != nil {
			return err
		}
	}
	return nil
}
