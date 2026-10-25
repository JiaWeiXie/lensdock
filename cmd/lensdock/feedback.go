package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/JiaWeiXie/lensdock/internal/delivery"
)

const (
	// defaultGatewayAPI is the Gateway of the full product; $LENSDOCK_API
	// overrides it like for the other commands.
	defaultGatewayAPI = "http://127.0.0.1:18081"
	envTokenFile      = "LENSDOCK_TOKEN_FILE"
	maxTextArgBytes   = 64 << 10
)

// feedbackUsageText is appended to the CLI usage text.
const feedbackUsageText = `  lensdock feedback list --workspace W [--state S] [--cursor C] [--token-file F] [--api URL]
  lensdock feedback get <id> (--revision N | --workspace W) [--token-file F] [--api URL]
  lensdock feedback pull <id> --output NEWDIR (--revision N | --workspace W) [--token-file F] [--api URL]
      downloads and hash-verifies the share bundle into NEWDIR, which must not exist;
      the bundle appears atomically or not at all. Pulling never acknowledges.
  lensdock feedback ack <id> --image-viewed [--bundle DIR | --revision N | --workspace W] [--token-file F] [--api URL]
      --image-viewed asserts you actually looked at the screenshot; --bundle verifies a pulled bundle first
  lensdock feedback resolve <id> (--revision N | --workspace W) (--summary T | --summary-file F|-)
      (--verification T | --verification-file F|-) [--result-feedback-id ID]... [--token-file F] [--api URL]
      the bearer token is only ever read from a private (0600) token file, --token-file or $LENSDOCK_TOKEN_FILE;
      --api defaults to $LENSDOCK_API or ` + defaultGatewayAPI + `
`

// feedbackCommand dispatches "lensdock feedback ...".
func (c *cli) feedbackCommand(ctx context.Context, args []string) error {
	return c.subcommand(args, "feedback", map[string]func([]string) error{
		"list":    func(a []string) error { return c.feedbackList(ctx, a) },
		"get":     func(a []string) error { return c.feedbackGet(ctx, a) },
		"pull":    func(a []string) error { return c.feedbackPull(ctx, a) },
		"ack":     func(a []string) error { return c.feedbackAck(ctx, a) },
		"resolve": func(a []string) error { return c.feedbackResolve(ctx, a) },
	})
}

// feedbackFlags are the flags every feedback command shares.
type feedbackFlags struct {
	api       *apiFlag
	tokenFile string
	workspace string
	revision  int
}

func (c *cli) addFeedbackFlags(fs *flag.FlagSet, workspace, revision bool) *feedbackFlags {
	ff := &feedbackFlags{api: &apiFlag{value: defaultGatewayAPI}}
	if v := c.getenv(envAPI); v != "" {
		ff.api.value = v
	}
	fs.Func("api", "Gateway base URL (default $LENSDOCK_API or "+defaultGatewayAPI+")", func(s string) error {
		ff.api.value, ff.api.set = s, true
		return nil
	})
	fs.StringVar(&ff.tokenFile, "token-file", "", "private (0600) file holding the bearer token (default $"+envTokenFile+")")
	if workspace {
		fs.StringVar(&ff.workspace, "workspace", "", "workspace ID")
	}
	if revision {
		fs.IntVar(&ff.revision, "revision", 0, "revision number (default: the revision the server assigned to you)")
	}
	return ff
}

// client builds the authenticated Gateway client. The token comes from a
// private file only, never from argv or the environment; a missing or
// non-private file fails here, before any request.
func (c *cli) feedbackClient(ff *feedbackFlags) (*delivery.Client, error) {
	path := ff.tokenFile
	if path == "" {
		path = c.getenv(envTokenFile)
	}
	if path == "" {
		return nil, usageErrorf("--token-file (or $%s) is required", envTokenFile)
	}
	tokens := delivery.FileTokenSource(path)
	if _, err := tokens(); err != nil {
		return nil, err
	}
	client, err := delivery.New(ff.api.value, tokens, nil)
	if err != nil {
		return nil, usageErrorf("%v", err)
	}
	return client, nil
}

// feedbackID validates the one positional feedback ID.
func feedbackID(pos []string) (string, error) {
	if err := validateFeedbackID(pos[0]); err != nil {
		return "", err
	}
	return pos[0], nil
}

// validateFeedbackID is shared with `lease acquire --feedback-id`.
func validateFeedbackID(id string) error {
	if err := delivery.ValidateID("feedback ID", id); err != nil {
		return usageErrorf("%v", err)
	}
	return nil
}

// revisionFor returns the explicit revision, or asks the server which one it
// assigned to this token. It never guesses "latest".
func (c *cli) revisionFor(ctx context.Context, client *delivery.Client, ff *feedbackFlags, id string) (int, error) {
	if ff.revision < 0 {
		return 0, usageErrorf("--revision must be at least 1")
	}
	if ff.revision > 0 {
		return ff.revision, nil
	}
	if ff.workspace == "" {
		return 0, usageErrorf("--revision, or --workspace to look up the revision assigned to you, is required")
	}
	return client.AssignedRevision(ctx, ff.workspace, id)
}

func (c *cli) feedbackList(ctx context.Context, args []string) error {
	fs := c.newFlagSet("feedback list")
	ff := c.addFeedbackFlags(fs, true, false)
	state := fs.String("state", "", "only this delivery state")
	cursor := fs.String("cursor", "", "nextCursor of the previous page")
	if _, err := parse(fs, args, 0, 0); err != nil {
		return err
	}
	if ff.workspace == "" {
		return usageErrorf("--workspace is required")
	}
	client, err := c.feedbackClient(ff)
	if err != nil {
		return err
	}
	l, err := client.List(ctx, delivery.ListQuery{WorkspaceID: ff.workspace, State: *state, Cursor: *cursor})
	if err != nil {
		return err
	}
	return c.writeJSONLine(l)
}

func (c *cli) feedbackGet(ctx context.Context, args []string) error {
	fs := c.newFlagSet("feedback get")
	ff := c.addFeedbackFlags(fs, true, true)
	pos, err := parse(fs, args, 1, 1)
	if err != nil {
		return err
	}
	id, err := feedbackID(pos)
	if err != nil {
		return err
	}
	client, err := c.feedbackClient(ff)
	if err != nil {
		return err
	}
	rev, err := c.revisionFor(ctx, client, ff, id)
	if err != nil {
		return err
	}
	f, err := client.Get(ctx, id, rev)
	if err != nil {
		return err
	}
	var line bytes.Buffer
	if err := json.Compact(&line, f.Raw); err != nil {
		return fmt.Errorf("encode feedback: %w", err)
	}
	line.WriteByte('\n')
	_, err = c.stdout.Write(line.Bytes())
	return err
}

func (c *cli) feedbackPull(ctx context.Context, args []string) error {
	fs := c.newFlagSet("feedback pull")
	ff := c.addFeedbackFlags(fs, true, true)
	output := fs.String("output", "", "new directory to create (must not exist; its parent must)")
	pos, err := parse(fs, args, 1, 1)
	if err != nil {
		return err
	}
	if *output == "" {
		return usageErrorf("--output is required")
	}
	id, err := feedbackID(pos)
	if err != nil {
		return err
	}
	if ff.revision < 0 {
		return usageErrorf("--revision must be at least 1")
	}
	if ff.revision == 0 && ff.workspace == "" {
		return usageErrorf("--revision, or --workspace to look up the revision assigned to you, is required")
	}
	client, err := c.feedbackClient(ff)
	if err != nil {
		return err
	}
	res, err := client.Pull(ctx, delivery.PullOptions{
		WorkspaceID: ff.workspace, FeedbackID: id, Revision: ff.revision, Output: *output,
	})
	if err != nil {
		return err
	}
	return c.writeJSONLine(map[string]any{
		"dir":                    res.Dir,
		"feedbackId":             res.Manifest.FeedbackID,
		"revision":               res.Manifest.Revision,
		"workspaceId":            res.Manifest.WorkspaceID,
		"files":                  res.Manifest.Files,
		"verified":               true,
		"acknowledged":           false,
		"untrustedContentNotice": delivery.UntrustedNotice,
		"next": "open " + filepath.Join(res.Dir, delivery.FileAnnotated) + " and read task.md, then: lensdock feedback ack " +
			id + " --image-viewed --bundle " + res.Dir,
	})
}

func (c *cli) feedbackAck(ctx context.Context, args []string) error {
	fs := c.newFlagSet("feedback ack")
	ff := c.addFeedbackFlags(fs, true, true)
	bundle := fs.String("bundle", "", "pulled bundle directory to verify; supplies the revision")
	viewed := fs.Bool("image-viewed", false, "assert that you actually looked at the screenshot")
	pos, err := parse(fs, args, 1, 1)
	if err != nil {
		return err
	}
	id, err := feedbackID(pos)
	if err != nil {
		return err
	}
	if !*viewed {
		return usageErrorf("--image-viewed is required: acknowledge only after you actually looked at the screenshot " +
			"(`lensdock feedback pull`, then open screenshot.annotated.png)")
	}
	if ff.revision < 0 {
		return usageErrorf("--revision must be at least 1")
	}
	if *bundle != "" {
		m, err := delivery.VerifyBundle(*bundle)
		if err != nil {
			return fmt.Errorf("bundle %s: %w", *bundle, err)
		}
		if m.FeedbackID != id || (ff.revision != 0 && ff.revision != m.Revision) || (ff.workspace != "" && ff.workspace != m.WorkspaceID) {
			return fmt.Errorf("bundle %s holds feedback %s revision %d of workspace %s, which does not match the request",
				*bundle, m.FeedbackID, m.Revision, m.WorkspaceID)
		}
		ff.revision, ff.workspace = m.Revision, m.WorkspaceID
	}
	client, err := c.feedbackClient(ff)
	if err != nil {
		return err
	}
	rev, err := c.revisionFor(ctx, client, ff, id)
	if err != nil {
		return err
	}
	d, err := client.Ack(ctx, id, rev)
	if err != nil {
		return err
	}
	return c.writeJSONLine(d)
}

func (c *cli) feedbackResolve(ctx context.Context, args []string) error {
	fs := c.newFlagSet("feedback resolve")
	ff := c.addFeedbackFlags(fs, true, true)
	summary := fs.String("summary", "", "what you changed")
	summaryFile := fs.String("summary-file", "", "file (or - for stdin) holding the summary")
	verification := fs.String("verification", "", "the verification you actually performed")
	verificationFile := fs.String("verification-file", "", "file (or - for stdin) holding the verification")
	var results []string
	fs.Func("result-feedback-id", "published feedback holding new evidence (repeatable, at most 10)", func(s string) error {
		results = append(results, s)
		return nil
	})
	pos, err := parse(fs, args, 1, 1)
	if err != nil {
		return err
	}
	id, err := feedbackID(pos)
	if err != nil {
		return err
	}
	if *summaryFile == "-" && *verificationFile == "-" {
		return usageErrorf("only one of --summary-file and --verification-file can read stdin")
	}
	sum, err := c.textArg("summary", *summary, *summaryFile)
	if err != nil {
		return err
	}
	ver, err := c.textArg("verification", *verification, *verificationFile)
	if err != nil {
		return err
	}
	res := delivery.Resolution{Summary: sum, Verification: ver, ResultFeedbackIDs: results}
	if err := res.Validate(); err != nil {
		return usageErrorf("%v", err)
	}
	client, err := c.feedbackClient(ff)
	if err != nil {
		return err
	}
	rev, err := c.revisionFor(ctx, client, ff, id)
	if err != nil {
		return err
	}
	d, err := client.Resolve(ctx, id, rev, res)
	if err != nil {
		return err
	}
	return c.writeJSONLine(d)
}

// textArg returns the inline value or the content of the file ("-" is
// stdin). Exactly one source is required.
func (c *cli) textArg(name, value, file string) (string, error) {
	if (value == "") == (file == "") {
		return "", usageErrorf("exactly one of --%s and --%s-file is required", name, name)
	}
	if file == "" {
		return value, nil
	}
	var r io.Reader = c.stdin
	if file != "-" {
		f, err := os.Open(file)
		if err != nil {
			return "", fmt.Errorf("read --%s-file: %w", name, err)
		}
		defer f.Close()
		r = f
	}
	data, err := io.ReadAll(io.LimitReader(r, maxTextArgBytes+1))
	if err != nil {
		return "", fmt.Errorf("read %s: %w", name, err)
	}
	if len(data) > maxTextArgBytes {
		return "", usageErrorf("%s exceeds %d bytes", name, maxTextArgBytes)
	}
	text := strings.TrimRight(string(data), "\r\n")
	if text == "" {
		return "", errors.New(name + " is empty")
	}
	return text, nil
}
