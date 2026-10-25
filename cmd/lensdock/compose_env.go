package main

import (
	"github.com/JiaWeiXie/lensdock/internal/product"
	"github.com/JiaWeiXie/lensdock/internal/update"
)

// baseRunner starts external commands (update.ExecRunner unless a test
// replaced it).
func (c *cli) baseRunner() update.Runner {
	if c.runner != nil {
		return c.runner
	}
	return update.ExecRunner{}
}

// deploymentDocker is the Docker wrapper for commands that operate dep: the
// standalone CLI runs the docker binary of the caller's PATH and environment.
func (c *cli) deploymentDocker(dep *update.Deployment) update.Docker {
	return product.DeploymentDocker(dep, c.baseRunner())
}
