package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strconv"
	"time"

	"github.com/JiaWeiXie/lensdock/internal/apiclient"
	"github.com/JiaWeiXie/lensdock/internal/auth"
	"github.com/JiaWeiXie/lensdock/internal/cdp"
	"github.com/JiaWeiXie/lensdock/internal/lease"
)

const (
	initialRenewMax = 15 * time.Second
	endpointTimeout = 30 * time.Second
)

// loopbackListenAddr validates a bridge --listen value. Only 127.0.0.1 (or
// localhost, which is pinned to 127.0.0.1) is accepted because the bridge
// injects the lease token: any local process that reaches the port can use
// the lease, unless --path-secret also requires the secret URL prefix.
func loopbackListenAddr(s string) (string, error) {
	host, port, err := net.SplitHostPort(s)
	if err != nil {
		return "", fmt.Errorf("--listen must be 127.0.0.1:PORT")
	}
	if host != "127.0.0.1" && host != "localhost" {
		return "", errors.New("--listen must bind 127.0.0.1; non-loopback addresses are refused")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 0 || n > 65535 {
		return "", errors.New("--listen has an invalid port")
	}
	return net.JoinHostPort("127.0.0.1", port), nil
}

type bridgeOutput struct {
	DiscoveryURL        string `json:"discoveryUrl"`
	BrowserWebSocketURL string `json:"browserWebSocketUrl"`
	LeaseID             string `json:"leaseId"`
	LeaseFile           string `json:"leaseFile,omitempty"`
}

// bridgeUsageText is appended to the CLI usage text.
const bridgeUsageText = `  lensdock cdp bridge [--lease-file F | --project PATH | --workspace W [--token-file F] [--api URL]] [--listen 127.0.0.1:0]
      [--url-file F] [--path-secret] [--lease-file-out F]
      serves CDP at the loopback root (http://127.0.0.1:PORT); with --path-secret every URL carries a random secret path
      prefix instead. The bridge holds one AGENT lease for its whole lifetime: without --lease-file it acquires the lease,
      renews it and releases it on exit, and every CDP connection ends with that lease. --lease-file-out F also writes
      that lease (mode 0600, removed on exit) so "lensdock desktop ... --lease-file F" drives the native browser UI
      under the same lease at the same time
` + agentLeaseUsageText

func (c *cli) cdpBridge(ctx context.Context, args []string) error {
	fs := c.newFlagSet("cdp bridge")
	af := c.addAgentLeaseFlags(fs)
	listen := fs.String("listen", "127.0.0.1:0", "loopback address to listen on")
	urlFile := fs.String("url-file", "", "also write the output line to this new file (mode 0600)")
	pathSecret := fs.Bool("path-secret", false, "serve under a random secret URL prefix instead of the root")
	leaseOut := fs.String("lease-file-out", "", "write the acquired AGENT lease to this new file (mode 0600) for other commands; removed on exit")
	if _, err := parse(fs, args, 0, 0); err != nil {
		return err
	}
	if err := af.validate(); err != nil {
		return err
	}
	if *leaseOut != "" && af.leaseFile != "" {
		return usageErrorf("--lease-file-out shares a lease the bridge acquired; it cannot be combined with --lease-file")
	}
	addr, err := loopbackListenAddr(*listen)
	if err != nil {
		return usageErrorf("%v", err)
	}
	var (
		client *apiclient.Client
		lf     apiclient.LeaseFile
	)
	if af.leaseFile != "" {
		lf, err = apiclient.ReadLeaseFile(af.leaseFile)
		if err != nil {
			return leaseErrorf("lease file: %v", err)
		}
		if lease.Mode(lf.Lease.Mode) != lease.ModeAgent {
			return usageErrorf("cdp bridge requires an AGENT lease; refusing a %s lease", lf.Lease.Mode)
		}
		if err := c.checkLeaseFileProject(ctx, fs, af.project, lf); err != nil {
			return err
		}
		client, err = c.newClient(lf.APIBaseURL)
		if err != nil {
			return err
		}
	} else {
		var release func()
		client, lf, release, err = c.acquireAgentLease(ctx, af)
		if err != nil {
			return err
		}
		defer release()
		if *leaseOut != "" {
			remove, err := writeLeaseOut(*leaseOut, lf)
			if err != nil {
				return fmt.Errorf("write lease file: %w", err)
			}
			defer remove()
		}
	}
	upstream, err := url.Parse(client.BaseURL() + "/api/v1/workspaces/" + url.PathEscape(lf.WorkspaceID) + "/cdp")
	if err != nil {
		return usageErrorf("invalid workspace id in lease file")
	}

	renew := func(ctx context.Context) (time.Time, error) {
		l, err := client.RenewLease(ctx, lf.WorkspaceID, lf.Lease.LeaseID, lf.CapabilityToken, lf.Lease.Epoch, lf.Lease.RuntimeGeneration)
		return l.ExpiresAt, err
	}

	// Renew once up front: it proves the lease is still valid and yields
	// the TTL the renew loop's interval is derived from.
	rctx, cancel := context.WithTimeout(ctx, initialRenewMax)
	expires, err := renew(rctx)
	cancel()
	if err != nil {
		return err
	}
	ttl := min(max(time.Until(expires), lease.MinTTL), lease.MaxTTL)

	log := slog.New(slog.NewTextHandler(c.stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	bridge, err := cdp.NewBridge(cdp.BridgeConfig{
		Upstream:          upstream,
		Token:             lf.CapabilityToken,
		Epoch:             lf.Lease.Epoch,
		RuntimeGeneration: lf.Lease.RuntimeGeneration,
		Renew:             renew,
		RenewEvery:        ttl / 3,
		IsTerminal:        isTerminal,
		PathSecret:        *pathSecret,
		Logger:            log,
	})
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	defer ln.Close()

	ectx, ecancel := context.WithTimeout(ctx, endpointTimeout)
	eps, err := bridge.Endpoints(ectx, ln.Addr())
	ecancel()
	if err != nil {
		return &exitError{code: exitUnreachable, msg: fmt.Sprintf("discover browser endpoint: %v", err)}
	}
	line, err := json.Marshal(bridgeOutput{DiscoveryURL: eps.DiscoveryURL, BrowserWebSocketURL: eps.BrowserWebSocketURL, LeaseID: lf.Lease.LeaseID, LeaseFile: *leaseOut})
	if err != nil {
		return err
	}
	line = append(line, '\n')
	// With --path-secret the URLs contain the bridge's secret prefix; the
	// file is private (mode 0600) in both modes.
	if *urlFile != "" {
		if err := auth.WritePrivateFile(*urlFile, line); err != nil {
			return fmt.Errorf("write url file: %w", err)
		}
	}
	if _, err := c.stdout.Write(line); err != nil {
		return fmt.Errorf("write stdout: %w", err)
	}

	switch err := bridge.Serve(ctx, ln); {
	case err == nil, errors.Is(err, context.Canceled):
		return nil
	case errors.Is(err, cdp.ErrLeaseLost):
		return leaseErrorf("lease lost: the runtime ended or invalidated the lease")
	default:
		return fmt.Errorf("bridge: %w", err)
	}
}

func isTerminal(err error) bool {
	var apiErr *apiclient.APIError
	return errors.As(err, &apiErr) && apiErr.Terminal()
}
