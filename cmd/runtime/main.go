// Command runtime is the lensdock-runtime container entrypoint. Build it
// with CGO_ENABLED=0 (privilege dropping uses AllThreadsSyscall):
//
//	CGO_ENABLED=0 go build -o lensdock-runtime ./cmd/runtime
//
// Subcommands: supervise (container entrypoint), healthcheck, restore-browser
// (the desktop menu action, run as the browser user by openbox), and the hidden
// helpers __signal, __preflight-userns, __files and __profile-lock that the
// supervisor re-executes as the browser user.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"

	"github.com/JiaWeiXie/lensdock/internal/netx"
	"github.com/JiaWeiXie/lensdock/internal/supervisor"
	"github.com/JiaWeiXie/lensdock/internal/transfer"
)

const (
	defaultHealthPort       = "8080"
	defaultHealthListen     = "127.0.0.1:8081"
	defaultHealthListenPort = "8081"
)

// healthURL derives the readiness URL the healthcheck probes: the listen
// port of LENSDOCK_LISTEN on loopback (a loopback listen host is kept, any
// other host or an empty one probes 127.0.0.1), path /readyz. An unset or
// malformed value falls back to port 8080. With mTLS configured
// (LENSDOCK_TLS_CERT_FILE set) it probes LENSDOCK_HEALTH_LISTEN instead.
func healthURL(getenv func(string) string) string {
	host, port := "127.0.0.1", defaultHealthPort
	listen := getenv(supervisor.EnvListen)
	if getenv(supervisor.EnvTLSCertFile) != "" {
		// The API listener needs a client certificate; the loopback plain
		// health listener answers the probe instead.
		listen = getenv(supervisor.EnvHealthListen)
		if listen == "" {
			listen = defaultHealthListen
		}
		port = defaultHealthListenPort
	}
	if h, p, err := net.SplitHostPort(listen); err == nil {
		if n, err := strconv.Atoi(p); err == nil && n >= 1 && n <= 65535 {
			port = p
			if netx.IsLoopbackIP(h) {
				host = h
			}
		}
	}
	return "http://" + net.JoinHostPort(host, port) + "/readyz"
}

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}

func run(args []string, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: lensdock-runtime supervise [--mode NOVNC|AGENT] | healthcheck [--url URL] | restore-browser")
		return 2
	}
	switch args[0] {
	case "supervise":
		fs := flag.NewFlagSet("supervise", flag.ContinueOnError)
		fs.SetOutput(stderr)
		mode := fs.String("mode", "", "initial usage mode NOVNC or AGENT (overrides LENSDOCK_MODE; used only until a policy is saved)")
		if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 {
			return 2
		}
		return report(superviseCommand(*mode), stderr)
	case "healthcheck":
		fs := flag.NewFlagSet("healthcheck", flag.ContinueOnError)
		fs.SetOutput(stderr)
		url := fs.String("url", healthURL(os.Getenv), "loopback readiness URL (default derived from LENSDOCK_LISTEN)")
		if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 {
			return 2
		}
		return report(supervisor.RunHealthcheck(context.Background(), *url), stderr)
	case "restore-browser":
		if len(args) != 1 {
			fmt.Fprintln(stderr, "usage: lensdock-runtime restore-browser")
			return 2
		}
		return report(restoreBrowser(context.Background(), chromiumDevToolsAddr, os.Stdout), stderr)
	case "__signal":
		return report(supervisor.RunSignalHelper(args[1:]), stderr)
	case "__preflight-userns":
		return report(supervisor.RunUsernsPreflight(args[1:]), stderr)
	case transfer.HelperName:
		// Runs as the browser user; the exit status encodes the error.
		err := transfer.RunHelper(args[1:], os.Stdin, os.Stdout, downloadsDir)
		if err != nil {
			fmt.Fprintf(stderr, "lensdock-runtime: %v\n", err)
		}
		return transfer.ExitCode(err)
	case supervisor.ProfileLockHelperName:
		err := supervisor.RunProfileLockHelper(os.Stdin, os.Stdout, "")
		if errors.Is(err, supervisor.ErrProfileLocked) {
			fmt.Fprintf(stderr, "lensdock-runtime: %v\n", err)
			return supervisor.ExitProfileLocked
		}
		return report(err, stderr)
	default:
		fmt.Fprintf(stderr, "lensdock-runtime: unknown subcommand %q\n", args[0])
		return 2
	}
}

func report(err error, stderr io.Writer) int {
	if err == nil {
		return 0
	}
	fmt.Fprintf(stderr, "lensdock-runtime: %v\n", err)
	return 1
}
