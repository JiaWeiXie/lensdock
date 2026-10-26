package main

import (
	"context"
	"errors"
	"flag"
	"math"

	"github.com/JiaWeiXie/lensdock/internal/hostmanager"
)

// projectLimits deliberately reconfigures the optional Runtime and Gateway caps
// of one existing project (`lensdock admin project limits`). args are the flags
// after the subcommand. Only the flags that were given change; every omitted
// dimension keeps the project's current value, and an explicit 0 removes that
// cap. The manager refuses a running or queued project, so this command can
// never resize a live browser.
func (c *cli) projectLimits(ctx context.Context, args []string) error {
	fs := c.newFlagSet("admin project limits")
	id := fs.String("id", "", "stable project ID")
	runtimeMem := fs.Int64("runtime-memory-mib", 0, "Runtime memory cap in MiB; 0 removes the cap")
	runtimeCPU := fs.Float64("runtime-cpus", 0, "Runtime CPU cap; 0 removes the cap")
	gatewayMem := fs.Int64("gateway-memory-mib", 0, "Gateway memory cap in MiB; 0 removes the cap")
	gatewayCPU := fs.Float64("gateway-cpus", 0, "Gateway CPU cap; 0 removes the cap")
	if _, err := parse(fs, args, 0, 0); err != nil {
		return err
	}
	if *id == "" {
		return usageErrorf("--id is required")
	}
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
	if !given["runtime-memory-mib"] && !given["runtime-cpus"] && !given["gateway-memory-mib"] && !given["gateway-cpus"] {
		return usageErrorf("at least one of --runtime-memory-mib, --runtime-cpus, --gateway-memory-mib or --gateway-cpus is required")
	}
	for _, n := range []int64{*runtimeMem, *gatewayMem} {
		if n < 0 || n > 1<<30 {
			return usageErrorf("memory quantities must be between 0 and 2^30 MiB")
		}
	}
	for _, n := range []float64{*runtimeCPU, *gatewayCPU} {
		if math.IsNaN(n) || math.IsInf(n, 0) || n < 0 || n > 100000 || n > 0 && n*1e9 < 1 {
			return usageErrorf("CPU quantities must be finite, between 0 and 100000, and at least one nanocpu when positive")
		}
	}
	root, err := c.managerRoot()
	if err != nil {
		return err
	}
	if err := hostmanager.EnsureRunning(ctx, root); err != nil {
		return err
	}
	token := hostmanager.AdminTokenFile(root)
	current, err := hostmanager.Call(ctx, root, token, hostmanager.Request{Action: hostmanager.ActionStatus, ProjectID: *id})
	if err != nil {
		return err
	}
	if current.Project == nil {
		return errors.New("manager returned no project")
	}
	observed := current.Project.Limits
	limits := observed
	if given["runtime-memory-mib"] {
		limits.Runtime.MemoryBytes = *runtimeMem << 20
	}
	if given["runtime-cpus"] {
		limits.Runtime.NanoCPUs = int64(math.Round(*runtimeCPU * 1e9))
	}
	if given["gateway-memory-mib"] {
		limits.Gateway.MemoryBytes = *gatewayMem << 20
	}
	if given["gateway-cpus"] {
		limits.Gateway.NanoCPUs = int64(math.Round(*gatewayCPU * 1e9))
	}
	// The manager applies the change only if the project's limits still equal
	// the snapshot these edits were computed from, so a concurrent change to
	// another dimension is refused instead of silently overwritten.
	resp, err := hostmanager.Call(ctx, root, token, hostmanager.Request{Action: hostmanager.ActionLimits, ProjectID: *id, Limits: &limits, ExpectedLimits: &observed})
	if err != nil {
		return err
	}
	return c.writeJSONLine(resp)
}
