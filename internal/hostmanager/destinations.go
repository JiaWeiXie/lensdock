package hostmanager

import (
	"context"
	"path/filepath"
	"time"

	"github.com/JiaWeiXie/lensdock/internal/gateway"
	"github.com/JiaWeiXie/lensdock/internal/product"
	"github.com/JiaWeiXie/lensdock/internal/update"
)

const withdrawTimeout = 5 * time.Second

// destinationName is the display name of a project's browser destination: the
// base name of its authorized directory, never the path. A name that is not
// displayable yields "", which lets the Gateway derive one from the project ID.
func destinationName(root string) string {
	name := filepath.Base(root)
	if name == string(filepath.Separator) || name == "." || gateway.ValidateDestinationName(name) != nil {
		return ""
	}
	return name
}

// withdrawDestination removes a project's browser destination from the shared
// directory. It is best effort: a project that never started has none, and the
// listing is auxiliary to every lifecycle operation.
func withdrawDestination(ctx context.Context, stateDir string) {
	dep, err := update.LoadDeployment(filepath.Join(stateDir, "deployment.json"))
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, withdrawTimeout)
	defer cancel()
	_ = product.RemoveDestination(ctx, dep)
}
