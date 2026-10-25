package main

import (
	"context"
	"flag"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/JiaWeiXie/lensdock/internal/product"
	"github.com/JiaWeiXie/lensdock/internal/update"
)

// productStart starts the product of an initialized deployment through
// internal/product, which owns the pinning, ownership and readiness rules.
func (c *cli) productStart(ctx context.Context, args []string) error {
	fs := c.newFlagSet("admin product-start")
	df := c.addDeploymentFlags(fs)
	pullDefault := c.getenv(product.EnvPull)
	if pullDefault == "" {
		pullDefault = product.PullAlways
	}
	pull := fs.String("pull", pullDefault, "image pull policy of the first start: always, missing or never (default $"+product.EnvPull+" or always)")
	if _, err := parse(fs, args, 0, 0); err != nil {
		return err
	}
	depPath, err := df.path()
	if err != nil {
		return err
	}
	dep, err := update.LoadDeployment(depPath)
	if err != nil {
		return err
	}
	if dep.ManagedBy != nil {
		explicitPull := false
		fs.Visit(func(f *flag.Flag) {
			if f.Name == "pull" {
				explicitPull = true
			}
		})
		if explicitPull || c.getenv(product.EnvPull) != "" {
			return usageErrorf("managed deployments use the manager's pull policy; omit --pull and LENSDOCK_PULL")
		}
		return c.startManagedProduct(ctx, dep)
	}
	if err := product.Start(ctx, dep, c.deploymentDocker(dep), product.StartOptions{PullPolicy: *pull}, c.stderr); err != nil {
		return productError(err)
	}
	if dep.Gateway == nil || dep.Gateway.ReadyURL == "" {
		return nil
	}
	_, err = fmt.Fprintf(c.stdout, "LensDock is ready at %s/\nPair with the contents of the private file %s (never put a token in a URL or command line).\n", strings.TrimSuffix(dep.Gateway.ReadyURL, "/readyz"), filepath.Join(dep.StateDir, "admin-token"))
	return err
}
