package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/JiaWeiXie/lensdock/internal/apiclient"
	"github.com/JiaWeiXie/lensdock/internal/auth"
	"github.com/JiaWeiXie/lensdock/internal/delivery"
	"github.com/JiaWeiXie/lensdock/internal/lease"
)

const (
	defaultAgentLeaseTTL = 30
	releaseTimeout       = 10 * time.Second
	// renewFailuresMax is how many consecutive transport failures of a
	// renewal the command tolerates before it treats the lease as lost.
	renewFailuresMax = 3
)

// agentLeaseUsageText documents the shared lease flags of the commands that
// drive the browser as an Agent.
const agentLeaseUsageText = `      Lease: without --lease-file the command acquires its own AGENT lease with the Agent credential, renews it while it
      runs and releases it when it ends. It never takes control from a human: an acquisition conflict exits 1;
      a lost lease or paused/disabled Agent exits 3. Connection: --project PATH, or --workspace W [--token-file F]
      [--api URL] (defaults $LENSDOCK_TOKEN_FILE and $LENSDOCK_API). --ttl SECONDS (10-120, default 30) sets the lease
      TTL; --feedback-id ID [--feedback-revision N] binds the lease to acknowledged feedback. --lease-file F reuses an
      AGENT lease acquired earlier (advanced; the command renews it while it runs and never releases it).
`

// agentLeaseFlags are the flags shared by every command that needs an AGENT
// lease: the desktop commands and "cdp bridge".
type agentLeaseFlags struct {
	fs               *flag.FlagSet
	leaseFile        string
	api              *apiFlag
	tokenFile        string
	workspace        string
	project          *projectFlag
	ttl              int
	feedbackID       string
	feedbackRevision int
}

func (c *cli) addAgentLeaseFlags(fs *flag.FlagSet) *agentLeaseFlags {
	af := &agentLeaseFlags{fs: fs, api: &apiFlag{value: defaultGatewayAPI}}
	if v := c.getenv(envAPI); v != "" {
		af.api.value = v
	}
	fs.StringVar(&af.leaseFile, "lease-file", "", "reuse this AGENT lease file instead of acquiring a lease")
	fs.Func("api", "Gateway base URL (default $LENSDOCK_API or "+defaultGatewayAPI+")", func(s string) error {
		af.api.value, af.api.set = s, true
		return nil
	})
	fs.StringVar(&af.tokenFile, "token-file", "", "private (0600) file holding the Agent identity token (default $"+envTokenFile+")")
	fs.StringVar(&af.workspace, "workspace", "", "workspace ID")
	fs.IntVar(&af.ttl, "ttl", defaultAgentLeaseTTL, "lease TTL in seconds (10-120)")
	fs.StringVar(&af.feedbackID, "feedback-id", "", "acknowledged feedback to begin working on")
	fs.IntVar(&af.feedbackRevision, "feedback-revision", 0, "feedback revision to claim; required when several revisions are active")
	af.project = addProjectFlag(fs)
	return af
}

// validate checks the flags that need no network. It runs before anything
// contacts a manager, a Gateway or a runtime.
func (af *agentLeaseFlags) validate() error {
	minTTL, maxTTL := int(lease.MinTTL/time.Second), int(lease.MaxTTL/time.Second)
	if af.ttl < minTTL || af.ttl > maxTTL {
		return usageErrorf("--ttl must be between %d and %d seconds", minTTL, maxTTL)
	}
	if af.feedbackRevision < 0 || (af.feedbackRevision != 0 && af.feedbackID == "") {
		return usageErrorf("--feedback-revision must be positive and requires --feedback-id")
	}
	if af.feedbackID != "" {
		if err := validateFeedbackID(af.feedbackID); err != nil {
			return err
		}
	}
	if af.leaseFile != "" {
		explicit := false
		af.fs.Visit(func(f *flag.Flag) {
			switch f.Name {
			case "api", "workspace", "token-file", "feedback-id", "feedback-revision", "ttl":
				explicit = true
			}
		})
		if explicit {
			return usageErrorf("--lease-file cannot be combined with --api, --workspace, --token-file, --ttl, --feedback-id or --feedback-revision")
		}
	}
	return nil
}

// acquireAgentLease acquires one fresh AGENT lease for the Agent identity
// described by the flags. There is no takeover: a workspace held by a human
// or by another lease answers a conflict, and the caller reports it. release
// is idempotent and ends the lease with its own token.
func (c *cli) acquireAgentLease(ctx context.Context, af *agentLeaseFlags) (*apiclient.Client, apiclient.LeaseFile, func(), error) {
	base, ws, tokenPath := af.api.value, af.workspace, af.tokenFile
	conn, err := c.resolveProjectFlag(ctx, af.fs, af.project)
	if err != nil {
		return nil, apiclient.LeaseFile{}, nil, err
	}
	if conn != nil {
		base, ws, tokenPath = conn.API, conn.Workspace, conn.TokenFile
	} else if tokenPath == "" {
		tokenPath = c.getenv(envTokenFile)
	}
	if ws == "" {
		return nil, apiclient.LeaseFile{}, nil, usageErrorf("--project, --workspace or --lease-file is required")
	}
	if err := delivery.ValidateID("workspace ID", ws); err != nil {
		return nil, apiclient.LeaseFile{}, nil, usageErrorf("%v", err)
	}
	if tokenPath == "" {
		return nil, apiclient.LeaseFile{}, nil, usageErrorf("--token-file or $%s is required (or use --project or --lease-file)", envTokenFile)
	}
	identity, err := readIdentityTokenFile(tokenPath)
	if err != nil {
		return nil, apiclient.LeaseFile{}, nil, err
	}
	client, err := c.newClient(base)
	if err != nil {
		return nil, apiclient.LeaseFile{}, nil, err
	}
	key, err := idempotencyKey()
	if err != nil {
		return nil, apiclient.LeaseFile{}, nil, err
	}
	grant, err := client.AcquireLease(ctx, ws, identity, lease.ModeAgent, time.Duration(af.ttl)*time.Second, key, af.feedbackID, af.feedbackRevision)
	if err != nil {
		return nil, apiclient.LeaseFile{}, nil, err
	}
	if grant.CapabilityToken == "" || lease.Mode(grant.Lease.Mode) != lease.ModeAgent {
		return nil, apiclient.LeaseFile{}, nil, errors.New("the Gateway did not return an AGENT lease capability")
	}
	lf := apiclient.LeaseFile{
		Version:         apiclient.LeaseFileVersion,
		APIBaseURL:      client.BaseURL(),
		WorkspaceID:     ws,
		Lease:           grant.Lease,
		CapabilityToken: auth.Secret(grant.CapabilityToken),
	}
	var once sync.Once
	release := func() {
		once.Do(func() {
			rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
			defer cancel()
			// A lease that already ended answers an error; nothing is left to do.
			_ = client.ReleaseLease(rctx, ws, lf.Lease.LeaseID, lf.CapabilityToken)
		})
	}
	return client, lf, release, nil
}

// agentLease is an AGENT lease that one command drives the browser with. The
// context returned by openAgentLease ends as soon as the lease is lost, so
// every channel opened under it dies with the lease.
type agentLease struct {
	client *apiclient.Client
	lf     apiclient.LeaseFile

	release func()
	stop    context.CancelFunc
	done    chan struct{}

	mu   sync.Mutex
	lost error
}

// openAgentLease attaches to the --lease-file lease, or acquires a fresh
// one, and keeps it renewed until close. The returned context is cancelled
// when the lease can no longer be renewed.
func (c *cli) openAgentLease(ctx context.Context, af *agentLeaseFlags) (context.Context, *agentLease, error) {
	if err := af.validate(); err != nil {
		return nil, nil, err
	}
	l := &agentLease{release: func() {}}
	var expires time.Time
	if af.leaseFile != "" {
		lf, err := apiclient.ReadLeaseFile(af.leaseFile)
		if err != nil {
			return nil, nil, leaseErrorf("lease file: %v", err)
		}
		if lease.Mode(lf.Lease.Mode) != lease.ModeAgent {
			return nil, nil, usageErrorf("the desktop commands require an AGENT lease; refusing a %s lease", lf.Lease.Mode)
		}
		if err := c.checkLeaseFileProject(ctx, af.fs, af.project, lf); err != nil {
			return nil, nil, err
		}
		client, err := c.newClient(lf.APIBaseURL)
		if err != nil {
			return nil, nil, err
		}
		// Renewing first proves the lease is still valid for this epoch and
		// generation before any channel is opened with it.
		rctx, cancel := context.WithTimeout(ctx, initialRenewMax)
		renewed, err := client.RenewLease(rctx, lf.WorkspaceID, lf.Lease.LeaseID, lf.CapabilityToken, lf.Lease.Epoch, lf.Lease.RuntimeGeneration)
		cancel()
		if err != nil {
			return nil, nil, err
		}
		l.client, l.lf, expires = client, lf, renewed.ExpiresAt
	} else {
		client, lf, release, err := c.acquireAgentLease(ctx, af)
		if err != nil {
			return nil, nil, err
		}
		l.client, l.lf, l.release, expires = client, lf, release, lf.Lease.ExpiresAt
	}
	ttl := min(max(time.Until(expires), lease.MinTTL), lease.MaxTTL)
	leaseCtx, cancel := context.WithCancel(ctx)
	l.stop = cancel
	l.done = make(chan struct{})
	go l.keepAlive(leaseCtx, cancel, ttl/3)
	return leaseCtx, l, nil
}

// keepAlive renews the lease until ctx ends. A terminal answer or repeated
// transport failures mark the lease lost and cancel every channel opened
// under it.
func (l *agentLease) keepAlive(ctx context.Context, cancel context.CancelFunc, every time.Duration) {
	defer close(l.done)
	t := time.NewTicker(every)
	defer t.Stop()
	failures := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		rctx, rcancel := context.WithTimeout(ctx, initialRenewMax)
		_, err := l.client.RenewLease(rctx, l.lf.WorkspaceID, l.lf.Lease.LeaseID, l.lf.CapabilityToken, l.lf.Lease.Epoch, l.lf.Lease.RuntimeGeneration)
		rcancel()
		switch {
		case err == nil:
			failures = 0
		case ctx.Err() != nil:
			return
		case isTerminal(err):
			l.markLost(fmt.Errorf("lease lost: %w", err))
			cancel()
			return
		default:
			if failures++; failures >= renewFailuresMax {
				l.markLost(fmt.Errorf("lease not renewable: %w", err))
				cancel()
				return
			}
		}
	}
}

func (l *agentLease) markLost(err error) {
	l.mu.Lock()
	l.lost = err
	l.mu.Unlock()
}

// err returns the reason the lease was lost, or nil.
func (l *agentLease) err() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.lost == nil {
		return nil
	}
	return &exitError{code: exitLease, msg: l.lost.Error()}
}

// close stops the renewal and, for a lease this command acquired, ends it.
func (l *agentLease) close() {
	l.stop()
	<-l.done
	l.release()
}

// writeLeaseOut stores a lease for other commands to share with --lease-file.
func writeLeaseOut(path string, lf apiclient.LeaseFile) (remove func(), err error) {
	if err := apiclient.WriteLeaseFile(path, lf); err != nil {
		return nil, err
	}
	return func() { _ = os.Remove(path) }, nil
}
