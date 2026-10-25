package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/JiaWeiXie/lensdock/internal/apiwire"
	"github.com/JiaWeiXie/lensdock/internal/auth"
	"github.com/JiaWeiXie/lensdock/internal/delivery"
)

func (c *cli) workspaceWatch(ctx context.Context, args []string) error {
	fs := c.newFlagSet("workspace watch")
	tokenFile := fs.String("token-file", c.getenv("LENSDOCK_TOKEN_FILE"), "private Agent or Human identity token file")
	api := c.addAPIFlag(fs)
	pos, err := parse(fs, args, 1, 1)
	if err != nil {
		return err
	}
	if err := delivery.ValidateID("workspace ID", pos[0]); err != nil {
		return usageErrorf("%v", err)
	}
	if *tokenFile == "" {
		return usageErrorf("--token-file or LENSDOCK_TOKEN_FILE is required")
	}
	data, err := auth.ReadPrivateFile(*tokenFile, 4096)
	if err != nil {
		return fmt.Errorf("read identity token file: %w", err)
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return errors.New("identity token file is empty")
	}
	client, err := c.newClient(api.value)
	if err != nil {
		return err
	}
	err = client.WatchAgentControl(ctx, pos[0], auth.Secret(token), func(state apiwire.AgentControl) error {
		return c.writeJSONLine(state)
	})
	if errors.Is(err, context.Canceled) && ctx.Err() != nil {
		return nil
	}
	return err
}
