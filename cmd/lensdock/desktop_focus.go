package main

import (
	"context"
	"errors"
	"net/url"

	"github.com/JiaWeiXie/lensdock/internal/cdp"
)

type desktopFocusResult struct {
	Role     string `json:"role"`
	Name     string `json:"name"`
	Selector string `json:"selector"`
}

func (c *cli) desktopFocus(ctx context.Context, args []string) error {
	fs := c.newFlagSet("desktop focus")
	af := c.addAgentLeaseFlags(fs)
	if _, err := parse(fs, args, 0, 0); err != nil {
		return err
	}
	leaseCtx, l, err := c.openAgentLease(ctx, af)
	if err != nil {
		return err
	}
	defer l.close()

	upstream, err := url.Parse(l.client.BaseURL() + "/api/v1/workspaces/" + url.PathEscape(l.lf.WorkspaceID) + "/cdp")
	if err != nil {
		return usageErrorf("invalid workspace id in lease file")
	}
	result, err := cdp.ReadFocusedElement(leaseCtx, cdp.FocusConfig{
		Upstream:          upstream,
		Token:             l.lf.CapabilityToken,
		Epoch:             l.lf.Lease.Epoch,
		RuntimeGeneration: l.lf.Lease.RuntimeGeneration,
	})
	if err != nil {
		if errors.Is(err, cdp.ErrLeaseLost) {
			return leaseErrorf("lease lost: the runtime ended or invalidated the focus read")
		}
		return l.explain(err)
	}
	return c.writeJSONLine(desktopFocusResult{Role: result.Role, Name: result.Name, Selector: result.Selector})
}
