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
	"github.com/JiaWeiXie/lensdock/internal/clipboard"
	"github.com/JiaWeiXie/lensdock/internal/lease"
)

const (
	operatorTokenFile = "operator-token"
	runtimeEnvFile    = "runtime.env"
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
	if err := os.MkdirAll(*dir, 0o700); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}
	tokenPath := filepath.Join(*dir, operatorTokenFile)
	envPath := filepath.Join(*dir, runtimeEnvFile)
	for _, p := range []string{tokenPath, envPath} {
		if _, err := os.Lstat(p); err == nil {
			return fmt.Errorf("%s already exists; refusing to overwrite", p)
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect %s: %w", p, err)
		}
	}
	tok, err := auth.NewSecret("ldo_")
	if err != nil {
		return fmt.Errorf("generate operator token: %w", err)
	}
	if err := auth.WritePrivateFile(tokenPath, []byte(tok.Reveal()+"\n")); err != nil {
		return fmt.Errorf("write operator token: %w", err)
	}
	envLine := "LENSDOCK_OPERATOR_TOKEN_SHA256=" + auth.DigestOf(tok.Reveal()).Hex() + "\n"
	if err := auth.WritePrivateFile(envPath, []byte(envLine)); err != nil {
		_ = os.Remove(tokenPath) // do not leave a token without its runtime.env
		return fmt.Errorf("write runtime.env: %w", err)
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

func (c *cli) leaseAcquire(args []string) error {
	fs := c.newFlagSet("lease acquire")
	mode := fs.String("mode", "", "agent or human")
	ttl := fs.Int("ttl", 30, "lease TTL in seconds (10-120)")
	tokenFile := fs.String("operator-token-file", "", "file holding the operator token")
	output := fs.String("output", "", "lease file to create (mode 0600)")
	feedbackID := fs.String("feedback-id", "", "acknowledged feedback to begin working on (Gateway AGENT lease only)")
	feedbackRevision := fs.Int("feedback-revision", 0, "feedback revision to claim; required when several revisions are active")
	api := c.addAPIFlag(fs)
	pos, err := parse(fs, args, 1, 1)
	if err != nil {
		return err
	}
	m, err := parseMode(*mode)
	if err != nil {
		return err
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
	ws := pos[0]
	client, err := c.newClient(api.value)
	if err != nil {
		return err
	}
	op, err := readTokenFile(*tokenFile)
	if err != nil {
		return err
	}
	// Fail before acquiring if the lease file cannot be created.
	if _, err := os.Lstat(*output); err == nil {
		return fmt.Errorf("%s already exists; refusing to overwrite", *output)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect %s: %w", *output, err)
	}
	key, err := idempotencyKey()
	if err != nil {
		return err
	}
	ctx := context.Background()
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
		// The token would be lost: do not strand the lease.
		_ = client.ReleaseLease(ctx, ws, grant.Lease.LeaseID, token)
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

func (c *cli) leaseRefresh(args []string) error {
	fs := c.newFlagSet("lease refresh")
	input := fs.String("lease-file", "", "existing private lease file")
	output := fs.String("output", "", "new private lease file; existing files are refused")
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
	client, err := c.newClient(lf.APIBaseURL)
	if err != nil {
		return err
	}
	current, err := client.CurrentLease(context.Background(), lf.WorkspaceID, lf.CapabilityToken)
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

func (c *cli) leaseRelease(args []string) error {
	fs := c.newFlagSet("lease release")
	leaseFile := fs.String("lease-file", "", "lease file (releases with the lease's own token)")
	leaseID := fs.String("lease-id", "", "lease ID (operator release)")
	tokenFile := fs.String("operator-token-file", "", "file holding the operator token")
	api := c.addAPIFlag(fs)
	// The workspace is positional only in operator mode, so count it first.
	pos, err := parse(fs, args, 0, 1)
	if err != nil {
		return err
	}
	ctx := context.Background()

	if *leaseFile != "" {
		if len(pos) != 0 || *leaseID != "" || *tokenFile != "" {
			return usageErrorf("--lease-file cannot be combined with <ws>, --lease-id or --operator-token-file")
		}
		lf, err := apiclient.ReadLeaseFile(*leaseFile)
		if err != nil {
			return leaseErrorf("lease file: %v", err)
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

	if len(pos) != 1 || *leaseID == "" {
		return usageErrorf("lease release needs --lease-file, or <ws> with --lease-id and --operator-token-file")
	}
	client, err := c.newClient(api.value)
	if err != nil {
		return err
	}
	op, err := readTokenFile(*tokenFile)
	if err != nil {
		return err
	}
	return client.ReleaseLease(ctx, pos[0], *leaseID, op)
}

func (c *cli) workspaceStatus(args []string) error {
	fs := c.newFlagSet("workspace status")
	tokenFile := fs.String("operator-token-file", "", "file holding the operator token")
	api := c.addAPIFlag(fs)
	pos, err := parse(fs, args, 1, 1)
	if err != nil {
		return err
	}
	client, err := c.newClient(api.value)
	if err != nil {
		return err
	}
	op, err := readTokenFile(*tokenFile)
	if err != nil {
		return err
	}
	ws, err := client.Workspace(context.Background(), pos[0], op)
	if err != nil {
		return err
	}
	return c.writeJSONLine(ws)
}

func (c *cli) clipboardSet(args []string) error {
	fs := c.newFlagSet("clipboard set")
	leaseFile := fs.String("lease-file", "", "HUMAN lease file")
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
	text, err := io.ReadAll(io.LimitReader(c.stdin, clipboard.DefaultMaxBytes+1))
	if err != nil {
		return fmt.Errorf("read stdin: %w", err)
	}
	if len(text) > clipboard.DefaultMaxBytes {
		return fmt.Errorf("clipboard text exceeds %d bytes", clipboard.DefaultMaxBytes)
	}
	client, err := c.newClient(lf.APIBaseURL)
	if err != nil {
		return err
	}
	return client.SetClipboard(context.Background(), lf.WorkspaceID, lf.CapabilityToken, text, lf.Lease.Epoch, lf.Lease.RuntimeGeneration)
}
