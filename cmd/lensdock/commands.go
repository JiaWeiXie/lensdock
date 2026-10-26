package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/JiaWeiXie/lensdock/internal/apiclient"
	"github.com/JiaWeiXie/lensdock/internal/auth"
	"github.com/JiaWeiXie/lensdock/internal/lease"
	"github.com/JiaWeiXie/lensdock/internal/product"
)

func (c *cli) adminInit(args []string) error {
	fs := c.newFlagSet("admin init")
	dir := fs.String("state-dir", "", "directory for operator-token and runtime.env")
	if _, err := parse(fs, args, 0, 0); err != nil {
		return err
	}
	if *dir == "" {
		return usageErrorf("--state-dir is required")
	}
	tokenPath := filepath.Join(*dir, product.OperatorTokenFile)
	envPath := filepath.Join(*dir, product.RuntimeEnvFile)
	// Explicit initialization never adopts credentials; product-init reuses
	// them through product.EnsureOperatorCredentials directly.
	for _, p := range []string{tokenPath, envPath} {
		if _, err := os.Lstat(p); err == nil {
			return fmt.Errorf("%s already exists; refusing to overwrite", p)
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect %s: %w", p, err)
		}
	}
	if _, err := product.EnsureOperatorCredentials(*dir); err != nil {
		return err
	}
	fmt.Fprintf(c.stdout, "wrote %s and %s (mode 0600)\n", tokenPath, envPath)
	return nil
}

func parseMode(s string) (lease.Mode, error) {
	switch strings.ToLower(s) {
	case "agent":
		return lease.ModeAgent, nil
	case "human":
		return lease.ModeHuman, nil
	}
	return "", usageErrorf("--mode must be agent or human")
}

func idempotencyKey() (string, error) {
	id, err := auth.RandomHex(16)
	if err != nil {
		return "", fmt.Errorf("generate idempotency key: %w", err)
	}
	return "cli-" + id, nil
}

func (c *cli) writeJSONLine(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(c.stdout, "%s\n", data)
	return err
}

func (c *cli) leaseAcquire(ctx context.Context, args []string) error {
	fs := c.newFlagSet("lease acquire")
	mode := fs.String("mode", "", "agent or human")
	ttl := fs.Int("ttl", 30, "lease TTL in seconds (10-120)")
	tokenFile := fs.String("operator-token-file", "", "file holding the operator token")
	output := fs.String("output", "", "lease file to create (mode 0600)")
	feedbackID := fs.String("feedback-id", "", "acknowledged feedback to begin working on (Gateway AGENT lease only)")
	feedbackRevision := fs.Int("feedback-revision", 0, "feedback revision to claim; required when several revisions are active")
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
	m, err := parseMode(*mode)
	if err != nil {
		return err
	}
	if pf.set && m != lease.ModeAgent {
		return usageErrorf("--project acquires with the project's Agent identity; only --mode agent is available")
	}
	if *feedbackRevision < 0 || (*feedbackRevision != 0 && *feedbackID == "") {
		return usageErrorf("--feedback-revision must be positive and requires --feedback-id")
	}
	if *feedbackID != "" {
		if m != lease.ModeAgent {
			return usageErrorf("--feedback-id requires --mode agent")
		}
		if err := validateFeedbackID(*feedbackID); err != nil {
			return err
		}
	}
	minTTL, maxTTL := int(lease.MinTTL/time.Second), int(lease.MaxTTL/time.Second)
	if *ttl < minTTL || *ttl > maxTTL {
		return usageErrorf("--ttl must be between %d and %d seconds", minTTL, maxTTL)
	}
	if *output == "" {
		return usageErrorf("--output is required")
	}
	// A missing directory would only surface after the lease was acquired.
	if fi, err := os.Stat(filepath.Dir(*output)); err != nil || !fi.IsDir() {
		return usageErrorf("--output directory %s does not exist", filepath.Dir(*output))
	}
	// Fail before acquiring if the lease file cannot be created.
	if _, err := os.Lstat(*output); err == nil {
		return fmt.Errorf("%s already exists; refusing to overwrite", *output)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect %s: %w", *output, err)
	}
	conn, err := c.resolveProjectFlag(ctx, fs, pf)
	if err != nil {
		return err
	}
	base := api.value
	if conn != nil {
		base, ws = conn.API, conn.Workspace
	}
	client, err := c.newClient(base)
	if err != nil {
		return err
	}
	var op auth.Secret
	if conn != nil {
		op, err = readIdentityTokenFile(conn.TokenFile)
	} else {
		op, err = readTokenFile(*tokenFile)
	}
	if err != nil {
		return err
	}
	key, err := idempotencyKey()
	if err != nil {
		return err
	}
	grant, err := client.AcquireLease(ctx, ws, op, m, time.Duration(*ttl)*time.Second, key, *feedbackID, *feedbackRevision)
	if err != nil {
		return err
	}
	token := auth.Secret(grant.CapabilityToken)
	werr := apiclient.WriteLeaseFile(*output, apiclient.LeaseFile{
		Version:         apiclient.LeaseFileVersion,
		APIBaseURL:      client.BaseURL(),
		WorkspaceID:     ws,
		Lease:           grant.Lease,
		CapabilityToken: token,
	})
	if werr != nil {
		// The token would be lost: do not strand the lease, even when the
		// command was interrupted meanwhile.
		_ = client.ReleaseLease(context.WithoutCancel(ctx), ws, grant.Lease.LeaseID, token)
		return fmt.Errorf("write lease file: %w", werr)
	}
	return c.writeJSONLine(map[string]any{
		"leaseId":   grant.Lease.LeaseID,
		"mode":      grant.Lease.Mode,
		"epoch":     grant.Lease.Epoch,
		"expiresAt": grant.Lease.ExpiresAt,
		"leaseFile": *output,
	})
}

func (c *cli) leaseRefresh(ctx context.Context, args []string) error {
	fs := c.newFlagSet("lease refresh")
	input := fs.String("lease-file", "", "existing private lease file")
	output := fs.String("output", "", "new private lease file; existing files are refused")
	pf := addProjectFlag(fs)
	if _, err := parse(fs, args, 0, 0); err != nil {
		return err
	}
	if *input == "" || *output == "" {
		return usageErrorf("--lease-file and --output are required")
	}
	lf, err := apiclient.ReadLeaseFile(*input)
	if err != nil {
		return leaseErrorf("lease file: %v", err)
	}
	if err := c.checkLeaseFileProject(ctx, fs, pf, lf); err != nil {
		return err
	}
	client, err := c.newClient(lf.APIBaseURL)
	if err != nil {
		return err
	}
	current, err := client.CurrentLease(ctx, lf.WorkspaceID, lf.CapabilityToken)
	if err != nil {
		return err
	}
	if current.WorkspaceID != lf.WorkspaceID || current.HolderID != lf.Lease.HolderID || current.Mode != lf.Lease.Mode || current.RuntimeGeneration != lf.Lease.RuntimeGeneration || current.Epoch < lf.Lease.Epoch {
		return leaseErrorf("current grant does not match the original lease holder and generation")
	}
	lf.Lease = current
	if err := apiclient.WriteLeaseFile(*output, lf); err != nil {
		return err
	}
	return c.writeJSONLine(current)
}

func (c *cli) leaseRelease(ctx context.Context, args []string) error {
	fs := c.newFlagSet("lease release")
	leaseFile := fs.String("lease-file", "", "lease file (releases with the lease's own token)")
	leaseID := fs.String("lease-id", "", "lease ID (operator release)")
	tokenFile := fs.String("operator-token-file", "", "file holding the operator token")
	api := c.addAPIFlag(fs)
	pf := addProjectFlag(fs)
	// The workspace is positional only in operator mode, so count it first.
	pos, err := parse(fs, args, 0, 1)
	if err != nil {
		return err
	}

	if *leaseFile != "" {
		if len(pos) != 0 || *leaseID != "" || *tokenFile != "" {
			return usageErrorf("--lease-file cannot be combined with <ws>, --lease-id or --operator-token-file")
		}
		lf, err := apiclient.ReadLeaseFile(*leaseFile)
		if err != nil {
			return leaseErrorf("lease file: %v", err)
		}
		if err := c.checkLeaseFileProject(ctx, fs, pf, lf); err != nil {
			return err
		}
		base := lf.APIBaseURL
		if api.set {
			base = api.value
		}
		client, err := c.newClient(base)
		if err != nil {
			return err
		}
		return client.ReleaseLease(ctx, lf.WorkspaceID, lf.Lease.LeaseID, lf.CapabilityToken)
	}

	if *leaseID == "" {
		return usageErrorf("lease release needs --lease-file, or (<ws> or --project) with --lease-id and its credential")
	}
	ws, err := projectWorkspaceArg(pf, pos)
	if err != nil {
		return err
	}
	conn, err := c.resolveProjectFlag(ctx, fs, pf)
	if err != nil {
		return err
	}
	base := api.value
	if conn != nil {
		base, ws = conn.API, conn.Workspace
	}
	client, err := c.newClient(base)
	if err != nil {
		return err
	}
	var op auth.Secret
	if conn != nil {
		op, err = readIdentityTokenFile(conn.TokenFile)
	} else {
		op, err = readTokenFile(*tokenFile)
	}
	if err != nil {
		return err
	}
	return client.ReleaseLease(ctx, ws, *leaseID, op)
}

func (c *cli) workspaceStatus(ctx context.Context, args []string) error {
	fs := c.newFlagSet("workspace status")
	tokenFile := fs.String("operator-token-file", "", "file holding the operator token")
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
	conn, err := c.resolveProjectFlag(ctx, fs, pf)
	if err != nil {
		return err
	}
	base := api.value
	if conn != nil {
		base, ws = conn.API, conn.Workspace
	}
	client, err := c.newClient(base)
	if err != nil {
		return err
	}
	var op auth.Secret
	if conn != nil {
		op, err = readIdentityTokenFile(conn.TokenFile)
	} else {
		op, err = readTokenFile(*tokenFile)
	}
	if err != nil {
		return err
	}
	state, err := client.Workspace(ctx, ws, op)
	if err != nil {
		return err
	}
	return c.writeJSONLine(state)
}

func (c *cli) clipboardSet(ctx context.Context, args []string) error {
	fs := c.newFlagSet("clipboard set")
	leaseFile := fs.String("lease-file", "", "HUMAN or AGENT lease file")
	pf := addProjectFlag(fs)
	if _, err := parse(fs, args, 0, 0); err != nil {
		return err
	}
	if *leaseFile == "" {
		return usageErrorf("--lease-file is required")
	}
	lf, err := apiclient.ReadLeaseFile(*leaseFile)
	if err != nil {
		return leaseErrorf("lease file: %v", err)
	}
	text, err := io.ReadAll(c.stdin)
	if err != nil {
		return fmt.Errorf("read stdin: %w", err)
	}
	if err := c.checkLeaseFileProject(ctx, fs, pf, lf); err != nil {
		return err
	}
	client, err := c.newClient(lf.APIBaseURL)
	if err != nil {
		return err
	}
	return client.SetClipboard(ctx, lf.WorkspaceID, lf.CapabilityToken, text, lf.Lease.Epoch, lf.Lease.RuntimeGeneration)
}
