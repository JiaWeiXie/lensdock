package capture

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image/png"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sync"
	"time"

	"github.com/JiaWeiXie/lensdock/internal/auth"
)

const (
	// MaxImageBytes and MaxImagePixels are the screenshot limits of SPEC §18.
	MaxImageBytes  = 32 << 20
	MaxImagePixels = 24_000_000

	maxAttempts       = 2
	maxTargets        = 64
	maxDiscoveryBytes = 4 << 20
	probeConcurrency  = 4

	defaultCallTimeout       = 5 * time.Second
	defaultScreenshotTimeout = 10 * time.Second
	visibilityTimeout        = 1500 * time.Millisecond
)

var (
	// ErrInvalidTarget reports a target ID that is not a plain identifier.
	ErrInvalidTarget = errors.New("capture: invalid target id")
	// ErrTargetNotFound reports a target ID that names no page.
	ErrTargetNotFound = errors.New("capture: target not found")
	// ErrCaptureFailed reports a CDP failure while capturing.
	ErrCaptureFailed = errors.New("capture: viewport capture failed")
	// ErrImageLimit reports a screenshot beyond the size or pixel limit.
	ErrImageLimit = errors.New("capture: screenshot exceeds the size limit")
)

var targetIDRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,256}$`)

// Config configures a Capturer. Zero timeouts take their defaults.
type Config struct {
	// Endpoint is Chromium's DevTools host:port, e.g. 127.0.0.1:9223.
	Endpoint          string
	CallTimeout       time.Duration
	ScreenshotTimeout time.Duration
	Now               func() time.Time
}

// Options are the per-capture choices of the caller.
type Options struct {
	// Activate brings the target to the foreground before the screenshot.
	// Without it a tab hidden behind another may not render a frame and the
	// capture fails instead of silently taking focus.
	Activate bool
}

// Capturer talks to Chromium's DevTools endpoint on behalf of the runtime.
type Capturer struct {
	endpoint string
	client   *http.Client
	callTO   time.Duration
	shotTO   time.Duration
	now      func() time.Time
}

// New validates cfg and returns a Capturer.
func New(cfg Config) (*Capturer, error) {
	host, port, err := net.SplitHostPort(cfg.Endpoint)
	if err != nil || host == "" || port == "" {
		return nil, errors.New("capture: Endpoint must be host:port")
	}
	c := &Capturer{
		endpoint: cfg.Endpoint,
		callTO:   cfg.CallTimeout,
		shotTO:   cfg.ScreenshotTimeout,
		now:      cfg.Now,
		client: &http.Client{
			Transport: &http.Transport{
				Proxy:                 nil,
				DialContext:           (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
				DisableCompression:    true,
				MaxIdleConns:          2,
				IdleConnTimeout:       30 * time.Second,
				ResponseHeaderTimeout: 10 * time.Second,
			},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
	if c.callTO <= 0 {
		c.callTO = defaultCallTimeout
	}
	if c.shotTO <= 0 {
		c.shotTO = defaultScreenshotTimeout
	}
	if c.now == nil {
		c.now = time.Now
	}
	return c, nil
}

type rawTarget struct {
	ID    string `json:"id"`
	Type  string `json:"type"`
	Title string `json:"title"`
	URL   string `json:"url"`
}

func (c *Capturer) getJSON(ctx context.Context, path string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, c.callTO)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+c.endpoint+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("devtools %s: status %d", path, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxDiscoveryBytes+1))
	if err != nil {
		return err
	}
	if len(body) > maxDiscoveryBytes {
		return fmt.Errorf("devtools %s: response too large", path)
	}
	return json.Unmarshal(body, out)
}

func (c *Capturer) pages(ctx context.Context) ([]rawTarget, error) {
	var all []rawTarget
	if err := c.getJSON(ctx, "/json/list", &all); err != nil {
		return nil, fmt.Errorf("list targets: %w", err)
	}
	pages := all[:0:0]
	for _, t := range all {
		if t.Type == "page" && targetIDRE.MatchString(t.ID) {
			pages = append(pages, t)
		}
	}
	return pages, nil
}

// Version returns Chromium's Browser string and the path of its browser-level
// DevTools WebSocket.
func (c *Capturer) Version(ctx context.Context) (browser, browserWSPath string, err error) {
	var v struct {
		Browser              string `json:"Browser"`
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	if err := c.getJSON(ctx, "/json/version", &v); err != nil {
		return "", "", fmt.Errorf("browser version: %w", err)
	}
	if u, perr := url.Parse(v.WebSocketDebuggerURL); perr == nil {
		browserWSPath = u.Path
	}
	return v.Browser, browserWSPath, nil
}

// ListTargets returns the browser's page targets with a visibility probe each.
func (c *Capturer) ListTargets(ctx context.Context) ([]Target, error) {
	pages, err := c.pages(ctx)
	if err != nil {
		return nil, err
	}
	if len(pages) > maxTargets {
		pages = pages[:maxTargets]
	}
	out := make([]Target, len(pages))
	var wg sync.WaitGroup
	sem := make(chan struct{}, probeConcurrency)
	for i, p := range pages {
		out[i] = Target{ID: p.ID, Type: p.Type, Title: p.Title, URL: p.URL}
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			out[i].Visible = c.probeVisible(ctx, p.ID)
		}()
	}
	wg.Wait()
	return out, nil
}

// probeVisible reports document.visibilityState == "visible", or nil when the
// target cannot be probed in time.
func (c *Capturer) probeVisible(ctx context.Context, id string) *bool {
	ctx, cancel := context.WithTimeout(ctx, visibilityTimeout)
	defer cancel()
	conn, err := dialCDP(ctx, c.client, c.endpoint, "/devtools/page/"+id)
	if err != nil {
		return nil
	}
	defer conn.close()
	var res struct {
		Result struct {
			Value string `json:"value"`
		} `json:"result"`
		ExceptionDetails json.RawMessage `json:"exceptionDetails"`
	}
	err = conn.call(ctx, "Runtime.evaluate", map[string]any{
		"expression":    "document.visibilityState",
		"returnByValue": true,
		"timeout":       1000,
	}, &res)
	if err != nil || len(res.ExceptionDetails) > 0 || res.Result.Value == "" {
		return nil
	}
	v := res.Result.Value == "visible"
	return &v
}

// CloseBrowser requests normal Chromium teardown through Browser.close so it
// commits profile state and removes Singleton locks. SIGTERM's SessionEnding
// path exits immediately after writing session data, bypassing lock cleanup.
// Chromium may close the WebSocket before replying; the caller waits for the
// process before falling back to signals.
func (c *Capturer) CloseBrowser(ctx context.Context) error {
	_, wsPath, err := c.Version(ctx)
	if err != nil {
		return err
	}
	if wsPath == "" {
		return errors.New("capture: browser reports no DevTools WebSocket")
	}
	ctx, cancel := context.WithTimeout(ctx, c.callTO)
	defer cancel()
	conn, err := dialCDP(ctx, c.client, c.endpoint, wsPath)
	if err != nil {
		return err
	}
	defer conn.close()
	return conn.call(ctx, "Browser.close", nil, nil)
}

// Capture takes a page-viewport PNG of the explicit target. It observes the
// target before and after the screenshot; when the document changed in
// between it retries once, and reports the last attempt's consistency. The
// caller must already have fenced every other controller (CAPTURE state).
func (c *Capturer) Capture(ctx context.Context, targetID string, opts Options) (Result, []byte, error) {
	if !targetIDRE.MatchString(targetID) {
		return Result{}, nil, ErrInvalidTarget
	}
	browser, _, err := c.Version(ctx)
	if err != nil {
		return Result{}, nil, fmt.Errorf("%w: %w", ErrCaptureFailed, err)
	}
	suffix, err := auth.RandomHex(8)
	if err != nil {
		return Result{}, nil, err
	}
	var (
		res  Result
		data []byte
	)
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		pre, post, img, postOK, err := c.attempt(ctx, targetID, opts)
		if err != nil {
			return Result{}, nil, err
		}
		cfg, err := decodeConfig(img)
		if err != nil {
			return Result{}, nil, err
		}
		sum := sha256.Sum256(img)
		res = Result{
			CaptureID:      "rcap_" + suffix,
			Source:         Source,
			TargetID:       targetID,
			NavigationID:   fmt.Sprintf("%s:%d", post.LoaderID, post.NavigationEntryID),
			Consistency:    classify(pre, post, postOK),
			Attempts:       attempt,
			CapturedAt:     c.now().UTC(),
			BrowserVersion: browser,
			Image: Image{
				Width:       cfg.Width,
				Height:      cfg.Height,
				Bytes:       len(img),
				SHA256:      hex.EncodeToString(sum[:]),
				ContentType: "image/png",
			},
			Pre:  pre,
			Post: post,
		}
		if pre.Viewport.CSSWidth > 0 && pre.Viewport.CSSHeight > 0 {
			res.Scale = Scale{
				PixelsPerCSSX: float64(cfg.Width) / pre.Viewport.CSSWidth,
				PixelsPerCSSY: float64(cfg.Height) / pre.Viewport.CSSHeight,
			}
		}
		data = img
		if res.Consistency != ConsistencyUnknown || !postOK {
			break
		}
	}
	return res, data, nil
}

func decodeConfig(img []byte) (cfg struct{ Width, Height int }, err error) {
	c, err := png.DecodeConfig(bytes.NewReader(img))
	if err != nil {
		return cfg, fmt.Errorf("%w: the browser returned an unreadable PNG", ErrCaptureFailed)
	}
	if c.Width <= 0 || c.Height <= 0 || c.Width*c.Height > MaxImagePixels {
		return cfg, ErrImageLimit
	}
	cfg.Width, cfg.Height = c.Width, c.Height
	return cfg, nil
}

// attempt runs one pre-observe / screenshot / post-observe cycle. postOK is
// false when the post observation failed; post then carries only the target
// ID, never made-up values.
func (c *Capturer) attempt(ctx context.Context, id string, opts Options) (pre, post State, img []byte, postOK bool, err error) {
	pages, err := c.pages(ctx)
	if err != nil {
		return State{}, State{}, nil, false, fmt.Errorf("%w: %w", ErrCaptureFailed, err)
	}
	found := false
	for _, p := range pages {
		if p.ID == id {
			found = true
			break
		}
	}
	if !found {
		return State{}, State{}, nil, false, ErrTargetNotFound
	}

	dialCtx, cancel := context.WithTimeout(ctx, c.callTO)
	conn, err := dialCDP(dialCtx, c.client, c.endpoint, "/devtools/page/"+id)
	cancel()
	if err != nil {
		return State{}, State{}, nil, false, fmt.Errorf("%w: %w", ErrCaptureFailed, err)
	}
	defer conn.close()

	if pre, err = c.observe(ctx, conn, id); err != nil {
		return State{}, State{}, nil, false, fmt.Errorf("%w: observe before capture: %w", ErrCaptureFailed, err)
	}
	if opts.Activate {
		cctx, cancel := context.WithTimeout(ctx, c.callTO)
		err := conn.call(cctx, "Page.bringToFront", nil, nil)
		cancel()
		if err != nil {
			return State{}, State{}, nil, false, fmt.Errorf("%w: activate target: %w", ErrCaptureFailed, err)
		}
	}

	shotCtx, cancel := context.WithTimeout(ctx, c.shotTO)
	var shot struct {
		Data string `json:"data"`
	}
	err = conn.call(shotCtx, "Page.captureScreenshot", map[string]any{
		"format":                "png",
		"fromSurface":           true,
		"captureBeyondViewport": false,
	}, &shot)
	cancel()
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) && !opts.Activate {
			return State{}, State{}, nil, false, fmt.Errorf("%w: no frame within %s; a background tab does not render, retry with activate", ErrCaptureFailed, c.shotTO)
		}
		return State{}, State{}, nil, false, fmt.Errorf("%w: %w", ErrCaptureFailed, err)
	}
	if len(shot.Data) > base64.StdEncoding.EncodedLen(MaxImageBytes) {
		return State{}, State{}, nil, false, ErrImageLimit
	}
	img, err = base64.StdEncoding.DecodeString(shot.Data)
	if err != nil {
		return State{}, State{}, nil, false, fmt.Errorf("%w: the browser returned invalid image data", ErrCaptureFailed)
	}
	if len(img) > MaxImageBytes {
		return State{}, State{}, nil, false, ErrImageLimit
	}

	post, perr := c.observe(ctx, conn, id)
	if perr != nil {
		return pre, State{TargetID: id}, img, false, nil
	}
	return pre, post, img, true, nil
}

// observe reads the navigation, viewport and scroll state of the target.
func (c *Capturer) observe(ctx context.Context, conn *cdpConn, id string) (State, error) {
	call := func(method string, params, out any) error {
		cctx, cancel := context.WithTimeout(ctx, c.callTO)
		defer cancel()
		return conn.call(cctx, method, params, out)
	}

	var ft struct {
		FrameTree struct {
			Frame struct {
				LoaderID string `json:"loaderId"`
				URL      string `json:"url"`
			} `json:"frame"`
		} `json:"frameTree"`
	}
	if err := call("Page.getFrameTree", nil, &ft); err != nil {
		return State{}, err
	}
	var nh struct {
		CurrentIndex int `json:"currentIndex"`
		Entries      []struct {
			ID    int    `json:"id"`
			Title string `json:"title"`
		} `json:"entries"`
	}
	if err := call("Page.getNavigationHistory", nil, &nh); err != nil {
		return State{}, err
	}
	if nh.CurrentIndex < 0 || nh.CurrentIndex >= len(nh.Entries) {
		return State{}, errors.New("navigation history has no current entry")
	}
	var lm struct {
		Layout struct {
			PageX float64 `json:"pageX"`
			PageY float64 `json:"pageY"`
		} `json:"cssLayoutViewport"`
		Visual struct {
			ClientWidth  float64 `json:"clientWidth"`
			ClientHeight float64 `json:"clientHeight"`
			Scale        float64 `json:"scale"`
			Zoom         float64 `json:"zoom"`
		} `json:"cssVisualViewport"`
	}
	if err := call("Page.getLayoutMetrics", nil, &lm); err != nil {
		return State{}, err
	}
	var dpr struct {
		Result struct {
			Value float64 `json:"value"`
		} `json:"result"`
	}
	if err := call("Runtime.evaluate", map[string]any{
		"expression":    "window.devicePixelRatio",
		"returnByValue": true,
		"timeout":       1000,
	}, &dpr); err != nil {
		return State{}, err
	}
	entry := nh.Entries[nh.CurrentIndex]
	return State{
		TargetID:          id,
		URL:               ft.FrameTree.Frame.URL,
		Title:             entry.Title,
		NavigationEntryID: entry.ID,
		LoaderID:          ft.FrameTree.Frame.LoaderID,
		DevicePixelRatio:  dpr.Result.Value,
		Viewport: Viewport{
			CSSWidth:  lm.Visual.ClientWidth,
			CSSHeight: lm.Visual.ClientHeight,
			Scale:     lm.Visual.Scale,
			Zoom:      lm.Visual.Zoom,
		},
		Scroll: Scroll{X: lm.Layout.PageX, Y: lm.Layout.PageY},
	}, nil
}

// classify grades the consistency of a capture from its two observations.
func classify(pre, post State, postOK bool) string {
	if !postOK {
		return ConsistencyUnknown
	}
	if pre.TargetID != post.TargetID || pre.LoaderID != post.LoaderID || pre.NavigationEntryID != post.NavigationEntryID {
		return ConsistencyUnknown
	}
	if pre.Viewport != post.Viewport || pre.Scroll != post.Scroll || pre.DevicePixelRatio != post.DevicePixelRatio {
		return ConsistencyBestEffort
	}
	return ConsistencyStable
}
