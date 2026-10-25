package supervisor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/JiaWeiXie/lensdock/internal/netx"
)

const healthcheckTimeout = 5 * time.Second

// RunHealthcheck performs GET url (a loopback readiness URL such as
// http://127.0.0.1:8080/readyz) and returns nil only for HTTP 200. The
// response body is never surfaced.
func RunHealthcheck(ctx context.Context, rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("healthcheck: invalid url: %w", err)
	}
	if u.Scheme != "http" {
		return errors.New("healthcheck: url must use http")
	}
	if !netx.IsLoopbackHost(u.Hostname()) {
		return errors.New("healthcheck: url must target loopback")
	}

	ctx, cancel := context.WithTimeout(ctx, healthcheckTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return fmt.Errorf("healthcheck: %w", err)
	}
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("healthcheck: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthcheck: status %d", resp.StatusCode)
	}
	return nil
}
