package main

import (
	"context"

	"github.com/JiaWeiXie/lensdock/internal/delivery"
	lensmcp "github.com/JiaWeiXie/lensdock/internal/mcp"
)

// mcpUsageText is appended to the CLI usage text.
const mcpUsageText = `  lensdock mcp serve --workspace W [--token-file F] [--api URL]
      MCP server on stdio (stdout carries only the protocol): feedback_list, feedback_get,
      feedback_image, feedback_ack, feedback_resolve; token as for "feedback"
`

// mcpCommand dispatches "lensdock mcp ...".
func (c *cli) mcpCommand(ctx context.Context, args []string) error {
	return c.subcommand(args, "mcp", map[string]func([]string) error{
		"serve": func(a []string) error { return c.mcpServe(ctx, a) },
	})
}

// mcpServe runs the stdio MCP server. It is bound to one workspace and reads
// the bearer token from a private file on every request; nothing but MCP
// frames is written to stdout, diagnostics go to stderr via the error return.
func (c *cli) mcpServe(ctx context.Context, args []string) error {
	fs := c.newFlagSet("mcp serve")
	ff := c.addFeedbackFlags(fs, true, false)
	if _, err := parse(fs, args, 0, 0); err != nil {
		return err
	}
	if ff.workspace == "" {
		return usageErrorf("--workspace is required")
	}
	if err := delivery.ValidateID("workspace ID", ff.workspace); err != nil {
		return usageErrorf("%v", err)
	}
	client, err := c.feedbackClient(ff)
	if err != nil {
		return err
	}
	return lensmcp.Serve(ctx, lensmcp.Config{WorkspaceID: ff.workspace, Backend: client}, c.stdin, c.stdout)
}
