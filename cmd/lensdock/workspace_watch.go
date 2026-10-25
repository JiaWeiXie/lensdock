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

// readIdentityTokenFile reads a private Agent or Human identity token file.
func readIdentityTokenFile(path string) (auth.Secret, error) {
	data, err := auth.ReadPrivateFile(path, 4096)
	if err != nil {
		return "", fmt.Errorf("read identity token file: %w", err)
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", errors.New("identity token file is empty")
	}
	return auth.Secret(token), nil
}

func (c *cli) workspaceWatch(ctx context.Context, args []string) error {
	fs := c.newFlagSet("workspace watch")
	tokenFile := fs.String("token-file", c.getenv(envTokenFile), "private Agent or Human identity token file")
	api := c.addAPIFlag(fs)
	pf := addProjectFlag(fs)
	pos, err := parse(fs, args, 0, 1)
	if err != nil {
		return err
	}
	ws, err := projectWorkspaceArg(pf, pos)
	if err != nil {
		return err
	}
	base, tokenPath := api.value, *tokenFile
	conn, err := c.resolveProjectFlag(ctx, fs, pf)
	if err != nil {
		return err
	}
	if conn != nil {
		base, tokenPath, ws = conn.API, conn.TokenFile, conn.Workspace
	}
	if err := delivery.ValidateID("workspace ID", ws); err != nil {
		return usageErrorf("%v", err)
	}
	if tokenPath == "" {
		return usageErrorf("--token-file or LENSDOCK_TOKEN_FILE is required")
	}
	token, err := readIdentityTokenFile(tokenPath)
	if err != nil {
		return err
	}
	client, err := c.newClient(base)
	if err != nil {
		return err
	}
	err = client.WatchAgentControl(ctx, ws, token, func(state apiwire.AgentControl) error {
		return c.writeJSONLine(state)
	})
	if errors.Is(err, context.Canceled) && ctx.Err() != nil {
		return nil
	}
	return err
}
