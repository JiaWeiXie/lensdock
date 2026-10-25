// Command lensdock is the operator and agent CLI of a LensDock runtime. It
// never prints operator or lease tokens.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/JiaWeiXie/lensdock/internal/apiclient"
	"github.com/JiaWeiXie/lensdock/internal/auth"
	"github.com/JiaWeiXie/lensdock/internal/update"
)

// Exit codes.
const (
	exitOK          = 0
	exitFailure     = 1
	exitUsage       = 2
	exitLease       = 3 // lease ended, invalid or refused
	exitUnreachable = 4 // runtime could not be reached
)

const (
	defaultAPI = "http://127.0.0.1:18081"
	envAPI     = "LENSDOCK_API"
	usageText  = `usage:
  lensdock admin init --state-dir DIR
  lensdock admin product-init [--state-dir DIR] [--origin http://127.0.0.1:18081] [--mode NOVNC|AGENT] [--project NAME]
      [--runtime-image REF] [--gateway-image REF] [--bundle-dir DIR]
  lensdock admin product-start [--state-dir DIR | --deployment FILE] [--pull always|missing|never]
  lensdock version
  lensdock lease acquire <ws> --mode agent|human [--ttl 30] [--api URL] --operator-token-file F --output LEASEFILE
      --ttl is in seconds, 10 to 120; --output's directory must exist
      [--feedback-id ID --feedback-revision N] binds an agent lease to that acknowledged revision
  lensdock lease release (--lease-file F | <ws> --lease-id ID --operator-token-file F) [--api URL]
  lensdock lease refresh --lease-file F --output NEW_LEASEFILE
  lensdock workspace status <ws> [--api URL] --operator-token-file F
  lensdock workspace watch <ws> [--api URL] [--token-file F]
      streams mode/pause notifications as JSONL; token file defaults to $LENSDOCK_TOKEN_FILE
  lensdock cdp bridge --lease-file F [--listen 127.0.0.1:0] [--url-file F] [--path-secret]
      serves CDP at the loopback root (http://127.0.0.1:PORT); with --path-secret
      every URL carries a random secret path prefix instead
  lensdock clipboard set --lease-file F   (text on stdin)
` + updateUsageText + releaseUsageText + feedbackUsageText + mcpUsageText
)

// cli carries the process environment so commands can be tested.
type cli struct {
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
	getenv func(string) string
	// executable returns the path of the running executable (default
	// os.Executable); tests replace it.
	executable func() (string, error)
	// runner starts docker (default update.ExecRunner); tests replace it.
	runner update.Runner
}

// exitError carries an explicit exit code. An empty message means the cause
// was already reported (for example by the flag package).
type exitError struct {
	code int
	msg  string
}

func (e *exitError) Error() string { return e.msg }

func usageErrorf(format string, args ...any) error {
	return &exitError{code: exitUsage, msg: fmt.Sprintf(format, args...)}
}

func leaseErrorf(format string, args ...any) error {
	return &exitError{code: exitLease, msg: fmt.Sprintf(format, args...)}
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	c := &cli{stdin: os.Stdin, stdout: os.Stdout, stderr: os.Stderr, getenv: os.Getenv}
	os.Exit(c.run(ctx, os.Args[1:]))
}

func (c *cli) run(ctx context.Context, args []string) int {
	err := c.dispatch(ctx, args)
	if err == nil {
		return exitOK
	}
	code := exitCode(err)
	var ee *exitError
	if errors.As(err, &ee) && ee.msg == "" {
		return code
	}
	fmt.Fprintf(c.stderr, "lensdock: %v\n", err)
	if code == exitUsage {
		fmt.Fprint(c.stderr, usageText)
	}
	return code
}

// exitCode classifies err. Terminal API errors (the credential or lease is
// no longer usable) exit 3, transport failures exit 4.
func exitCode(err error) int {
	var ee *exitError
	var apiErr *apiclient.APIError
	switch {
	case errors.As(err, &ee):
		return ee.code
	case errors.Is(err, apiclient.ErrUnreachable):
		return exitUnreachable
	case errors.As(err, &apiErr):
		if apiErr.Terminal() {
			return exitLease
		}
	}
	return exitFailure
}

func (c *cli) dispatch(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return usageErrorf("missing command")
	}
	sub := args[1:]
	switch args[0] {
	case "admin":
		return c.subcommand(sub, "admin", map[string]func([]string) error{"init": c.adminInit, "product-init": c.productInit, "product-start": func(a []string) error { return c.productStart(ctx, a) }})
	case "lease":
		return c.subcommand(sub, "lease", map[string]func([]string) error{
			"acquire": c.leaseAcquire,
			"release": c.leaseRelease,
			"refresh": c.leaseRefresh,
		})
	case "workspace":
		return c.subcommand(sub, "workspace", map[string]func([]string) error{
			"status": c.workspaceStatus,
			"watch":  func(a []string) error { return c.workspaceWatch(ctx, a) },
		})
	case "cdp":
		return c.subcommand(sub, "cdp", map[string]func([]string) error{
			"bridge": func(a []string) error { return c.cdpBridge(ctx, a) },
		})
	case "clipboard":
		return c.subcommand(sub, "clipboard", map[string]func([]string) error{"set": c.clipboardSet})
	case "update":
		return c.subcommand(sub, "update", c.updateSubcommands(ctx))
	case "release":
		return c.subcommand(sub, "release", c.releaseSubcommands(ctx))
	case "feedback":
		return c.feedbackCommand(ctx, sub)
	case "mcp":
		return c.mcpCommand(ctx, sub)
	case "version", "-version", "--version":
		fmt.Fprint(c.stdout, versionText())
		return nil
	case "help", "-h", "--help":
		fmt.Fprint(c.stdout, usageText)
		return nil
	}
	return usageErrorf("unknown command %q", args[0])
}

func (c *cli) subcommand(args []string, group string, cmds map[string]func([]string) error) error {
	if len(args) == 0 {
		return usageErrorf("%s: missing subcommand", group)
	}
	fn, ok := cmds[args[0]]
	if !ok {
		return usageErrorf("%s: unknown subcommand %q", group, args[0])
	}
	return fn(args[1:])
}

// newFlagSet returns a FlagSet whose parse errors are reported once by the
// flag package itself.
func (c *cli) newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	return fs
}

// parse parses args allowing between min and max positional arguments
// before, between or after flags.
func parse(fs *flag.FlagSet, args []string, min, max int) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, &exitError{code: exitOK}
			}
			return nil, &exitError{code: exitUsage}
		}
		if fs.NArg() == 0 {
			break
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
	if len(pos) < min || len(pos) > max {
		return nil, usageErrorf("%s: unexpected number of positional arguments (%d)", fs.Name(), len(pos))
	}
	return pos, nil
}

type apiFlag struct {
	value string
	set   bool
}

// addAPIFlag registers --api. The default comes from LENSDOCK_API.
func (c *cli) addAPIFlag(fs *flag.FlagSet) *apiFlag {
	f := &apiFlag{value: defaultAPI}
	if v := c.getenv(envAPI); v != "" {
		f.value = v
	}
	fs.Func("api", "runtime API base URL (default $LENSDOCK_API or "+defaultAPI+")", func(s string) error {
		f.value, f.set = s, true
		return nil
	})
	return f
}

func (c *cli) newClient(base string) (*apiclient.Client, error) {
	client, err := apiclient.New(base, nil)
	if err != nil {
		return nil, usageErrorf("%v", err)
	}
	return client, nil
}

func readTokenFile(path string) (auth.Secret, error) {
	if path == "" {
		return "", usageErrorf("--operator-token-file is required")
	}
	data, err := auth.ReadPrivateFile(path, 4096)
	if err != nil {
		return "", fmt.Errorf("read operator token file: %w", err)
	}
	tok := strings.TrimSpace(string(data))
	if tok == "" {
		return "", errors.New("operator token file is empty")
	}
	return auth.Secret(tok), nil
}
