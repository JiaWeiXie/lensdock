package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"

	"github.com/JiaWeiXie/lensdock/internal/apiclient"
	"github.com/JiaWeiXie/lensdock/internal/hostmanager"
)

// manualConnectionFlags select a runtime API, workspace or credential by
// hand. --project supplies all three, so combining them is ambiguous and is
// refused instead of letting one silently override the other.
var manualConnectionFlags = []string{"api", "workspace", "token-file", "operator-token-file"}

// projectFlag is the value of --project PATH.
type projectFlag struct {
	path string
	set  bool
}

func addProjectFlag(fs *flag.FlagSet) *projectFlag {
	pf := &projectFlag{}
	fs.Func("project", "managed LensDock project directory; supplies the API URL, workspace and credential", func(s string) error {
		if s == "" {
			return errors.New("the project path must not be empty")
		}
		pf.path, pf.set = s, true
		return nil
	})
	return pf
}

// resolveProjectFlag resolves --project exactly once and returns nil when the
// flag is absent. Manual connection flags are refused before the manager is
// contacted, and nothing here reads a Gateway credential.
func (c *cli) resolveProjectFlag(ctx context.Context, fs *flag.FlagSet, pf *projectFlag) (*hostmanager.Connection, error) {
	if !pf.set {
		return nil, nil
	}
	if err := c.rejectProjectOverrides(fs); err != nil {
		return nil, err
	}
	conn, err := c.projectConnection(ctx, pf.path)
	if err != nil {
		return nil, err
	}
	return &conn, nil
}

func (c *cli) rejectProjectOverrides(fs *flag.FlagSet) error {
	var conflict string
	fs.Visit(func(f *flag.Flag) {
		if conflict == "" && slices.Contains(manualConnectionFlags, f.Name) {
			conflict = f.Name
		}
	})
	if conflict != "" {
		return usageErrorf("--project cannot be combined with --%s: the project supplies its own API URL, workspace and credential", conflict)
	}
	return nil
}

// projectWorkspaceArg enforces the positional workspace rule: forbidden with
// --project, which supplies it, and required without.
func projectWorkspaceArg(pf *projectFlag, pos []string) (string, error) {
	if pf.set {
		if len(pos) != 0 {
			return "", usageErrorf("<ws> cannot be combined with --project: the project supplies its workspace")
		}
		return "", nil
	}
	if len(pos) != 1 {
		return "", usageErrorf("a <ws> argument (or --project PATH) is required")
	}
	return pos[0], nil
}

// managedProject initializes the local manager and enrolls new directories.
// Explicitly revoked, missing or replaced grants are never repaired implicitly.
func (c *cli) managedProject(ctx context.Context, path string) (string, hostmanager.Project, error) {
	root, err := c.managerRoot()
	if err != nil {
		return "", hostmanager.Project{}, err
	}
	if _, err := hostmanager.LoadConfig(root); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return root, hostmanager.Project{}, err
		}
		quiet := *c
		quiet.stdout = io.Discard
		if err := quiet.managerInit(ctx, nil); err != nil {
			return root, hostmanager.Project{}, fmt.Errorf("initialize the host manager: %w", err)
		}
	}
	if err := hostmanager.EnsureRunning(ctx, root); err != nil {
		return root, hostmanager.Project{}, fmt.Errorf("start the host manager: %w", err)
	}
	p, err := hostmanager.ResolveProject(root, path)
	if errors.Is(err, hostmanager.ErrProjectNotAuthorized) {
		resp, enrollErr := hostmanager.Call(ctx, root, hostmanager.AdminTokenFile(root), hostmanager.Request{Action: hostmanager.ActionEnroll, Path: path})
		if enrollErr != nil {
			return root, hostmanager.Project{}, fmt.Errorf("enroll project %s: %w", path, enrollErr)
		}
		if resp.Project == nil {
			return root, hostmanager.Project{}, errors.New("manager returned no enrolled project")
		}
		// Resolve from the persisted registry rather than trusting an unverified
		// response to choose the project credential or Gateway origin.
		p, err = hostmanager.ResolveProject(root, path)
	}
	return root, p, err
}

// projectConnection pins the project's complete connection and starts it when
// needed unless explicit-start policy is enabled. Stdout stays protocol-clean.
func (c *cli) projectConnection(ctx context.Context, path string) (hostmanager.Connection, error) {
	root, p, err := c.managedProject(ctx, path)
	if err != nil {
		return hostmanager.Connection{}, err
	}
	cfg, err := hostmanager.LoadConfig(root)
	if err != nil {
		return hostmanager.Connection{}, err
	}
	if !cfg.Policy.RequireExplicitStart {
		resp, err := hostmanager.Call(ctx, root, p.TokenFile, hostmanager.Request{Action: hostmanager.ActionEnsure, ProjectID: p.ID})
		if err != nil {
			return hostmanager.Connection{}, err
		}
		resp, err = c.waitProjectResult(ctx, root, p.TokenFile, p, resp, 0, true)
		if err != nil {
			return hostmanager.Connection{}, err
		}
		return *resp.Connection, nil
	}
	resp, err := hostmanager.Call(ctx, root, p.TokenFile, hostmanager.Request{Action: hostmanager.ActionConnect, ProjectID: p.ID})
	if err != nil {
		return hostmanager.Connection{}, fmt.Errorf("connect to project %s: %w (explicit-start policy: run lensdock project up --project %q)", p.Root, err, p.Root)
	}
	return checkProjectConnection(p, resp)
}

// checkProjectConnection accepts a manager answer only when it describes the
// project that was asked for, on its stable loopback origin, with that
// project's exact Gateway agent credential location.
func checkProjectConnection(p hostmanager.Project, resp hostmanager.Response) (hostmanager.Connection, error) {
	conn := resp.Connection
	switch {
	case conn == nil:
		return hostmanager.Connection{}, fmt.Errorf("the host manager returned no connection for project %s", p.Root)
	case conn.ProjectID != p.ID:
		return hostmanager.Connection{}, fmt.Errorf("the host manager answered for project %q, not %q", conn.ProjectID, p.ID)
	case conn.Workspace == "" || conn.Workspace != p.Workspace:
		return hostmanager.Connection{}, fmt.Errorf("the host manager returned workspace %q for project %s, want %q", conn.Workspace, p.Root, p.Workspace)
	}
	api, err := normalizeAPI(conn.API)
	if err != nil {
		return hostmanager.Connection{}, fmt.Errorf("the host manager returned an unusable API URL: %w", err)
	}
	origin, err := normalizeAPI(p.Origin)
	if err != nil {
		return hostmanager.Connection{}, fmt.Errorf("project %s has an unusable registered origin: %w", p.Root, err)
	}
	if api != origin {
		return hostmanager.Connection{}, fmt.Errorf("the host manager returned API %s for project %s, want its registered origin %s", api, p.Root, origin)
	}
	if !filepath.IsAbs(conn.TokenFile) {
		return hostmanager.Connection{}, errors.New("the host manager returned a relative credential file location")
	}
	if filepath.Clean(conn.TokenFile) != filepath.Join(p.StateDir, "product", "agent-token") {
		return hostmanager.Connection{}, errors.New("the host manager returned a Gateway credential outside the project's product directory")
	}
	return hostmanager.Connection{ProjectID: p.ID, API: api, Workspace: p.Workspace, TokenFile: filepath.Clean(conn.TokenFile)}, nil
}

// normalizeAPI returns the form apiclient writes into lease files, so URLs
// compare equal exactly when they address the same endpoint.
func normalizeAPI(base string) (string, error) {
	client, err := apiclient.New(base, nil)
	if err != nil {
		return "", err
	}
	return client.BaseURL(), nil
}

// checkLeaseFileProject makes a lease file usable with --project only when it
// was issued by that project's Gateway for its workspace. The comparison
// precedes every Gateway request and never rewrites the file's API URL.
func (c *cli) checkLeaseFileProject(ctx context.Context, fs *flag.FlagSet, pf *projectFlag, lf apiclient.LeaseFile) error {
	conn, err := c.resolveProjectFlag(ctx, fs, pf)
	if err != nil || conn == nil {
		return err
	}
	api, err := normalizeAPI(lf.APIBaseURL)
	if err != nil || api != conn.API || lf.WorkspaceID != conn.Workspace {
		return leaseErrorf("the lease file belongs to workspace %q at %s, not to the project (workspace %q at %s)",
			lf.WorkspaceID, lf.APIBaseURL, conn.Workspace, conn.API)
	}
	return nil
}
