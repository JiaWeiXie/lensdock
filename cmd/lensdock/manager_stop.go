package main

import (
	"context"

	"github.com/JiaWeiXie/lensdock/internal/hostmanager"
)

func (c *cli) managerShutdown(ctx context.Context, args []string) error {
	fs := c.newFlagSet("admin manager stop")
	if _, err := parse(fs, args, 0, 0); err != nil {
		return err
	}
	root, err := c.managerRoot()
	if err != nil {
		return err
	}
	if err := hostmanager.Shutdown(ctx, root); err != nil {
		return err
	}
	return c.writeJSONLine(map[string]any{"root": root, "stopped": true, "containersStopped": false})
}
