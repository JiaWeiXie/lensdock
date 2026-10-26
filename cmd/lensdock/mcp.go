package main

import (
	"context"

	"github.com/JiaWeiXie/lensdock/internal/delivery"
	lensmcp "github.com/JiaWeiXie/lensdock/internal/mcp"
)

// mcpUsageText is appended to the CLI usage text.
const mcpUsageText = `  lensdock mcp serve (--workspace W [--token-file F] [--api URL] | --project PATH) [--max-inline-image-bytes N]
      MCP server on stdio (stdout carries only the protocol): feedback_list, feedback_get,
      feedback_image, feedback_ack, feedback_resolve; token as for "feedback"
      --project PATH binds to that managed project's workspace and credential; it ignores a stale $LENSDOCK_API or
      $LENSDOCK_TOKEN_FILE, forbids an explicit --api, --workspace or --token-file, and by default initializes, registers
      and starts the project unless the manager's explicit-start or authorization policy applies
      images are returned inline at any size; --max-inline-image-bytes N (or $LENSDOCK_MCP_MAX_INLINE_IMAGE_BYTES)
      opts in to refusing larger ones with a pointer to "lensdock feedback pull"; the size flags of "feedback" apply too
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
// With --project the connection is resolved once, before the client exists,
// so a later change of any default cannot retarget a running server.
func (c *cli) mcpServe(ctx context.Context, args []string) error {
	fs := c.newFlagSet("mcp serve")
	ff := c.addFeedbackFlags(fs, true, false)
	inlineDefault, err := c.envInt64(envMaxInlineImageBytes)
	if err != nil {
		return err
	}
	maxInline := fs.Int64("max-inline-image-bytes", inlineDefault, "refuse to return screenshots larger than this many bytes as inline MCP images (default $"+envMaxInlineImageBytes+"; 0 means no limit)")
	if _, err := parse(fs, args, 0, 0); err != nil {
		return err
	}
	if err := c.useProject(ctx, ff); err != nil {
		return err
	}
	if ff.workspace == "" {
		return usageErrorf("--workspace or --project is required")
	}
	if err := delivery.ValidateID("workspace ID", ff.workspace); err != nil {
		return usageErrorf("%v", err)
	}
	client, err := c.feedbackClient(ff)
	if err != nil {
		return err
	}
	return lensmcp.Serve(ctx, lensmcp.Config{WorkspaceID: ff.workspace, Backend: client, MaxInlineImageBytes: *maxInline}, c.stdin, c.stdout)
}
