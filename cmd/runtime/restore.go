package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/JiaWeiXie/lensdock/internal/capture"
)

const (
	// restoreBrowserTimeout bounds one invocation, including the wait for a
	// Chromium that the supervisor is still restarting.
	restoreBrowserTimeout = 10 * time.Second
	restoreCallTimeout    = 3 * time.Second
)

// restoreBrowser is the fixed action behind the desktop menu item "Restore
// browser". It only talks to the running Chromium's DevTools endpoint; it
// never starts a browser, so a restart in progress stays with the supervisor
// and the Profile lock.
func restoreBrowser(ctx context.Context, endpoint string, stdout io.Writer) error {
	ctx, cancel := context.WithTimeout(ctx, restoreBrowserTimeout)
	defer cancel()
	c, err := capture.New(capture.Config{Endpoint: endpoint, CallTimeout: restoreCallTimeout})
	if err != nil {
		return err
	}
	outcome, err := c.RestoreWindow(ctx)
	if err != nil {
		if errors.Is(err, capture.ErrBrowserUnavailable) {
			return fmt.Errorf("restore browser: the browser is not running (the supervisor may still be restarting it): %w", err)
		}
		return fmt.Errorf("restore browser: %w", err)
	}
	_, _ = fmt.Fprintf(stdout, "lensdock-runtime: restore browser: %s\n", outcome)
	return nil
}
