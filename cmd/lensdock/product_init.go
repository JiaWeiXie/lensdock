package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/JiaWeiXie/lensdock/internal/product"
)

// productError shows an invalid product option as a usage error.
func productError(err error) error {
	var oe *product.OptionError
	if errors.As(err, &oe) {
		return usageErrorf("%s", oe.Msg)
	}
	return err
}

// productInit chooses the flags and defaults of a standalone deployment and
// hands the initialization to internal/product.
func (c *cli) productInit(args []string) error {
	fs := c.newFlagSet("admin product-init")
	state := fs.String("state-dir", c.defaultStateDir(), "private deployment state directory (default $LENSDOCK_STATE_DIR, $HOME/.local/share/lensdock for a release, else .lensdock)")
	originDefault := c.getenv(product.EnvPublicOrigin)
	if originDefault == "" {
		originDefault = product.DefaultOrigin
	}
	origin := fs.String("origin", originDefault, "exact public Gateway origin (default $"+product.EnvPublicOrigin+" or "+product.DefaultOrigin+")")
	ws := fs.String("workspace", product.DefaultWorkspace, "workspace ID")
	mode := fs.String("mode", c.getenv(product.EnvMode), "initial usage mode: NOVNC or AGENT (default AGENT)")
	project := fs.String("project", c.getenv(product.EnvComposeProject), "Compose project name; its Docker volumes hold the data (default $"+product.EnvComposeProject+" or "+product.DefaultProject+")")
	runtimeImage := fs.String("runtime-image", c.getenv(product.EnvRuntimeImage), "Runtime image (default $"+product.EnvRuntimeImage+", else "+product.ReleaseRuntimeImage+" for a release)")
	gatewayImage := fs.String("gateway-image", c.getenv(product.EnvGatewayImage), "Gateway image (default $"+product.EnvGatewayImage+", else "+product.ReleaseGatewayImage+" for a release)")
	bundleDir := fs.String("bundle-dir", "", "release bundle to copy the Compose file and helpers from (default: the bundle of the running executable)")
	requirePairing := fs.Bool("require-pairing", false, "require a pairing token instead of automatic local browser access")
	if _, err := parse(fs, args, 0, 0); err != nil {
		return err
	}
	bundle, err := c.findBundle(*bundleDir)
	if err != nil {
		return err
	}
	// Without a bundle this is a source checkout: the deployment points at the
	// checkout's own files and local :dev images.
	var cwd string
	if bundle == nil {
		if cwd, err = os.Getwd(); err != nil {
			return err
		}
	}
	if *project == "" {
		*project = product.DefaultProject
	}
	defaultRuntime, defaultGateway := product.DefaultImages(bundle)
	if *runtimeImage == "" {
		*runtimeImage = defaultRuntime
	}
	if *gatewayImage == "" {
		*gatewayImage = defaultGateway
	}
	dep, err := product.Initialize(product.InitOptions{
		StateDir: *state, Origin: *origin, WorkspaceID: *ws, Mode: *mode, Project: *project,
		RuntimeImage: *runtimeImage, GatewayImage: *gatewayImage, SourceDir: cwd, Bundle: bundle,
		RequirePairing: *requirePairing,
	})
	if err != nil {
		return productError(err)
	}
	if _, err = fmt.Fprintf(c.stdout, "Product configuration: %s\nPairing credentials: admin-token, human-token, viewer-token (private files; never put tokens in URLs).\nAgent credential: agent-token.\nGateway origin: %s\nRuntime mutual-TLS certificates expire in 365 days.\n", dep.StateDir, *origin); err != nil {
		return err
	}
	if bundle != nil {
		_, err = fmt.Fprintf(c.stdout, "Release %s: Compose file and helpers copied to %s.\nImages (resolved and pinned at the first start): runtime %s, gateway %s.\nNext: lensdock admin product-start --state-dir %s\n", bundle.Version, filepath.Join(dep.StateDir, product.ReleaseDir), *runtimeImage, *gatewayImage, filepath.Dir(dep.File))
	}
	return err
}
