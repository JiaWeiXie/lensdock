package capture

import (
	"context"
	"errors"
	"fmt"
	"time"
)

const (
	// restorePollInterval paces the wait for a DevTools endpoint that is not
	// answering (the supervisor may be restarting Chromium) and the wait for
	// the window manager to confirm a restored window.
	restorePollInterval = 250 * time.Millisecond
	// restoreSettle bounds the wait for a restored window to leave the
	// minimized state.
	restoreSettle = 3 * time.Second

	windowStateMinimized = "minimized"
	restoreBlankURL      = "about:blank"
)

// ErrBrowserUnavailable reports a DevTools endpoint that did not answer
// before the caller's deadline. RestoreWindow never starts Chromium: when the
// browser is down the supervisor owns the relaunch and its Profile lock.
var ErrBrowserUnavailable = errors.New("capture: browser DevTools endpoint is not reachable")

// ErrRestoreFailed reports a window that stayed minimized after the restore request.
var ErrRestoreFailed = errors.New("capture: browser window did not leave the minimized state")

// RestoreOutcome says what RestoreWindow did.
type RestoreOutcome string

const (
	// RestoredExisting: an existing browser window was un-minimized (when
	// needed) and activated. Tabs and the Profile are untouched.
	RestoredExisting RestoreOutcome = "restored-existing-window"
	// OpenedNewWindow: Chromium was alive without any window, so a new
	// about:blank window was created in the same process.
	OpenedNewWindow RestoreOutcome = "opened-new-window"
)

// windowForTarget is the result of Browser.getWindowForTarget.
type windowForTarget struct {
	WindowID int `json:"windowId"`
	Bounds   struct {
		WindowState string `json:"windowState"`
	} `json:"bounds"`
}

// RestoreWindow makes the running browser visible again after its last window
// was minimized or closed. It talks only to the live DevTools endpoint: it
// never launches or signals Chromium, so it cannot race the supervisor's
// relaunch or the Profile lock. While the endpoint is down it polls until ctx
// ends and then reports ErrBrowserUnavailable.
//
// Existing windows are restored with Browser.setWindowBounds and the most
// recent page is activated, which also asks the window manager to map it.
// When no page has a window, one new about:blank window is created.
func (c *Capturer) RestoreWindow(ctx context.Context) (RestoreOutcome, error) {
	wsPath, err := c.awaitBrowser(ctx)
	if err != nil {
		return "", err
	}
	conn, err := dialCDP(ctx, c.client, c.endpoint, wsPath)
	if err != nil {
		return "", err
	}
	defer conn.close()

	// Right before acting, so a restart in between is noticed by the calls below.
	pages, err := c.pages(ctx)
	if err != nil {
		return "", err
	}
	for _, p := range pages {
		var window windowForTarget
		if err := c.callBrowser(ctx, conn, "Browser.getWindowForTarget", map[string]any{"targetId": p.ID}, &window); err != nil {
			var protocolError *cdpError
			if errors.As(err, &protocolError) && protocolError.Code == -32000 &&
				(protocolError.Message == "No window for target" || protocolError.Message == "No target with given id found") {
				continue
			}
			return "", fmt.Errorf("find browser window: %w", err)
		}
		if window.Bounds.WindowState == windowStateMinimized {
			if err := c.callBrowser(ctx, conn, "Browser.setWindowBounds", map[string]any{
				"windowId": window.WindowID,
				"bounds":   map[string]any{"windowState": "normal"},
			}, nil); err != nil {
				return "", fmt.Errorf("restore window %d: %w", window.WindowID, err)
			}
		}
		if err := c.callBrowser(ctx, conn, "Target.activateTarget", map[string]any{"targetId": p.ID}, nil); err != nil {
			return "", fmt.Errorf("activate target: %w", err)
		}
		if err := c.awaitNotMinimized(ctx, conn, p.ID); err != nil {
			return "", err
		}
		return RestoredExisting, nil
	}
	if err := c.openNewWindow(ctx, conn); err != nil {
		return "", err
	}
	return OpenedNewWindow, nil
}

// awaitBrowser polls /json/version until Chromium answers or ctx ends, and
// returns the browser-level DevTools WebSocket path.
func (c *Capturer) awaitBrowser(ctx context.Context) (string, error) {
	for {
		_, wsPath, err := c.Version(ctx)
		if err == nil {
			if wsPath == "" {
				return "", errors.New("capture: browser reports no DevTools WebSocket")
			}
			return wsPath, nil
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("%w: %w", ErrBrowserUnavailable, err)
		case <-time.After(restorePollInterval):
		}
	}
}

func (c *Capturer) openNewWindow(ctx context.Context, conn *cdpConn) error {
	var res struct {
		TargetID string `json:"targetId"`
	}
	err := c.callBrowser(ctx, conn, "Target.createTarget", map[string]any{
		"url":       restoreBlankURL,
		"newWindow": true,
	}, &res)
	if err != nil {
		return fmt.Errorf("open new window: %w", err)
	}
	if err := c.callBrowser(ctx, conn, "Target.activateTarget", map[string]any{"targetId": res.TargetID}, nil); err != nil {
		return fmt.Errorf("activate new window: %w", err)
	}
	return c.awaitNotMinimized(ctx, conn, res.TargetID)
}

// awaitNotMinimized waits for the window of target to report a state other
// than minimized.
func (c *Capturer) awaitNotMinimized(ctx context.Context, conn *cdpConn, target string) error {
	ctx, cancel := context.WithTimeout(ctx, restoreSettle)
	defer cancel()
	for {
		var w windowForTarget
		if err := c.callBrowser(ctx, conn, "Browser.getWindowForTarget", map[string]any{"targetId": target}, &w); err != nil {
			return fmt.Errorf("verify restored window: %w", err)
		}
		switch w.Bounds.WindowState {
		case "normal", "maximized", "fullscreen":
			return nil
		case windowStateMinimized:
		default:
			return fmt.Errorf("verify restored window: unknown window state %q", w.Bounds.WindowState)
		}
		select {
		case <-ctx.Done():
			return ErrRestoreFailed
		case <-time.After(restorePollInterval):
		}
	}
}

// callBrowser runs one browser-level command bounded by the call timeout.
func (c *Capturer) callBrowser(ctx context.Context, conn *cdpConn, method string, params, out any) error {
	ctx, cancel := context.WithTimeout(ctx, c.callTO)
	defer cancel()
	return conn.call(ctx, method, params, out)
}
